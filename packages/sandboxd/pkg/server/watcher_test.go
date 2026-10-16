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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
)

// newWatcherClient spins up an in-memory gRPC server hosting a
// FileWatcherServer rooted at rootDir and returns a connected client.
func newWatcherClient(t *testing.T, rootDir string) watcherv1.FileWatcherServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	watcherv1.RegisterFileWatcherServiceServer(grpcServer,
		NewFileWatcherServer(rootDir, logr.Discard()))
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, conn.Close())
		grpcServer.GracefulStop()
		_ = lis.Close()
	})
	return watcherv1.NewFileWatcherServiceClient(conn)
}

// eventReader drains a WatchDir stream in a single goroutine (gRPC streams
// are not safe for concurrent Recv) and delivers events over a channel.
type eventReader struct {
	ch     chan *watcherv1.FileEvent
	cancel context.CancelFunc
}

// startEventReader spawns a goroutine that reads all FileEvents from stream.
// Call reader.cancel() to stop reading and release the goroutine.
func startEventReader(ctx context.Context, t *testing.T, stream watcherv1.FileWatcherService_WatchDirClient) *eventReader {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	r := &eventReader{
		ch:     make(chan *watcherv1.FileEvent, 64),
		cancel: cancel,
	}
	go func() {
		defer close(r.ch)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case r.ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return r
}

// defaultWaitTimeout is the default timeout for waitForEvent.
const defaultWaitTimeout = 3 * time.Second

// waitForEvent reads events from the reader until one matches pred or the
// timeout expires. Returns the matching event or nil.
func waitForEvent(t *testing.T, r *eventReader, pred func(*watcherv1.FileEvent) bool) *watcherv1.FileEvent {
	t.Helper()
	deadline := time.After(defaultWaitTimeout)
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return nil
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			return nil
		}
	}
}

// drainEvents collects all events available within timeout.
func drainEvents(t *testing.T, r *eventReader, timeout time.Duration) []*watcherv1.FileEvent {
	t.Helper()
	var events []*watcherv1.FileEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

func TestWatchDirCreateEvent(t *testing.T) {
	root := t.TempDir()
	client := newWatcherClient(t, root)

	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)

	// Small delay to let the watcher fully initialize.
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(root, "new.txt"), []byte("hello"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "new.txt"
	})
	require.NotNil(t, ev, "expected CREATE event for new.txt")
}

func TestWatchDirWriteEvent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("initial"), 0o644))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("updated"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_WRITE && e.Path == "existing.txt"
	})
	require.NotNil(t, ev, "expected WRITE event for existing.txt")
}

func TestWatchDirRemoveEvent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "removeme.txt"), []byte("bye"), 0o644))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.Remove(filepath.Join(root, "removeme.txt")))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_REMOVE && e.Path == "removeme.txt"
	})
	require.NotNil(t, ev, "expected REMOVE event for removeme.txt")
}

// TestWatchDirSandboxRootRemoved verifies that when the watched path is the
// sandbox root itself (i.e. Path == "."), removing the root produces a
// terminal ERROR event and closes the stream. This exercises the branch in
// handleFSEvent that matches ev.Name == rootWatch when no parent watch is
// configured.
func TestWatchDirSandboxRootRemoved(t *testing.T) {
	root := t.TempDir()

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Remove the sandbox root itself. fsnotify reports this as a Remove
	// event with ev.Name == rootWatch; because parentDir == "" for the
	// sandbox root, handleFSEvent must detect it directly.
	require.NoError(t, os.RemoveAll(root))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR
	})
	require.NotNil(t, ev, "expected terminal ERROR event after sandbox root removal")
	require.Contains(t, ev.Error, "watched directory was removed or renamed")

	// The stream should close after the ERROR event.
	deadline := time.After(defaultWaitTimeout)
	for {
		select {
		case _, ok := <-reader.ch:
			if !ok {
				return // stream closed as expected
			}
		case <-deadline:
			t.Fatal("stream did not close after terminal ERROR event")
		}
	}
}

// TestWatchDirSandboxRootRenamed verifies that when the watched path is the
// sandbox root itself, renaming the root produces a terminal ERROR event and
// closes the stream.
func TestWatchDirSandboxRootRenamed(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Dir(root)
	newPath := filepath.Join(parent, "renamed-root")

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.Rename(root, newPath))
	// Restore so t.TempDir cleanup does not fail.
	t.Cleanup(func() { _ = os.Rename(newPath, root) })

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR
	})
	require.NotNil(t, ev, "expected terminal ERROR event after sandbox root rename")
	require.Contains(t, ev.Error, "watched directory was removed or renamed")
}

func TestWatchDirRenameEvent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "old.txt"), []byte("data"), 0o644))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_RENAME
	})
	require.NotNil(t, ev, "expected RENAME event")
}

func TestWatchDirPathTraversal(t *testing.T) {
	root := t.TempDir()
	client := newWatcherClient(t, root)

	stream, err := client.WatchDir(context.Background(), &watcherv1.WatchDirRequest{Path: "../../etc/passwd"})
	require.NoError(t, err) // streaming RPCs report errors via Recv
	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, st.Code())
}

func TestWatchDirNonExistentPath(t *testing.T) {
	root := t.TempDir()
	client := newWatcherClient(t, root)

	stream, err := client.WatchDir(context.Background(), &watcherv1.WatchDirRequest{Path: "does-not-exist"})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, st.Code())
}

func TestWatchDirNonDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "afile.txt"), []byte("data"), 0o644))

	client := newWatcherClient(t, root)
	stream, err := client.WatchDir(context.Background(), &watcherv1.WatchDirRequest{Path: "afile.txt"})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestWatchDirRecursive(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: ".", Recursive: true})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(subdir, "deep.txt"), []byte("deep"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "subdir/deep.txt"
	})
	require.NotNil(t, ev, "expected CREATE event for subdir/deep.txt in recursive mode")
}

func TestWatchDirNonRecursiveIgnoresSubdirs(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: ".", Recursive: false})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Create a file in the subdirectory — should NOT trigger an event.
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "deep.txt"), []byte("deep"), 0o644))

	events := drainEvents(t, reader, watcherBatchWindow+500*time.Millisecond)
	for _, ev := range events {
		require.NotEqual(t, "subdir/deep.txt", ev.Path,
			"non-recursive watch should not report events from subdirectories")
	}
}

func TestWatchDirRecursiveDynamicAdd(t *testing.T) {
	root := t.TempDir()

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: ".", Recursive: true})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Create a new subdirectory AFTER the watch starts.
	newSubdir := filepath.Join(root, "newsubdir")
	require.NoError(t, os.Mkdir(newSubdir, 0o755))

	// Wait for the CREATE event on the directory itself to propagate.
	time.Sleep(watcherBatchWindow + 200*time.Millisecond)

	// Now create a file inside the new subdirectory.
	require.NoError(t, os.WriteFile(filepath.Join(newSubdir, "file.txt"), []byte("data"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "newsubdir/file.txt"
	})
	require.NotNil(t, ev, "expected CREATE event in dynamically-added subdirectory")
}

// TestWatchDirRootChmodReportsDotPath verifies that an event on the watched
// root itself reports path "." — its parent lies outside the root, so
// toSandboxRelative must special-case it instead of sanitizing the parent.
func TestWatchDirRootChmodReportsDotPath(t *testing.T) {
	root := t.TempDir()
	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.Chmod(root, 0o755))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CHMOD
	})
	require.NotNil(t, ev, "expected CHMOD event for the watched root itself")
	require.Equal(t, ".", ev.Path, "event on the sandbox root must report path \".\"")
}

// TestWatchDirRecursiveRenameNoStalePaths verifies that renaming a directory
// within a recursive watch drops the old subtree's watches: kernel watches
// follow inodes, so stale registrations would keep reporting pre-rename
// paths. After the rename, events must only appear under the new path.
func TestWatchDirRecursiveRenameNoStalePaths(t *testing.T) {
	root := t.TempDir()

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: ".", Recursive: true})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Build a/sub and let the Create events register watches on both.
	require.NoError(t, os.Mkdir(filepath.Join(root, "a"), 0o755))
	time.Sleep(watcherBatchWindow + 200*time.Millisecond)
	require.NoError(t, os.Mkdir(filepath.Join(root, "a", "sub"), 0o755))
	time.Sleep(watcherBatchWindow + 200*time.Millisecond)

	// Rename a → b; the destination Create re-adds the subtree.
	require.NoError(t, os.Rename(filepath.Join(root, "a"), filepath.Join(root, "b")))
	time.Sleep(watcherBatchWindow + 200*time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(root, "b", "sub", "file.txt"), []byte("x"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "b/sub/file.txt"
	})
	require.NotNil(t, ev, "expected CREATE for b/sub/file.txt under the new path")

	// Drain briefly: no event may reference the stale "a/" subtree.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case e, ok := <-reader.ch:
			if !ok {
				return
			}
			require.NotEqual(t, "a", e.GetPath(), "stale event under pre-rename path")
			if len(e.GetPath()) >= 2 && e.GetPath()[:2] == "a/" {
				t.Fatalf("stale event under pre-rename subtree: %v", e)
			}
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestMaybeUpdateRecursiveWatchRemovesStaleSubtree verifies that a
// Remove/Rename event on a directory drops watches on its descendants, not
// just the directory itself; stale registrations would keep reporting
// pre-rename paths.
func TestMaybeUpdateRecursiveWatchRemovesStaleSubtree(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	sub := filepath.Join(dirA, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Close() })
	require.NoError(t, watcher.Add(root))
	require.NoError(t, watcher.Add(dirA))
	require.NoError(t, watcher.Add(sub))

	s := NewFileWatcherServer(root, logr.Discard())
	s.maybeUpdateRecursiveWatch(fsnotify.Event{Name: dirA, Op: fsnotify.Rename}, watcher)

	list := watcher.WatchList()
	require.Contains(t, list, root, "unrelated watches must survive")
	require.NotContains(t, list, dirA, "renamed directory watch must be dropped")
	require.NotContains(t, list, sub, "descendant watches must be dropped")
}

// TestAddSubdirsCollectsFailures verifies that per-directory watch failures
// are collected for client reporting instead of being log-only. Skipped as
// root: root bypasses the permission bits used to make Add fail.
func TestAddSubdirsCollectsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block inotify/kqueue adds")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "nested"), 0o755))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Close() })

	s := NewFileWatcherServer(root, logr.Discard())
	failures := &watchFailures{}
	s.addSubdirs(watcher, root, failures)

	require.GreaterOrEqual(t, failures.total, 1, "expected the unreadable subtree to be reported")
	msg := failures.summarize()
	require.Contains(t, msg, "recursive watch incomplete")
	require.Contains(t, msg, locked)
}

// TestHandleFSEventSurfacesWatchFailure verifies a failed dynamic subdirectory
// watch yields a non-terminal ERROR FileEvent alongside the mapped event.
func TestHandleFSEventSurfacesWatchFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block inotify/kqueue adds")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	watcher, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Close() })
	require.NoError(t, watcher.Add(root))

	s := NewFileWatcherServer(root, logr.Discard())
	events, terminal := s.handleFSEvent(
		fsnotify.Event{Name: locked, Op: fsnotify.Create},
		root, "", "", true, watcher)

	require.False(t, terminal, "watch-setup failures must not terminate the stream")
	require.Len(t, events, 2, "expected a non-terminal ERROR event plus the mapped CREATE")
	require.Equal(t, watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR, events[0].Type)
	require.Contains(t, events[0].Error, "recursive watch incomplete")
	require.Equal(t, watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE, events[1].Type)
	require.Equal(t, "locked", events[1].Path)
}

func TestWatchFailuresSummarizeBounded(t *testing.T) {
	f := &watchFailures{}
	for i := range maxWatchFailures + 4 {
		f.add(fmt.Errorf("dir-%d: permission denied", i))
	}
	require.Equal(t, maxWatchFailures+4, f.total)
	require.Len(t, f.first, maxWatchFailures)

	msg := f.summarize()
	require.Contains(t, msg, fmt.Sprintf("%d subdirectories could not be watched", maxWatchFailures+4))
	require.Contains(t, msg, "(4 more)")
	require.NotContains(t, msg, fmt.Sprintf("dir-%d:", maxWatchFailures), "capped entries must not appear")
}

func TestWatchDirClientDisconnectCleanup(t *testing.T) {
	root := t.TempDir()
	client := newWatcherClient(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Trigger an event to confirm the stream is alive.
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644))
	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE
	})
	require.NotNil(t, ev)

	// Cancel the stream context — the reader goroutine should exit cleanly.
	cancel()

	// The channel should close within a reasonable time.
	select {
	case <-reader.ch:
		// Channel closed or has leftover event, both are fine
	case <-time.After(3 * time.Second):
		t.Fatal("reader channel did not close after context cancellation")
	}
}

func TestWatchDirSubdirWatch(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "logs")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "logs"})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(subdir, "app.log"), []byte("log"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "logs/app.log"
	})
	require.NotNil(t, ev, "expected CREATE event for logs/app.log in watched subdirectory")
}

// TestWatchDirSymlinkEscapeOnWatch verifies that a watch request whose path
// is a symlink pointing outside the sandbox root is rejected with
// PERMISSION_DENIED (SanitizePath refuses to resolve it inside the root).
func TestWatchDirSymlinkEscapeOnWatch(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// Symlink inside root → directory outside root.
	link := filepath.Join(root, "escape-link")
	require.NoError(t, os.Symlink(outside, link))

	client := newWatcherClient(t, root)
	stream, err := client.WatchDir(context.Background(), &watcherv1.WatchDirRequest{Path: "escape-link"})
	require.NoError(t, err) // streaming RPCs report errors via Recv
	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, st.Code())
}

// TestWatchDirMidWatchSymlinkSwap verifies that events arriving through a
// symlink that is swapped mid-watch to point outside the sandbox root are
// silently dropped. The server re-validates every event path via
// toSandboxRelative; a post-swap path no longer resolves inside the root
// and must not surface to the client.
func TestWatchDirMidWatchSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// Start with a real subdirectory inside the root.
	subdir := filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "work"})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Write inside the real subdir — should be visible.
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "before.txt"), []byte("x"), 0o644))
	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "work/before.txt"
	})
	require.NotNil(t, ev, "expected CREATE for work/before.txt before symlink swap")

	// Swap: remove the real subdir and replace with a symlink pointing
	// outside the sandbox root. fsnotify reports the Remove; subsequent
	// writes through the symlink resolve to the outside directory.
	require.NoError(t, os.RemoveAll(subdir))
	require.NoError(t, os.Symlink(outside, subdir))

	// Write through the escape symlink. The server's per-event
	// re-validation (toSandboxRelative → SanitizePath) must refuse to
	// map this path inside the root and drop the event.
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "after.txt"), []byte("x"), 0o644))

	// Drain for a short window — no CREATE event for after.txt should
	// arrive because the path escapes the root.
	select {
	case ev := <-reader.ch:
		if ev.GetType() == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && ev.GetPath() == "work/after.txt" {
			t.Fatal("received CREATE event for path that escapes sandbox root via symlink swap")
		}
		// Other events (e.g. the Remove of the original subdir) are fine.
	case <-time.After(500 * time.Millisecond):
		// No escape event — expected.
	}
}

// TestWatchDirRecursiveSymlinkNotFollowed verifies that recursive watch
// does NOT follow symlinks to directories, even when the symlink target is
// inside the sandbox root. Following symlinks would waste inotify watches
// and could double-report events; the Lstat guard in
// maybeUpdateRecursiveWatch prevents this.
func TestWatchDirRecursiveSymlinkNotFollowed(t *testing.T) {
	root := t.TempDir()

	// Real directory with a file, and a symlink pointing at it.
	realDir := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "inside.txt"), []byte("x"), 0o644))

	link := filepath.Join(root, "link-to-real")
	require.NoError(t, os.Symlink(realDir, link))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: ".", Recursive: true})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Write inside the real directory — should be visible via the real
	// path.
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "new.txt"), []byte("x"), 0o644))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE && e.Path == "real/new.txt"
	})
	require.NotNil(t, ev, "expected CREATE for real/new.txt")

	// The symlink should NOT have been traversed to add an additional
	// watch on realDir. We cannot directly observe "no duplicate watch"
	// from the event stream, but we can verify the symlink itself is
	// reported as a Create event (it exists at startup) and that no
	// events arrive with path prefix "link-to-real/".
	//
	// Drain briefly and assert no link-to-real/* events arrived.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev, ok := <-reader.ch:
			if !ok {
				return
			}
			if len(ev.GetPath()) >= len("link-to-real/") && ev.GetPath()[:len("link-to-real/")] == "link-to-real/" {
				t.Fatalf("unexpected event through symlink path: %v", ev)
			}
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestWatchDirSymlinkNamePreserved verifies that events for a symlink report
// the symlink's own name, not its resolved target. This guards against
// toSandboxRelative resolving the leaf name via EvalSymlinks, which would
// mis-report CREATE/MODIFY events on symlinks.
func TestWatchDirSymlinkNamePreserved(t *testing.T) {
	root := t.TempDir()
	realFile := filepath.Join(root, "real.txt")
	require.NoError(t, os.WriteFile(realFile, []byte("data"), 0o644))

	client := newWatcherClient(t, root)
	ctx := t.Context()

	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{Path: "."})
	require.NoError(t, err)
	reader := startEventReader(ctx, t, stream)
	time.Sleep(50 * time.Millisecond)

	// Create a symlink AFTER the watch starts. fsnotify reports a Create
	// event for the symlink itself; the client should see "link.txt", not
	// the resolved "real.txt".
	link := filepath.Join(root, "link.txt")
	require.NoError(t, os.Symlink("real.txt", link))

	ev := waitForEvent(t, reader, func(e *watcherv1.FileEvent) bool {
		return e.Type == watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE
	})
	require.NotNil(t, ev, "expected CREATE event for symlink")
	require.Equal(t, "link.txt", ev.Path, "event path must be the symlink name, not the resolved target")
}
