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

// This example uses the existing generated sandboxd client, not a new SDK transport.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
)

type options struct {
	Address, Sandbox, Namespace, Mode string
	CAFile, ServerName, TokenFile     string
	SecretFile, PodIP, Input          string
	Port                              int
	Plaintext                         bool
	Timeout, TokenTTL                 time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.Address, "address", "localhost:8443", "Router/Gateway host:port, without a URL scheme")
	flag.StringVar(&opts.Sandbox, "sandbox", "box-a", "X-Sandbox-ID on every RPC")
	flag.StringVar(&opts.Namespace, "namespace", "grpc-router-demo", "X-Sandbox-Namespace on every RPC")
	flag.IntVar(&opts.Port, "port", 9090, "Explicit sandboxd target port, not the router listener")
	flag.StringVar(&opts.PodIP, "pod-ip", "", "Optional X-Sandbox-Pod-IP override; normally use Service/cache discovery")
	flag.StringVar(&opts.Mode, "mode", "execute", "execute, start, interact, signal, or mint-token")
	flag.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Positive total budget for the call, including routing/authentication")
	flag.BoolVar(&opts.Plaintext, "plaintext", false, "Use h2c only on a trusted internal connection")
	flag.StringVar(&opts.CAFile, "ca-file", "", "PEM CA bundle; empty uses system roots")
	flag.StringVar(&opts.ServerName, "server-name", "", "TLS certificate name; empty verifies the address hostname")
	flag.StringVar(&opts.TokenFile, "token-file", "", "Bearer token file (never passed on the command line or logged)")
	flag.StringVar(&opts.SecretFile, "secret-file", "", "Existing scoped-token v1 signing secret; mint-token only")
	flag.DurationVar(&opts.TokenTTL, "token-ttl", 10*time.Minute, "Lifetime of the demo scoped token")
	flag.StringVar(&opts.Input, "stdin", "hello through router", "Line sent after the interact process emits ready")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code, err := run(ctx, opts, flag.Args(), os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if opts.Mode != "mint-token" {
		fmt.Fprintf(os.Stderr, "\nexit_code=%d\n", code)
	}
	if code < 0 || code > 255 {
		code = 1
	}
	os.Exit(code)
}

func run(parent context.Context, opts options, command []string, stdout, stderr io.Writer) (int, error) {
	if opts.Mode == "mint-token" {
		secret, err := os.ReadFile(opts.SecretFile)
		if err != nil {
			return 0, fmt.Errorf("read signing secret: %w", err)
		}
		token, err := authz.MintScopedToken(secret, opts.Namespace, opts.Sandbox, opts.TokenTTL)
		if err != nil {
			return 0, fmt.Errorf("mint demo scoped token: %w", err)
		}
		if _, err := fmt.Fprintln(stdout, token); err != nil {
			return 0, fmt.Errorf("write scoped token: %w", err)
		}
		return 0, nil
	}
	if opts.Timeout <= 0 {
		return 0, fmt.Errorf("--timeout must be positive")
	}
	if opts.Mode != "execute" && opts.Mode != "start" && opts.Mode != "interact" && opts.Mode != "signal" {
		return 0, fmt.Errorf("unknown mode %q", opts.Mode)
	}
	if opts.Plaintext && (opts.CAFile != "" || opts.ServerName != "") {
		return 0, fmt.Errorf("TLS options cannot be used with --plaintext")
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	creds := insecure.NewCredentials()
	if !opts.Plaintext {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: opts.ServerName}
		if opts.CAFile != "" {
			ca, err := os.ReadFile(opts.CAFile)
			if err != nil {
				return 0, fmt.Errorf("read CA bundle: %w", err)
			}
			tlsConfig.RootCAs = x509.NewCertPool()
			if !tlsConfig.RootCAs.AppendCertsFromPEM(ca) {
				return 0, fmt.Errorf("CA bundle contains no certificates")
			}
		}
		creds = credentials.NewTLS(tlsConfig)
	}
	conn, err := grpc.NewClient(opts.Address, grpc.WithTransportCredentials(creds), grpc.WithDisableRetry())
	if err != nil {
		return 0, fmt.Errorf("create gRPC client: %w", err)
	}
	defer conn.Close()
	md := metadata.Pairs("x-sandbox-id", opts.Sandbox, "x-sandbox-namespace", opts.Namespace, "x-sandbox-port", fmt.Sprint(opts.Port))
	if opts.PodIP != "" {
		md.Set("x-sandbox-pod-ip", opts.PodIP)
	}
	if opts.TokenFile != "" {
		token, err := os.ReadFile(opts.TokenFile)
		if err != nil {
			return 0, fmt.Errorf("read bearer token: %w", err)
		}
		md.Set("authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	ctx = metadata.NewOutgoingContext(ctx, md)
	client := processv1.NewProcessServiceClient(conn)
	if len(command) == 0 {
		command = []string{"/bin/sh", "-c", "printf 'hello through router\n'"}
	}
	if opts.Mode == "execute" {
		resp, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: command}})
		if err != nil {
			return 0, fmt.Errorf("execute: %w", err)
		}
		if _, err := stdout.Write(resp.GetStdout()); err != nil {
			return 0, fmt.Errorf("write command stdout: %w", err)
		}
		if _, err := stderr.Write(resp.GetStderr()); err != nil {
			return int(resp.GetExitCode()), fmt.Errorf("write command stderr: %w", err)
		}
		return int(resp.GetExitCode()), nil
	}
	switch opts.Mode {
	case "interact":
		command = []string{"/bin/sh", "-c", "printf 'ready\n'; read line; printf 'input:%s\n' \"$line\""}
	case "signal":
		command = []string{"/bin/sh", "-c", "printf 'ready\n'; sleep 300"}
	}
	stream, err := client.Start(ctx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: command}})
	if err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	var pid int32
	var ready strings.Builder
	var exitCode int
	var exited, controlled bool
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if !exited {
				return 0, fmt.Errorf("start ended without a process exit event")
			}
			return exitCode, nil
		}
		if err != nil {
			return 0, fmt.Errorf("receive Start: %w", err)
		}
		if init := event.GetInit(); init != nil {
			pid = init.GetProcessId()
			if _, err := fmt.Fprintf(stderr, "process_id=%d\n", pid); err != nil {
				return 0, fmt.Errorf("write process ID: %w", err)
			}
		}
		if _, err := stdout.Write(event.GetStdout()); err != nil {
			return 0, fmt.Errorf("write process stdout: %w", err)
		}
		if _, err := stderr.Write(event.GetStderr()); err != nil {
			return 0, fmt.Errorf("write process stderr: %w", err)
		}
		if !controlled && (opts.Mode == "interact" || opts.Mode == "signal") {
			ready.Write(event.GetStdout())
			if pid != 0 && strings.Contains(ready.String(), "ready\n") {
				if opts.Mode == "interact" {
					_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{ProcessId: pid, Payload: &processv1.WriteStdinRequest_Input{Input: []byte(opts.Input + "\n")}})
				} else {
					_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: pid, Signal: processv1.Signal_SIGNAL_SIGTERM})
				}
				if err != nil {
					return 0, fmt.Errorf("control running process: %w", err)
				}
				controlled = true
			}
		}
		if exit := event.GetExit(); exit != nil {
			exited = true
			exitCode = int(exit.GetExitCode())
		}
	}
}
