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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
)

// ReloadCallback is invoked after every key reload attempt. ok reports
// whether the reload succeeded; err carries the read or parse error on
// failure. It exists so metrics can be wired without coupling this package
// to a metrics library.
type ReloadCallback func(ok bool, err error)

// ReloadingScopedTokenAuthorizer is a ScopedTokenAuthorizer whose v2
// verification keys follow a file. Every change to the file (an edit, or a
// projected ConfigMap or Secret update, which the kubelet applies by swapping
// a "..data" symlink) rebuilds the authorizer from Options plus the keys the
// file now holds, so a reader-first rotation (add the new kid, let issuers
// use it, remove the old kid once its tokens expire) needs no restart.
//
// A file that cannot be read or parsed leaves the previous authorizer in
// place, as CertReloader does for certificates: a half-written or mistyped
// update never turns every request into a 401. The failure is reported
// through the callback and the log, so an operator removing a compromised
// key can see that the removal has not taken effect.
//
// Authorize reads the current authorizer through an atomic pointer, so
// requests never block on a reload.
type ReloadingScopedTokenAuthorizer struct {
	keysFile string
	opts     ScopedTokenOptions
	log      logr.Logger
	cb       ReloadCallback

	cur atomic.Pointer[ScopedTokenAuthorizer]
}

var _ Authorizer = (*ReloadingScopedTokenAuthorizer)(nil)

// NewReloadingScopedTokenAuthorizer reads keysFile, builds the initial
// authorizer from opts with those keys as VerificationKeys, and returns it
// ready to serve; call Start to follow the file. opts.VerificationKeys must
// be empty: the file is their only source. cb may be nil.
func NewReloadingScopedTokenAuthorizer(keysFile string, opts ScopedTokenOptions, log logr.Logger, cb ReloadCallback) (*ReloadingScopedTokenAuthorizer, error) {
	if keysFile == "" {
		return nil, errors.New("scopedtoken: keysFile must be non-empty")
	}
	if len(opts.VerificationKeys) > 0 {
		return nil, errors.New("scopedtoken: VerificationKeys come from keysFile; leave them unset")
	}
	a := &ReloadingScopedTokenAuthorizer{keysFile: keysFile, opts: opts, log: log, cb: cb}
	if err := a.Reload(); err != nil {
		return nil, fmt.Errorf("initial scoped-token key load: %w", err)
	}
	return a, nil
}

// Authorize implements Authorizer with the most recently loaded keys.
func (a *ReloadingScopedTokenAuthorizer) Authorize(ctx context.Context, r *http.Request, target AuthorizationTarget) error {
	return a.cur.Load().Authorize(ctx, r, target)
}

// Reload reads and parses the key file and, on success, atomically replaces
// the authorizer. On failure the previous authorizer stays in place and the
// error is returned.
func (a *ReloadingScopedTokenAuthorizer) Reload() error {
	err := a.reload()
	if a.cb != nil {
		a.cb(err == nil, err)
	}
	return err
}

func (a *ReloadingScopedTokenAuthorizer) reload() error {
	data, err := os.ReadFile(a.keysFile)
	if err != nil {
		return fmt.Errorf("read verification keys: %w", err)
	}
	keys, err := ParseScopedTokenVerificationKeys(data)
	if err != nil {
		return err
	}
	opts := a.opts
	opts.VerificationKeys = keys
	next, err := NewScopedTokenAuthorizer(opts)
	if err != nil {
		return err
	}
	a.cur.Store(next)
	return nil
}

// Start watches the key file's directory and reloads after every change,
// until ctx is done. The directory is watched, not the file, because a
// projected volume update renames a new directory into place, which detaches
// a watch on the file itself.
func (a *ReloadingScopedTokenAuthorizer) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify watcher: %w", err)
	}
	if err := watcher.Add(filepath.Dir(a.keysFile)); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch %s: %w", filepath.Dir(a.keysFile), err)
	}
	go a.run(ctx, watcher)
	return nil
}

// reloadDebounce coalesces the burst of events one update produces.
const reloadDebounce = 250 * time.Millisecond

func (a *ReloadingScopedTokenAuthorizer) run(ctx context.Context, watcher *fsnotify.Watcher) {
	defer func() { _ = watcher.Close() }()
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			// The file itself, or the kubelet's AtomicWriter swapping
			// "..data" (see CertReloader); anything else in the directory
			// is ignored, and a spurious reload only re-reads the file.
			name := filepath.Clean(ev.Name)
			if name != filepath.Clean(a.keysFile) && !strings.HasPrefix(filepath.Base(name), "..") {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(reloadDebounce)
			timerC = timer.C
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			a.log.Error(err, "scoped-token key watcher error")
		case <-timerC:
			timer, timerC = nil, nil
			if err := a.Reload(); err != nil {
				a.log.Error(err, "scoped-token key reload failed; keeping the previous keys", "file", a.keysFile)
			} else {
				a.log.Info("scoped-token keys reloaded", "file", a.keysFile)
			}
		}
	}
}
