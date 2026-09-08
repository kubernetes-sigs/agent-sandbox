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

// sync-k8s-manifests copies pieces from the parent repo k8s/ tree into operator-sdk
// config paths by resource kind, so Makefile line numbers stay out of sync with k8s/.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func main() {
	controllerPath := flag.String(
		"controller",
		"../k8s/controller.yaml",
		"multi-doc YAML (Namespace, RBAC bootstrap, Service, non-extensions Deployment, …)",
	)
	extensionsPath := flag.String("extensions", "../k8s/extensions.controller.yaml", "extensions controller Deployment")
	supportOut := flag.String(
		"support-out",
		"config/rbac/support.yaml",
		"write ServiceAccount + ClusterRoleBinding + Service from controller",
	)
	managerOut := flag.String(
		"manager-out",
		"config/manager/manager.yaml",
		"write Namespace from controller + Deployment from extensions",
	)
	image := flag.String("image", "controller:latest", "container image for the extensions Deployment")
	routerDir := flag.String(
		"router-dir",
		"",
		"directory of copied sandbox-router manifests to rewrite namespace in-place (optional)",
	)
	routerNS := flag.String("router-namespace", "agent-sandbox-system", "target namespace for router manifests")
	crdDir := flag.String(
		"crd-dir",
		"",
		"directory of copied CRD bases; generates kustomization.yaml in the parent (optional)",
	)
	flag.Parse()

	if err := run(*controllerPath, *extensionsPath, *supportOut, *managerOut, *image); err != nil {
		fmt.Fprintf(os.Stderr, "sync-k8s-manifests: %v\n", err)
		os.Exit(1)
	}
	if *routerDir != "" {
		if err := rewriteRouterNamespace(*routerDir, *routerNS); err != nil {
			fmt.Fprintf(os.Stderr, "sync-k8s-manifests: %v\n", err)
			os.Exit(1)
		}
	}
	if *crdDir != "" {
		if err := writeCRDKustomization(*crdDir); err != nil {
			fmt.Fprintf(os.Stderr, "sync-k8s-manifests: %v\n", err)
			os.Exit(1)
		}
	}
}

func run(controllerPath, extensionsPath, supportOut, managerOut, image string) error {
	controllerDocs, err := readDocuments(controllerPath)
	if err != nil {
		return fmt.Errorf("read controller: %w", err)
	}
	var ns map[string]interface{}
	var support []map[string]interface{}
	for _, doc := range controllerDocs {
		kind, _ := doc["kind"].(string)
		switch kind {
		case "Namespace":
			if ns != nil {
				return fmt.Errorf("%s: multiple Namespace documents", controllerPath)
			}
			ns = doc
		case "Deployment":
			// Core controller Deployment lives in controller.yaml; operator uses extensions Deployment only.
			continue
		default:
			support = append(support, doc)
		}
	}
	if ns == nil {
		return fmt.Errorf("%s: no Namespace document", controllerPath)
	}
	if len(support) == 0 {
		return fmt.Errorf(
			"%s: no support documents (expected ServiceAccount, ClusterRoleBinding, Service, …)",
			controllerPath,
		)
	}

	extDocs, err := readDocuments(extensionsPath)
	if err != nil {
		return fmt.Errorf("read extensions: %w", err)
	}
	var dep map[string]interface{}
	for _, doc := range extDocs {
		if doc["kind"] == "Deployment" {
			if dep != nil {
				return fmt.Errorf("%s: multiple Deployment documents", extensionsPath)
			}
			dep = doc
		}
	}
	if dep == nil {
		return fmt.Errorf("%s: no Deployment document", extensionsPath)
	}
	if err := replaceControllerImage(dep, image); err != nil {
		return err
	}

	if err := writeMultiDoc(supportOut, support); err != nil {
		return fmt.Errorf("write support: %w", err)
	}
	if err := writeMultiDoc(managerOut, []map[string]interface{}{ns, dep}); err != nil {
		return fmt.Errorf("write manager: %w", err)
	}
	return nil
}

func readDocuments(path string) ([]map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []map[string]interface{}
	for {
		var doc map[string]interface{}
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(doc) == 0 {
			continue
		}
		out = append(out, doc)
	}
	return out, nil
}

const koControllerImage = "ko://sigs.k8s.io/agent-sandbox/cmd/agent-sandbox-controller"

func replaceControllerImage(dep map[string]interface{}, replacement string) error {
	spec, ok := dep["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("extensions deployment: missing spec")
	}
	tpl, ok := spec["template"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("extensions deployment: missing spec.template")
	}
	pod, ok := tpl["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("extensions deployment: missing spec.template.spec")
	}
	raw, ok := pod["containers"].([]interface{})
	if !ok || len(raw) == 0 {
		return fmt.Errorf("extensions deployment: missing or empty spec.template.spec.containers")
	}
	for _, c := range raw {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		img, ok := cm["image"].(string)
		if !ok {
			continue
		}
		if img == koControllerImage {
			cm["image"] = replacement
			return nil
		}
	}
	return fmt.Errorf("extensions deployment: no container image matching %q", koControllerImage)
}

func writeCRDKustomization(basesDir string) error {
	entries, err := os.ReadDir(basesDir)
	if err != nil {
		return fmt.Errorf("read CRD dir %s: %w", basesDir, err)
	}
	resources := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		resources = append(resources, "bases/"+e.Name())
	}
	out := filepath.Join(filepath.Dir(filepath.Clean(basesDir)), "kustomization.yaml")
	return writeResourcesKustomization(out, resources)
}

// writeResourcesKustomization writes a kustomization that only lists resources,
// matching config/crd/kustomization.yaml. Image remaps belong on a parent
// overlay that copy-k8s-config does not regenerate (config/default).
func writeResourcesKustomization(path string, resources []string) error {
	var buf bytes.Buffer
	buf.WriteString("resources:\n")
	for _, r := range resources {
		fmt.Fprintf(&buf, "- %s\n", r)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// nsDefaultRe matches "namespace: default" with any surrounding whitespace,
// preserving indentation. This avoids a full YAML parse-marshal round-trip
// that would drop comments and reorder keys.
var nsDefaultRe = regexp.MustCompile(`(?m)^(\s*namespace:\s+)default\s*$`)

func rewriteRouterNamespace(dir, namespace string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read router dir %s: %w", dir, err)
	}
	excludeFromResources := map[string]bool{
		"kustomization.yaml":    true,
		"networkpolicy.yaml":    true,
		"rbac-tokenreview.yaml": true,
	}
	resources := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Name(), err)
		}
		data = nsDefaultRe.ReplaceAll(data, []byte("${1}"+namespace))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", e.Name(), err)
		}
		if !excludeFromResources[e.Name()] {
			resources = append(resources, e.Name())
		}
	}
	return writeResourcesKustomization(filepath.Join(dir, "kustomization.yaml"), resources)
}

func writeMultiDoc(path string, docs []map[string]interface{}) error {
	var buf bytes.Buffer
	for i, doc := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		b, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
