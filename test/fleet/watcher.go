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
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// watchSandboxes follows the cluster's sandbox stream and records the
// first Ready observation per sandbox: the client wall-clock time plus
// the Ready condition's lastTransitionTime (the controller's clock, 1s
// granularity) - the fleet verdict is computed from the server stamps
// so driver lag and cross-region skew cannot inflate the result.
//
// Reconnects resume from the last seen resourceVersion and are ALWAYS
// logged: a silently re-watching client hid a serious problem from the
// single-cluster campaign for days.
func (c *cluster) watchSandboxes(ctx context.Context, namespace string, rec *recorder) {
	iface := c.watch.Resource(gvrSandboxes).Namespace(namespace)
	reconnects := 0
	// resourceVersion to resume from; "" means "position unknown, must
	// resync". A watch started at "" resumes from the CURRENT state and
	// sees only FUTURE events - so on a 410 (the apiserver watch cache
	// evicting our position, which happens routinely at fleet churn) we
	// must LIST, not just re-watch, or we silently drop every Ready
	// transition that happened in the gap. A single fleet run lost ~370k
	// readies this way and reported a 40% low verdict.
	resourceVersion := ""
	for ctx.Err() == nil {
		if resourceVersion == "" {
			rv, err := c.relistReady(ctx, iface, rec)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("[%s] sandbox relist error, retrying: %v", c.Name, err)
					time.Sleep(time.Second)
				}
				continue
			}
			resourceVersion = rv
		}
		w, err := iface.Watch(ctx, metav1.ListOptions{Watch: true, ResourceVersion: resourceVersion, AllowWatchBookmarks: true})
		if err != nil {
			if apiStatus, ok := err.(apierrors.APIStatus); ok && apiStatus.Status().Code == 410 {
				resourceVersion = "" // force a resync LIST, not a from-now watch
			}
			if ctx.Err() == nil {
				log.Printf("[%s] sandbox watch error, retrying: %v", c.Name, err)
				time.Sleep(time.Second)
			}
			continue
		}
	inner:
		for {
			select {
			case <-ctx.Done():
				w.Stop()
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					reconnects++
					log.Printf("[%s] sandbox watch closed by server, re-watching from rv=%q (reconnect #%d)", c.Name, resourceVersion, reconnects)
					break inner
				}
				if ev.Type == watch.Error {
					// 410/Expired: our resume point is gone. Force a resync
					// LIST (resourceVersion="") so we re-derive the current
					// Ready set instead of watching from now and losing the
					// transitions that occurred during the gap.
					log.Printf("[%s] sandbox watch error event, resyncing via LIST: %v", c.Name, ev.Object)
					resourceVersion = ""
					w.Stop()
					break inner
				}
				u, ok := ev.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				resourceVersion = u.GetResourceVersion()
				if ev.Type == watch.Bookmark {
					continue
				}
				if ready, ltt := sandboxReady(u); ready {
					if rec.RecordReady(c, u.GetName(), time.Now(), ltt) {
						c.ready.Add(1)
					}
				}
			}
		}
	}
}

// relistReady does the reflector's ListAndWatch resync: LIST all
// sandboxes, record any already Ready (the recorder dedups by name, so
// re-recording across resyncs is harmless and the server-side
// lastTransitionTime keeps the verdict honest), and return the list's
// resourceVersion to start watching from. Only a fresh LIST guarantees we
// don't lose Ready transitions the watch cache dropped on a 410.
func (c *cluster) relistReady(ctx context.Context, iface dynamic.ResourceInterface, rec *recorder) (string, error) {
	list, err := iface.List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range list.Items {
		u := &list.Items[i]
		if ready, ltt := sandboxReady(u); ready {
			if rec.RecordReady(c, u.GetName(), time.Now(), ltt) {
				c.ready.Add(1)
			}
		}
	}
	return list.GetResourceVersion(), nil
}

// sandboxReady reports whether the Ready condition is True, returning
// its lastTransitionTime.
func sandboxReady(u *unstructured.Unstructured) (bool, time.Time) {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false, time.Time{}
	}
	for _, cv := range conditions {
		cond, ok := cv.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == "True" {
			var ltt time.Time
			if s, ok := cond["lastTransitionTime"].(string); ok {
				ltt, _ = time.Parse(time.RFC3339, s)
			}
			return true, ltt
		}
	}
	return false, time.Time{}
}
