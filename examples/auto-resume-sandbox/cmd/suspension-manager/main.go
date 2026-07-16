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
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1beta1",
	Resource: "sandboxes",
}

type ThawRequest struct {
	Namespace   string `json:"namespace"`
	SandboxName string `json:"sandboxName"`
}

type ThawResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type Manager struct {
	dynClient dynamic.Interface
}

func (m *Manager) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		klog.Warning("Rejecting thaw request: missing or invalid Authorization header")
		http.Error(w, "Unauthorized: missing or invalid Bearer token", http.StatusUnauthorized)
		return
	}
	klog.V(2).Infof("Received authorized thaw request with Bearer token header")

	var req ThawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	if req.Namespace == "" || req.SandboxName == "" {
		http.Error(w, "namespace and sandboxName are required", http.StatusBadRequest)
		return
	}

	klog.Infof("Processing thaw signal for Sandbox CRD: %s/%s", req.Namespace, req.SandboxName)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Get current Sandbox resource from API Server
	sb, err := m.dynClient.Resource(sandboxGVR).Namespace(req.Namespace).Get(ctx, req.SandboxName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed fetching Sandbox %s/%s from API Server: %v", req.Namespace, req.SandboxName, err)
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(ThawResponse{Status: "error", Message: err.Error()})
		return
	}

	spec, ok := sb.Object["spec"].(map[string]interface{})
	if !ok {
		spec = make(map[string]interface{})
	}
	currentMode, _ := spec["operatingMode"].(string)

	if currentMode == "Running" {
		klog.Infof("Sandbox %s/%s is already Running (idempotent noop)", req.Namespace, req.SandboxName)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ThawResponse{Status: "already_running"})
		return
	}

	// Patch operatingMode to "Running" in Kubernetes API Server
	patchBytes := []byte(`{"spec":{"operatingMode":"Running"}}`)
	_, err = m.dynClient.Resource(sandboxGVR).Namespace(req.Namespace).Patch(
		ctx,
		req.SandboxName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		klog.Errorf("Failed patching operatingMode to Running for Sandbox %s/%s: %v", req.Namespace, req.SandboxName, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ThawResponse{Status: "error", Message: err.Error()})
		return
	}

	klog.Infof("Successfully patched Sandbox %s/%s operatingMode -> Running!", req.Namespace, req.SandboxName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ThawResponse{Status: "resumed", Message: "operatingMode set to Running"})
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	defer klog.Flush()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			klog.Fatalf("Failed to load k8s client config: %v", err)
		}
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create dynamic k8s client: %v", err)
	}

	mgr := &Manager{dynClient: dynClient}

	http.HandleFunc("/v1/sandboxes/resume", mgr.handleResume)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           nil,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	klog.Infof("Starting Sandbox Suspension Manager service on port %s...", port)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Manager HTTP server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	klog.Info("Shutting down Sandbox Suspension Manager gracefully...")
	server.Shutdown(context.Background())
}
