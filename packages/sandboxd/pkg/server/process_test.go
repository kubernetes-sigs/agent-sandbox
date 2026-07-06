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

package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/processmanager"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type mockStartServer struct {
	grpc.ServerStream
	ctx      context.Context
	events   []*processv1.StartResponse
}

func (m *mockStartServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockStartServer) Send(res *processv1.StartResponse) error {
	m.events = append(m.events, res)
	return nil
}

func TestProcessServerExecute(t *testing.T) {
	tempDir := t.TempDir()
	registry := processmanager.NewProcessRegistry()
	procServer := NewProcessServer(tempDir, registry)
	ctx := context.Background()

	res, err := procServer.Execute(ctx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"echo", "hello sandboxd"},
		},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if res.GetExitCode() != 0 {
		t.Errorf("expected exit code 0, got %d", res.GetExitCode())
	}

	if string(res.GetStdout()) != "hello sandboxd\n" {
		t.Errorf("expected stdout %q, got %q", "hello sandboxd\n", string(res.GetStdout()))
	}
}

func TestProcessServerStart(t *testing.T) {
	tempDir := t.TempDir()
	registry := processmanager.NewProcessRegistry()
	procServer := NewProcessServer(tempDir, registry)

	startStream := &mockStartServer{}
	err := procServer.Start(&processv1.StartRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"echo", "streaming output"},
		},
	}, startStream)

	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if len(startStream.events) < 3 {
		t.Fatalf("expected at least 3 events (Init, Stdout, Exit), got %d", len(startStream.events))
	}

	// Verify Init event
	initEvent := startStream.events[0].GetInit()
	if initEvent == nil || initEvent.GetProcessId() <= 0 {
		t.Errorf("expected valid InitEvent, got %v", startStream.events[0])
	}

	// Verify Exit event
	lastIdx := len(startStream.events) - 1
	exitEvent := startStream.events[lastIdx].GetExit()
	if exitEvent == nil || exitEvent.GetExitCode() != 0 {
		t.Errorf("expected ExitEvent with exit code 0, got %v", startStream.events[lastIdx])
	}
}

func TestProcessServerSignal(t *testing.T) {
	tempDir := t.TempDir()
	registry := processmanager.NewProcessRegistry()
	procServer := NewProcessServer(tempDir, registry)

	startStream := &mockStartServer{}

	go func() {
		_ = procServer.Start(&processv1.StartRequest{
			Config: &processv1.ProcessConfig{
				Command: []string{"sleep", "10"},
			},
		}, startStream)
	}()

	// Wait for process to launch and register
	var pid int32
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		active := registry.ListActive()
		if len(active) > 0 {
			pid = active[0].ID
			break
		}
	}

	if pid <= 0 {
		t.Fatalf("process failed to register within timeout")
	}

	// Send SIGKILL signal
	_, err := procServer.SendSignal(context.Background(), &processv1.SendSignalRequest{
		ProcessId: pid,
		Signal:    processv1.Signal_SIGNAL_SIGKILL,
	})
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}
}
