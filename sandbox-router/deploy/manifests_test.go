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

package deploy_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

const (
	serviceAccountPath   = "/var/run/secrets/kubernetes.io/serviceaccount"
	verificationKeysPath = "/var/run/secrets/sandbox-router/scoped-token"
)

type coreKustomization struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Resources  []string `json:"resources"`
}

type overlayKustomization struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Resources  []string `json:"resources"`
	Patches    []struct {
		Path string `json:"path"`
	} `json:"patches"`
}

func readManifest[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var object T
	if err := yaml.UnmarshalStrict(data, &object); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return object
}

func routerContainer(t *testing.T, deployment appsv1.Deployment) corev1.Container {
	t.Helper()
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "sandbox-router" {
			return container
		}
	}
	t.Fatal("sandbox-router container is missing")
	return corev1.Container{}
}

// routerConfig parses container args exactly as the router binary does and
// fails the test when the result is not a configuration it would start with.
func routerConfig(t *testing.T, args []string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	fs := flag.NewFlagSet("sandbox-router", flag.ContinueOnError)
	config.RegisterFlags(fs, &cfg, nil)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse router args %v: %v", args, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("router args %v are not a valid configuration: %v", args, err)
	}
	return cfg
}

// yamlDocuments splits a multi-document manifest, skipping documents that
// hold only comments.
func yamlDocuments(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var documents [][]byte
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents
		}
		if err != nil {
			t.Fatalf("split %s: %v", path, err)
		}
		var object map[string]any
		if err := yaml.Unmarshal(document, &object); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if len(object) > 0 {
			documents = append(documents, document)
		}
	}
}

func namedVolume(t *testing.T, deployment appsv1.Deployment, name string) corev1.Volume {
	t.Helper()
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("volume %q is missing", name)
	return corev1.Volume{}
}

func TestDeploymentUsesBoundedExplicitKubernetesAPIToken(t *testing.T) {
	serviceAccount := readManifest[corev1.ServiceAccount](t, "serviceaccount.yaml")
	if serviceAccount.AutomountServiceAccountToken == nil || *serviceAccount.AutomountServiceAccountToken {
		t.Fatal("ServiceAccount must disable implicit token automount")
	}

	deployment := readManifest[appsv1.Deployment](t, "deployment.yaml")
	podSpec := deployment.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Fatal("Pod must disable implicit token automount")
	}
	container := routerContainer(t, deployment)
	var foundMount bool
	for _, mount := range container.VolumeMounts {
		if mount.Name == "kubernetes-api-access" {
			foundMount = mount.ReadOnly && mount.MountPath == serviceAccountPath
		}
	}
	if !foundMount {
		t.Fatalf("kubernetes-api-access must be read-only at %s", serviceAccountPath)
	}

	volume := namedVolume(t, deployment, "kubernetes-api-access")
	if volume.Projected == nil {
		t.Fatal("kubernetes-api-access must be a projected volume")
	}
	var tokenProjection *corev1.ServiceAccountTokenProjection
	for _, source := range volume.Projected.Sources {
		if source.ServiceAccountToken != nil {
			tokenProjection = source.ServiceAccountToken
		}
	}
	if tokenProjection == nil {
		t.Fatal("projected ServiceAccount token is missing")
	}
	// An explicit audience would have to match every cluster's API
	// server; empty selects the API server's own audience.
	if tokenProjection.Audience != "" {
		t.Fatalf("token audience: got %q want the API server default", tokenProjection.Audience)
	}
	if tokenProjection.ExpirationSeconds == nil || *tokenProjection.ExpirationSeconds != 3600 {
		t.Fatalf("token lifetime: got %v want 3600 seconds", tokenProjection.ExpirationSeconds)
	}
	if tokenProjection.Path != "token" {
		t.Fatalf("token path: got %q want token", tokenProjection.Path)
	}

	// client-go's in-cluster config also reads ca.crt (API server trust)
	// and namespace from the same directory.
	var foundCA, foundNamespace bool
	for _, source := range volume.Projected.Sources {
		if cm := source.ConfigMap; cm != nil && cm.Name == "kube-root-ca.crt" {
			foundCA = len(cm.Items) == 1 && cm.Items[0].Key == "ca.crt" && cm.Items[0].Path == "ca.crt"
		}
		if api := source.DownwardAPI; api != nil {
			foundNamespace = len(api.Items) == 1 && api.Items[0].Path == "namespace" &&
				api.Items[0].FieldRef != nil && api.Items[0].FieldRef.FieldPath == "metadata.namespace"
		}
	}
	if !foundCA {
		t.Fatal("projection must map kube-root-ca.crt key ca.crt to ca.crt")
	}
	if !foundNamespace {
		t.Fatal("projection must map metadata.namespace to namespace")
	}
}

func TestCoreDeploymentArgsAreAValidRouterConfiguration(t *testing.T) {
	deployment := readManifest[appsv1.Deployment](t, "deployment.yaml")
	cfg := routerConfig(t, routerContainer(t, deployment).Args)
	if cfg.AuthzMode != config.AuthzAllowAll || !cfg.CacheEnabled {
		t.Fatalf("core install must keep allow-all with the Pod cache: mode=%q cache=%v", cfg.AuthzMode, cfg.CacheEnabled)
	}
}

func TestInformerRBACRemainsPodReadOnly(t *testing.T) {
	documents := yamlDocuments(t, "rbac.yaml")
	if len(documents) != 2 {
		t.Fatalf("rbac.yaml: got %d documents want 2", len(documents))
	}

	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(documents[0], &role); err != nil {
		t.Fatalf("parse ClusterRole: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("ClusterRole rules: got %d want 1", len(role.Rules))
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{""}) ||
		!slices.Equal(rule.Resources, []string{"pods"}) ||
		!slices.Equal(rule.Verbs, []string{"get", "list", "watch"}) ||
		len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
		t.Fatalf("unexpected informer RBAC rule: %+v", rule)
	}

	var binding rbacv1.ClusterRoleBinding
	if err := yaml.UnmarshalStrict(documents[1], &binding); err != nil {
		t.Fatalf("parse ClusterRoleBinding: %v", err)
	}
	if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "sandbox-router" {
		t.Fatalf("unexpected roleRef: %+v", binding.RoleRef)
	}
	wantSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: "sandbox-router", Namespace: "agent-sandbox-system"}
	if len(binding.Subjects) != 1 || binding.Subjects[0] != wantSubject {
		t.Fatalf("unexpected subjects: %+v", binding.Subjects)
	}
}

func TestCoreKustomizationExcludesTokenReviewRBAC(t *testing.T) {
	kustomization := readManifest[coreKustomization](t, "kustomization.yaml")
	wantResources := []string{
		"serviceaccount.yaml",
		"rbac.yaml",
		"deployment.yaml",
		"service.yaml",
		"pdb.yaml",
		"networkpolicy.yaml",
	}
	if !slices.Equal(kustomization.Resources, wantResources) {
		t.Fatalf("core resources: got %v want %v", kustomization.Resources, wantResources)
	}
	if slices.Contains(kustomization.Resources, "rbac-tokenreview.yaml") {
		t.Fatal("core deployment must not grant TokenReview RBAC")
	}
}

func TestScopedTokenV2OverlayComposesPatchOntoCoreInstall(t *testing.T) {
	// Applying the patch after the core install would start allow-all
	// replicas first, so the overlay must carry both.
	overlay := readManifest[overlayKustomization](t, "../examples/scoped-token-v2/kustomization.yaml")
	if !slices.Equal(overlay.Resources, []string{"../../deploy"}) {
		t.Fatalf("overlay resources: got %v want the core install ../../deploy", overlay.Resources)
	}
	if len(overlay.Patches) != 1 || overlay.Patches[0].Path != "deployment-patch.yaml" {
		t.Fatalf("overlay patches: got %+v want deployment-patch.yaml", overlay.Patches)
	}
}

func TestScopedTokenV2PatchRendersVersionedKeyDistribution(t *testing.T) {
	baseYAML, err := os.ReadFile("deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment.yaml: %v", err)
	}
	patchYAML, err := os.ReadFile("../examples/scoped-token-v2/deployment-patch.yaml")
	if err != nil {
		t.Fatalf("read scoped-token patch: %v", err)
	}
	baseJSON, err := yaml.YAMLToJSON(baseYAML)
	if err != nil {
		t.Fatalf("convert deployment to JSON: %v", err)
	}
	patchJSON, err := yaml.YAMLToJSON(patchYAML)
	if err != nil {
		t.Fatalf("convert patch to JSON: %v", err)
	}
	renderedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, patchJSON, appsv1.Deployment{})
	if err != nil {
		t.Fatalf("render scoped-token deployment: %v", err)
	}
	var deployment appsv1.Deployment
	if err := json.Unmarshal(renderedJSON, &deployment); err != nil {
		t.Fatalf("parse rendered deployment: %v", err)
	}

	container := routerContainer(t, deployment)
	cfg := routerConfig(t, container.Args)
	if cfg.AuthzMode != config.AuthzScopedToken || cfg.AuthzScopedTokenVerificationKeysFile != verificationKeysPath+"/verification-keys.json" {
		t.Fatalf("rendered args do not activate scoped-token v2: %v", container.Args)
	}
	if cfg.AuthzScopedTokenSecretFile != "" {
		t.Fatalf("v1 secret must not be configured in the v2 deployment: %q", cfg.AuthzScopedTokenSecretFile)
	}
	// A strategic merge replaces the args list wholesale, so the patch must
	// restate every core flag or the overlay silently drops it.
	base := readManifest[appsv1.Deployment](t, "deployment.yaml")
	for _, arg := range routerContainer(t, base).Args {
		if !slices.Contains(container.Args, arg) {
			t.Fatalf("scoped-token patch drops core router arg %q", arg)
		}
	}
	var foundMount bool
	for _, mount := range container.VolumeMounts {
		if mount.Name == "scoped-token-verification-keys" {
			foundMount = mount.ReadOnly && mount.MountPath == verificationKeysPath
		}
	}
	if !foundMount {
		t.Fatalf("verification keys must be read-only at %s", verificationKeysPath)
	}
	var keptAPIMount bool
	for _, mount := range container.VolumeMounts {
		if mount.Name == "kubernetes-api-access" {
			keptAPIMount = mount.MountPath == serviceAccountPath
		}
	}
	if !keptAPIMount {
		t.Fatal("scoped-token patch must keep the Kubernetes API credential mount")
	}
	if apiVolume := namedVolume(t, deployment, "kubernetes-api-access"); apiVolume.Projected == nil {
		t.Fatal("scoped-token patch must keep the projected Kubernetes API credential")
	}
	volume := namedVolume(t, deployment, "scoped-token-verification-keys")
	if volume.ConfigMap == nil || volume.ConfigMap.Name != "sandbox-router-scoped-token-keys-v1" {
		t.Fatalf("unexpected verification-key ConfigMap: %+v", volume.ConfigMap)
	}
	if len(volume.ConfigMap.Items) != 1 || volume.ConfigMap.Items[0].Key != "verification-keys.json" || volume.ConfigMap.Items[0].Path != "verification-keys.json" {
		t.Fatalf("unexpected verification-key projection: %+v", volume.ConfigMap.Items)
	}
}
