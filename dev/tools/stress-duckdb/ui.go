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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func duckdbExec(ctx context.Context, cfg *config, statement string) error {
	args := []string{
		"run", "--rm", "-i",
		"-v", cfg.dataDir + ":/data",
		"-v", cfg.dataDir + "/.duckdb:/root/.duckdb",
		cfg.image, "/duckdb", "/data/stress.duckdb",
		"-c", statement,
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func stopUI(cfg *config) error {
	_ = exec.Command("docker", "rm", "-f", uiContainer, uiFwdContainer).Run()
	fmt.Println("stopped")
	return nil
}

func startUI(ctx context.Context, cfg *config, listen string) error {
	_ = exec.Command("docker", "rm", "-f", uiContainer, uiFwdContainer).Run()

	// Seed while ui.db is unlocked. The long-running UI takes an exclusive
	// DuckDB lock on that file, so this must happen before we start it.
	seedNotebooks(ctx, cfg)

	uiPort, err := strconv.Atoi(cfg.uiPort)
	if err != nil {
		return fmt.Errorf("invalid UI port %q: %w", cfg.uiPort, err)
	}
	// DuckDB UI hard-binds to localhost and validates Origin/Referer against
	// http://localhost:<ui_local_port>. The browser-visible port must therefore
	// equal ui_local_port. Socat listens on a different port that Docker
	// publishes as cfg.uiPort on the host.
	//
	// We avoid --network=host: on Docker Desktop (Mac/Windows) that is the
	// Linux VM, not the host OS.
	proxyPort := strconv.Itoa(uiPort + 1)
	publish := fmt.Sprintf("%s:%s:%s", listen, cfg.uiPort, proxyPort)

	// -t is required: the UI server does not come up without a tty.
	// -dark-mode skips the CLI's 5s terminal background-color probe.
	runArgs := []string{
		"run", "-dit", "--name", uiContainer,
		"-p", publish,
		"-v", cfg.dataDir + ":/data",
		"-v", cfg.dataDir + "/.duckdb:/root/.duckdb",
		cfg.image, "/duckdb", "-dark-mode", "/data/stress.duckdb",
		"-cmd", fmt.Sprintf("SET ui_local_port=%s; CALL start_ui_server();", cfg.uiPort),
	}
	cmd := exec.CommandContext(ctx, "docker", runArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting duckdb ui: %w\n%s", err, out)
	}

	uiTarget, err := waitUILoopback(ctx, uiContainer, cfg.uiPort, 45*time.Second)
	if err != nil {
		logs := dockerLogs(uiContainer)
		_ = exec.Command("docker", "rm", "-f", uiContainer).Run()
		return fmt.Errorf("%w\n--- duckdb logs ---\n%s", err, logs)
	}

	fwd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", uiFwdContainer,
		"--network", "container:"+uiContainer,
		"alpine/socat",
		fmt.Sprintf("TCP-LISTEN:%s,fork,reuseaddr", proxyPort),
		uiTarget,
	)
	if out, err := fwd.CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", uiContainer).Run()
		return fmt.Errorf("starting socat forwarder: %w\n%s", err, out)
	}

	// Origin/Referer must be http://localhost:<port>, not 127.0.0.1.
	uiURL := fmt.Sprintf("http://localhost:%s", cfg.uiPort)
	if err := waitHTTP(ctx, uiURL, 30*time.Second); err != nil {
		logs := dockerLogs(uiContainer) + "\n--- socat logs ---\n" + dockerLogs(uiFwdContainer)
		_ = exec.Command("docker", "rm", "-f", uiContainer, uiFwdContainer).Run()
		return fmt.Errorf("UI not reachable at %s: %w\n%s", uiURL, err, logs)
	}

	if listen != "127.0.0.1" && listen != "localhost" {
		fmt.Println("WARNING: the DuckDB UI has no authentication; anyone who can reach")
		fmt.Printf("%s:%s can query (and modify) the database.\n", listen, cfg.uiPort)
		fmt.Println("WARNING: open the UI via http://localhost (not a LAN IP); the UI")
		fmt.Println("rejects requests whose Origin/Referer are not http://localhost:<port>.")
	}

	fmt.Printf("DuckDB UI: %s  (notebooks persist in %s/.duckdb)\n", uiURL, cfg.dataDir)
	fmt.Println("Use http://localhost (not http://127.0.0.1) — the UI checks Origin.")
	fmt.Printf("Open the %q notebook for starter queries.\n", stressAnalysisTitle)
	fmt.Println("Press Ctrl-C to stop.")

	waitDone := make(chan error, 1)
	go func() {
		c := exec.Command("docker", "wait", uiContainer)
		out, err := c.CombinedOutput()
		if err != nil {
			waitDone <- fmt.Errorf("docker wait: %w (%s)", err, strings.TrimSpace(string(out)))
			return
		}
		waitDone <- nil
	}()

	select {
	case <-ctx.Done():
		_ = exec.Command("docker", "rm", "-f", uiContainer, uiFwdContainer).Run()
		time.Sleep(200 * time.Millisecond)
		return nil
	case err := <-waitDone:
		_ = exec.Command("docker", "rm", "-f", uiFwdContainer).Run()
		return err
	}
}

// waitUILoopback waits until DuckDB's UI accepts TCP on loopback inside the
// container netns. It returns a socat target address (IPv4 or IPv6).
func waitUILoopback(ctx context.Context, container, port string, timeout time.Duration) (string, error) {
	candidates := []string{
		"TCP:127.0.0.1:" + port,
		"TCP6:[::1]:" + port,
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !dockerRunning(container) {
			return "", fmt.Errorf("ui container exited before listening on port %s", port)
		}
		for _, target := range candidates {
			if err := probeContainerTCP(ctx, container, target); err == nil {
				return target, nil
			} else {
				lastErr = err
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no listener")
	}
	return "", fmt.Errorf("timed out waiting for UI on localhost:%s (%v)", port, lastErr)
}

func probeContainerTCP(ctx context.Context, container, socatTarget string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--network", "container:"+container,
		"alpine/socat",
		"-u", "STDIN", socatTarget+",connect-timeout=1",
	)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", socatTarget, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitHTTP(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}

func dockerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func dockerLogs(name string) string {
	out, err := exec.Command("docker", "logs", "--tail", "80", name).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(failed to read logs for %s: %v)", name, err)
	}
	return strings.TrimSpace(string(out))
}
