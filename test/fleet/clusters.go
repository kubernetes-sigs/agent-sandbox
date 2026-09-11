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
	"context"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"net/http"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

var gvrSandboxes = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// cluster is one member of the fleet: a name (derived from the
// kubeconfig filename), a set of sharded dynamic clients for mutating
// requests, and a dedicated client for the sandbox watch.
type cluster struct {
	Index   int
	Name    string
	clients []dynamic.Interface // round-robin sharded over separate connections
	watch   dynamic.Interface
	rr      atomic.Uint32

	created atomic.Int64
	ready   atomic.Int64
}

func (c *cluster) client() dynamic.Interface {
	return c.clients[int(c.rr.Add(1))%len(c.clients)]
}

func connectClusters(ctx context.Context, cfg config) ([]*cluster, error) {
	var clusters []*cluster
	for i, kc := range cfg.Kubeconfigs {
		restConfig, err := clientcmd.BuildConfigFromFlags("", kc)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", kc, err)
		}
		restConfig.QPS = -1
		restConfig.Burst = -1

		name := strings.TrimSuffix(filepath.Base(kc), filepath.Ext(kc))
		c := &cluster{Index: i, Name: name}

		// Sharded mutating clients: the apiserver caps each HTTP/2
		// connection at ~100 concurrent streams; a create burst wider
		// than 100*conns queues client-side and pollutes the ack
		// latency. Distinct WrapTransport closures defeat client-go's
		// transport cache so each client really gets its own connection.
		for j := 0; j < cfg.ConnsPerCluster; j++ {
			shard := *restConfig
			j := j
			shard.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
				_ = j
				if ht, ok := rt.(*http.Transport); ok {
					ht.MaxIdleConns = 10000
					ht.MaxIdleConnsPerHost = 500
					ht.IdleConnTimeout = 90 * time.Second
				}
				return rt
			}
			dc, err := dynamic.NewForConfig(&shard)
			if err != nil {
				return nil, fmt.Errorf("client for %s: %w", name, err)
			}
			c.clients = append(c.clients, dc)
		}
		watchCfg := *restConfig
		watchCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return rt }
		wc, err := dynamic.NewForConfig(&watchCfg)
		if err != nil {
			return nil, err
		}
		c.watch = wc

		// Reachability check + namespace creation up front, so a dead
		// cluster fails the run before any creates happen anywhere.
		nsClient := c.clients[0].Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
		ns := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": cfg.Namespace},
		}}
		// Tolerate AlreadyExists: a create whose response was lost (WAN
		// timeout) succeeds server-side, and the retry must not abort the run.
		if _, err := nsClient.Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("cluster %s: creating namespace: %w", name, err)
		}
		log.Printf("cluster %d (%s): connected, namespace %s created", i, name, cfg.Namespace)
		clusters = append(clusters, c)
	}
	return clusters, nil
}
