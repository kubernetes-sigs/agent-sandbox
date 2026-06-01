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
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/api/workloadapi"
)

const socketPath = "/var/run/tool-agent.sock"

func main() {
	log.Println("Starting tool-agent...")

	// Handle standard shutdown signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	log := klog.FromContext(ctx)

	// Ensure the parent directory of the socket exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for socket: %w", err)
	}

	// Remove any existing socket file
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket file: %w", err)
	}

	// Listen on Unix Domain Socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	defer listener.Close()

	// Chmod socket so any user in the container can execute sudo via UDS
	if err := os.Chmod(socketPath, 0666); err != nil {
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	log.Info("Listening on Unix Domain Socket", "path", socketPath)

	// Accept connections in a goroutine to respect context cancellation
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Error(err, "Failed to accept UDS connection")
					continue
				}
			}
			go func() {
				udsConn := newUDSConnection(conn)
				defer udsConn.Close()

				handleConnection(ctx, udsConn)
			}()
		}
	}()

	// Wait for termination signal
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

type udsConnection struct {
	conn net.Conn

	mu     sync.Mutex
	writer *json.Encoder
	reader *json.Decoder
}

func (c *udsConnection) Close() error {
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			return err
		}
	}
	return nil
}

func newUDSConnection(conn net.Conn) *udsConnection {
	c := &udsConnection{}
	c.conn = conn

	c.reader = json.NewDecoder(conn)
	c.writer = json.NewEncoder(conn)

	return c
}

func (c *udsConnection) Read(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reader.Decode(v)
}

func (c *udsConnection) Write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer.Encode(v)
}

func (c *udsConnection) SendError(err error) {
	_ = c.Write(workloadapi.SudoResponse{Error: err.Error()})
}

func handleConnection(ctx context.Context, conn *udsConnection) {
	log := klog.FromContext(ctx)

	var req workloadapi.SudoRequest
	if err := conn.Read(&req); err != nil {
		conn.SendError(fmt.Errorf("failed to decode request: %w", err))
		return
	}

	if len(req.Command) == 0 {
		conn.SendError(errors.New("request contains empty command"))
		return
	}

	// Validate against the carefully curated allowlist of commands
	parsed := parseAllowedCommand(req.Command)
	if parsed == nil {
		conn.SendError(fmt.Errorf("command not allowed: %v", req.Command))
		return
	}

	log.Info("executing allowed command", "command", req.Command)

	runOptions := RunOptions{}
	runOptions.Stdout = &udsWriter{conn: conn, stream: "stdout"}
	runOptions.Stderr = &udsWriter{conn: conn, stream: "stderr"}

	err := parsed.Run(ctx, runOptions)
	exitCode := 0

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			conn.SendError(fmt.Errorf("failed to start process: %w", err))
			return
		}
	}

	// Record installed packages to homedir upon successful execution of apt-get install
	if exitCode == 0 {
		if err := parsed.PostRun(ctx); err != nil {
			log.Error(err, "error from post-run")
		}
	}

	conn.Write(workloadapi.SudoResponse{ExitCode: &exitCode})
}

// parseAllowedCommand checks if the requested command is on the allowlist
func parseAllowedCommand(args []string) Command {
	if len(args) == 0 {
		return nil
	}

	if v := parseAptGetInstall(args); v != nil {
		return v
	}
	if v := parseAptGetUpdate(args); v != nil {
		return v
	}

	return nil
}

// isPackageName returns true if s is a valid package name, false otherwise
// valid package names: contain only lowercase letters, numbers, hyphens, and underscores
// They must start with a letter or number
func isPackageName(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				return false
			}
		} else {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

type RunOptions struct {
	Stdout io.Writer
	Stderr io.Writer
}

type Command interface {
	Run(ctx context.Context, opt RunOptions) error
	PostRun(ctx context.Context) error
}

// udsWriter streams standard output/error back to the UDS client in JSON-lines format
type udsWriter struct {
	conn   *udsConnection
	stream string
}

func (w *udsWriter) Write(p []byte) (n int, err error) {
	resp := workloadapi.SudoResponse{
		Stream: w.stream,
		Data:   string(p),
	}

	if err := w.conn.Write(resp); err != nil {
		return 0, err
	}
	return len(p), nil
}
