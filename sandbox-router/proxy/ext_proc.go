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

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"

	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
)

// ExtProcServer implements envoy.service.ext_proc.v3.ExternalProcessorServer
// for Envoy Gateway request stream interception, auto-resume signaling, and activity tracking.
type ExtProcServer struct {
	extproc.UnimplementedExternalProcessorServer

	cfg                  *config.Config
	suspensionManagerURL string
	httpClient           *http.Client
	singleflightGroup    singleflight.Group
	cache                Lookup
	log                  logr.Logger

	mu                 sync.Mutex
	activityTimestamps map[string]time.Time
}

// ExtProcOptions configures an ExtProcServer instance.
type ExtProcOptions struct {
	Config               *config.Config
	SuspensionManagerURL string
	HTTPClient           *http.Client
	Cache                Lookup
	Logger               logr.Logger
}

// NewExtProcServer constructs a new ExtProcServer.
func NewExtProcServer(o ExtProcOptions) *ExtProcServer {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &ExtProcServer{
		cfg:                  o.Config,
		suspensionManagerURL: o.SuspensionManagerURL,
		httpClient:           o.HTTPClient,
		cache:                o.Cache,
		log:                  o.Logger,
		activityTimestamps:   make(map[string]time.Time),
	}
}

// Process implements extproc.ExternalProcessorServer.
func (s *ExtProcServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ext_proc: error receiving stream request: %w", err)
		}

		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			resp := s.handleRequestHeaders(ctx, v.RequestHeaders)
			if err := stream.Send(resp); err != nil {
				return fmt.Errorf("ext_proc: error sending request headers response: %w", err)
			}
		default:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extproc.HeadersResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				return fmt.Errorf("ext_proc: error sending default pass-through response: %w", err)
			}
		}
	}
}

func (s *ExtProcServer) handleRequestHeaders(ctx context.Context, headers *extproc.HttpHeaders) *extproc.ProcessingResponse {
	headerMap := make(map[string]string)
	if headers != nil && headers.Headers != nil {
		for _, h := range headers.Headers.Headers {
			key := strings.ToLower(h.Key)
			val := h.Value
			if len(h.RawValue) > 0 && val == "" {
				val = string(h.RawValue)
			}
			headerMap[key] = val
		}
	}

	s.log.V(4).Info("ext_proc received headers detail", "headers", headerMap)

	sandboxID := headerMap["x-sandbox-id"]
	sandboxNamespace := headerMap["x-sandbox-namespace"]

	if sandboxID == "" || sandboxNamespace == "" {
		host := headerMap[":authority"]
		if host == "" {
			host = headerMap["host"]
		}
		if host != "" {
			hostOnly, _, err := net.SplitHostPort(host)
			if err != nil {
				hostOnly = host
			}
			hostOnly = strings.TrimSuffix(strings.ToLower(hostOnly), ".")
			if before, ok := strings.CutSuffix(hostOnly, ".sandbox.local"); ok {
				trimmed := before
				parts := strings.Split(trimmed, ".")
				if sandboxID == "" && len(parts) > 0 && parts[0] != "" {
					sandboxID = parts[0]
				}
				if sandboxNamespace == "" && len(parts) > 1 && parts[1] != "" {
					sandboxNamespace = parts[1]
				}
			}
		}
	}

	if sandboxNamespace == "" {
		sandboxNamespace = "default"
	}

	s.log.V(4).Info("ext_proc received headers", "sandboxID", sandboxID, "sandboxNamespace", sandboxNamespace, "headerCount", len(headerMap))

	if sandboxID != "" {
		if s.suspensionManagerURL != "" {
			port := DefaultSandboxPort
			if rawPort := headerMap["x-sandbox-port"]; rawPort != "" {
				if p, err := strconv.Atoi(rawPort); err == nil && p > 0 && p <= 65535 {
					port = p
				}
			}
			if err := s.ensureSandboxRunning(ctx, sandboxNamespace, sandboxID, port); err != nil {
				s.log.Error(err, "ext_proc: auto-resume notification or readiness check failed",
					"sandbox", sandboxID,
					"namespace", sandboxNamespace,
				)
				return &extproc.ProcessingResponse{
					Response: &extproc.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extproc.ImmediateResponse{
							Status: &typev3.HttpStatus{
								Code: typev3.StatusCode_GatewayTimeout,
							},
							Details: "sandbox_resume_failed",
							Body:    fmt.Appendf(nil, "failed to resume sandbox %s/%s: %v\n", sandboxNamespace, sandboxID, err),
						},
					},
				}
			}
		}

		s.RecordActivity(sandboxNamespace + "/" + sandboxID)
	}

	var podIP string
	if s.cache != nil && sandboxID != "" {
		if getter, ok := s.cache.(interface {
			GetByName(namespace, name string) (cache.Entry, bool)
		}); ok {
			if entry, ok := getter.GetByName(sandboxNamespace, sandboxID); ok && entry.PodIP != "" {
				podIP = entry.PodIP
			}
		}
	}

	headerMutations := []*corev3.HeaderValueOption{}
	if sandboxID != "" {
		headerMutations = append(headerMutations,
			&corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{
					Key:      "x-sandbox-id",
					Value:    sandboxID,
					RawValue: []byte(sandboxID),
				},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			},
			&corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{
					Key:      "x-sandbox-namespace",
					Value:    sandboxNamespace,
					RawValue: []byte(sandboxNamespace),
				},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			},
			&corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{
					Key:      "x-sandbox-gateway-processed",
					Value:    "true",
					RawValue: []byte("true"),
				},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			},
		)
		if podIP != "" {
			headerMutations = append(headerMutations, &corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{
					Key:      "x-sandbox-pod-ip",
					Value:    podIP,
					RawValue: []byte(podIP),
				},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			})
		}
	}

	return &extproc.ProcessingResponse{
		Response: &extproc.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{
					Status:          extproc.CommonResponse_CONTINUE,
					ClearRouteCache: true,
					HeaderMutation: &extproc.HeaderMutation{
						SetHeaders: headerMutations,
					},
				},
			},
		},
	}
}

func (s *ExtProcServer) ensureSandboxRunning(ctx context.Context, namespace, name string, port int) error {
	if s.cache != nil {
		if getter, ok := s.cache.(interface {
			GetByName(namespace, name string) (cache.Entry, bool)
		}); ok {
			if entry, ok := getter.GetByName(namespace, name); ok && entry.PodIP != "" {
				return nil
			}
		}
	}

	key := fmt.Sprintf("%s/%s", namespace, name)
	_, err, _ := s.singleflightGroup.Do(key, func() (any, error) {
		url := strings.TrimRight(s.suspensionManagerURL, "/") + "/v1/sandboxes/resume"
		payload := map[string]string{
			"name":      name,
			"namespace": namespace,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed calling resume API: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return nil, fmt.Errorf("resume API returned status code: %d", resp.StatusCode)
		}

		timeout := 60 * time.Second
		if s.cfg != nil && s.cfg.DefaultResumeTimeout > 0 {
			timeout = s.cfg.DefaultResumeTimeout
		}

		// Wait up to timeout for the Pod to become active in the informer cache
		// AND for its target port to be reachable!
		if s.cache != nil {
			if getter, ok := s.cache.(interface {
				GetByName(namespace, name string) (cache.Entry, bool)
			}); ok {
				deadline := time.Now().Add(timeout)
				ready := false
				for time.Now().Before(deadline) {
					if entry, ok := getter.GetByName(namespace, name); ok && entry.PodIP != "" {
						targetAddr := net.JoinHostPort(entry.PodIP, strconv.Itoa(port))
						conn, err := net.DialTimeout("tcp", targetAddr, 200*time.Millisecond)
						if err == nil {
							conn.Close()
							ready = true
							break
						}
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(100 * time.Millisecond):
					}
				}
				if !ready {
					return nil, fmt.Errorf("timeout (%s) waiting for resumed sandbox %s/%s to become reachable on port %d", timeout, namespace, name, port)
				}
			}
		}

		return nil, nil
	})
	return err
}

// RecordActivity stores an activity timestamp for the sandbox in memory.
func (s *ExtProcServer) RecordActivity(sandboxKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activityTimestamps[sandboxKey] = time.Now()
}

// StartActivityFlusher runs a periodic background task to flush activity timestamps to the suspension manager.
func (s *ExtProcServer) StartActivityFlusher(ctx context.Context, interval time.Duration) {
	if s.suspensionManagerURL == "" || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flushActivityTimestamps(ctx)
		}
	}
}

const maxActivityBatchSize = 500

func (s *ExtProcServer) flushActivityTimestamps(ctx context.Context) {
	s.mu.Lock()
	if len(s.activityTimestamps) == 0 {
		s.mu.Unlock()
		return
	}
	snapshot := s.activityTimestamps
	s.activityTimestamps = make(map[string]time.Time)
	s.mu.Unlock()

	url := strings.TrimRight(s.suspensionManagerURL, "/") + "/v1/sandboxes/activity"
	batch := make(map[string]string)
	for k, v := range snapshot {
		batch[k] = v.Format(time.RFC3339)
		if len(batch) >= maxActivityBatchSize {
			s.sendActivityBatch(ctx, url, batch, snapshot)
			batch = make(map[string]string)
		}
	}
	if len(batch) > 0 {
		s.sendActivityBatch(ctx, url, batch, snapshot)
	}
}

func (s *ExtProcServer) sendActivityBatch(ctx context.Context, url string, batch map[string]string, fullSnapshot map[string]time.Time) {
	data, err := json.Marshal(batch)
	if err != nil {
		s.log.Error(err, "ext_proc: failed to marshal activity flush payload")
		s.mergeActivityBatch(batch, fullSnapshot)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		s.log.Error(err, "ext_proc: failed creating activity flush request")
		s.mergeActivityBatch(batch, fullSnapshot)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.log.Error(err, "ext_proc: failed flushing activity timestamps")
		s.mergeActivityBatch(batch, fullSnapshot)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.log.Error(fmt.Errorf("status %d", resp.StatusCode), "ext_proc: activity flush returned non-200")
		s.mergeActivityBatch(batch, fullSnapshot)
	}
}

func (s *ExtProcServer) mergeActivityBatch(failedBatch map[string]string, fullSnapshot map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range failedBatch {
		ts := fullSnapshot[k]
		if existing, ok := s.activityTimestamps[k]; !ok || ts.After(existing) {
			s.activityTimestamps[k] = ts
		}
	}
}
