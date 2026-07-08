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

	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestAllowAllAuthorize(t *testing.T) {
	a := AllowAll{}
	req, _ := http.NewRequest("GET", "/", nil)
	if err := a.Authorize(context.Background(), req, "any", "any"); err != nil {
		t.Fatalf("AllowAll should never deny: %v", err)
	}
	if err := a.Authorize(context.Background(), nil, "", ""); err != nil {
		t.Fatalf("AllowAll should not care about nil request: %v", err)
	}
}

func TestFromUserInfo(t *testing.T) {
	u := authenticationv1.UserInfo{
		Username: "user@example.com",
		UID:      "uid-1",
		Groups:   []string{"g1", "g2"},
		Extra: map[string]authenticationv1.ExtraValue{
			"k": {"v1", "v2"},
		},
	}
	got := FromUserInfo(u, "bearer-token")
	want := Identity{
		Username: "user@example.com",
		UID:      "uid-1",
		Groups:   []string{"g1", "g2"},
		Extra:    map[string][]string{"k": {"v1", "v2"}},
		Source:   "bearer-token",
	}
	if !equalIdentity(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func equalIdentity(a, b Identity) bool {
	if a.Username != b.Username || a.UID != b.UID || a.Source != b.Source {
		return false
	}
	if len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Groups {
		if a.Groups[i] != b.Groups[i] {
			return false
		}
	}
	if len(a.Extra) != len(b.Extra) {
		return false
	}
	for k, va := range a.Extra {
		vb, ok := b.Extra[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
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

func TestIdentityIsAuthenticated(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"empty", Identity{}, false},
		{"username only", Identity{Username: "u"}, true},
		{"uid only", Identity{UID: "abc"}, true},
		{"groups only", Identity{Groups: []string{"g"}}, true},
		{"source set but nothing else", Identity{Source: "tls"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.IsAuthenticated(); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
