
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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// SandboxAutoSuspensionReconciler reconciles Sandbox objects to enforce auto-suspension rules based on last activity.
type SandboxAutoSuspensionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get;update;patch

// Reconcile evaluates sandbox idleness against inactivityDuration / idle-timeout annotation and patches operatingMode to Suspended.
func (r *SandboxAutoSuspensionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sandbox agentsv1beta1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if sandbox.Spec.OperatingMode == agentsv1beta1.SandboxOperatingModeSuspended {
		return ctrl.Result{}, nil
	}

	inactivityDuration := getInactivityDuration(&sandbox)
	if inactivityDuration <= 0 {
		return ctrl.Result{}, nil
	}

	lastActivity := sandbox.Status.LastActivityTime
	if lastActivity == nil {
		now := metav1.Now()
		patch := client.MergeFrom(sandbox.DeepCopy())
		sandbox.Status.LastActivityTime = &now
		if err := r.Status().Patch(ctx, &sandbox, patch); err != nil {
			return ctrl.Result{}, err
		}
		lastActivity = &now
	}

	elapsed := time.Since(lastActivity.Time)
	if elapsed >= inactivityDuration {
		logger.Info("auto-suspending sandbox due to idleness",
			"sandbox", req.NamespacedName,
			"elapsed", elapsed,
			"inactivityDuration", inactivityDuration,
		)
		patch := client.MergeFromWithOptions(sandbox.DeepCopy(), client.MergeFromWithOptimisticLock{})
		sandbox.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended
		if err := r.Patch(ctx, &sandbox, patch); err != nil {
			if apierrors.IsConflict(err) {
				logger.V(4).Info("conflict patching operatingMode to Suspended; requeuing to re-evaluate lastActivityTime", "sandbox", req.NamespacedName)
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed patching sandbox to suspended: %w", err)
		}
		return ctrl.Result{}, nil
	}

	remaining := inactivityDuration - elapsed
	logger.V(4).Info("sandbox active; requeuing for idle evaluation",
		"sandbox", req.NamespacedName,
		"remaining", remaining,
	)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

func getInactivityDuration(sandbox *agentsv1beta1.Sandbox) time.Duration {
	if sandbox.Spec.Lifecycle.AutoSuspend != nil && sandbox.Spec.Lifecycle.AutoSuspend.InactivityDuration != nil {
		return sandbox.Spec.Lifecycle.AutoSuspend.InactivityDuration.Duration
	}
	return 0
}

func (r *SandboxAutoSuspensionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.Sandbox{}).
		Named("sandbox-auto-suspension").
		Complete(r)
}

// SuspensionServer provides HTTP endpoints for auto-resume signaling and activity timestamp updates.
type SuspensionServer struct {
	client.Client
	log logr.Logger
}

func NewSuspensionServer(c client.Client, logger logr.Logger) *SuspensionServer {
	return &SuspensionServer{
		Client: c,
		log:    logger,
	}
}

func (s *SuspensionServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandboxes/resume", s.handleResume)
	mux.HandleFunc("/v1/resume", s.handleResume)
	mux.HandleFunc("/v1/sandboxes/activity", s.handleActivity)
	return mux
}

const maxPayloadBytes = 1 << 20 // 1 MB

type resumeRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (s *SuspensionServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadBytes)
	var req resumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}
	if req.Name == "" || !validDNSLabel(req.Name) {
		http.Error(w, "invalid or missing sandbox name format", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if !validDNSLabel(req.Namespace) {
		http.Error(w, "invalid sandbox namespace format", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var sandbox agentsv1beta1.Sandbox
	nsName := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	if err := s.Get(ctx, nsName, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if sandbox.Spec.OperatingMode != agentsv1beta1.SandboxOperatingModeRunning {
		// 1. Update Status.LastActivityTime FIRST so that when Spec.OperatingMode
		// transitions to Running, the reconciler sees the fresh timestamp and does not
		// instantly re-suspend due to old elapsed inactivity. Retry on conflict so we
		// don't abort due to concurrent updates.
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var latest agentsv1beta1.Sandbox
			if err := s.Get(ctx, nsName, &latest); err != nil {
				return err
			}
			statusPatch := client.MergeFrom(latest.DeepCopy())
			now := metav1.Now()
			latest.Status.LastActivityTime = &now
			return s.Status().Patch(ctx, &latest, statusPatch)
		})
		if err != nil {
			s.log.Error(err, "failed updating lastActivityTime on resume", "sandbox", nsName)
			http.Error(w, "failed updating lastActivityTime: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// 2. Patch Spec.OperatingMode to Running, retrying on conflict so we never
		// leave the Sandbox in Suspended with a fresh LastActivityTime.
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var latest agentsv1beta1.Sandbox
			if err := s.Get(ctx, nsName, &latest); err != nil {
				return err
			}
			if latest.Spec.OperatingMode == agentsv1beta1.SandboxOperatingModeRunning {
				return nil
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeRunning
			return s.Patch(ctx, &latest, patch)
		})
		if err != nil {
			s.log.Error(err, "failed patching sandbox operatingMode to Running", "sandbox", nsName)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.log.Info("resumed sandbox via API request and reset lastActivityTime", "sandbox", nsName)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "resuming",
		"sandbox": req.Name,
		"message": "Sandbox operatingMode patched to Running successfully",
	})
}

func (s *SuspensionServer) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadBytes)
	var timestamps map[string]string
	if err := json.NewDecoder(r.Body).Decode(&timestamps); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	var errs []string
	ctx := r.Context()
	for key, tsStr := range timestamps {
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			errs = append(errs, fmt.Sprintf("invalid sandbox key format: %s", key))
			continue
		}
		ns, name := parts[0], parts[1]
		if !validDNSLabel(ns) || !validDNSLabel(name) {
			errs = append(errs, fmt.Sprintf("invalid sandbox namespace or name format: %s", key))
			continue
		}

		parsedTime, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			errs = append(errs, fmt.Sprintf("invalid RFC3339 timestamp for %s: %v", key, err))
			continue
		}

		var sandbox agentsv1beta1.Sandbox
		nsName := types.NamespacedName{Name: name, Namespace: ns}
		if err := s.Get(ctx, nsName, &sandbox); err != nil {
			errs = append(errs, fmt.Sprintf("failed to get sandbox %s: %v", nsName, err))
			continue
		}

		duration := getInactivityDuration(&sandbox)
		threshold := 60 * time.Second
		if duration > 0 {
			threshold = duration / 10
			if threshold < 5*time.Second {
				threshold = 0
			}
		}

		if sandbox.Status.LastActivityTime == nil || parsedTime.Sub(sandbox.Status.LastActivityTime.Time) > threshold {
			var patchErr error
			for i := 0; i < 3; i++ {
				statusPatch := client.MergeFrom(sandbox.DeepCopy())
				sandbox.Status.LastActivityTime = &metav1.Time{Time: parsedTime}
				if patchErr = s.Status().Patch(ctx, &sandbox, statusPatch); patchErr == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
				if err := s.Get(ctx, nsName, &sandbox); err != nil {
					break
				}
			}
			if patchErr != nil {
				s.log.Error(patchErr, "failed updating lastActivityTime status via patch", "sandbox", nsName)
				errs = append(errs, fmt.Sprintf("failed updating status for %s: %v", nsName, patchErr))
			}
		}
	}

	if len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
		return
	}

	w.WriteHeader(http.StatusOK)
}

// validDNSLabel reports whether s is a syntactically valid DNS-1123 label (RFC 1123).
func validDNSLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
