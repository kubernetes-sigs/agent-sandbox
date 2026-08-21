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
	"math"
	"mime/multipart"
	"net/http"
	pathpkg "path"
	"slices"
	"strings"
	"time"

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
	for i := range len(s) {
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

func encodeFilePath(path string) string {
	encoded := percentEncode(path)
	switch encoded {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return encoded
	}
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
	runtime      Runtime
	tracer       trace.Tracer
	svcName      string
	log          logr.Logger
	maxDownload  int64
	maxUpload    int64
	errPrefix    func() string
	trackOp      func() func()
	lifecycleCtx func() context.Context
}

// filesEndpoint returns the sandboxd REST path for a file path.
func filesEndpoint(path string) string {
	return "v1/files/" + encodeFilePath(path)
}

// httpErrorFromResponse builds an HTTPError from a non-2xx response. For the
// sandboxd runtime the body is an APIError JSON document; its message is
// surfaced directly when it decodes cleanly.
func (f *Files) httpErrorFromResponse(resp *http.Response, op string) *HTTPError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if f.runtime == RuntimeSandboxd {
		var apiErr sandboxdAPIError
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Code != "" {
			return &HTTPError{StatusCode: resp.StatusCode, Body: apiErr.Code + ": " + apiErr.Message, Operation: op}
		}
	}
	return &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: op}
}

// Write uploads content to the sandbox.
//
// With the legacy runtime the path must be a plain filename without
// directory separators (e.g., "script.py", not "dir/script.py"). The
// sandboxd runtime supports relative paths and creates parent directories
// automatically.
//
// The entire content is buffered in memory to support retries on transient
// failures. Content exceeding MaxUploadSize (default 256 MB) is rejected
// before any network I/O.
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

	var method, endpoint, contentType string
	var body *bytes.Reader
	var err error
	if f.runtime == RuntimeSandboxd {
		method, endpoint, contentType, body, err = f.sandboxdWriteReq(path, content)
	} else {
		method, endpoint, contentType, body, err = f.legacyWriteReq(path, content)
	}
	if err != nil {
		recordError(span, err)
		return err
	}

	resp, err := f.connector.SendRequest(ctx, method, endpoint, body, contentType, maxAttempts)
	if err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: write(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retErr := fmt.Errorf("%s: write(%q): %w", f.errPrefix(), path, f.httpErrorFromResponse(resp, "write"))
		recordError(span, retErr)
		return retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()
	f.log.V(1).Info("write completed", "path", path, "size", len(content))
	return nil
}

// sandboxdWriteReq builds the sandboxd PUT request: an idempotent write of the
// raw bytes to /v1/files/{path}. Parent directories are created server-side.
// Relative paths are allowed, but ".." components that escape the sandbox root
// are rejected client-side as defense in depth (sandboxd also enforces this
// server-side via SanitizePath).
func (f *Files) sandboxdWriteReq(path string, content []byte) (method, endpoint, contentType string, body *bytes.Reader, err error) {
	if path == "" || path == "." || path == ".." || strings.HasSuffix(path, "/") {
		return "", "", "", nil, fmt.Errorf("%s: write: %q is not a valid file path", f.errPrefix(), path)
	}
	if slices.Contains(strings.Split(path, "/"), "..") {
		return "", "", "", nil, fmt.Errorf("%s: write: %q must not contain %q path segments (escapes the sandbox root)", f.errPrefix(), path, "..")
	}
	return http.MethodPut, filesEndpoint(path), "application/octet-stream", bytes.NewReader(content), nil
}

// legacyWriteReq builds the python-runtime multipart upload request. The path
// must be a plain filename (no directory separators); the whole body is
// buffered so the request can be retried.
func (f *Files) legacyWriteReq(path string, content []byte) (method, endpoint, contentType string, body *bytes.Reader, err error) {
	base := pathpkg.Base(path)
	if base == "." || base == ".." || base == "/" || base != path {
		return "", "", "", nil, fmt.Errorf("%s: write: %q is not a plain filename (resolved to %q); pass only the filename, not a path with directories", f.errPrefix(), path, base)
	}
	var buf bytes.Buffer
	buf.Grow(len(content) + 512)
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", base)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("%s: failed to create form file: %w", f.errPrefix(), err)
	}
	if _, err := part.Write(content); err != nil {
		return "", "", "", nil, fmt.Errorf("%s: failed to write content: %w", f.errPrefix(), err)
	}
	if err := writer.Close(); err != nil {
		return "", "", "", nil, fmt.Errorf("%s: failed to close multipart writer: %w", f.errPrefix(), err)
	}
	return http.MethodPost, "upload", writer.FormDataContentType(), bytes.NewReader(buf.Bytes()), nil
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

	endpoint := "download/" + encodeFilePath(path)
	if f.runtime == RuntimeSandboxd {
		endpoint = filesEndpoint(path)
	}
	resp, err := f.connector.SendRequest(ctx, http.MethodGet, endpoint, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: read(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retErr := fmt.Errorf("%s: read(%q): %w", f.errPrefix(), path, f.httpErrorFromResponse(resp, "read"))
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

	endpoint := "list/" + encodeFilePath(path)
	if f.runtime == RuntimeSandboxd {
		endpoint = filesEndpoint(path)
	}
	resp, err := f.connector.SendRequest(ctx, http.MethodGet, endpoint, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: list(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retErr := fmt.Errorf("%s: list(%q): %w", f.errPrefix(), path, f.httpErrorFromResponse(resp, "list"))
		recordError(span, retErr)
		return nil, retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()

	var entries []FileEntry
	limited := io.LimitReader(resp.Body, maxMetadataResponseSize)
	if f.runtime == RuntimeSandboxd {
		var listing sandboxdDirectoryListing
		if err := json.NewDecoder(limited).Decode(&listing); err != nil {
			recordError(span, err)
			return nil, fmt.Errorf("%s: failed to decode file listing: %w", f.errPrefix(), err)
		}
		entries = make([]FileEntry, 0, len(listing.Entries))
		for _, e := range listing.Entries {
			if e.Type != FileTypeFile && e.Type != FileTypeDirectory {
				f.log.V(1).Info("skipping entry with unsupported file type", "path", path, "entry", e.Name, "type", e.Type)
				continue
			}
			// RFC3339Nano so fractional-second timestamps are preserved
			// (it also parses whole-second RFC3339 values).
			modTime, parseErr := time.Parse(time.RFC3339Nano, e.ModifiedAt)
			if parseErr != nil {
				f.log.V(1).Info("entry has unparseable modified_at; using zero time", "path", path, "entry", e.Name, "modified_at", e.ModifiedAt)
			}
			entries = append(entries, FileEntry{Name: e.Name, Size: e.Size, Type: e.Type, ModTime: modTime, Mode: e.Mode})
		}
	} else {
		var legacy []legacyFileEntry
		if err := json.NewDecoder(limited).Decode(&legacy); err != nil {
			recordError(span, err)
			return nil, fmt.Errorf("%s: failed to decode file listing: %w", f.errPrefix(), err)
		}
		entries = make([]FileEntry, 0, len(legacy))
		for _, e := range legacy {
			if e.Type != FileTypeFile && e.Type != FileTypeDirectory {
				f.log.V(1).Info("skipping entry with unsupported file type", "path", path, "entry", e.Name, "type", e.Type)
				continue
			}
			sec, frac := math.Modf(e.ModTime)
			entries = append(entries, FileEntry{Name: e.Name, Size: e.Size, Type: e.Type, ModTime: time.Unix(int64(sec), int64(frac*1e9)).UTC()})
		}
	}
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

	if f.runtime == RuntimeSandboxd {
		// sandboxd has no dedicated exists endpoint: HEAD on the file path
		// answers existence without transferring the body (200 vs 404).
		resp, err := f.connector.SendRequest(ctx, http.MethodHead, filesEndpoint(path), nil, "", maxAttempts)
		if err != nil {
			recordError(span, err)
			return false, fmt.Errorf("%s: exists(%q) failed: %w", f.errPrefix(), path, err)
		}
		defer resp.Body.Close()
		defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			span.SetAttributes(AttrFileExists.Bool(true))
			f.log.V(1).Info("exists completed", "path", path, "exists", true)
			return true, nil
		case resp.StatusCode == http.StatusNotFound:
			span.SetAttributes(AttrFileExists.Bool(false))
			f.log.V(1).Info("exists completed", "path", path, "exists", false)
			return false, nil
		default:
			retErr := fmt.Errorf("%s: exists(%q): %w", f.errPrefix(), path, f.httpErrorFromResponse(resp, "exists"))
			recordError(span, retErr)
			return false, retErr
		}
	}

	encoded := encodeFilePath(path)
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

// Delete removes a file or directory in the sandbox. When recursive is
// true, directories are removed with their contents (rm -rf semantics);
// otherwise deleting a non-empty directory fails with a 409 HTTPError.
//
// Only supported by the sandboxd runtime: the legacy python-runtime has no
// delete endpoint, and calls return ErrUnsupportedByRuntime.
func (f *Files) Delete(ctx context.Context, path string, recursive bool, opts ...CallOption) error {
	defer f.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := startSpan(withLifecycleSpan(ctx, f.lifecycleCtx()), f.tracer, f.svcName, "delete", AttrFilePath.String(path))
	defer span.End()

	if f.runtime != RuntimeSandboxd {
		err := fmt.Errorf("%s: delete(%q): %w: the legacy python-runtime has no delete endpoint", f.errPrefix(), path, ErrUnsupportedByRuntime)
		recordError(span, err)
		return err
	}
	if path == "" {
		err := fmt.Errorf("%s: delete: path must not be empty", f.errPrefix())
		recordError(span, err)
		return err
	}

	endpoint := filesEndpoint(path)
	if recursive {
		endpoint += "?recursive=true"
	}
	resp, err := f.connector.SendRequest(ctx, http.MethodDelete, endpoint, nil, "", maxAttempts)
	if err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: delete(%q) failed: %w", f.errPrefix(), path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retErr := fmt.Errorf("%s: delete(%q): %w", f.errPrefix(), path, f.httpErrorFromResponse(resp, "delete"))
		recordError(span, retErr)
		return retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()
	f.log.V(1).Info("delete completed", "path", path, "recursive", recursive)
	return nil
}
