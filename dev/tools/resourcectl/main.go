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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// State is the local state of the resourcectl CLI.
type State struct {
	// BoskosResources resources that this resourcectl instance has checked out.
	BoskosResources []BoskosResource `json:"boskosResources"`
}

// BoskosResource holds information about a Boskos resource that
// resourcectl has acquired.
type BoskosResource struct {
	// Name is the name of the resource we acquired from boskos
	Name string `json:"name"`
	// Type is the type of resource we acquired from boskos
	Type string `json:"type"`
	// HeartbeatPID is the pid of the heartbeat process that is keeping this
	// resource alive.
	HeartbeatPID int `json:"heartbeatPid"`
}

// ReleaseFromBoskos releases the Boskos resource by sending a request to Boskos.
func (r *BoskosResource) ReleaseFromBoskos(ctx context.Context) error {
	boskosHost := os.Getenv("BOSKOS_HOST")
	if boskosHost != "" && r.Name != "" {
		url := fmt.Sprintf("%s/release?name=%s&state=busy&dest=free", boskosHost, url.QueryEscape(r.Name))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return fmt.Errorf("error creating request to release resource from boskos: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("error releasing resource from boskos: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("boskos returned status %d on release: %s", resp.StatusCode, string(body))
		}
		fmt.Printf("Released resource %s from boskos\n", r.Name)
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// run is the main entry point for the resourcectl CLI.
func run(ctx context.Context) error {
	if len(os.Args) < 2 {
		return fmt.Errorf("Usage: resourcectl <get|cleanup|heartbeat> [args]")
	}

	command := os.Args[1]
	switch command {
	case "get":
		if len(os.Args) < 3 {
			return fmt.Errorf("Usage: resourcectl get <type>")
		}
		getType := os.Args[2]
		return runGet(ctx, getType)
	case "cleanup":
		return runCleanup(ctx)
	case "heartbeat":
		if len(os.Args) < 4 || os.Args[2] != "--name" {
			return fmt.Errorf("Usage: resourcectl heartbeat --name <name>")
		}
		name := os.Args[3]
		return runHeartbeat(ctx, name)
	default:
		return fmt.Errorf("Unknown command: %s", command)
	}
}

// stateFilePath returns the path to the local state file.
func stateFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("error getting user home dir: %v", err)
	}
	dir := filepath.Join(home, ".local", "resourcectl")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("error creating state dir: %v", err)
	}
	return filepath.Join(dir, "state.json"), nil
}

// readState reads the local state file.
func readState() (*State, error) {
	p, err := stateFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("error reading state file: %v", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("error unmarshalling state: %v", err)
	}
	return &state, nil
}

// writeState writes the local state file.
func writeState(state *State) error {
	p, err := stateFilePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("error encoding state: %v", err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("error writing state file: %v", err)
	}
	return nil
}

// runGet acquires a resource of the given type from Boskos and starts a
// heartbeat process for it.
func runGet(ctx context.Context, resourceType string) error {
	boskosHost := os.Getenv("BOSKOS_HOST")
	if boskosHost == "" {
		return fmt.Errorf("BOSKOS_HOST env var is not set")
	}

	url := fmt.Sprintf("%s/acquire?type=%s&state=free&dest=busy", boskosHost, url.QueryEscape(resourceType))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error calling Boskos: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("boskos returned status %d: %s", resp.StatusCode, string(body))
	}

	var resource struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resource); err != nil {
		return fmt.Errorf("error decoding response: %v", err)
	}

	// Start heartbeat process
	cmd := exec.Command(os.Args[0], "heartbeat", "--name", resource.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}
	// TODO: Log stdout/stderr of the heartbeat process
	if err := cmd.Start(); err != nil {
		// We will fail the test here, but we don't want a resource to be reclaimed mid test
		return fmt.Errorf("error starting heartbeat process: %w", err)
	}

	state, err := readState()
	if err != nil {
		return err
	}

	state.BoskosResources = append(state.BoskosResources, BoskosResource{
		Name:         resource.Name,
		Type:         resourceType,
		HeartbeatPID: cmd.Process.Pid,
	})

	if err := writeState(state); err != nil {
		return err
	}

	// Everything has worked; print the resource name for the caller.
	fmt.Println(resource.Name)

	return nil
}

// runCleanup releases all resources that this resourcectl instance has
// acquired from Boskos.
func runCleanup(ctx context.Context) error {
	state, err := readState()
	if err != nil {
		return err
	}
	var errs []error
	for i := range state.BoskosResources {
		r := &state.BoskosResources[i]
		if err := killHeartbeatProcess(ctx, r); err != nil {
			errs = append(errs, err)
		} else {
			r.HeartbeatPID = 0
		}

		if err := r.ReleaseFromBoskos(ctx); err != nil {
			errs = append(errs, err)
		} else {
			r.Name = ""
			r.Type = ""
		}
	}

	if err := writeState(state); err != nil {
		return err
	}

	return errors.Join(errs...)
}

// killHeartbeatProcess kills the heartbeat process for a resource.
func killHeartbeatProcess(ctx context.Context, r *BoskosResource) error {
	if r.HeartbeatPID == 0 {
		return nil
	}

	process, err := os.FindProcess(r.HeartbeatPID)
	if err != nil {
		return fmt.Errorf("error finding heartbeat process: %w", err)
	}

	// TODO: Verify the process is actually the heartbeat process

	if err := process.Kill(); err != nil {
		return fmt.Errorf("error killing heartbeat process: %w", err)
	}

	return nil
}

// runHeartbeat sends periodic heartbeats to Boskos to keep the resource alive.
// This is run as a child process of runGet.
func runHeartbeat(ctx context.Context, name string) error {
	boskosHost := os.Getenv("BOSKOS_HOST")
	if boskosHost == "" {
		return fmt.Errorf("BOSKOS_HOST env var is not set")
	}

	url := fmt.Sprintf("%s/update?name=%s&state=busy", boskosHost, url.QueryEscape(name))

	// Send initial heartbeat
	if err := sendOneHeartbeat(ctx, url); err != nil {
		return fmt.Errorf("error sending initial heartbeat: %v", err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sendOneHeartbeat(ctx, url); err != nil {
				fmt.Fprintf(os.Stderr, "error sending heartbeat: %v\n", err)
			}
		}
	}
}

// sendOneHeartbeat sends a single heartbeat to Boskos.
func sendOneHeartbeat(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending heartbeat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("boskos returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
