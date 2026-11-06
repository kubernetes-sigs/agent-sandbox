// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agtsbx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// healthPollInterval is how often waitHealthy retries. sandboxd starts in
// well under a second locally, so poll briskly.
const healthPollInterval = 200 * time.Millisecond

// waitHealthy blocks until sandboxd's REST readiness probe returns 200 or ctx
// is done, taking its deadline from ctx so it shares one startup budget with
// whatever ran before it.
//
// REST rather than a gRPC dial: with published container ports the engine's
// proxy binds the port before sandboxd is up, so a TCP probe would report
// ready too early and the first RPC would fail.
func waitHealthy(ctx context.Context, client *http.Client, restAddr string) error {
	url := "http://" + restAddr + "/v1/health"
	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	// lastErr lets a timeout explain what was actually going wrong.
	start := time.Now()
	var lastErr error
	for {
		if err := probeHealth(ctx, client, url); err != nil {
			lastErr = err
		} else {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox did not become healthy at %s after %s: %w", restAddr, time.Since(start).Round(time.Second), lastErr)
		case <-ticker.C:
		}
	}
}

// probeHealth performs one readiness request.
func probeHealth(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused across polls.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// execRequest is a single command to run inside a started sandbox.
type execRequest struct {
	// Command is the argv to execute.
	Command []string
	// Env is the per-command environment, as "KEY=VALUE".
	Env []string
	// Workdir is resolved inside the sandbox root. Empty means the root.
	Workdir string
	// Stdout and Stderr receive the process output as it streams.
	Stdout io.Writer
	Stderr io.Writer
}

// runCommand executes req against sandboxd at grpcAddr and returns the
// command's exit code. It uses the streaming ProcessService.Start rather than
// the unary Execute so output reaches the terminal as it is produced, and it
// is the only place the command's environment travels.
func runCommand(ctx context.Context, grpcAddr string, req execRequest) (int, error) {
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return 0, fmt.Errorf("connecting to sandboxd at %s: %w", grpcAddr, err)
	}
	defer conn.Close()

	config := &processv1.ProcessConfig{
		Command: req.Command,
		EnvVars: parseEnv(req.Env),
	}
	if req.Workdir != "" {
		config.Cwd = &req.Workdir
	}

	// A separate cancelable context for the stream lets a failed stdin EOF
	// tear the stream down instead of leaving copyStream waiting on a
	// process that will never see EOF and never exits.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	client := processv1.NewProcessServiceClient(conn)
	stream, err := client.Start(streamCtx, &processv1.StartRequest{Config: config})
	if err != nil {
		return 0, annotateWorkdir(fmt.Errorf("starting command in sandbox: %w", err), req.Workdir)
	}

	// This path never forwards the caller's stdin, so closing it as soon as
	// the process exists is what tells a command reading from it (cat, for
	// example) to stop rather than block forever.
	onInit := func(processID int32) error {
		if err := sendStdinEOF(ctx, client, processID); err != nil {
			cancelStream()
			return fmt.Errorf("sending stdin EOF: %w", err)
		}
		return nil
	}

	exitCode, err := copyStream(stream, req.Stdout, req.Stderr, onInit)
	return exitCode, annotateWorkdir(err, req.Workdir)
}

// stdinWriter is the WriteStdin half of the ProcessService client, narrowed
// so sendStdinEOF can be tested without a gRPC server.
type stdinWriter interface {
	WriteStdin(ctx context.Context, in *processv1.WriteStdinRequest, opts ...grpc.CallOption) (*processv1.WriteStdinResponse, error)
}

// sendStdinEOF closes the command's stdin. A command that exits before the
// EOF arrives has already been dropped from sandboxd's registry, so NotFound
// means there is no stdin left to close, not a failure. Any other error is
// returned because the command may still be waiting on its stdin.
func sendStdinEOF(ctx context.Context, client stdinWriter, processID int32) error {
	_, err := client.WriteStdin(ctx, &processv1.WriteStdinRequest{
		ProcessId: processID,
		Payload:   &processv1.WriteStdinRequest_Eof{Eof: &emptypb.Empty{}},
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// annotateWorkdir adds a hint to NotFound errors raised under a --workdir.
// sandboxd confines cwd to the sandbox root, so a host path such as /tmp
// resolves somewhere that does not exist, and the failed chdir is reported as
// if the command binary were missing.
func annotateWorkdir(err error, workdir string) error {
	if err == nil || workdir == "" {
		return err
	}
	if status.Code(err) != codes.NotFound && !strings.Contains(err.Error(), "NotFound") {
		return err
	}
	return fmt.Errorf("%w (note: --workdir %q is resolved inside the sandbox root and must already exist there)", err, workdir)
}

// startStream is the receive side of ProcessService.Start, narrowed so the
// stream handling can be tested without a gRPC server.
type startStream interface {
	Recv() (*processv1.StartResponse, error)
}

// copyStream drains a Start stream, forwarding output to stdout/stderr and
// returning the process exit code. onInit, if not nil, is called once with
// the process ID carried by the stream's Init event; an error from onInit
// stops the stream immediately instead of waiting for more events that may
// never arrive.
func copyStream(stream startStream, stdout, stderr io.Writer, onInit func(processID int32) error) (int, error) {
	// sandboxd closes the stream after the ExitEvent, so a clean EOF without
	// one means the sandbox died mid-command and the status is unknown.
	exitCode := 0
	sawExit := false

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if !sawExit {
				return 0, fmt.Errorf("sandbox closed the command stream before reporting an exit code")
			}
			return exitCode, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reading command output: %w", err)
		}

		switch event := resp.GetEvent().(type) {
		case *processv1.StartResponse_Stdout:
			if _, err := stdout.Write(event.Stdout); err != nil {
				return 0, fmt.Errorf("writing command stdout: %w", err)
			}
		case *processv1.StartResponse_Stderr:
			if _, err := stderr.Write(event.Stderr); err != nil {
				return 0, fmt.Errorf("writing command stderr: %w", err)
			}
		case *processv1.StartResponse_Exit:
			exitCode = int(event.Exit.GetExitCode())
			sawExit = true
		case *processv1.StartResponse_Init:
			if onInit != nil {
				if err := onInit(event.Init.GetProcessId()); err != nil {
					return 0, err
				}
			}
		}
	}
}

// parseEnv converts "KEY=VALUE" strings into the map sandboxd expects.
// Malformed entries are dropped rather than rejected: parseRunArgs already
// validated them. Values may contain "=", so only the first one separates.
func parseEnv(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			continue
		}
		out[key] = value
	}
	return out
}
