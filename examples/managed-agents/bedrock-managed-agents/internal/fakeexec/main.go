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

// Command fakeexec is a stand-in for `codex exec-server` used by unit tests
// and the kind e2e. It accepts the same flags but scripts its behavior via
// FAKEEXEC_MODE:
//
//	ready        print READY to stdout, then sleep forever (default)
//	alive        sleep forever without printing READY
//	exit3        write "boom" to stderr and exit 3
//	ignore-term  print READY, trap SIGTERM, and sleep forever (forces SIGKILL paths)
//	leak-secret  write a fake AWS credential to stderr and exit 1 (redaction tests)
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// sleepForever blocks without triggering Go's all-goroutines-asleep deadlock
// detector (a bare `select {}` would) and keeps default signal handling, so
// SIGTERM still terminates the process gracefully.
func sleepForever() { time.Sleep(time.Duration(math.MaxInt64)) }

func main() {
	remote := flag.String("remote", "", "executor remote URL (accepted, never logged)")
	envID := flag.String("environment-id", "", "environment ID")
	// The BMA attach flags are accepted and ignored; fakeexec never signs.
	flag.String("remote-transport", "", "accepted for flag parity with codex")
	flag.Bool("aws-sigv4", false, "accepted for flag parity with codex")
	flag.String("aws-service", "", "accepted for flag parity with codex")
	flag.String("aws-region", "", "accepted for flag parity with codex")
	flag.Parse()
	_ = remote // capability: deliberately unused and unlogged
	fmt.Fprintf(os.Stderr, "fakeexec starting env=%s\n", truncate(*envID, 12))

	switch os.Getenv("FAKEEXEC_MODE") {
	case "alive":
		sleepForever()
	case "exit3":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "ignore-term":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		// Ready marker only after the trap is installed: a SIGTERM arriving
		// earlier would take the default action and fail forced-stop tests.
		fmt.Println("READY")
		select {} // registered signal goroutine keeps the runtime awake
	case "leak-secret":
		// Echo the pod's credentials like a chatty child would; the
		// supervisor must redact them from the captured tail.
		fmt.Fprintf(os.Stderr, "secret=%s token=%s key=AKIAIOSFODNN7EXAMPLE\n",
			os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN"))
		os.Exit(1)
	default: // "ready"
		fmt.Println("READY")
		sleepForever()
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
