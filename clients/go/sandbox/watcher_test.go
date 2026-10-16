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

package sandbox

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
)

// fakeWatcherService is a test double for FileWatcherService. It records
// the request and sends pre-configured events on the stream.
type fakeWatcherService struct {
	watcherv1.UnimplementedFileWatcherServiceServer
	mu          sync.Mutex
	lastRequest *watcherv1.WatchDirRequest
	handlerDone chan struct{} // closed when WatchDir handler has recorded the request
	events      []*watcherv1.FileEvent
	err         error // if set, returned from WatchDir instead of streaming
}

func newFakeWatcherService() *fakeWatcherService {
	return &fakeWatcherService{handlerDone: make(chan struct{})}
}

func (f *fakeWatcherService) WatchDir(req *watcherv1.WatchDirRequest, stream watcherv1.FileWatcherService_WatchDirServer) error {
	f.mu.Lock()
	f.lastRequest = req
	close(f.handlerDone)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	for _, ev := range f.events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	// Keep the stream open until the client cancels.
	<-stream.Context().Done()
	return stream.Context().Err()
}

// waitForHandler blocks until the WatchDir handler has recorded the request.
func (f *fakeWatcherService) waitForHandler(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-f.handlerDone:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for WatchDir handler to be invoked")
	}
}

// startFakeWatcherServer runs a gRPC FileWatcherService on an ephemeral
// loopback port and wires the sandbox's connector at it.
func startFakeWatcherServer(t *testing.T, sb *Sandbox, svc *fakeWatcherService) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	watcherv1.RegisterFileWatcherServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = lis.Close()
	})
	sb.connector.SetGRPCTarget(lis.Addr().String())
}

func TestWatchers_WatchDir_ReceivesEvents(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://localhost:0")

	svc := newFakeWatcherService()
	svc.events = []*watcherv1.FileEvent{
		{Type: watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE, Path: "hello.txt"},
		{Type: watcherv1.FileEventType_FILE_EVENT_TYPE_WRITE, Path: "hello.txt"},
		{Type: watcherv1.FileEventType_FILE_EVENT_TYPE_REMOVE, Path: "hello.txt"},
	}
	startFakeWatcherServer(t, sb, svc)

	ctx := t.Context()

	ch, err := sb.WatchDir(ctx, ".")
	require.NoError(t, err)

	var events []FileEvent
	timeout := time.After(3 * time.Second)
	for range 3 {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "channel closed prematurely")
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}

	require.Len(t, events, 3)
	require.Equal(t, FileEventCreate, events[0].Type)
	require.Equal(t, "hello.txt", events[0].Path)
	require.Equal(t, FileEventWrite, events[1].Type)
	require.Equal(t, FileEventRemove, events[2].Type)

	svc.waitForHandler(t, 3*time.Second)
	require.Equal(t, ".", svc.lastRequest.GetPath())
	require.False(t, svc.lastRequest.GetRecursive())
}

func TestWatchers_WatchDir_Recursive(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://localhost:0")

	svc := newFakeWatcherService()
	startFakeWatcherServer(t, sb, svc)

	ctx, cancel := context.WithCancel(context.Background())

	_, err := sb.WatchDir(ctx, "src", WatchOptions{Recursive: true})
	require.NoError(t, err)

	// Wait for server to receive the request, then cancel.
	svc.waitForHandler(t, 3*time.Second)
	cancel()

	require.Equal(t, "src", svc.lastRequest.GetPath())
	require.True(t, svc.lastRequest.GetRecursive())
}

func TestWatchers_WatchDir_GRPCError(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://localhost:0")

	svc := newFakeWatcherService()
	svc.err = status.Errorf(codes.PermissionDenied, "path escapes sandbox root")
	startFakeWatcherServer(t, sb, svc)

	ctx := t.Context()

	// The server returns the error from the handler; it surfaces via
	// stream.Recv() in the background goroutine as a terminal Error event.
	ch, err := sb.WatchDir(ctx, "../../etc")
	require.NoError(t, err)

	// Read the error event from the channel.
	select {
	case ev := <-ch:
		require.Equal(t, FileEventError, ev.Type)
		require.Contains(t, ev.Error, "PermissionDenied")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for error event")
	}
}

func TestWatchers_WatchDir_RenameEvent(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://localhost:0")

	svc := newFakeWatcherService()
	svc.events = []*watcherv1.FileEvent{
		{
			Type:    watcherv1.FileEventType_FILE_EVENT_TYPE_RENAME,
			Path:    "new.txt",
			OldPath: "old.txt",
		},
	}
	startFakeWatcherServer(t, sb, svc)

	ctx := t.Context()

	ch, err := sb.WatchDir(ctx, ".")
	require.NoError(t, err)

	select {
	case ev := <-ch:
		require.Equal(t, FileEventRename, ev.Type)
		require.Equal(t, "new.txt", ev.Path)
		require.Equal(t, "old.txt", ev.OldPath)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for rename event")
	}
}

func TestWatchers_WatchDir_NotSandboxdRuntime(t *testing.T) {
	opts := Options{
		Namespace:      "default",
		Runtime:        RuntimeLegacyPython,
		RequestTimeout: 5 * time.Second,
		Quiet:          true,
	}
	opts.setDefaults()
	opts.K8sHelper = &K8sHelper{Log: opts.Logger}
	sb, err := New(context.Background(), opts)
	require.NoError(t, err)

	_, err = sb.WatchDir(context.Background(), ".")
	require.Error(t, err)
	require.Contains(t, err.Error(), "sandboxd runtime")
}
