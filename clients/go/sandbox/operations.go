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
)

const maxErrorBodySize = 512              // limits untrusted server content in error chains
const maxMetadataResponseSize = 8 << 20   // 8 MB; bounds List/Exists JSON decode
const maxExecutionResponseSize = 16 << 20 // 16 MB; bounds Run JSON decode

const upperHex = "0123456789ABCDEF"

// percentEncode encodes a string using percent-encoding for all bytes
// outside the RFC 3986 unreserved set (A-Za-z0-9 - _ . ~).
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

// applyCallOpts applies per-call options and returns a context governed by
// any WithTimeout override. The returned CancelFunc must always be deferred.
func applyCallOpts(ctx context.Context, opts []CallOption) (context.Context, context.CancelFunc) {
	var co callOptions
	for _, o := range opts {
		o(&co)
	}
	if co.timeout > 0 {
		return context.WithTimeout(ctx, co.timeout)
	}
	return ctx, func() {}
}

// Run executes a command in the sandbox and returns the result.
func (c *SandboxClient) Run(ctx context.Context, command string, opts ...CallOption) (_ *ExecutionResult, retErr error) {
	defer c.trackOp()()
	ctx, callCancel := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := c.startSpan(ctx, "run", AttrCommand.String(command))
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	payload, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return nil, fmt.Errorf("%s: failed to marshal command: %w", c.errPrefix(), err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "execute", bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, fmt.Errorf("%s: run failed: %w", c.errPrefix(), err)
	}
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("%s: run: %w", c.errPrefix(), &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "run"})
	}

	result := ExecutionResult{ExitCode: -1}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxExecutionResponseSize)).Decode(&result); err != nil {
		return nil, fmt.Errorf("%s: failed to decode run result: %w", c.errPrefix(), err)
	}
	span.SetAttributes(AttrExitCode.Int(result.ExitCode))
	c.log.Info("run completed", "exitCode", result.ExitCode)
	return &result, nil
}

// Write uploads content to the sandbox. Only the filename (base name of path)
// is sent to the server; directory components are discarded.
func (c *SandboxClient) Write(ctx context.Context, path string, content []byte, opts ...CallOption) (retErr error) {
	defer c.trackOp()()
	ctx, callCancel := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := c.startSpan(ctx, "write", AttrFilePath.String(path), AttrFileSize.Int(len(content)))
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	base := pathpkg.Base(path)
	if base == "." || base == ".." || base == "/" {
		return fmt.Errorf("%s: write: invalid filename %q derived from path %q", c.errPrefix(), base, path)
	}
	var buf bytes.Buffer
	buf.Grow(len(content) + 512) // pre-size for content + multipart overhead
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", base)
	if err != nil {
		return fmt.Errorf("%s: failed to create form file: %w", c.errPrefix(), err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("%s: failed to write content: %w", c.errPrefix(), err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("%s: failed to close multipart writer: %w", c.errPrefix(), err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "upload", bytes.NewReader(buf.Bytes()), writer.FormDataContentType())
	if err != nil {
		return fmt.Errorf("%s: write(%q) failed: %w", c.errPrefix(), path, err)
	}
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return fmt.Errorf("%s: write(%q): %w", c.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "write"})
	}
	c.log.Info("write completed", "path", path, "size", len(content))
	return nil
}

// Read downloads a file from the sandbox.
func (c *SandboxClient) Read(ctx context.Context, path string, opts ...CallOption) (_ []byte, retErr error) {
	defer c.trackOp()()
	ctx, callCancel := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := c.startSpan(ctx, "read", AttrFilePath.String(path))
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	encoded := percentEncode(path)
	resp, err := c.doRequest(ctx, http.MethodGet, "download/"+encoded, nil, "")
	if err != nil {
		return nil, fmt.Errorf("%s: read(%q) failed: %w", c.errPrefix(), path, err)
	}
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("%s: read(%q): %w", c.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "read"})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxDownloadSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read file content: %w", c.errPrefix(), err)
	}
	if int64(len(data)) > c.opts.MaxDownloadSize {
		return nil, fmt.Errorf("%s: file size exceeds limit of %d bytes", c.errPrefix(), c.opts.MaxDownloadSize)
	}
	span.SetAttributes(AttrFileSize.Int(len(data)))
	c.log.Info("read completed", "path", path, "size", len(data))
	return data, nil
}

// List returns the contents of a directory in the sandbox.
func (c *SandboxClient) List(ctx context.Context, path string, opts ...CallOption) (_ []FileEntry, retErr error) {
	defer c.trackOp()()
	ctx, callCancel := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := c.startSpan(ctx, "list", AttrFilePath.String(path))
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	encoded := percentEncode(path)
	resp, err := c.doRequest(ctx, http.MethodGet, "list/"+encoded, nil, "")
	if err != nil {
		return nil, fmt.Errorf("%s: list(%q) failed: %w", c.errPrefix(), path, err)
	}
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("%s: list(%q): %w", c.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "list"})
	}

	var entries []FileEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataResponseSize)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("%s: failed to decode file listing: %w", c.errPrefix(), err)
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	for i := range entries {
		if entries[i].Type != FileTypeFile && entries[i].Type != FileTypeDirectory {
			return nil, fmt.Errorf("%s: list(%q): unsupported file type %q for entry %q", c.errPrefix(), path, entries[i].Type, entries[i].Name)
		}
	}
	span.SetAttributes(AttrFileCount.Int(len(entries)))
	c.log.Info("list completed", "path", path, "entries", len(entries))
	return entries, nil
}

// Exists checks if a file or directory exists at the given path in the sandbox.
func (c *SandboxClient) Exists(ctx context.Context, path string, opts ...CallOption) (_ bool, retErr error) {
	defer c.trackOp()()
	ctx, callCancel := applyCallOpts(ctx, opts)
	defer callCancel()
	ctx, span := c.startSpan(ctx, "exists", AttrFilePath.String(path))
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	encoded := percentEncode(path)
	resp, err := c.doRequest(ctx, http.MethodGet, "exists/"+encoded, nil, "")
	if err != nil {
		return false, fmt.Errorf("%s: exists(%q) failed: %w", c.errPrefix(), path, err)
	}
	defer resp.Body.Close()
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return false, fmt.Errorf("%s: exists(%q): %w", c.errPrefix(), path, &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "exists"})
	}

	var result struct {
		Exists bool `json:"exists"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataResponseSize)).Decode(&result); err != nil {
		return false, fmt.Errorf("%s: failed to decode exists response: %w", c.errPrefix(), err)
	}
	span.SetAttributes(AttrFileExists.Bool(result.Exists))
	c.log.Info("exists completed", "path", path, "exists", result.Exists)
	return result.Exists, nil
}
