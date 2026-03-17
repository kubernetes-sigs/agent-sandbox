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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

var gatewayGVR = schema.GroupVersionResource{
	Group:    gatewayAPIGroup,
	Version:  gatewayAPIVersion,
	Resource: gatewayPlural,
}

// waitForGatewayIP blocks until the named Gateway has an external address,
// then sets c.baseURL.
func (c *SandboxClient) waitForGatewayIP(ctx context.Context) (retErr error) {
	ctx, span := c.startSpan(ctx, "wait_for_gateway",
		AttrGatewayName.String(c.opts.GatewayName),
		AttrGatewayNamespace.String(c.opts.GatewayNamespace),
	)
	defer func() {
		if retErr != nil {
			recordError(span, retErr)
		}
		span.End()
	}()

	if c.dynamicClient == nil {
		return fmt.Errorf("sandbox: dynamic client required for gateway discovery")
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.GatewayReadyTimeout)
	defer cancel()

	listOpts := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", c.opts.GatewayName),
	}

	watchBackoff := 100 * time.Millisecond
	const maxWatchBackoff = 5 * time.Second

	for {
		list, listErr := c.dynamicClient.Resource(gatewayGVR).Namespace(c.opts.GatewayNamespace).List(ctx, listOpts)
		if listErr == nil {
			for i := range list.Items {
				if addr := extractGatewayAddress(&list.Items[i]); addr != "" {
					c.setGatewayURL(addr)
					return nil
				}
			}
			listOpts.ResourceVersion = list.GetResourceVersion()
		} else {
			c.log.V(1).Info("list gateways failed, falling through to watch", "error", listErr, "gateway", c.opts.GatewayName)
		}

		watcher, err := c.dynamicClient.Resource(gatewayGVR).Namespace(c.opts.GatewayNamespace).Watch(ctx, listOpts)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: gateway %s did not get an address within %s", ErrTimeout, c.opts.GatewayName, c.opts.GatewayReadyTimeout)
			}
			// Transient watch creation error; backoff and re-list.
			c.log.V(1).Info("watch creation failed, retrying", "error", err, "gateway", c.opts.GatewayName)
			listOpts.ResourceVersion = ""
			c.sleepWithContext(ctx, watchBackoff)
			watchBackoff *= 2
			if watchBackoff > maxWatchBackoff {
				watchBackoff = maxWatchBackoff
			}
			continue
		}

		done, watchErr := c.drainGatewayWatch(ctx, watcher)
		watcher.Stop()
		if done {
			return nil
		}
		if watchErr != nil {
			return watchErr
		}
		c.log.V(1).Info("gateway watch closed, re-establishing", "gateway", c.opts.GatewayName)
		listOpts.ResourceVersion = ""

		c.sleepWithContext(ctx, watchBackoff)
		watchBackoff *= 2
		if watchBackoff > maxWatchBackoff {
			watchBackoff = maxWatchBackoff
		}
	}
}

// drainGatewayWatch returns (true, nil) when an IP is found,
// (false, err) on fatal error, or (false, nil) if the watch channel closes.
func (c *SandboxClient) drainGatewayWatch(ctx context.Context, watcher watch.Interface) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("%w: gateway %s did not get an address within %s", ErrTimeout, c.opts.GatewayName, c.opts.GatewayReadyTimeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return false, nil // watch closed, caller should retry
			}
			if event.Type == watch.Error {
				// All watch errors (410 Gone, transient 5xx, etc.) are
				// recoverable: return nil so the outer loop re-lists with
				// backoff rather than aborting and deleting the claim.
				c.log.V(1).Info("transient gateway watch error, will re-list", "error", event.Object)
				return false, nil
			}
			if event.Type == watch.Deleted {
				return false, ErrGatewayDeleted
			}
			gw, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			addr := extractGatewayAddress(gw)
			if addr != "" {
				c.setGatewayURL(addr)
				return true, nil
			}
			c.log.V(1).Info("gateway object received but address not yet available", "gateway", gw.GetName())
		}
	}
}

func (c *SandboxClient) setGatewayURL(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scheme := c.opts.GatewayScheme
	// Bracket IPv6 addresses per RFC 3986.
	if net.ParseIP(addr) != nil && strings.Contains(addr, ":") {
		c.baseURL = fmt.Sprintf("%s://[%s]", scheme, addr)
	} else {
		c.baseURL = fmt.Sprintf("%s://%s", scheme, addr)
	}
}

func extractGatewayAddress(gw *unstructured.Unstructured) string {
	status, ok := gw.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	addresses, ok := status["addresses"].([]interface{})
	if !ok || len(addresses) == 0 {
		return ""
	}
	first, ok := addresses[0].(map[string]interface{})
	if !ok {
		return ""
	}
	val, ok := first["value"].(string)
	if !ok || val == "" {
		return ""
	}
	return val
}
