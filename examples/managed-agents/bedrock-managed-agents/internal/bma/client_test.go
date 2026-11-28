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

package bma

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", ""),
	}
	c, err := NewClient(context.Background(), Options{
		Region:    "us-east-1",
		Endpoint:  srv.URL,
		AccountID: "123456789012",
		AWSConfig: &cfg,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestDoSignsRequest(t *testing.T) {
	var gotAuth, gotAccount, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("x-amzn-account-id")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sess_1"}`))
	}))
	defer srv.Close()

	var out struct {
		ID string `json:"id"`
	}
	c := testClient(t, srv)
	if err := c.Do(context.Background(), "GET", "/agents/sessions/sess_1", nil, nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.ID != "sess_1" {
		t.Errorf("decoded id = %q", out.ID)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want SigV4", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/bedrock-mantle/aws4_request") {
		t.Errorf("Authorization scope = %q, want region/service", gotAuth)
	}
	if !strings.Contains(gotAuth, "x-amzn-account-id") {
		t.Errorf("x-amzn-account-id not signed: %q", gotAuth)
	}
	if gotAccount != "123456789012" {
		t.Errorf("x-amzn-account-id = %q", gotAccount)
	}
	if gotPath != "/openai/v1/agents/sessions/sess_1" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestDoQueryAndBody(t *testing.T) {
	var gotQuery, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := testClient(t, srv)
	err := c.Do(context.Background(), "POST", "/agents/sessions",
		map[string][]string{"limit": {"100"}},
		strings.NewReader(`{"stream":false}`), nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotQuery != "limit=100" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotBody != `{"stream":false}` {
		t.Errorf("body = %q", gotBody)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
}

func TestDoStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amzn-requestid", "req-1")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"gone"}`))
	}))
	defer srv.Close()
	err := testClient(t, srv).Do(context.Background(), "GET", "/agents/sessions/x", nil, nil, nil)
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.RequestId != "req-1" {
		t.Errorf("StatusError = %+v, want request id", se)
	}
}

func TestDoUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := testClient(t, srv).Do(context.Background(), "GET", "/agents/sessions", nil, nil, nil)
	if !IsUnauthorized(err) {
		t.Fatalf("IsUnauthorized(%v) = false", err)
	}
}

func TestNewClientRequiresRegion(t *testing.T) {
	if _, err := NewClient(context.Background(), Options{}); err == nil {
		t.Fatal("NewClient without region succeeded, want error")
	}
}
