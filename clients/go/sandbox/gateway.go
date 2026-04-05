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

package sandbox

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

var gatewayGVR = schema.GroupVersionResource{
	Group:    gatewayAPIGroup,
	Version:  gatewayAPIVersion,
	Resource: gatewayPlural,
}

// gatewayStrategy discovers the sandbox-router URL by watching a Kubernetes
// Gateway resource for an external address.
type gatewayStrategy struct {
	dynamicClient    dynamic.Interface
	gatewayName      string
	gatewayNamespace string
	gatewayScheme    string
	timeout          time.Duration
	log              logr.Logger
	tracer           trace.Tracer
	svcName          string
}

func (g *gatewayStrategy) Connect(ctx context.Context) (string, error) {
	ctx, span := startSpan(ctx, g.tracer, g.svcName, "wait_for_gateway",
		AttrGatewayName.String(g.gatewayName),
		AttrGatewayNamespace.String(g.gatewayNamespace),
	)
	defer span.End()

	if g.dynamicClient == nil {
		err := fmt.Errorf("sandbox: dynamic client required for gateway discovery")
		recordError(span, err)
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", g.gatewayName),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second

	for {
		list, listErr := g.dynamicClient.Resource(gatewayGVR).Namespace(g.gatewayNamespace).List(ctx, listOpts)
		if listErr == nil {
			for i := range list.Items {
				if list.Items[i].GetName() != g.gatewayName {
					continue
				}
				if addr, rejected := extractGatewayAddress(&list.Items[i]); addr != "" {
					return g.formatURL(addr), nil
				} else if rejected != "" {
					g.log.Info("gateway address rejected by validation", "gateway", list.Items[i].GetName(), "address", rejected)
				}
			}
			listOpts.ResourceVersion = list.GetResourceVersion()
		} else {
			g.log.V(1).Info("list gateways failed, falling through to watch", "error", listErr, "gateway", g.gatewayName)
		}

		watcher, err := g.dynamicClient.Resource(gatewayGVR).Namespace(g.gatewayNamespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				retErr := fmt.Errorf("%w: gateway %s did not get an address within %s", ErrTimeout, g.gatewayName, g.timeout)
				recordError(span, retErr)
				return "", retErr
			}
			g.log.V(1).Info("watch creation failed, retrying", "error", err, "gateway", g.gatewayName)
			listOpts.ResourceVersion = ""
			sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		url, done, watchErr := g.drainWatch(ctx, watcher)
		watcher.Stop()
		if done {
			return url, nil
		}
		if watchErr != nil {
			recordError(span, watchErr)
			return "", watchErr
		}
		g.log.V(1).Info("gateway watch closed, re-establishing", "gateway", g.gatewayName)
		listOpts.ResourceVersion = ""

		sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

func (g *gatewayStrategy) Close() error { return nil }

func (g *gatewayStrategy) drainWatch(ctx context.Context, watcher watch.Interface) (string, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return "", false, fmt.Errorf("%w: gateway %s did not get an address within %s", ErrTimeout, g.gatewayName, g.timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", false, nil
			}
			if event.Type == watch.Error {
				g.log.V(1).Info("transient gateway watch error, will re-list", "error", event.Object)
				return "", false, nil
			}
			if event.Type == watch.Deleted {
				return "", false, ErrGatewayDeleted
			}
			gw, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if gw.GetName() != g.gatewayName {
				continue
			}
			addr, rejected := extractGatewayAddress(gw)
			if addr != "" {
				return g.formatURL(addr), true, nil
			}
			if rejected != "" {
				g.log.Info("gateway address rejected by validation", "gateway", gw.GetName(), "address", rejected)
			} else {
				g.log.V(1).Info("gateway object received but address not yet available", "gateway", gw.GetName())
			}
		}
	}
}

func (g *gatewayStrategy) formatURL(addr string) string {
	if net.ParseIP(addr) != nil && strings.Contains(addr, ":") {
		return fmt.Sprintf("%s://[%s]", g.gatewayScheme, addr)
	}
	return fmt.Sprintf("%s://%s", g.gatewayScheme, addr)
}

// extractGatewayAddress returns the validated address from a Gateway status.
// If the address is present but fails validation, rejected contains the raw
// value so the caller can log a diagnostic message.
func extractGatewayAddress(gw *unstructured.Unstructured) (addr, rejected string) {
	status, ok := gw.Object["status"].(map[string]interface{})
	if !ok {
		return "", ""
	}
	addresses, ok := status["addresses"].([]interface{})
	if !ok || len(addresses) == 0 {
		return "", ""
	}
	first, ok := addresses[0].(map[string]interface{})
	if !ok {
		return "", ""
	}
	val, ok := first["value"].(string)
	if !ok || val == "" {
		return "", ""
	}
	// Validate the address to prevent SSRF via a compromised Gateway resource.
	// The address must be a valid IP or a hostname; reject values containing
	// path separators, query strings, or fragments.
	if strings.ContainsAny(val, "/?#@") {
		return "", val
	}
	if net.ParseIP(val) == nil && !isValidGatewayHostname(val) {
		return "", val
	}
	return val, ""
}

// isValidGatewayHostname checks that s looks like a DNS hostname: only
// [a-zA-Z0-9.-] and no empty labels. This prevents injection of path,
// query, or userinfo components into the constructed URL.
func isValidGatewayHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || s[i-1] == '.' {
				return false
			}
		case c == '.':
			if i == 0 || s[i-1] == '.' || s[i-1] == '-' {
				return false
			}
		default:
			return false
		}
	}
	last := s[len(s)-1]
	return last != '-' && last != '.'
}
