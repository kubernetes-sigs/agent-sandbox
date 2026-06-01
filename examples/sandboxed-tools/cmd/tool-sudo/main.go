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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/api/workloadapi"
)

const socketPath = "/var/run/tool-agent.sock"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sudo <command> [args...]")
		os.Exit(1)
	}

	// Handle standard shutdown signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Connect to the tool-agent Unix Domain Socket
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to agent socket at %s: %w", socketPath, err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	// Send the command line arguments as a Request
	req := workloadapi.SudoRequest{
		Command: os.Args[1:],
	}
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("failed to send request to agent: %w", err)
	}

	// Stream responses in real-time
	for {
		var resp workloadapi.SudoResponse
		if err := dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("failed to read response from agent: %w", err)
		}

		if resp.Error != "" {
			fmt.Fprintf(os.Stderr, "%s\n", resp.Error)
			os.Exit(1)
		}

		switch resp.Stream {
		case "stdout":
			_, _ = os.Stdout.Write([]byte(resp.Data))
		case "stderr":
			_, _ = os.Stderr.Write([]byte(resp.Data))
		}

		if resp.ExitCode != nil {
			os.Exit(*resp.ExitCode)
		}
	}
	return nil
}
