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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func getInactivityDuration(sandbox *agentsv1beta1.Sandbox) time.Duration {
	if sandbox.Spec.AutoSuspend != nil && sandbox.Spec.AutoSuspend.InactivityDuration != nil {
		return sandbox.Spec.AutoSuspend.InactivityDuration.Duration
	}
	return 0
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
	mux.HandleFunc("/v1/sandboxes/activity", s.handleActivity)
	return mux
}

const (
	maxPayloadBytes    = 1 << 20 // 1 MB
	maxActivityEntries = 500     // Maximum number of timestamp entries allowed in one request
	maxActivityWorkers = 5       // Bounded concurrency for processing activity timestamp patches
)

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

	// Update Status.LastActivityTime on resume so the reconciler sees the fresh timestamp
	// and does not evaluate the sandbox as idle.
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

	s.log.Info("resumed sandbox via API request and reset lastActivityTime", "sandbox", nsName)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "resuming",
		"sandbox": req.Name,
		"message": "Sandbox lastActivityTime updated and resumed successfully",
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

	type activityItem struct {
		key   string
		tsStr string
	}
	capacity := min(len(timestamps), maxActivityEntries)
	items := make(chan activityItem, capacity)
	count := 0
	for k, v := range timestamps {
		if count >= maxActivityEntries {
			s.log.V(4).Info("capping activity timestamp processing at maxActivityEntries", "total", len(timestamps), "limit", maxActivityEntries)
			break
		}
		items <- activityItem{key: k, tsStr: v}
		count++
	}
	close(items)

	var (
		errs   []string
		errsMu sync.Mutex
		wg     sync.WaitGroup
	)

	workerCount := min(len(timestamps), maxActivityWorkers)

	ctx := r.Context()
	for range workerCount {
		wg.Go(func() {
			for item := range items {
				if ctx.Err() != nil {
					return
				}
				parts := strings.Split(item.key, "/")
				if len(parts) != 2 {
					s.log.V(4).Info("skipping activity update: invalid sandbox key format", "key", item.key)
					continue
				}
				ns, name := parts[0], parts[1]
				if !validDNSLabel(ns) || !validDNSLabel(name) {
					s.log.V(4).Info("skipping activity update: invalid sandbox namespace or name format", "key", item.key)
					continue
				}

				parsedTime, err := time.Parse(time.RFC3339, item.tsStr)
				if err != nil {
					s.log.V(4).Info("skipping activity update: invalid RFC3339 timestamp", "key", item.key, "error", err)
					continue
				}
				if now := time.Now(); parsedTime.After(now) {
					parsedTime = now
				}

				nsName := types.NamespacedName{Name: name, Namespace: ns}
				err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					var sandbox agentsv1beta1.Sandbox
					if getErr := s.Get(ctx, nsName, &sandbox); getErr != nil {
						if apierrors.IsNotFound(getErr) {
							s.log.V(4).Info("ignoring activity update for deleted sandbox", "sandbox", nsName)
							return nil
						}
						return getErr
					}

					duration := getInactivityDuration(&sandbox)
					threshold := 60 * time.Second
					if duration > 0 {
						threshold = duration / 10
						if threshold < 5*time.Second {
							threshold = 0
						}
					}

					if sandbox.Status.LastActivityTime != nil && parsedTime.Sub(sandbox.Status.LastActivityTime.Time) <= threshold {
						return nil
					}

					statusPatch := client.MergeFrom(sandbox.DeepCopy())
					sandbox.Status.LastActivityTime = &metav1.Time{Time: parsedTime}
					return s.Status().Patch(ctx, &sandbox, statusPatch)
				})

				if err != nil && ctx.Err() == nil {
					s.log.Error(err, "failed updating lastActivityTime status via patch", "sandbox", nsName)
					errsMu.Lock()
					errs = append(errs, fmt.Sprintf("failed updating status for %s: %v", nsName, err))
					errsMu.Unlock()
				}
			}
		})
	}

	wg.Wait()

	if ctx.Err() != nil {
		http.Error(w, "request timed out or canceled", http.StatusRequestTimeout)
		return
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
