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
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

const (
	kubernetesAPIAudience = "https://kubernetes.default.svc"
	serviceAccountPath    = "/var/run/secrets/kubernetes.io/serviceaccount"
	verificationKeysPath  = "/var/run/secrets/sandbox-router/scoped-token"
)

type coreKustomization struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Resources  []string `json:"resources"`
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
	if tokenProjection.Audience != kubernetesAPIAudience {
		t.Fatalf("token audience: got %q want %q", tokenProjection.Audience, kubernetesAPIAudience)
	}
	if tokenProjection.ExpirationSeconds == nil || *tokenProjection.ExpirationSeconds != 3600 {
		t.Fatalf("token lifetime: got %v want 3600 seconds", tokenProjection.ExpirationSeconds)
	}
	if tokenProjection.Path != "token" {
		t.Fatalf("token path: got %q want token", tokenProjection.Path)
	}
}

func TestInformerRBACRemainsPodReadOnly(t *testing.T) {
	data, err := os.ReadFile("rbac.yaml")
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	documents := strings.Split(string(data), "\n---\n")
	if len(documents) != 2 {
		t.Fatalf("rbac.yaml: got %d documents want 2", len(documents))
	}

	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict([]byte(documents[0]), &role); err != nil {
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
	if err := yaml.UnmarshalStrict([]byte(documents[1]), &binding); err != nil {
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

func TestDeploymentGatesReadinessOnTheCacheEnabledRouter(t *testing.T) {
	deployment := readManifest[appsv1.Deployment](t, "deployment.yaml")
	container := routerContainer(t, deployment)
	if !slices.Contains(container.Args, "--cache-enabled=true") {
		t.Fatal("deployment must enable the informer cache")
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet == nil {
		t.Fatal("readiness HTTP probe is missing")
	}
	probe := container.ReadinessProbe.HTTPGet
	if probe.Path != "/readyz" || probe.Port.StrVal != "healthz" {
		t.Fatalf("unexpected readiness probe: %+v", probe)
	}
}

func TestScopedTokenV2PatchRendersVersionedKeyDistribution(t *testing.T) {
	baseYAML, err := os.ReadFile("deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment.yaml: %v", err)
	}
	patchYAML, err := os.ReadFile("examples/scoped-token-v2/deployment-patch.yaml")
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
	wantKeyArg := "--authz-scoped-token-verification-keys-file=" + verificationKeysPath + "/verification-keys.json"
	if !slices.Contains(container.Args, "--authz-mode=scoped-token") || !slices.Contains(container.Args, wantKeyArg) {
		t.Fatalf("rendered args do not activate scoped-token v2: %v", container.Args)
	}
	for _, arg := range container.Args {
		if strings.HasPrefix(arg, "--authz-scoped-token-secret-file=") {
			t.Fatalf("v1 secret must not be configured in the v2 deployment: %q", arg)
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
	volume := namedVolume(t, deployment, "scoped-token-verification-keys")
	if volume.ConfigMap == nil || volume.ConfigMap.Name != "sandbox-router-scoped-token-keys-v1" {
		t.Fatalf("unexpected verification-key ConfigMap: %+v", volume.ConfigMap)
	}
	if len(volume.ConfigMap.Items) != 1 || volume.ConfigMap.Items[0].Key != "verification-keys.json" || volume.ConfigMap.Items[0].Path != "verification-keys.json" {
		t.Fatalf("unexpected verification-key projection: %+v", volume.ConfigMap.Items)
	}
}
