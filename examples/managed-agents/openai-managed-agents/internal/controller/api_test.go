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

package controller

import (
	"context"
	"errors"
	"testing"
)

func TestFakeAPIListManagedOnly(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{
		"s1": {ID: "s1", Managed: true},
		"s2": {ID: "s2", Managed: false},
	}}
	got, err := api.ListManagedSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("listed %+v", got)
	}
	if api.ListCalls != 1 {
		t.Fatalf("ListCalls = %d", api.ListCalls)
	}
}

func TestFakeAPIListOverride(t *testing.T) {
	api := &FakeAPI{
		ListOverride: []SessionInfo{{ID: "stale", NeedsConnection: true, Managed: true}},
		Sessions: map[string]SessionInfo{
			"stale": {ID: "stale", NeedsConnection: false, Managed: true},
		},
	}
	got, err := api.ListManagedSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].NeedsConnection {
		t.Fatalf("override not used: %+v", got)
	}
	// Get serves the fresh state.
	fresh, err := api.GetSession(context.Background(), "stale")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.NeedsConnection {
		t.Fatal("Get should return fresh state")
	}
}

func TestFakeAPIGetNotFound(t *testing.T) {
	api := &FakeAPI{Sessions: map[string]SessionInfo{}}
	_, err := api.GetSession(context.Background(), "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestFakeAPIErrors(t *testing.T) {
	api := &FakeAPI{ListErr: errors.New("429"), Sessions: map[string]SessionInfo{}}
	if _, err := api.ListManagedSessions(context.Background()); err == nil {
		t.Fatal("want list error")
	}
	api2 := &FakeAPI{GetErr: errors.New("500"), Sessions: map[string]SessionInfo{"s1": {ID: "s1"}}}
	if _, err := api2.GetSession(context.Background(), "s1"); err == nil {
		t.Fatal("want get error")
	}
}
