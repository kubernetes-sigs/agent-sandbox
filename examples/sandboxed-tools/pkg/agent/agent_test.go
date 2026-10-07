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

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestSession returns a Session backed by a scratch backup directory.
// getBackupDir() resolves backups under os.UserHomeDir()/.local/sandboxed-tools/<name>/fs,
// so redirecting HOME keeps these tests off the real user's home directory.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &Session{Name: "test-session"}
}

// writeBackup creates a fake backup tarball with the given timestamp
// component (e.g. "20260103T000000") and returns its full path.
func writeBackup(t *testing.T, session *Session, timestamp string) string {
	t.Helper()
	dir, err := session.getBackupDir()
	if err != nil {
		t.Fatalf("getBackupDir: %v", err)
	}
	path := filepath.Join(dir, "backup-"+timestamp+".tar.gz")
	if err := os.WriteFile(path, []byte("fake-tarball"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

func remainingBackups(t *testing.T, session *Session) []string {
	t.Helper()
	dir, err := session.getBackupDir()
	if err != nil {
		t.Fatalf("getBackupDir: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "backup-*.tar.gz"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	return matches
}

func TestFindLatestBackup_NoBackups(t *testing.T) {
	session := newTestSession(t)

	got, err := session.FindLatestBackup()
	if err != nil {
		t.Fatalf("FindLatestBackup: %v", err)
	}
	if got != "" {
		t.Errorf("FindLatestBackup() = %q, want empty string when no backups exist", got)
	}
}

func TestFindLatestBackup_ReturnsMostRecentByTimestamp(t *testing.T) {
	session := newTestSession(t)
	// Written out of chronological order -- FindLatestBackup must sort by
	// name, not by creation order or mtime.
	writeBackup(t, session, "20260101T000000")
	want := writeBackup(t, session, "20260103T000000")
	writeBackup(t, session, "20260102T000000")

	got, err := session.FindLatestBackup()
	if err != nil {
		t.Fatalf("FindLatestBackup: %v", err)
	}
	if got != want {
		t.Errorf("FindLatestBackup() = %q, want %q (the newest timestamp)", got, want)
	}
}

func TestFindLatestBackup_IgnoresNonBackupFiles(t *testing.T) {
	session := newTestSession(t)
	want := writeBackup(t, session, "20260101T000000")

	dir, err := session.getBackupDir()
	if err != nil {
		t.Fatalf("getBackupDir: %v", err)
	}
	for _, name := range []string{"notes.txt", "backup-corrupt", "backup-20260102T000000.tar.gz.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := session.FindLatestBackup()
	if err != nil {
		t.Fatalf("FindLatestBackup: %v", err)
	}
	if got != want {
		t.Errorf("FindLatestBackup() = %q, want %q (non-matching files must be ignored)", got, want)
	}
}

func TestPruneBackups_NoBackupsIsNoop(t *testing.T) {
	session := newTestSession(t)

	if err := session.PruneBackups(t.Context(), 5); err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}
}

func TestPruneBackups_KeepsEverythingWhenAtOrUnderKeepCount(t *testing.T) {
	session := newTestSession(t)
	writeBackup(t, session, "20260101T000000")
	writeBackup(t, session, "20260102T000000")

	if err := session.PruneBackups(t.Context(), 5); err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}

	if got := remainingBackups(t, session); len(got) != 2 {
		t.Errorf("remaining backups = %v, want 2 (nothing pruned)", got)
	}
}

func TestPruneBackups_ExactlyAtKeepCountIsNoop(t *testing.T) {
	session := newTestSession(t)
	writeBackup(t, session, "20260101T000000")
	writeBackup(t, session, "20260102T000000")
	writeBackup(t, session, "20260103T000000")

	if err := session.PruneBackups(t.Context(), 3); err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}

	if got := remainingBackups(t, session); len(got) != 3 {
		t.Errorf("remaining backups = %v, want 3 (boundary case: len == keepCount, nothing pruned)", got)
	}
}

func TestPruneBackups_DeletesOldestKeepingNewest(t *testing.T) {
	session := newTestSession(t)
	writeBackup(t, session, "20260101T000000") // oldest, must be pruned
	writeBackup(t, session, "20260102T000000") // must be pruned
	keep1 := writeBackup(t, session, "20260103T000000")
	keep2 := writeBackup(t, session, "20260104T000000")

	if err := session.PruneBackups(t.Context(), 2); err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}

	got := remainingBackups(t, session)
	if len(got) != 2 {
		t.Fatalf("remaining backups = %v, want exactly 2", got)
	}
	remaining := map[string]bool{got[0]: true, got[1]: true}
	if !remaining[keep1] || !remaining[keep2] {
		t.Errorf("remaining backups = %v, want the two newest (%q, %q) -- pruning kept the wrong files", got, keep1, keep2)
	}
}

func TestPruneBackups_ZeroKeepCountDeletesEverything(t *testing.T) {
	session := newTestSession(t)
	writeBackup(t, session, "20260101T000000")
	writeBackup(t, session, "20260102T000000")

	if err := session.PruneBackups(t.Context(), 0); err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}

	if got := remainingBackups(t, session); len(got) != 0 {
		t.Errorf("remaining backups = %v, want 0", got)
	}
}
