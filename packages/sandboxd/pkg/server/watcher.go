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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
)

const (
	// watcherBatchWindow is the batching window for fsnotify events: the
	// first event of a burst arms the timer, later events accumulate
	// without resetting it, and the batch is flushed in order when it
	// fires. Individual events are not merged. Same 250ms value as
	// sandbox-router/tlsutil CertReloader.
	watcherBatchWindow = 250 * time.Millisecond

	// maxConcurrentWatches caps the number of simultaneous WatchDir streams
	// per sandboxd process. Each stream consumes one inotify instance
	// (fs.inotify.max_user_instances, commonly 128 system-wide); without a
	// cap a single tenant can exhaust the container's inotify budget and
	// make every subsequent watch fail with Internal. 64 leaves headroom
	// for other inotify consumers in the pod.
	maxConcurrentWatches = 64

	// maxWatchFailures caps the errors retained for the client-facing
	// summary when every Add fails (e.g. inotify limit exhausted); the
	// total count is always kept.
	maxWatchFailures = 16
)

// watchFailures accumulates subdirectory watch setup failures, retaining at
// most maxWatchFailures errors; total counts every failure.
type watchFailures struct {
	total int
	first []error
}

func (f *watchFailures) add(err error) {
	f.total++
	if len(f.first) < maxWatchFailures {
		f.first = append(f.first, err)
	}
}

// summarize renders the failures into one bounded message for a non-terminal
// ERROR FileEvent.
func (f *watchFailures) summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "recursive watch incomplete: %d subdirectories could not be watched", f.total)
	for i, err := range f.first {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(err.Error())
	}
	if f.total > len(f.first) {
		fmt.Fprintf(&b, "; ... (%d more)", f.total-len(f.first))
	}
	return b.String()
}

// FileWatcherServer implements the FileWatcherService gRPC API. Each WatchDir
// call creates an independent fsnotify watcher whose lifecycle is tied to the
// stream context: client disconnect or server shutdown closes the watcher and
// terminates the goroutine cleanly (goleak-safe).
type FileWatcherServer struct {
	watcherv1.UnimplementedFileWatcherServiceServer
	rootDir string
	log     logr.Logger
	// slots bounds concurrent WatchDir streams. A send acquires a slot;
	// a receive releases it. Closed-over via defer in WatchDir so the
	// slot is released even on panic or early return.
	slots chan struct{}
	// done is closed by Shutdown. Event loops select on it so daemon
	// shutdown ends every active watch with a clean end-of-stream
	// instead of waiting for grpc GracefulStop to time out into a
	// forced Stop (which clients observe as UNAVAILABLE).
	done         chan struct{}
	shutdownOnce sync.Once
}

// NewFileWatcherServer builds a FileWatcherServer rooted at rootDir.
func NewFileWatcherServer(rootDir string, log logr.Logger) *FileWatcherServer {
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &FileWatcherServer{
		rootDir: rootDir,
		log:     log,
		slots:   make(chan struct{}, maxConcurrentWatches),
		done:    make(chan struct{}),
	}
}

// Shutdown ends all active WatchDir streams with a clean end-of-stream,
// unblocking a subsequent grpc GracefulStop — the watcher counterpart of
// Server.ShutdownProcesses, which ends the Start streams the same way.
// Idempotent; WatchDir calls that arrive after Shutdown are rejected by
// GracefulStop itself (it stops accepting new RPCs), so no registration
// of new watches needs to be blocked here.
func (s *FileWatcherServer) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.done) })
}

// WatchDir streams filesystem events for the requested directory. The
// goroutine lifecycle is:
//
//  1. Validate the path against the sandbox root (symlink-aware).
//  2. Acquire a concurrency slot (ResourceExhausted when the limit is hit).
//  3. Create an fsnotify watcher; add subdirectories if recursive.
//  4. Run the event loop until ctx is cancelled (client disconnect).
//  5. Defer watcher.Close() and release the slot so resources are released
//     even on error.
func (s *FileWatcherServer) WatchDir(req *watcherv1.WatchDirRequest, stream watcherv1.FileWatcherService_WatchDirServer) error {
	watchPath := req.GetPath()

	// Step 1: confine the requested path to the sandbox root.
	absWatch, err := pathutil.SanitizePath(s.rootDir, watchPath)
	if err != nil {
		if errors.Is(err, pathutil.ErrPathEscapes) {
			return status.Errorf(codes.PermissionDenied, "watch path: %v", err)
		}
		return status.Errorf(codes.Internal, "watch path: %v", err)
	}

	// Stat the resolved path. SanitizePath's "nearest existing ancestor"
	// fallback means a non-existent leaf (e.g. "root/no-such-dir") still
	// returns a valid resolved path whose parent exists. os.Stat catches
	// this: the leaf does not exist even though SanitizePath succeeded.
	info, err := os.Stat(absWatch)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Errorf(codes.NotFound, "watch path does not exist: %s", watchPath)
		}
		return status.Errorf(codes.Internal, "stat watch path: %v", err)
	}
	if !info.IsDir() {
		return status.Errorf(codes.InvalidArgument, "watch path is not a directory: %s", watchPath)
	}

	// Step 2: acquire a concurrency slot before allocating inotify
	// resources. The slot is released when WatchDir returns (via defer).
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return status.Errorf(codes.ResourceExhausted,
			"too many concurrent file watches (limit %d)", maxConcurrentWatches)
	}

	// Step 3: create the fsnotify watcher and seed it with directories.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return status.Errorf(codes.Internal, "create fsnotify watcher: %v", err)
	}
	// Close is idempotent and safe to defer immediately; the event loop
	// goroutine also closes on its way out, but deferring here guards
	// early returns before the goroutine starts.
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(absWatch); err != nil {
		return status.Errorf(codes.Internal, "watch %s: %v", watchPath, err)
	}

	// Also watch the parent directory so we can detect when the root watch
	// itself is removed or renamed. fsnotify's IN_DELETE_SELF is consumed
	// internally and not surfaced to the user; without the parent watch,
	// deleting the watched root would leave the event loop hanging with
	// no way to inform the client.
	parentDir := filepath.Dir(absWatch)
	rootBase := filepath.Base(absWatch)
	// Resolve the sandbox root the same way SanitizePath resolves absWatch
	// so the comparison below is symlink-safe.
	resolvedRoot := s.rootDir
	if cleanRoot, cErr := filepath.Abs(filepath.Clean(s.rootDir)); cErr == nil {
		if resolved, rErr := filepath.EvalSymlinks(cleanRoot); rErr == nil {
			resolvedRoot = resolved
		} else {
			resolvedRoot = cleanRoot
		}
	}
	switch {
	case parentDir == absWatch:
		// absWatch is the filesystem root (e.g. "/"); no parent to watch.
		parentDir = ""
		rootBase = ""
	case absWatch == resolvedRoot:
		// The watched path IS the sandbox root. Adding a watch on its
		// parent would observe events outside the sandbox boundary —
		// filtered, but noisy on busy filesystems. Skip the parent watch
		// in this case; the sandbox root is managed by the runtime and
		// is not expected to be removed out from under us.
		parentDir = ""
		rootBase = ""
	default:
		if pErr := watcher.Add(parentDir); pErr != nil {
			s.log.V(4).Info("failed to watch parent of root directory", "parent", parentDir, "error", pErr)
		}
	}

	if req.GetRecursive() {
		failures := &watchFailures{}
		s.addSubdirs(watcher, absWatch, failures)
		if failures.total > 0 {
			// Non-terminal ERROR: proceed with partial coverage, but
			// tell the client which subtrees went unwatched. Safe to
			// send here — the event loop has not started yet.
			if err := stream.Send(&watcherv1.FileEvent{
				Type:  watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR,
				Error: failures.summarize(),
			}); err != nil {
				return err
			}
		}
	}

	// Step 4: run the event loop. This blocks until the stream context
	// is cancelled (client disconnects, server shuts down, or the
	// fsnotify channel closes).
	return s.eventLoop(stream.Context(), watcher, absWatch, parentDir, rootBase, req.GetRecursive(), stream)
}

// addSubdirs walks dir and adds every subdirectory to watcher. Failures on
// individual directories do not abort the walk — partial coverage is better
// than none — but are recorded in failures so callers can tell the client
// which subtrees went unwatched.
func (s *FileWatcherServer) addSubdirs(watcher *fsnotify.Watcher, dir string, failures *watchFailures) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.log.V(4).Info("walk error during recursive watch setup", "path", path, "error", err)
			failures.add(fmt.Errorf("walk %s: %w", path, err))
			return nil // skip inaccessible subtrees
		}
		if d.IsDir() && path != dir {
			if wErr := watcher.Add(path); wErr != nil {
				s.log.V(4).Info("failed to watch subdirectory", "path", path, "error", wErr)
				failures.add(fmt.Errorf("watch %s: %w", path, wErr))
			}
		}
		return nil
	})
}

// eventLoop is the main goroutine for a single WatchDir stream. It reads
// fsnotify events, batches them in watcherBatchWindow flushes, re-validates
// paths, and sends FileEvents to the client. Exits cleanly when ctx is
// cancelled or the watched root directory is removed.
func (s *FileWatcherServer) eventLoop(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	rootWatch string,
	parentDir string,
	rootBase string,
	recursive bool,
	stream watcherv1.FileWatcherService_WatchDirServer,
) error {
	// sendMu serializes stream.Send calls. The event loop is single-
	// goroutine, so the mutex is not strictly required today; it guards
	// against future refactor that might introduce concurrent sends.
	var sendMu sync.Mutex

	send := func(ev *watcherv1.FileEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ev)
	}

	// Batching timer state: armed by the first event of a batch, not
	// reset by later events; fires once to flush the batch in order.
	var (
		timer       *time.Timer
		timerCh     <-chan time.Time
		pending     []*watcherv1.FileEvent
		terminalErr bool
		stopTimer   = func() {
			if timer != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer = nil
			timerCh = nil
		}
	)
	defer stopTimer()

	// flush sends all pending events to the client and clears the batch.
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		for _, ev := range pending {
			if err := send(ev); err != nil {
				return err
			}
		}
		pending = pending[:0]
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			// Client disconnected or the stream context was cancelled.
			// Best-effort flush of any remaining events is skipped —
			// the stream is already gone.
			return status.FromContextError(ctx.Err()).Err()

		case <-s.done:
			// Daemon shutdown (see Shutdown). Return nil so the client
			// sees a normal end-of-stream instead of the UNAVAILABLE
			// that a forced grpc Stop would produce, and so
			// GracefulStop can complete without waiting out the
			// shutdown timeout. Pending events are not flushed: the
			// daemon is going away and blocking on Send here could
			// stall shutdown on an unresponsive client.
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return nil // watcher closed
			}
			events, terminal := s.handleFSEvent(ev, rootWatch, parentDir, rootBase, recursive, watcher)
			pending = append(pending, events...)
			if terminal {
				terminalErr = true
			}
			// Arm the batching timer only when we got events and none
			// is running; later events in the window accumulate. (The
			// old len(pending) == len(events) check fired a no-op
			// timer on every filtered-out event because 0 == 0.)
			if len(events) > 0 && timer == nil {
				timer = time.NewTimer(watcherBatchWindow)
				timerCh = timer.C
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			s.log.Error(err, "fsnotify error")
			// Surface as an error event to the client rather than
			// silently dropping it.
			pending = append(pending, &watcherv1.FileEvent{
				Type:  watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR,
				Error: err.Error(),
			})
			// Overflow errors are non-fatal — the kernel buffer filled
			// up and some events may have been lost, but the watch is
			// still valid. Do not mark the stream as terminal.
			if !errors.Is(err, fsnotify.ErrEventOverflow) {
				terminalErr = true
			}
			// Arm the batching timer only when pending first becomes non-empty.
			if len(pending) == 1 && timer == nil {
				timer = time.NewTimer(watcherBatchWindow)
				timerCh = timer.C
			}

		case <-timerCh:
			timerCh = nil
			timer = nil
			// Flush pending events. If we have a terminal error (e.g.,
			// root directory removal), close the stream after flushing.
			// Overflow errors are non-terminal and do not trigger closure.
			if err := flush(); err != nil {
				return err
			}
			if terminalErr {
				return nil
			}
		}
	}
}

// handleFSEvent translates a single fsnotify.Event into zero or more
// FileEvents. Events whose paths escape the sandbox root (due to mid-watch
// symlink swaps) are silently dropped. Returns nil for events that should
// be filtered out (e.g. unrelated files in a watched directory).
//
// When the event comes from the parent-directory watch and concerns the
// watched root itself (delete/rename), the returned slice contains a
// single ERROR event signalling that the watched root has gone away, and
// the terminal flag is set to true.
func (s *FileWatcherServer) handleFSEvent(
	ev fsnotify.Event,
	rootWatch string,
	parentDir string,
	rootBase string,
	recursive bool,
	watcher *fsnotify.Watcher,
) ([]*watcherv1.FileEvent, bool) {
	// Detect removal/rename of the watched root itself. fsnotify reports
	// this as a Remove or Rename event with ev.Name == rootWatch. This
	// check handles the case where no parent watch is configured (e.g.
	// when the watched path is the sandbox root); the parent-watch check
	// below handles the case where the root is detected via its parent.
	if ev.Name == rootWatch &&
		(ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
		return []*watcherv1.FileEvent{{
			Type:  watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR,
			Error: "watched directory was removed or renamed",
		}}, true
	}

	// Detect root-directory removal/rename via the parent watch. The
	// parent watch fires events on the root basename (e.g. "workspace")
	// when the root is deleted or renamed.
	if parentDir != "" && ev.Name == filepath.Join(parentDir, rootBase) {
		if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
			return []*watcherv1.FileEvent{{
				Type:  watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR,
				Error: "watched directory was removed or renamed",
			}}, true
		}
		// For non-terminal events on the parent (e.g. Create — a new
		// directory with the same name), fall through to normal
		// processing. The path validation below will filter them.
	}

	// Filter events to the watched subtree. When watching /root/logs,
	// ignore events on /root/other.txt that arrive via the parent watch.
	relToRoot, relErr := filepath.Rel(rootWatch, ev.Name)
	if relErr != nil || relToRoot == ".." ||
		(len(relToRoot) >= 3 && relToRoot[:3] == ".."+string(filepath.Separator)) {
		return nil, false
	}

	// Re-validate the event path against the sandbox root. fsnotify
	// reports absolute paths; a mid-watch symlink swap could redirect a
	// previously-safe path outside the root.
	relPath, err := toSandboxRelative(ev.Name, s.rootDir)
	if err != nil {
		// Path escapes or cannot be resolved — silently skip.
		return nil, false
	}

	// Handle recursive directory management BEFORE mapping the event.
	// New directories need to be watched; removed ones need cleanup.
	// Only apply recursive management when recursive mode is enabled.
	var out []*watcherv1.FileEvent
	if recursive && (ev.Has(fsnotify.Create) || ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
		if failures := s.maybeUpdateRecursiveWatch(ev, watcher); failures.total > 0 {
			// Non-terminal: the watch continues with partial coverage;
			// the client must still see which subtrees went unwatched.
			out = append(out, &watcherv1.FileEvent{
				Type:  watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR,
				Error: failures.summarize(),
			})
		}
	}

	evtType := fsOpToFileEventType(ev.Op)
	if evtType == watcherv1.FileEventType_FILE_EVENT_TYPE_UNSPECIFIED {
		return out, false
	}

	return append(out, &watcherv1.FileEvent{
		Type: evtType,
		Path: relPath,
	}), false
}

// maybeUpdateRecursiveWatch adjusts the watched directory set when a
// subdirectory is created or removed during a recursive watch, returning any
// watch failures for the caller to surface as a non-terminal ERROR event.
func (s *FileWatcherServer) maybeUpdateRecursiveWatch(
	ev fsnotify.Event,
	watcher *fsnotify.Watcher,
) *watchFailures {
	failures := &watchFailures{}
	// Handle removals BEFORE Lstat: a removed directory no longer exists
	// on disk, so Lstat would fail with ErrNotExist and the cleanup
	// branch would be unreachable. fsnotify.Remove is safe to call even
	// if the watch was already auto-removed by the kernel.
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		_ = watcher.Remove(ev.Name)
		// Kernel watches follow inodes, not paths: after a directory
		// rename, descendant watches would keep reporting stale paths
		// (which still pass SanitizePath's ancestor fallback). Drop the
		// whole subtree; an in-tree rename re-adds it via the Create
		// event at the destination.
		prefix := ev.Name + string(filepath.Separator)
		for _, w := range watcher.WatchList() {
			if strings.HasPrefix(w, prefix) {
				_ = watcher.Remove(w)
			}
		}
		return failures
	}

	if !ev.Has(fsnotify.Create) {
		return failures
	}

	// Only react to Create events on real directories from here on. Use
	// Lstat (not Stat) to detect symlinks: a symlink to a directory
	// should NOT cause us to follow it and add a watch on the target,
	// which could be outside the sandbox root and waste inotify watches.
	linfo, err := os.Lstat(ev.Name)
	if err != nil {
		return failures
	}
	if !linfo.IsDir() || linfo.Mode()&os.ModeSymlink != 0 {
		return failures
	}

	if err := watcher.Add(ev.Name); err != nil {
		s.log.V(4).Info("failed to watch new subdirectory", "path", ev.Name, "error", err)
		failures.add(fmt.Errorf("watch %s: %w", ev.Name, err))
	}
	// Also watch any subdirectories that may have been moved in as a
	// tree (e.g. mv dir/ watched/). addSubdirs uses WalkDir which does
	// not follow symlinks, so symlinked subtrees are correctly skipped.
	s.addSubdirs(watcher, ev.Name, failures)
	return failures
}

// fsOpToFileEventType maps fsnotify operations to our proto enum. fsnotify
// may report multiple ops in a single event (e.g. Chmod|Write); we pick the
// most informative one, prioritizing structural changes over metadata.
func fsOpToFileEventType(op fsnotify.Op) watcherv1.FileEventType {
	switch {
	case op.Has(fsnotify.Create):
		return watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE
	case op.Has(fsnotify.Remove):
		return watcherv1.FileEventType_FILE_EVENT_TYPE_REMOVE
	case op.Has(fsnotify.Rename):
		return watcherv1.FileEventType_FILE_EVENT_TYPE_RENAME
	case op.Has(fsnotify.Write):
		return watcherv1.FileEventType_FILE_EVENT_TYPE_WRITE
	case op.Has(fsnotify.Chmod):
		return watcherv1.FileEventType_FILE_EVENT_TYPE_CHMOD
	default:
		return watcherv1.FileEventType_FILE_EVENT_TYPE_UNSPECIFIED
	}
}

// toSandboxRelative converts an absolute path to a sandbox-relative path by
// stripping the rootDir prefix. Returns an error if the path escapes the root
// (re-validating through pathutil to catch symlink swaps).
func toSandboxRelative(absPath, rootDir string) (string, error) {
	cleanRoot, err := filepath.Abs(filepath.Clean(rootDir))
	if err != nil {
		return "", fmt.Errorf("invalid sandbox root dir: %w", err)
	}
	// Resolve root symlinks the same way SanitizePath does; fsnotify
	// reports the already-resolved paths handed to watcher.Add, so the
	// comparison below is exact.
	if resolvedRoot, rErr := filepath.EvalSymlinks(cleanRoot); rErr == nil {
		cleanRoot = resolvedRoot
	}

	// An event on the sandbox root itself has no in-root parent;
	// sanitizing Dir(absPath) would fabricate a spurious path. The root
	// maps to "." by definition.
	if absPath == cleanRoot {
		return ".", nil
	}

	// Re-sanitize the parent so intermediate symlink swaps are caught, but
	// do not resolve the leaf: the event concerns the entry itself (which
	// may be a symlink), not its target. Resolving the leaf would report
	// the symlink target instead of the symlink name.
	resolvedParent, err := pathutil.SanitizePath(rootDir, filepath.Dir(absPath))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(absPath))

	rel, err := filepath.Rel(cleanRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("compute relative path: %w", err)
	}
	// Normalize to forward slashes for cross-platform consistency.
	return strings.ReplaceAll(rel, string(os.PathSeparator), "/"), nil
}
