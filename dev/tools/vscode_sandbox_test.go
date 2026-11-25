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

package tools_test

import (
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
	"sigs.k8s.io/yaml"
)

func renderVSCodePodSpec(t *testing.T, variant string) corev1.PodSpec {
	t.Helper()
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	manifests, err := kustomizer.Run(filesys.MakeFsOnDisk(), filepath.Join("..", "..", "examples", "vscode-sandbox", variant))
	if err != nil {
		t.Fatalf("render VSCode %s: %v", variant, err)
	}
	sandbox, err := manifests.GetByCurrentId(resid.NewResId(resid.Gvk{
		Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox",
	}, "sandbox-example"))
	if err != nil {
		t.Fatalf("find rendered Sandbox: %v", err)
	}
	manifest, err := sandbox.AsYAML()
	if err != nil {
		t.Fatalf("serialize rendered Sandbox: %v", err)
	}
	var decoded struct {
		Spec struct {
			PodTemplate struct {
				Spec corev1.PodSpec
			}
		}
	}
	if err := yaml.Unmarshal(manifest, &decoded); err != nil {
		t.Fatalf("decode rendered Sandbox: %v", err)
	}
	return decoded.Spec.PodTemplate.Spec
}

func TestKataMSHVOverlayPreservesEnvbuilderConfiguration(t *testing.T) {
	base := renderVSCodePodSpec(t, "base")
	got := renderVSCodePodSpec(t, "overlays/kata-mshv")
	want := base.DeepCopy()
	runtimeClass := "kata-vm-isolation"
	want.RuntimeClassName = &runtimeClass
	for i := range want.Containers {
		if want.Containers[i].Name == "devcontainer-main" {
			want.Containers[i].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
		}
	}
	if !apiequality.Semantic.DeepEqual(got, *want) {
		t.Fatalf("AKS overlay must preserve the base Pod configuration except for runtime and guest memory:\ngot: %#v\nwant: %#v", got, *want)
	}
}
