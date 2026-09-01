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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteResourcesKustomization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kustomization.yaml")
	resources := []string{"deployment.yaml", "service.yaml"}
	if err := writeResourcesKustomization(path, resources); err != nil {
		t.Fatalf("writeResourcesKustomization: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kustomization.yaml: %v", err)
	}
	want := "resources:\n- deployment.yaml\n- service.yaml\n"
	if string(got) != want {
		t.Fatalf("kustomization.yaml mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRewriteRouterNamespaceResourceList(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"deployment.yaml",
		"service.yaml",
		"networkpolicy.yaml",
		"rbac-tokenreview.yaml",
	} {
		body := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: default\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := rewriteRouterNamespace(dir, "agent-sandbox-system"); err != nil {
		t.Fatalf("rewriteRouterNamespace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization.yaml: %v", err)
	}
	text := string(got)
	if !strings.HasPrefix(text, "resources:\n") {
		t.Fatalf("kustomization.yaml should be a resources list, got:\n%s", text)
	}
	if !strings.Contains(text, "- deployment.yaml\n") || !strings.Contains(text, "- service.yaml\n") {
		t.Fatalf("kustomization.yaml missing included resources:\n%s", text)
	}
	if strings.Contains(text, "networkpolicy.yaml") || strings.Contains(text, "rbac-tokenreview.yaml") {
		t.Fatalf("kustomization.yaml included excluded resources:\n%s", text)
	}
	if strings.Contains(text, "images:") {
		t.Fatalf("generated kustomization.yaml must not pin images:\n%s", text)
	}
	rewritten, err := os.ReadFile(filepath.Join(dir, "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment.yaml: %v", err)
	}
	if !strings.Contains(string(rewritten), "namespace: agent-sandbox-system") {
		t.Fatalf("deployment.yaml namespace not rewritten:\n%s", rewritten)
	}
}

func TestWriteCRDKustomization(t *testing.T) {
	dir := t.TempDir()
	basesDir := filepath.Join(dir, "bases")
	if err := os.Mkdir(basesDir, 0o755); err != nil {
		t.Fatalf("mkdir bases: %v", err)
	}
	crdStub := []byte("apiVersion: apiextensions.k8s.io/v1\n")
	for _, name := range []string{
		"agents.x-k8s.io_sandboxes.yaml",
		"extensions.agents.x-k8s.io_sandboxclaims.yaml",
	} {
		if err := os.WriteFile(filepath.Join(basesDir, name), crdStub, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(basesDir, "README.md"), []byte("skip me\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	if err := os.Mkdir(filepath.Join(basesDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(basesDir, "nested", "ignored.yaml"), []byte("kind: X\n"), 0o644); err != nil {
		t.Fatalf("write nested yaml: %v", err)
	}

	if err := writeCRDKustomization(basesDir); err != nil {
		t.Fatalf("writeCRDKustomization: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization.yaml: %v", err)
	}
	want := "resources:\n- bases/agents.x-k8s.io_sandboxes.yaml\n- bases/extensions.agents.x-k8s.io_sandboxclaims.yaml\n"
	if string(got) != want {
		t.Fatalf("kustomization.yaml mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(basesDir, "kustomization.yaml")); !os.IsNotExist(err) {
		t.Fatalf("kustomization.yaml should be written next to bases/, not inside it: %v", err)
	}
}

func TestRewriteRouterNamespaceBindings(t *testing.T) {
	dir := t.TempDir()
	// ClusterRoleBinding has only subjects[].namespace; RoleBinding has both
	// metadata.namespace and subjects[].namespace. Both must be rewritten.
	binding := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: sandbox-router
subjects:
- kind: ServiceAccount
  name: sandbox-router
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: sandbox-router
  namespace: default
subjects:
- kind: ServiceAccount
  name: sandbox-router
  namespace: default
`
	if err := os.WriteFile(filepath.Join(dir, "rbac.yaml"), []byte(binding), 0o644); err != nil {
		t.Fatalf("write rbac.yaml: %v", err)
	}
	if err := rewriteRouterNamespace(dir, "agent-sandbox-system"); err != nil {
		t.Fatalf("rewriteRouterNamespace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "rbac.yaml"))
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	text := string(got)
	if strings.Contains(text, "namespace: default") {
		t.Fatalf("leftover namespace: default in binding:\n%s", text)
	}
	wantCRB := `kind: ClusterRoleBinding
metadata:
  name: sandbox-router
subjects:
- kind: ServiceAccount
  name: sandbox-router
  namespace: agent-sandbox-system`
	if !strings.Contains(text, wantCRB) {
		t.Fatalf("ClusterRoleBinding subjects[].namespace not rewritten:\n%s", text)
	}
	wantRB := `kind: RoleBinding
metadata:
  name: sandbox-router
  namespace: agent-sandbox-system`
	if !strings.Contains(text, wantRB) {
		t.Fatalf("RoleBinding metadata.namespace not rewritten:\n%s", text)
	}
	wantSubject := `kind: ServiceAccount
  name: sandbox-router
  namespace: agent-sandbox-system`
	if n := strings.Count(text, wantSubject); n != 2 {
		t.Fatalf("want 2 rewritten subjects[].namespace fields, got %d:\n%s", n, text)
	}
}
