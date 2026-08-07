// Copyright 2025 The Kubernetes Authors.
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

package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const configMapName = "agent-sandbox-config"

// MapWatcher watches the agent-sandbox-config ConfigMap and
// cancels the manager context when its content changes. After the
// manager shuts down gracefully (releasing the leader lease), main()
// re-execs the process in-place (same PID) so kubelet sees no
// container restart and applies no backoff.
type MapWatcher struct {
	client.Client
	Namespace   string
	StartupHash string
	Shutdown    context.CancelFunc
}

func (w *MapWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if req.Name != configMapName || req.Namespace != w.Namespace {
		return ctrl.Result{}, nil
	}

	var cm corev1.ConfigMap
	if err := w.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: w.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			if w.StartupHash != hashData(nil) {
				logger.Info("agent-sandbox-config ConfigMap deleted, restarting to apply defaults")
				w.Shutdown()
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	currentHash := hashData(cm.Data)
	if currentHash != w.StartupHash {
		logger.Info("agent-sandbox-config ConfigMap changed, restarting to apply new configuration",
			"previousHash", w.StartupHash, "currentHash", currentHash)
		w.Shutdown()
	}

	return ctrl.Result{}, nil
}

func (w *MapWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == configMapName && obj.GetNamespace() == w.Namespace
		})).
		Complete(w)
}

// HashConfigMapData computes a deterministic hash of ConfigMap data for
// use as the startup snapshot.
func HashConfigMapData(data map[string]string) string {
	return hashData(data)
}

func hashData(data map[string]string) string {
	// Skip keys that cannot change effective runtime config via ApplyConfigMapData
	// (doc keys and NonTunableFlags) so editing them does not trigger a reload.
	// Unknown keys (e.g. allowed-label-domains) are still hashed — other readers
	// may consume them even when they are not flag overrides.
	keys := make([]string, 0, len(data))
	for k := range data {
		if IsIgnoredConfigKey(k) || NonTunableFlags[k] {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "empty"
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, data[k])
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}
