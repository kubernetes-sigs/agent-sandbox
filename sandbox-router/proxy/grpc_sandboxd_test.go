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

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sandboxd "sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func TestGRPCSandboxdProcessInteraction(t *testing.T) {
	daemon, err := sandboxd.New(sandboxd.Options{RootDir: t.TempDir(), Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := grpc.NewServer()
	daemon.RegisterGRPC(backend)
	go func() { _ = backend.Serve(ln) }()
	t.Cleanup(func() { backend.Stop(); daemon.ShutdownProcesses(context.Background()) })
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := processv1.NewProcessServiceClient(grpcRouterConn(t, Options{}, false))
	ctx := grpcTargetContext(t, host, port)
	res, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "printf hello; printf warning >&2; exit 7"}}})
	if err != nil || string(res.GetStdout()) != "hello" || string(res.GetStderr()) != "warning" || res.GetExitCode() != 7 {
		t.Fatalf("Execute: %v, %v", res, err)
	}
	stream, err := client.Start(ctx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "printf ready; read line; printf 'input:%s' \"$line\""}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetInit() == nil {
		t.Fatalf("init: %v, %v", first, err)
	}
	pid := first.GetInit().GetProcessId()
	ready, err := stream.Recv()
	if err != nil || string(ready.GetStdout()) != "ready" {
		t.Fatalf("output before stdin: %v, %v", ready, err)
	}
	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{ProcessId: pid, Payload: &processv1.WriteStdinRequest_Input{Input: []byte("router\n")}})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	var exited bool
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		output.Write(event.GetStdout())
		if event.GetExit() != nil {
			exited = true
			if event.GetExit().GetExitCode() != 0 {
				t.Fatal(event.GetExit())
			}
		}
	}
	if !exited || output.String() != "input:router" {
		t.Fatalf("exit=%v output=%q", exited, output.String())
	}
	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: pid, Signal: processv1.Signal_SIGNAL_SIGTERM})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("backend error should survive: %v", err)
	}

	ctx = grpcTargetContext(t, host, port)
	ptyStream, err := client.Start(ctx, &processv1.StartRequest{
		Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "printf ready; read line"}},
		Pty:    &processv1.PTY{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	init, err := ptyStream.Recv()
	if err != nil || init.GetInit() == nil {
		t.Fatalf("PTY init: %v, %v", init, err)
	}
	pid = init.GetInit().GetProcessId()
	if _, err := client.ResizeTTY(ctx, &processv1.ResizeTTYRequest{ProcessId: pid, Cols: 100, Rows: 40}); err != nil {
		t.Fatalf("ResizeTTY: %v", err)
	}
	if _, err := client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: pid, Signal: processv1.Signal_SIGNAL_SIGKILL}); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}
	exited = false
	for {
		event, err := ptyStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.GetExit() != nil {
			exited = true
			if event.GetExit().GetExitCode() == 0 {
				t.Fatal("SIGKILL should produce a nonzero process exit")
			}
		}
	}
	if !exited {
		t.Fatal("signal lost the process exit event")
	}
}
