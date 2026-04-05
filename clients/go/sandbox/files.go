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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
)

const maxErrorBodySize = 512            // limits untrusted server content in error chains
const maxMetadataResponseSize = 8 << 20 // 8 MB; bounds List/Exists JSON decode

const upperHex = "0123456789ABCDEF"

// percentEncode encodes a string using percent-encoding for all bytes
// outside the RFC 3986 unreserved set (A-Za-z0-9 - _ . ~).
// All special characters including '/' are encoded.
func percentEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

// applyCallOpts applies per-call options, returning a context with any
// WithTimeout deadline and the configured max retry count (0 = default).
func applyCallOpts(ctx context.Context, opts []CallOption) (context.Context, context.CancelFunc, int) {
	var co callOptions
	for _, o := range opts {
		o(&co)
	}
	if co.timeout > 0 {
		ctx, cancel := context.WithTimeout(ctx, co.timeout)
		return ctx, cancel, co.maxAttempts
	}
	return ctx, func() {}, co.maxAttempts
}

// Files provides file operations on a sandbox.
type Files struct {
	connector    *connector
	tracer       trace.Tracer
	svcName      string
	log          logr.Logger
	maxDownload  int64
	maxUpload    int64
	errPrefix    func() string
	trackOp      func() func()
	lifecycleCtx func() context.Context
}

// Write uploads content to the sandbox. The path must be a plain filename
// without directory separators (e.g., "script.py", not "dir/script.py").
//
// The entire content is buffered in memory as a multipart form body to
// support retries on transient failures. Content exceeding MaxUploadSize
// (default 256 MB) is rejected before any network I/O.
func (f *Files) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error {
	defer f.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := startSpan(withLifecycleSpan(ctx, f.lifecycleCtx()), f.tracer, f.svcName, "write", AttrFilePath.String(path), AttrFileSize.Int(len(content)))
	defer span.End()

	if int64(len(content)) > f.maxUpload {
		err := fmt.Errorf("%s: write(%q): content size %d exceeds MaxUploadSize %d", f.errPrefix(), path, len(content), f.maxUpload)
		recordError(span, err)
		return err
	}

	base := pathpkg.Base(path)
	if base == "." || base == ".." || base == "/" || base != path {
		err := fmt.Errorf("%s: write: %q is not a plain filename (resolved to %q); pass only the filename, not a path with directories", f.errPrefix(), path, base)
		recordError(span, err)
		return err
	}
	var buf bytes.Buffer
	buf.Grow(len(content) + 512)
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", base)
	if err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: failed to create form file: %w", f.errPrefix(), err)
	}
	if _, err := part.Write(content); err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: failed to write content: %w", f.errPrefix(), err)
	}
	if err := writer.Close(); err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: failed to close multipart writer: %w", f.errPrefix(), err)
	}

	resp, err := f.connector.SendRequest(ctx, http.MethodPost, "upload", bytes.NewReader(buf.Bytes()), writer.FormDataContentType(), maxAttempts)
	if err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: write(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		retErr := fmt.Errorf("%s: write(%q): %w", f.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "write"})
		recordError(span, retErr)
		return retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()
	f.log.V(1).Info("write completed", "path", path, "size", len(content))
	return nil
}

// Read downloads a file from the sandbox.
func (f *Files) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error) {
	defer f.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := startSpan(withLifecycleSpan(ctx, f.lifecycleCtx()), f.tracer, f.svcName, "read", AttrFilePath.String(path))
	defer span.End()

	if path == "" {
		err := fmt.Errorf("%s: read: path must not be empty", f.errPrefix())
		recordError(span, err)
		return nil, err
	}

	encoded := percentEncode(path)
	resp, err := f.connector.SendRequest(ctx, http.MethodGet, "download/"+encoded, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: read(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		retErr := fmt.Errorf("%s: read(%q): %w", f.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "read"})
		recordError(span, retErr)
		return nil, retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxDownload+1))
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: failed to read file content: %w", f.errPrefix(), err)
	}
	if int64(len(data)) > f.maxDownload {
		err := fmt.Errorf("%s: file size exceeds limit of %d bytes", f.errPrefix(), f.maxDownload)
		recordError(span, err)
		return nil, err
	}
	span.SetAttributes(AttrFileSize.Int(len(data)))
	f.log.V(1).Info("read completed", "path", path, "size", len(data))
	return data, nil
}

// List returns the contents of a directory in the sandbox.
func (f *Files) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error) {
	defer f.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := startSpan(withLifecycleSpan(ctx, f.lifecycleCtx()), f.tracer, f.svcName, "list", AttrFilePath.String(path))
	defer span.End()

	if path == "" {
		err := fmt.Errorf("%s: list: path must not be empty", f.errPrefix())
		recordError(span, err)
		return nil, err
	}

	encoded := percentEncode(path)
	resp, err := f.connector.SendRequest(ctx, http.MethodGet, "list/"+encoded, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: list(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		retErr := fmt.Errorf("%s: list(%q): %w", f.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "list"})
		recordError(span, retErr)
		return nil, retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()

	var entries []FileEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataResponseSize)).Decode(&entries); err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: failed to decode file listing: %w", f.errPrefix(), err)
	}
	filtered := make([]FileEntry, 0, len(entries))
	for i := range entries {
		if entries[i].Type == FileTypeFile || entries[i].Type == FileTypeDirectory {
			filtered = append(filtered, entries[i])
		} else {
			f.log.V(1).Info("skipping entry with unsupported file type", "path", path, "entry", entries[i].Name, "type", entries[i].Type)
		}
	}
	entries = filtered
	span.SetAttributes(AttrFileCount.Int(len(entries)))
	f.log.V(1).Info("list completed", "path", path, "entries", len(entries))
	return entries, nil
}

// Exists checks if a file or directory exists at the given path in the sandbox.
func (f *Files) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error) {
	defer f.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := startSpan(withLifecycleSpan(ctx, f.lifecycleCtx()), f.tracer, f.svcName, "exists", AttrFilePath.String(path))
	defer span.End()

	if path == "" {
		err := fmt.Errorf("%s: exists: path must not be empty", f.errPrefix())
		recordError(span, err)
		return false, err
	}

	encoded := percentEncode(path)
	resp, err := f.connector.SendRequest(ctx, http.MethodGet, "exists/"+encoded, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return false, fmt.Errorf("%s: exists(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		retErr := fmt.Errorf("%s: exists(%q): %w", f.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "exists"})
		recordError(span, retErr)
		return false, retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()

	var result struct {
		Exists bool `json:"exists"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataResponseSize)).Decode(&result); err != nil {
		recordError(span, err)
		return false, fmt.Errorf("%s: failed to decode exists response: %w", f.errPrefix(), err)
	}
	span.SetAttributes(AttrFileExists.Bool(result.Exists))
	f.log.V(1).Info("exists completed", "path", path, "exists", result.Exists)
	return result.Exists, nil
}
