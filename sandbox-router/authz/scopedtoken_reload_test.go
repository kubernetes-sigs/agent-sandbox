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

package authz

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func keySetJSON(keys map[string]ed25519.PublicKey) []byte {
	var entries []string
	for kid, pub := range keys {
		entries = append(entries, `{"kid":"`+kid+`","publicKey":"`+base64.RawURLEncoding.EncodeToString(pub)+`"}`)
	}
	return []byte(`{"keys":[` + strings.Join(entries, ",") + `]}`)
}

func writeKeySet(t *testing.T, path string, keys map[string]ed25519.PublicKey) {
	t.Helper()
	if err := os.WriteFile(path, keySetJSON(keys), 0o600); err != nil {
		t.Fatalf("write key set: %v", err)
	}
}

func mintV2(t *testing.T, priv ed25519.PrivateKey, kid string) string {
	t.Helper()
	token, err := MintScopedTokenV2(priv, kid, scopedTokenV2Target(), time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return token
}

// outcomes records every reload callback.
type outcomes struct {
	mu sync.Mutex
	ok []bool
}

func (o *outcomes) record(ok bool, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ok = append(o.ok, ok)
}

func (o *outcomes) last() (bool, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.ok) == 0 {
		return false, 0
	}
	return o.ok[len(o.ok)-1], len(o.ok)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReloadingScopedTokenAuthorizer_RotatesWithoutRestart(t *testing.T) {
	oldPub, oldPriv := newScopedTokenV2Key(t)
	newPub, newPriv := newScopedTokenV2Key(t)
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeySet(t, path, map[string]ed25519.PublicKey{"old": oldPub})

	var got outcomes
	a, err := NewReloadingScopedTokenAuthorizer(path, ScopedTokenOptions{}, logr.Discard(), got.record)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	oldToken, newToken := mintV2(t, oldPriv, "old"), mintV2(t, newPriv, "new")
	authorize := func(token string) error {
		return a.Authorize(context.Background(), reqWithBearer(token), scopedTokenV2Target())
	}
	if err := authorize(oldToken); err != nil {
		t.Fatalf("old key before rotation: %v", err)
	}
	if err := authorize(newToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("new key before rotation: %v, want ErrUnauthenticated", err)
	}

	// Reader-first: add the new kid; both verify.
	writeKeySet(t, path, map[string]ed25519.PublicKey{"old": oldPub, "new": newPub})
	eventually(t, "the new kid to verify", func() bool { return authorize(newToken) == nil })
	if err := authorize(oldToken); err != nil {
		t.Fatalf("old key during overlap: %v", err)
	}

	// Retire the old kid.
	writeKeySet(t, path, map[string]ed25519.PublicKey{"new": newPub})
	eventually(t, "the old kid to be refused", func() bool { return errors.Is(authorize(oldToken), ErrUnauthenticated) })
	if ok, _ := got.last(); !ok {
		t.Fatal("last reload reported failure")
	}
}

// A file that does not parse keeps the previous keys and reports the failure.
func TestReloadingScopedTokenAuthorizer_KeepsPreviousKeysOnBadFile(t *testing.T) {
	pub, priv := newScopedTokenV2Key(t)
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeySet(t, path, map[string]ed25519.PublicKey{"current": pub})
	var got outcomes
	a, err := NewReloadingScopedTokenAuthorizer(path, ScopedTokenOptions{}, logr.Discard(), got.record)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"keys":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a failed reload", func() bool { ok, n := got.last(); return n > 1 && !ok })
	token := mintV2(t, priv, "current")
	if err := a.Authorize(context.Background(), reqWithBearer(token), scopedTokenV2Target()); err != nil {
		t.Fatalf("token after a bad update: %v, want the previous keys kept", err)
	}
}

// A projected ConfigMap update (the kubelet writes a new "..<timestamp>"
// directory and swaps the "..data" symlink) reaches the authorizer, although
// the key file's own path never changes.
func TestReloadingScopedTokenAuthorizer_FollowsAProjectedVolumeUpdate(t *testing.T) {
	oldPub, _ := newScopedTokenV2Key(t)
	newPub, newPriv := newScopedTokenV2Key(t)
	dir := t.TempDir()
	project := func(gen string, keys map[string]ed25519.PublicKey) {
		t.Helper()
		data := filepath.Join(dir, ".."+gen)
		if err := os.Mkdir(data, 0o700); err != nil {
			t.Fatal(err)
		}
		writeKeySet(t, filepath.Join(data, "keys.json"), keys)
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(".."+gen, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	project("1", map[string]ed25519.PublicKey{"old": oldPub})
	if err := os.Symlink(filepath.Join("..data", "keys.json"), filepath.Join(dir, "keys.json")); err != nil {
		t.Fatal(err)
	}

	a, err := NewReloadingScopedTokenAuthorizer(filepath.Join(dir, "keys.json"), ScopedTokenOptions{}, logr.Discard(), nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	project("2", map[string]ed25519.PublicKey{"new": newPub})
	token := mintV2(t, newPriv, "new")
	eventually(t, "the projected key to verify", func() bool {
		return a.Authorize(context.Background(), reqWithBearer(token), scopedTokenV2Target()) == nil
	})
}

func TestNewReloadingScopedTokenAuthorizer_RequiresAReadableKeyFile(t *testing.T) {
	pub, _ := newScopedTokenV2Key(t)
	if _, err := NewReloadingScopedTokenAuthorizer("", ScopedTokenOptions{}, logr.Discard(), nil); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := NewReloadingScopedTokenAuthorizer(filepath.Join(t.TempDir(), "missing.json"), ScopedTokenOptions{}, logr.Discard(), nil); err == nil {
		t.Fatal("missing file accepted")
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeySet(t, path, map[string]ed25519.PublicKey{"current": pub})
	if _, err := NewReloadingScopedTokenAuthorizer(path, ScopedTokenOptions{
		VerificationKeys: map[string]ed25519.PublicKey{"current": pub},
	}, logr.Discard(), nil); err == nil {
		t.Fatal("VerificationKeys alongside the file accepted")
	}
}
