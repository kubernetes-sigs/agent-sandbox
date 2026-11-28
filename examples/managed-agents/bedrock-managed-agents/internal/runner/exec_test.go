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

package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestSupervisorStartStateStop(t *testing.T) {
	s := NewSupervisor(fakeBin(t), logr.Discard())
	if err := s.Start([]string{"--remote", "wss://x", "--environment-id", "env_1"},
		[]string{"FAKEEXEC_MODE=ready"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	// Double Start while alive must error.
	if err := s.Start(nil, nil); err == nil {
		t.Fatal("want error on double start")
	}
	waitFor(t, 2*time.Second, func() bool { st := s.State(); return st.Ready && st.Alive })
	if got := s.State().ExitCode; got != -1 {
		t.Fatalf("alive exit code = %d, want -1", got)
	}
	if s.State().StartedAt.IsZero() {
		t.Fatal("StartedAt not recorded")
	}
	if forced := s.Stop(5 * time.Second); forced {
		t.Fatal("graceful stop must not force")
	}
	if s.State().Alive {
		t.Fatal("still alive after stop")
	}
}

func TestSupervisorCrashNoAutoRestart(t *testing.T) {
	s := NewSupervisor(fakeBin(t), logr.Discard())
	if err := s.Start(nil, []string{"FAKEEXEC_MODE=exit3"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { st := s.State(); return !st.Alive && st.ExitCode == 3 })
	time.Sleep(200 * time.Millisecond) // supervisor must NOT restart it
	if s.State().Alive {
		t.Fatal("auto-restarted")
	}
	if !strings.Contains(s.State().StderrTail, "boom") {
		t.Fatalf("stderr not captured: %q", s.State().StderrTail)
	}
}

func TestSupervisorStopForced(t *testing.T) {
	s := NewSupervisor(fakeBin(t), logr.Discard())
	if err := s.Start(nil, []string{"FAKEEXEC_MODE=ignore-term"}); err != nil {
		t.Fatal(err)
	}
	// Registered before the readiness wait: a timeout there must not leak
	// the child. Stop on a stopped supervisor is a no-op.
	t.Cleanup(func() { s.Stop(time.Second) })
	// Ready implies the SIGTERM trap is installed; Alive alone could go true
	// before signal.Notify runs and the default action would beat the test.
	waitFor(t, 2*time.Second, func() bool { return s.State().Ready })
	if forced := s.Stop(200 * time.Millisecond); !forced {
		t.Fatal("want forced kill")
	}
	if s.State().Alive {
		t.Fatal("still alive after SIGKILL")
	}
}

func TestSupervisorStderrRedacted(t *testing.T) {
	s := NewSupervisor(fakeBin(t), logr.Discard())
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	const token = "session-token-value"
	if err := s.Start(nil, []string{
		"FAKEEXEC_MODE=leak-secret",
		"AWS_SECRET_ACCESS_KEY=" + secret,
		"AWS_SESSION_TOKEN=" + token,
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !s.State().Alive })
	tail := s.State().StderrTail
	// Env-registered secrets redact by exact value; the access key ID
	// redacts by shape without registration.
	for _, leak := range []string{secret, token, "AKIAIOSFODNN7EXAMPLE"} {
		if strings.Contains(tail, leak) {
			t.Fatalf("secret leaked: %q in %q", leak, tail)
		}
	}
	if !strings.Contains(tail, "[redacted]") {
		t.Fatalf("no redaction marker: %q", tail)
	}
	if len(tail) > 4096 {
		t.Fatal("tail not bounded")
	}
}

func TestSupervisorNotAliveMode(t *testing.T) {
	// "alive" mode never prints READY: Ready must stay false while Alive.
	s := NewSupervisor(fakeBin(t), logr.Discard())
	if err := s.Start(nil, []string{"FAKEEXEC_MODE=alive"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	waitFor(t, 2*time.Second, func() bool { return s.State().Alive })
	time.Sleep(150 * time.Millisecond)
	if s.State().Ready {
		t.Fatal("Ready set without marker")
	}
	_ = s.Stop(2 * time.Second)
}

func TestRingTailEvictionDoesNotSplitSecrets(t *testing.T) {
	// Eviction that cut a credential in half would keep its tail without
	// the "AKIA" prefix, which snapshotRedacted cannot recognize.
	r := newRingTail(64)
	secret := "AKIA" + strings.Repeat("A", 100)
	r.append([]byte("prefix line\n"))
	r.append([]byte(secret + "\nsuffix line\n"))
	tail := r.snapshotRedacted()
	if strings.Contains(tail, "AAAA") {
		t.Fatalf("credential tail survived eviction unredacted: %q", tail)
	}
	if !strings.Contains(tail, "suffix line") {
		t.Fatalf("eviction dropped unrelated tail data: %q", tail)
	}
}

func TestRingTailEvictionDropsRegisteredSecretWhole(t *testing.T) {
	// An AWS secret key contains "/" and "+", which are not token bytes:
	// eviction cutting inside it must drop it whole rather than keep an
	// unredactable fragment.
	r := newRingTail(64)
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // 40 chars
	r.addSecret(secret)
	r.append([]byte(strings.Repeat("x", 20)))
	r.append([]byte(secret))
	r.append([]byte(strings.Repeat("y", 30))) // cut lands inside the secret
	tail := r.snapshotRedacted()
	for _, frag := range []string{"wJalrXUtnFEMI", "K7MDENG", "bPxRfiCYEXAMPLEKEY"} {
		if strings.Contains(tail, frag) {
			t.Fatalf("secret fragment survived eviction: %q in %q", frag, tail)
		}
	}
	if !strings.Contains(tail, "yyy") {
		t.Fatalf("eviction dropped unrelated tail data: %q", tail)
	}
}

func TestRingTailSecretAcrossWrites(t *testing.T) {
	// A secret split across two writes still lands contiguously in the
	// buffer and must be redacted by exact value.
	r := newRingTail(4096)
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	r.addSecret(secret)
	r.append([]byte("leak: " + secret[:15]))
	r.append([]byte(secret[15:] + "\n"))
	tail := r.snapshotRedacted()
	if strings.Contains(tail, "wJalrXUtnFEMI") {
		t.Fatalf("secret leaked across writes: %q", tail)
	}
	if !strings.Contains(tail, "[redacted]") {
		t.Fatalf("no redaction marker: %q", tail)
	}
}

func TestSupervisorRestartAfterExit(t *testing.T) {
	s := NewSupervisor(fakeBin(t), logr.Discard())
	if err := s.Start(nil, []string{"FAKEEXEC_MODE=exit3"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !s.State().Alive })
	// A fresh Start after exit is allowed (controller-driven restart).
	if err := s.Start(nil, []string{"FAKEEXEC_MODE=ready"}); err != nil {
		t.Fatalf("restart after exit: %v", err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	waitFor(t, 2*time.Second, func() bool { return s.State().Ready })
	_ = s.Stop(2 * time.Second)
}
