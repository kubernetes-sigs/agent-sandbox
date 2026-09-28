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
	// FileEventRename indicates a file or directory was renamed.
	FileEventRename FileEventType = "rename"
	// FileEventChmod indicates permissions or ownership changed.
	FileEventChmod FileEventType = "chmod"
	// FileEventError indicates a watch error; the stream is terminating.
	FileEventError FileEventType = "error"
)

// FileEvent describes a single filesystem change within a watched directory.
type FileEvent struct {
	// Type classifies the change.
	Type FileEventType
	// Path is the sandbox-relative path of the affected file or directory.
	Path string
	// OldPath is reserved for future use. Currently always empty because
	// the server does not receive both rename endpoints from fsnotify.
	OldPath string
	// Error carries a human-readable message for Error events; empty otherwise.
	Error string
}

// WatchOptions configures a WatchDir call.
type WatchOptions struct {
	// Recursive watches subdirectories when true. New subdirectories
	// created during the watch are automatically added; removed ones
	// are dropped.
	Recursive bool
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
// path. Events are delivered on the returned channel until the context is
// cancelled, the watched directory is removed, or an error occurs. The channel
// is closed when no more events will arrive.
//
// The caller MUST consume events from the channel (or cancel ctx) to avoid
// leaking the background goroutine. When ctx is cancelled, the channel is
// closed after any in-flight events are drained.
//
// With the legacy runtime (non-sandboxd), WatchDir returns an error because
// the legacy python-runtime does not implement FileWatcherService.
func (w *Watchers) WatchDir(ctx context.Context, path string, opts ...WatchOptions) (_ <-chan FileEvent, retErr error) {
	defer w.trackOp()()
	ctx, span := startSpan(withLifecycleSpan(ctx, w.lifecycleCtx()), w.tracer, w.svcName, "watchers.watch_dir",
		attribute.String("sandbox.path", path))
	// For a streaming RPC the span covers the full stream lifetime: the
	// receiveEvents goroutine owns span.End(). On early error paths below,
	// we end the span here so it does not leak.
	defer func() {
		if retErr != nil {
			span.End()
		}
	}()

	if w.runtime != RuntimeSandboxd {
		err := fmt.Errorf("%s: watch_dir requires the sandboxd runtime", w.errPrefix())
		recordError(span, err)
		return nil, err
	}

	var opt WatchOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	span.SetAttributes(attribute.Bool("sandbox.recursive", opt.Recursive))

	conn, err := w.connector.GRPCConn()
	if err != nil {
		err = fmt.Errorf("%s: watch_dir: %w", w.errPrefix(), err)
		recordError(span, err)
		return nil, err
	}

	client := watcherv1.NewFileWatcherServiceClient(conn)
	stream, err := client.WatchDir(ctx, &watcherv1.WatchDirRequest{
		Path:      path,
		Recursive: opt.Recursive,
	})
	if err != nil {
		if st, ok := status.FromError(err); ok {
			err = fmt.Errorf("%s: watch_dir: file watcher service returned %s: %s: %w",
				w.errPrefix(), st.Code(), st.Message(), err)
		} else {
			err = fmt.Errorf("%s: watch_dir: %w", w.errPrefix(), err)
		}
		recordError(span, err)
		return nil, err
	}

	ch := make(chan FileEvent, 64)
	go w.receiveEvents(ctx, stream, ch, span)
	return ch, nil
}

// receiveEvents reads FileEvents from the gRPC stream and forwards them to
// the channel. When the stream ends (EOF, error, or context cancellation),
// the channel is closed. This goroutine owns the span's lifecycle.
func (w *Watchers) receiveEvents(ctx context.Context, stream watcherv1.FileWatcherService_WatchDirClient, ch chan<- FileEvent, span trace.Span) {
	defer close(ch)
	defer span.End()

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
			// Surface the error as a terminal event so the consumer
			// sees it rather than just a channel close.
			select {
			case ch <- FileEvent{Type: FileEventError, Error: err.Error()}:
			case <-ctx.Done():
			}
			return
		}

		sdkEvent := FileEvent{
			Type:    fileEventTypeFromProto(ev.GetType()),
			Path:    ev.GetPath(),
			OldPath: ev.GetOldPath(),
			Error:   ev.GetError(),
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
