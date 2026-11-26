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
	"fmt"
	"io"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	watcherv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/watcher/v1"
)

// FileEventType classifies a filesystem change observed by WatchDir.
type FileEventType string

const (
	// FileEventCreate indicates a new file or directory was created.
	FileEventCreate FileEventType = "create"
	// FileEventWrite indicates an existing file's contents were modified.
	FileEventWrite FileEventType = "write"
	// FileEventRemove indicates a file or directory was removed.
	FileEventRemove FileEventType = "remove"
	// FileEventRename indicates a file or directory was renamed or moved
	// out of the watched tree. Path carries the OLD (source) name; when
	// the new name lands inside the watched tree it arrives as a separate
	// FileEventCreate. Pair the two to reconstruct renames.
	FileEventRename FileEventType = "rename"
	// FileEventChmod indicates permissions or ownership changed.
	FileEventChmod FileEventType = "chmod"
	// FileEventError indicates a watch error. Terminal errors are the
	// last event before the stream ends; non-fatal ones (event overflow,
	// unwatchable subdirectories in recursive mode) arrive mid-stream
	// and the watch continues.
	FileEventError FileEventType = "error"
)

// FileEvent describes a single filesystem change within a watched directory.
type FileEvent struct {
	// Type classifies the change.
	Type FileEventType
	// Path is the sandbox-relative path of the affected file or
	// directory. For Rename events this is the old (source) path; the
	// new path, when inside the watched tree, arrives as a separate
	// Create event.
	Path string
	// Error carries a human-readable message for Error events; empty otherwise.
	Error string
}

// Watchers provides filesystem event streaming on a sandbox.
type Watchers struct {
	connector    *connector
	runtime      Runtime
	tracer       trace.Tracer
	svcName      string
	log          logr.Logger
	errPrefix    func() string
	trackOp      func() func()
	lifecycleCtx func() context.Context
}

// WatchDir streams filesystem events for the given sandbox-relative directory
// path. When recursive is true, subdirectories are watched too: directories
// created during the watch are added automatically and removed ones dropped.
// Events are delivered on the returned channel until the context is
// cancelled, the watched directory is removed, or an error occurs. The channel
// is closed when no more events will arrive.
//
// A nil return does not mean the watch was established: server-side rejections
// such as NOT_FOUND (path does not exist) and PERMISSION_DENIED (path escapes
// the sandbox root) arrive on the first Recv and are delivered as a terminal
// FileEvent with Type == FileEventError before the channel is closed. Callers
// must therefore consume the channel and inspect events even when WatchDir
// returns without error.
//
// The caller MUST consume events from the channel (or cancel ctx) to avoid
// leaking the background goroutine. When ctx is cancelled, the background
// goroutine exits and the channel is closed; events already buffered in
// the channel remain consumable, but events still in flight on the stream
// may be dropped.
//
// An explicit WithTimeout covers the entire stream lifetime; there is no
// default timeout. WatchDir never retries and rejects WithMaxAttempts > 1,
// like Commands.Start.
//
// With the legacy runtime (non-sandboxd), WatchDir returns an error because
// the legacy python-runtime does not implement FileWatcherService.
func (w *Watchers) WatchDir(ctx context.Context, path string, recursive bool, opts ...CallOption) (_ <-chan FileEvent, retErr error) {
	defer w.trackOp()()
	co := resolveCallOpts(opts)
	ctx, span := startSpan(withLifecycleSpan(ctx, w.lifecycleCtx()), w.tracer, w.svcName, "watchers.watch_dir",
		attribute.String("sandbox.path", path))
	// An explicit WithTimeout covers the whole stream, so the receiveEvents
	// goroutine owns callCancel — it must NOT be deferred here. The error
	// paths below release it via the retErr defer.
	var callCancel context.CancelFunc
	if co.timeout > 0 {
		ctx, callCancel = context.WithTimeout(ctx, co.timeout)
	} else {
		callCancel = func() {}
	}
	// For a streaming RPC the span covers the full stream lifetime: the
	// receiveEvents goroutine owns span.End(). On early error paths below,
	// we end the span here so it does not leak.
	defer func() {
		if retErr != nil {
			callCancel()
			span.End()
		}
	}()

	if w.runtime != RuntimeSandboxd {
		retErr = fmt.Errorf("%s: watch_dir: %w: the legacy python-runtime does not implement FileWatcherService (requires the sandboxd runtime)", w.errPrefix(), ErrUnsupportedByRuntime)
		recordError(span, retErr)
		return nil, retErr
	}
	if co.maxAttempts > 1 {
		retErr = fmt.Errorf("%s: watch_dir: WithMaxAttempts greater than 1 is unsupported: watch streams cannot be replayed", w.errPrefix())
		recordError(span, retErr)
		return nil, retErr
	}
	span.SetAttributes(attribute.Bool("sandbox.recursive", recursive))

	conn, err := w.connector.GRPCConn()
	if err != nil {
		retErr = fmt.Errorf("%s: watch_dir: %w", w.errPrefix(), err)
		recordError(span, retErr)
		return nil, retErr
	}

	client := watcherv1.NewFileWatcherServiceClient(conn)
	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{
		Path:      path,
		Recursive: recursive,
	})
	if err != nil {
		if st, ok := status.FromError(err); ok {
			retErr = fmt.Errorf("%s: watch_dir: file watcher service returned %s: %s: %w",
				w.errPrefix(), st.Code(), st.Message(), err)
		} else {
			retErr = fmt.Errorf("%s: watch_dir: %w", w.errPrefix(), err)
		}
		recordError(span, retErr)
		return nil, retErr
	}

	ch := make(chan FileEvent, 64)
	go w.receiveEvents(ctx, stream, ch, span, callCancel)
	return ch, nil
}

// receiveEvents reads FileEvents from the gRPC stream and forwards them to
// the channel. When the stream ends (EOF, error, or context cancellation),
// the channel is closed. This goroutine owns the span's lifecycle and the
// WithTimeout cancel func derived in WatchDir (a no-op when unset).
func (w *Watchers) receiveEvents(ctx context.Context, stream watcherv1.FileWatcherService_WatchDirClient, ch chan<- FileEvent, span trace.Span, callCancel context.CancelFunc) {
	defer close(ch)
	defer span.End()
	defer callCancel()

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// Context cancellation is the normal shutdown path; don't
			// log it as an error.
			if ctx.Err() != nil {
				return
			}
			if st, ok := status.FromError(err); ok && st.Code() == codes.Canceled {
				return
			}
			w.log.Error(err, "watch stream error")
			recordError(span, err)
			// Surface the error as a terminal event so the consumer
			// sees it rather than just a channel close.
			select {
			case ch <- FileEvent{Type: FileEventError, Error: err.Error()}:
			case <-ctx.Done():
			}
			return
		}

		sdkEvent := FileEvent{
			Type:  fileEventTypeFromProto(ev.GetType()),
			Path:  ev.GetPath(),
			Error: ev.GetError(),
		}

		select {
		case ch <- sdkEvent:
		case <-ctx.Done():
			return
		}
	}
}

// fileEventTypeFromProto maps the proto enum to the SDK string type.
func fileEventTypeFromProto(t watcherv1.FileEventType) FileEventType {
	switch t {
	case watcherv1.FileEventType_FILE_EVENT_TYPE_CREATE:
		return FileEventCreate
	case watcherv1.FileEventType_FILE_EVENT_TYPE_WRITE:
		return FileEventWrite
	case watcherv1.FileEventType_FILE_EVENT_TYPE_REMOVE:
		return FileEventRemove
	case watcherv1.FileEventType_FILE_EVENT_TYPE_RENAME:
		return FileEventRename
	case watcherv1.FileEventType_FILE_EVENT_TYPE_CHMOD:
		return FileEventChmod
	case watcherv1.FileEventType_FILE_EVENT_TYPE_ERROR:
		return FileEventError
	default:
		return FileEventType(fmt.Sprintf("unknown(%d)", t))
	}
}
