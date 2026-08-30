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

package agtsbx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// scriptedStream replays a fixed sequence of Start events and then EOF.
type scriptedStream struct {
	events []*processv1.StartResponse
	err    error
	index  int
}

func (s *scriptedStream) Recv() (*processv1.StartResponse, error) {
	if s.index >= len(s.events) {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func stdoutEvent(data string) *processv1.StartResponse {
	return &processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: []byte(data)}}
}

func stderrEvent(data string) *processv1.StartResponse {
	return &processv1.StartResponse{Event: &processv1.StartResponse_Stderr{Stderr: []byte(data)}}
}

func exitEvent(code int32) *processv1.StartResponse {
	return &processv1.StartResponse{Event: &processv1.StartResponse_Exit{Exit: &processv1.ExitEvent{ExitCode: code}}}
}

func TestCopyStream(t *testing.T) {
	t.Run("routes stdout and stderr to separate writers", func(t *testing.T) {
		stream := &scriptedStream{events: []*processv1.StartResponse{
			{Event: &processv1.StartResponse_Init{Init: &processv1.InitEvent{ProcessId: 42}}},
			stdoutEvent("hello "),
			stderrEvent("warning"),
			stdoutEvent("world"),
			exitEvent(0),
		}}

		var stdout, stderr bytes.Buffer
		code, err := copyStream(stream, &stdout, &stderr)
		require.NoError(t, err)

		assert.Equal(t, 0, code)
		// The daemon may split one logical write across several events.
		assert.Equal(t, "hello world", stdout.String())
		assert.Equal(t, "warning", stderr.String())
	})

	t.Run("propagates a non-zero exit code", func(t *testing.T) {
		stream := &scriptedStream{events: []*processv1.StartResponse{
			stderrEvent("boom"),
			exitEvent(17),
		}}

		var stdout, stderr bytes.Buffer
		code, err := copyStream(stream, &stdout, &stderr)
		require.NoError(t, err)
		assert.Equal(t, 17, code)
	})

	t.Run("errors when the stream ends without an exit event", func(t *testing.T) {
		// Reporting 0 here would turn a mid-command crash into success.
		stream := &scriptedStream{events: []*processv1.StartResponse{stdoutEvent("partial")}}

		var stdout, stderr bytes.Buffer
		_, err := copyStream(stream, &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "before reporting an exit code")
	})

	t.Run("propagates stream errors", func(t *testing.T) {
		stream := &scriptedStream{
			events: []*processv1.StartResponse{stdoutEvent("x")},
			err:    errors.New("connection reset"),
		}

		var stdout, stderr bytes.Buffer
		_, err := copyStream(stream, &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connection reset")
	})

	t.Run("ignores init events", func(t *testing.T) {
		stream := &scriptedStream{events: []*processv1.StartResponse{
			{Event: &processv1.StartResponse_Init{Init: &processv1.InitEvent{ProcessId: 7}}},
			exitEvent(0),
		}}

		var stdout, stderr bytes.Buffer
		code, err := copyStream(stream, &stdout, &stderr)
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
	})
}

func TestParseEnv(t *testing.T) {
	t.Run("nil for no entries", func(t *testing.T) {
		assert.Nil(t, parseEnv(nil))
	})

	t.Run("splits on the first separator only", func(t *testing.T) {
		// Values legitimately contain "=" (base64 padding, connection strings).
		got := parseEnv([]string{"TOKEN=abc==", "PATH=/a:/b"})
		assert.Equal(t, map[string]string{"TOKEN": "abc==", "PATH": "/a:/b"}, got)
	})

	t.Run("keeps empty values", func(t *testing.T) {
		assert.Equal(t, map[string]string{"EMPTY": ""}, parseEnv([]string{"EMPTY="}))
	})

	t.Run("drops malformed entries", func(t *testing.T) {
		assert.Empty(t, parseEnv([]string{"NOEQUALS", "=novalue"}))
	})

	t.Run("last occurrence wins", func(t *testing.T) {
		assert.Equal(t, map[string]string{"A": "2"}, parseEnv([]string{"A=1", "A=2"}))
	})
}

func TestWaitHealthy(t *testing.T) {
	t.Run("returns once the probe succeeds", func(t *testing.T) {
		// REST rather than a gRPC dial: the engine binds a published port
		// before sandboxd listens, so a TCP probe would be ready too early.
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/health", r.URL.Path)
			if attempts.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		addr := strings.TrimPrefix(server.URL, "http://")
		require.NoError(t, waitHealthy(ctx, server.Client(), addr))
		assert.GreaterOrEqual(t, attempts.Load(), int32(3))
	})

	t.Run("reports the last failure on timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		addr := strings.TrimPrefix(server.URL, "http://")
		err := waitHealthy(ctx, server.Client(), addr)
		require.Error(t, err)
		// The message must say what failed, not just that a deadline elapsed.
		assert.Contains(t, err.Error(), "HTTP 503")
	})

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		addr := strings.TrimPrefix(server.URL, "http://")
		err := waitHealthy(ctx, server.Client(), addr)
		require.Error(t, err)
	})

	t.Run("fails when nothing is listening", func(t *testing.T) {
		// Bind and immediately close to obtain a port nothing is serving.
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := strings.TrimPrefix(server.URL, "http://")
		client := server.Client()
		server.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		err := waitHealthy(ctx, client, addr)
		require.Error(t, err)
	})
}

func TestAnnotateWorkdir(t *testing.T) {
	notFound := status.Error(codes.NotFound, "command or path not found: fork/exec /usr/bin/sh")

	t.Run("adds a hint when a workdir was in effect", func(t *testing.T) {
		// sandboxd confines cwd to the sandbox root, so a host path like /tmp
		// resolves somewhere absent and the failed chdir is reported as if the
		// command binary were missing -- the wrong problem entirely.
		err := annotateWorkdir(notFound, "/tmp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolved inside the sandbox root")
		assert.Contains(t, err.Error(), "/tmp")
		// The original error must remain inspectable.
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("leaves errors alone when no workdir was set", func(t *testing.T) {
		assert.Equal(t, notFound, annotateWorkdir(notFound, ""))
	})

	t.Run("leaves nil alone", func(t *testing.T) {
		assert.NoError(t, annotateWorkdir(nil, "/tmp"))
	})

	t.Run("does not annotate unrelated failures", func(t *testing.T) {
		// A permission error already says what went wrong.
		denied := status.Error(codes.PermissionDenied, "cwd: path escapes root")
		assert.Equal(t, denied, annotateWorkdir(denied, "/tmp"))
	})
}

func TestRunCommandReportsUnreachableSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Port 1 on loopback is reserved and never served, so the RPC fails fast.
	_, err := runCommand(ctx, "127.0.0.1:1", execRequest{
		Command: []string{"true"},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	require.Error(t, err)
}
