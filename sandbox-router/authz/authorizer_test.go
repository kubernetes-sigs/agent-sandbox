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

package authz

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestAllowAllAuthorize(t *testing.T) {
	a := AllowAll{}
	req, _ := http.NewRequest("GET", "/", nil)
	if err := a.Authorize(context.Background(), req, testAuthorizationTarget("any", "any")); err != nil {
		t.Fatalf("AllowAll should never deny: %v", err)
	}
	if err := a.Authorize(context.Background(), nil, AuthorizationTarget{}); err != nil {
		t.Fatalf("AllowAll should not care about nil request: %v", err)
	}
}

func TestNormalizeAuthorizationTarget(t *testing.T) {
	target, err := NormalizeAuthorizationTarget(AuthorizationTarget{
		Namespace:   "team-a",
		SandboxName: "box-a",
		SandboxUID:  "uid-a",
		Port:        9090,
		Method:      "post",
		Path:        "/exec/some%2ffile",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if target.Method != "POST" || target.Path != "/exec/some%2Ffile" {
		t.Fatalf("got method %q path %q, want POST and /exec/some%%2Ffile", target.Method, target.Path)
	}
}

func TestNormalizeAuthorizationTarget_RejectsInvalidPathEncoding(t *testing.T) {
	target := testAuthorizationTarget("team-a", "box-a")
	target.Path = "/exec/%ZZ"
	if _, err := NormalizeAuthorizationTarget(target); err == nil {
		t.Fatal("expected invalid percent-encoding to be rejected")
	}
}

func TestHTTPStatusFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil → 200", nil, http.StatusOK},
		{"unauth → 401", ErrUnauthenticated, http.StatusUnauthorized},
		{"forbidden → 403", ErrForbidden, http.StatusForbidden},
		{"wrapped unauth → 401", errors.Join(errors.New("ctx"), ErrUnauthenticated), http.StatusUnauthorized},
		{"wrapped forbidden → 403", errors.Join(errors.New("ctx"), ErrForbidden), http.StatusForbidden},
		{"unknown → 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTPStatusFor(tc.err); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
