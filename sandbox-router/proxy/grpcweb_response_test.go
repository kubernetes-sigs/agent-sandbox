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

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// trailerFillingReader mimics the Go h2 client: the body streams message
// frames, and the trailers are installed on the response — as a fresh map,
// replacing the nil one a gRPC backend response starts with — at the
// moment the stream ends (see copyTrailers in the bundled h2 transport).
type trailerFillingReader struct {
	frames  []byte
	resp    *http.Response
	status  string
	extra   map[string]string
	err     error // when set, returned instead of a clean EOF
	sentErr bool
}

func (r *trailerFillingReader) Read(p []byte) (int, error) {
	if len(r.frames) > 0 {
		n := copy(p, r.frames)
		r.frames = r.frames[n:]
		return n, nil
	}
	if r.err != nil {
		if r.sentErr {
			return 0, io.EOF
		}
		r.sentErr = true
		return 0, r.err
	}
	if r.resp != nil && r.status != "" {
		trailers := http.Header{}
		trailers.Set("Grpc-Status", r.status)
		for k, v := range r.extra {
			trailers.Set(k, v)
		}
		r.resp.Trailer = trailers
	}
	return 0, io.EOF
}

func (r *trailerFillingReader) Close() error { return nil }

func messageFrame(payload string) []byte {
	f := make([]byte, 5+len(payload))
	f[0] = 0
	be32(f[1:5], uint32(len(payload)))
	copy(f[5:], payload)
	return f
}

func be32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func TestPrepareGRPCWebResponse_TrailersOnly(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":   []string{"application/grpc"},
			"Content-Length": []string{"0"}, // stale value the frame synthesis must replace
			"Grpc-Status":    []string{"7"},
			"Grpc-Message":   []string{"denied"},
			"X-Proc-Meta":    []string{"v"},
			"Date":           []string{"whatever"},
		},
		Body:          io.NopCloser(bytes.NewReader(nil)),
		ContentLength: 0,
	}
	var recorded []string
	if err := prepareGRPCWebResponse(resp, func(s string) { recorded = append(recorded, s) }); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)

	want := string(encodeTrailerFrame([]byte("grpc-status:7\r\ngrpc-message:denied\r\nx-proc-meta:v\r\n")))
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("content-length = %d, want %d", resp.ContentLength, len(body))
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length header = %q, want %d (stale value must be replaced)", got, len(body))
	}
	if resp.Header.Get("Grpc-Status") != "" || resp.Header.Get("Grpc-Message") != "" {
		t.Fatal("status headers must be removed from the initial block")
	}
	if resp.Header.Get("Date") == "" {
		t.Fatal("known HTTP fields must stay in the initial block")
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentTypeGRPCWeb {
		t.Fatalf("content-type = %q, want %q", ct, contentTypeGRPCWeb)
	}
	if resp.Trailer != nil {
		t.Fatalf("trailer announcement must be cleared, got %v", resp.Trailer)
	}
	if len(recorded) != 1 || recorded[0] != "7" {
		t.Fatalf("recorded = %v, want [7] exactly once", recorded)
	}
}

func TestPrepareGRPCWebResponse_ContentTypeCodecAndCORS(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":                []string{"application/grpc+proto"},
			"Grpc-Status":                 []string{"0"},
			"Access-Control-Allow-Origin": []string{"https://backend.example"},
		},
		Body: io.NopCloser(bytes.NewReader(nil)),
	}
	if err := prepareGRPCWebResponse(resp, func(string) {}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/grpc-web+proto" {
		t.Fatalf("content-type = %q, want the codec suffix preserved", ct)
	}
	// The router owns CORS for gPRC-Web; a backend echo would duplicate
	// the header the handler already stamped.
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("backend CORS headers must be stripped")
	}
}

func TestGRPCWebBodyTransformer_Streaming(t *testing.T) {
	frames := append(messageFrame("one"), messageFrame("two")...)
	// A compressed-flag frame must cross byte-identical: the transformer
	// never decodes message payloads.
	compressed := messageFrame("gz")
	compressed[0] = 0x01
	frames = append(frames, compressed...)

	resp := &http.Response{}
	src := &trailerFillingReader{frames: frames, resp: resp, status: "0",
		extra: map[string]string{"X-Exit-Code": "0"}}
	resp.Body = src
	var recorded []string
	tr := newGRPCWebBodyTransformer(resp, func(s string) { recorded = append(recorded, s) })

	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := append(append(messageFrame("one"), messageFrame("two")...), compressed...)
	want = append(want, encodeTrailerFrame([]byte("grpc-status:0\r\nx-exit-code:0\r\n"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("output mismatch:\n got %q\nwant %q", got, want)
	}
	if len(recorded) != 1 || recorded[0] != "0" {
		t.Fatalf("recorded = %v, want [0] exactly once", recorded)
	}
}

func TestGRPCWebBodyTransformer_MidStreamError(t *testing.T) {
	// A grpc-web client can only learn the RPC outcome from a trailer
	// frame; an aborted body without one leaves it hanging. The
	// transformer must synthesize Unavailable.
	src := &trailerFillingReader{frames: messageFrame("partial"), err: errors.New("connection reset")}
	resp := &http.Response{Body: src}
	src.resp = resp
	var recorded []string
	tr := newGRPCWebBodyTransformer(resp, func(s string) { recorded = append(recorded, s) })

	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read must end cleanly with a synthesized frame, got %v", err)
	}
	want := append(messageFrame("partial"),
		encodeTrailerFrame([]byte("grpc-status:14\r\ngrpc-message:upstream stream terminated\r\n"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("output mismatch:\n got %q\nwant %q", got, want)
	}
	if len(recorded) != 1 || recorded[0] != "14" {
		t.Fatalf("recorded = %v, want [14]", recorded)
	}
}

func TestGRPCWebBodyTransformer_DeadlineExceededMidStream(t *testing.T) {
	// When the RPC deadline (grpc-timeout or --grpc-proxy-timeout) fires
	// mid-stream, the body read fails with an error wrapping
	// context.DeadlineExceeded. The client must see DeadlineExceeded, not
	// the generic Unavailable the broken-stream path synthesizes.
	src := &trailerFillingReader{frames: messageFrame("partial"),
		err: fmt.Errorf("stream read: %w", context.DeadlineExceeded)}
	resp := &http.Response{Body: src}
	src.resp = resp
	var recorded []string
	tr := newGRPCWebBodyTransformer(resp, func(s string) { recorded = append(recorded, s) })

	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read must end cleanly with a synthesized frame, got %v", err)
	}
	want := append(messageFrame("partial"),
		encodeTrailerFrame([]byte("grpc-status:4\r\ngrpc-message:context deadline exceeded\r\n"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("output mismatch:\n got %q\nwant %q", got, want)
	}
	if len(recorded) != 1 || recorded[0] != "4" {
		t.Fatalf("recorded = %v, want [4]", recorded)
	}
}

func TestGRPCWebBodyTransformer_DeadlineExceededViaRequestContext(t *testing.T) {
	// The h2 transport can surface the deadline as an opaque stream error
	// that does not wrap context.DeadlineExceeded. The outbound request's
	// context carries the RPC deadline, so the transformer must consult it
	// before falling back to Unavailable.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	src := &trailerFillingReader{frames: messageFrame("partial"),
		err: errors.New("http2: stream closed")}
	resp := &http.Response{Body: src, Request: &http.Request{}}
	resp.Request = resp.Request.WithContext(ctx)
	src.resp = resp
	tr := newGRPCWebBodyTransformer(resp, func(string) {})

	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read must end cleanly with a synthesized frame, got %v", err)
	}
	want := append(messageFrame("partial"),
		encodeTrailerFrame([]byte("grpc-status:4\r\ngrpc-message:context deadline exceeded\r\n"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("output mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestGRPCWebBodyTransformer_EOFWithoutStatus(t *testing.T) {
	src := &trailerFillingReader{frames: messageFrame("x")} // clean EOF, trailers stay nil
	resp := &http.Response{Body: src}
	src.resp = resp
	var recorded []string
	tr := newGRPCWebBodyTransformer(resp, func(s string) { recorded = append(recorded, s) })

	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := append(messageFrame("x"),
		encodeTrailerFrame([]byte("grpc-status:13\r\ngrpc-message:backend closed stream without status\r\n"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("output mismatch:\n got %q\nwant %q", got, want)
	}
	if len(recorded) != 1 || recorded[0] != "13" {
		t.Fatalf("recorded = %v, want [13]", recorded)
	}
}

func TestPrepareGRPCWebResponse_StreamingClearsContentLength(t *testing.T) {
	// A backend-supplied Content-Length covers the message bytes only;
	// the appended trailer frame makes the body longer, and an HTTP/1.1
	// writer that kept the stale header would fail with ErrContentLength.
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":   []string{"application/grpc"},
			"Content-Length": []string{"5"},
		},
		Body:          &trailerFillingReader{frames: messageFrame("x"), status: "0"},
		ContentLength: 5,
	}
	resp.Body.(*trailerFillingReader).resp = resp
	if err := prepareGRPCWebResponse(resp, func(string) {}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length header = %q, want it removed", got)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1 (unknown, stream-framed)", resp.ContentLength)
	}
}
