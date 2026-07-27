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
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

const (
	// IdleTimeoutAnnotation is the annotation used to specify idle timeout duration (e.g., "30").
	IdleTimeoutAnnotation = "agents.x-k8s.io/idle-timeout-seconds"
)

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
		now := sandbox.CreationTimestamp
		sandbox.Status.LastActivityTime = &now
		if err := r.Status().Update(ctx, &sandbox); err != nil {
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
		patch := client.MergeFrom(sandbox.DeepCopy())
		sandbox.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended
		if err := r.Patch(ctx, &sandbox, patch); err != nil {
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
	if sandbox.Spec.Lifecycle.InactivityDuration != nil {
		return sandbox.Spec.Lifecycle.InactivityDuration.Duration
	}
	if ann, ok := sandbox.Annotations[IdleTimeoutAnnotation]; ok {
		if secs, err := strconv.Atoi(ann); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
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
	mux.HandleFunc("/v1/activity", s.handleActivity)
	return mux
}

type resumeRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (s *SuspensionServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed reading body", http.StatusBadRequest)
		return
	}

	var req resumeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
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
		// instantly re-suspend due to old elapsed inactivity.
		statusPatch := client.MergeFrom(sandbox.DeepCopy())
		now := metav1.Now()
		sandbox.Status.LastActivityTime = &now
		if err := s.Status().Patch(ctx, &sandbox, statusPatch); err != nil {
			s.log.Error(err, "failed updating lastActivityTime on resume", "sandbox", nsName)
			http.Error(w, "failed updating lastActivityTime: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// 2. Patch Spec.OperatingMode to Running.
		patch := client.MergeFrom(sandbox.DeepCopy())
		sandbox.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeRunning
		if err := s.Patch(ctx, &sandbox, patch); err != nil {
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed reading body", http.StatusBadRequest)
		return
	}

	var timestamps map[string]string
	if err := json.Unmarshal(body, &timestamps); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	for key, tsStr := range timestamps {
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			continue
		}
		ns, name := parts[0], parts[1]

		parsedTime, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}

		var sandbox agentsv1beta1.Sandbox
		nsName := types.NamespacedName{Name: name, Namespace: ns}
		if err := s.Get(ctx, nsName, &sandbox); err != nil {
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
			statusPatch := client.MergeFrom(sandbox.DeepCopy())
			sandbox.Status.LastActivityTime = &metav1.Time{Time: parsedTime}
			if err := s.Status().Patch(ctx, &sandbox, statusPatch); err != nil {
				s.log.Error(err, "failed updating lastActivityTime status via patch", "sandbox", nsName)
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}
