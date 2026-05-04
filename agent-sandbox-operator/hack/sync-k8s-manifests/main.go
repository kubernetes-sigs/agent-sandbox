// sync-k8s-manifests copies pieces from the parent repo k8s/ tree into operator-sdk
// config paths by resource kind, so Makefile line numbers stay out of sync with k8s/.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func main() {
	controllerPath := flag.String("controller", "../k8s/controller.yaml", "multi-doc YAML (Namespace, RBAC bootstrap, Service, non-extensions Deployment, …)")
	extensionsPath := flag.String("extensions", "../k8s/extensions.controller.yaml", "extensions controller Deployment")
	supportOut := flag.String("support-out", "config/rbac/support.yaml", "write ServiceAccount + ClusterRoleBinding + Service from controller")
	managerOut := flag.String("manager-out", "config/manager/manager.yaml", "write Namespace from controller + Deployment from extensions")
	image := flag.String("image", "controller:latest", "container image for the extensions Deployment")
	flag.Parse()

	if err := run(*controllerPath, *extensionsPath, *supportOut, *managerOut, *image); err != nil {
		fmt.Fprintf(os.Stderr, "sync-k8s-manifests: %v\n", err)
		os.Exit(1)
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
		return fmt.Errorf("%s: no support documents (expected ServiceAccount, ClusterRoleBinding, Service, …)", controllerPath)
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
		// Match previous Makefile sed: replace the ko placeholder (trim inline comment if present).
		img = strings.TrimSpace(strings.SplitN(img, "#", 2)[0])
		if img == koControllerImage || strings.HasPrefix(img, koControllerImage) {
			cm["image"] = replacement
			return nil
		}
	}
	return fmt.Errorf("extensions deployment: no container image matching %q", koControllerImage)
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
