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
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	defaultPort            = "50051"
	defaultManagerEndpoint = "http://sandbox-suspension-manager.agent-sandbox-system.svc.cluster.local:8090/v1/sandboxes/resume"
	sandboxHeaderID        = "x-sandbox-id"
	sandboxHeaderNamespace = "x-sandbox-namespace"
)

// CalloutServer implements the Envoy ext_proc gRPC ExternalProcessor service.
type CalloutServer struct {
	extproc.UnimplementedExternalProcessorServer
	managerEndpoint string
	singleFlight    singleflight.Group
	httpClient      *http.Client
	podLister       corev1listers.PodLister
	informerSynced  cache.InformerSynced
	mu              sync.RWMutex
	warmCache       map[string]string // map[namespace/sandboxID] -> targetHost (IP:Port)
}

func NewCalloutServer(ctx context.Context, managerEndpoint string) *CalloutServer {
	var podLister corev1listers.PodLister
	var informerSynced cache.InformerSynced

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err == nil {
		if clientset, err := kubernetes.NewForConfig(config); err == nil {
			informerFactory := informers.NewSharedInformerFactory(clientset, 30*time.Minute)
			podInformer := informerFactory.Core().V1().Pods()
			podLister = podInformer.Lister()
			informerSynced = podInformer.Informer().HasSynced

			informerFactory.Start(ctx.Done())
			klog.Infof("Initializing Kubernetes Pod Informer for zero-latency lookups...")
			if cache.WaitForCacheSync(ctx.Done(), informerSynced) {
				klog.Infof("Successfully synced Kubernetes Pod Informer cache")
			} else {
				klog.Warningf("Timed out waiting for Pod Informer cache sync")
			}
		} else {
			klog.Warningf("Failed to initialize Kubernetes client for Pod lookup: %v", err)
		}
	} else {
		klog.Warningf("Failed to load Kubernetes client config: %v", err)
	}

	return &CalloutServer{
		managerEndpoint: managerEndpoint,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		podLister:      podLister,
		informerSynced: informerSynced,
		warmCache:      make(map[string]string),
	}
}

// Process handles bi-directional gRPC streaming with Envoy.
func (s *CalloutServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				return nil
			}
			klog.V(4).Infof("ext_proc gRPC stream closed: %v", err)
			return err
		}

		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			resp, err := s.handleRequestHeaders(ctx, v.RequestHeaders)
			if err != nil {
				klog.Errorf("Failed handling request headers: %v", err)
				return err
			}
			if err := stream.Send(resp); err != nil {
				klog.Errorf("Failed sending ext_proc response: %v", err)
				return err
			}
		default:
			// Passthrough for non-header stages (responses/bodies skipped by policy)
			if err := stream.Send(&extproc.ProcessingResponse{}); err != nil {
				return err
			}
		}
	}
}

func (s *CalloutServer) handleRequestHeaders(ctx context.Context, req *extproc.HttpHeaders) (*extproc.ProcessingResponse, error) {
	headers := make(map[string]string)
	for _, h := range req.Headers.Headers {
		key := strings.ToLower(h.Key)
		val := h.Value
		if val == "" && len(h.RawValue) > 0 {
			val = string(h.RawValue)
		}
		headers[key] = val
		klog.V(4).Infof("Header: %q = %q", key, val)
	}

	sandboxID := headers[sandboxHeaderID]
	namespace := headers[sandboxHeaderNamespace]
	reqPort, _ := strconv.Atoi(headers["x-sandbox-port"])

	// Extract sandbox ID and namespace from Host or :authority header if not explicitly provided
	if sandboxID == "" {
		host := headers["host"]
		if host == "" {
			host = headers[":authority"]
		}
		if host != "" {
			hostOnly, _, err := net.SplitHostPort(host)
			if err != nil {
				hostOnly = host
			}
			parts := strings.Split(hostOnly, ".")
			if len(parts) > 0 {
				sandboxID = parts[0]
				if namespace == "" && len(parts) > 1 {
					namespace = parts[1]
				}
			}
		}
	}

	if namespace == "" {
		namespace = "default"
	}

	if sandboxID == "" {
		return nil, fmt.Errorf("could not determine sandbox identity from request headers")
	}

	klog.Infof("Processing ext_proc callout for Sandbox: %s/%s", namespace, sandboxID)
	targetHost, err := s.ensureSandboxWarm(ctx, namespace, sandboxID, reqPort)
	if err != nil {
		return nil, fmt.Errorf("failed ensuring sandbox %s/%s warm: %w", namespace, sandboxID, err)
	}

	podIP, containerPortStr, err := net.SplitHostPort(targetHost)
	if err != nil {
		podIP = targetHost
		containerPortStr = strconv.Itoa(reqPort)
	}

	// Respond to Envoy allowing request pipeline to unpause and proceed directly to target Pod IP / Host (KEP-1174 Contract 2)
	return &extproc.ProcessingResponse{
		Response: &extproc.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{
					Status:          extproc.CommonResponse_CONTINUE,
					ClearRouteCache: true,
					HeaderMutation: &extproc.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "x-sandbox-gateway-processed",
									Value:    "true",
									RawValue: []byte("true"),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &corev3.HeaderValue{
									Key:      "x-sandbox-pod-ip",
									Value:    podIP,
									RawValue: []byte(podIP),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &corev3.HeaderValue{
									Key:      "x-sandbox-port",
									Value:    containerPortStr,
									RawValue: []byte(containerPortStr),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &corev3.HeaderValue{
									Key:      "x-sandbox-id",
									Value:    sandboxID,
									RawValue: []byte(sandboxID),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
							{
								Header: &corev3.HeaderValue{
									Key:      "x-sandbox-namespace",
									Value:    namespace,
									RawValue: []byte(namespace),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
						},
					},
				},
			},
		},
	}, nil
}

func (s *CalloutServer) getPodEndpointFromK8s(namespace, sandboxID string) (string, int) {
	if s.podLister == nil {
		return "", 0
	}
	// Direct in-memory lookup from Pod Informer Lister
	pod, err := s.podLister.Pods(namespace).Get(sandboxID)
	if err != nil {
		// Fallback to label search in memory if Pod name differs from Sandbox name
		selector := labels.SelectorFromSet(labels.Set{"agents.x-k8s.io/sandbox-name": sandboxID})
		pods, listErr := s.podLister.Pods(namespace).List(selector)
		if listErr == nil && len(pods) > 0 {
			pod = pods[0]
		}
	}
	if pod != nil && pod.Status.PodIP != "" {
		port := 8080
		if len(pod.Spec.Containers) > 0 && len(pod.Spec.Containers[0].Ports) > 0 {
			port = int(pod.Spec.Containers[0].Ports[0].ContainerPort)
		}
		klog.V(4).Infof("Resolved Pod endpoint %s:%d from Informer cache for Sandbox %s/%s", pod.Status.PodIP, port, namespace, sandboxID)
		return pod.Status.PodIP, port
	}
	return "", 0
}

func (s *CalloutServer) ensureSandboxWarm(ctx context.Context, namespace, sandboxID string, reqPort int) (string, error) {
	key := fmt.Sprintf("%s/%s", namespace, sandboxID)

	s.mu.RLock()
	cachedHost := s.warmCache[key]
	s.mu.RUnlock()

	if cachedHost != "" {
		// Quick TCP probe to verify socket readiness; if pod was suspended externally, invalidate cache and trigger thaw
		if conn, err := net.DialTimeout("tcp", cachedHost, 100*time.Millisecond); err == nil {
			conn.Close()
			klog.V(4).Infof("Sandbox %s is verified warm at %s", key, cachedHost)
			return cachedHost, nil
		}
		klog.Infof("Sandbox %s was warm in cache at %s but TCP probe failed (suspended/terminated). Invalidating cache...", key, cachedHost)
		s.mu.Lock()
		delete(s.warmCache, key)
		s.mu.Unlock()
	}

	// Use singleflight to collapse duplicate cold-start requests for the same sandbox into 1 resume signal
	res, err, _ := s.singleFlight.Do(key, func() (interface{}, error) {
		sfCtx, sfCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer sfCancel()

		klog.Infof("Triggering cold-start thaw signal for Sandbox %s via Manager at %s", key, s.managerEndpoint)

		// Send resume callout to Sandbox Suspension Manager
		payload := fmt.Sprintf(`{"namespace":"%s","sandboxName":"%s"}`, namespace, sandboxID)
		req, err := http.NewRequestWithContext(sfCtx, http.MethodPost, s.managerEndpoint, strings.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("creating http request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		// Attach Kubernetes Projected ServiceAccount Token for authenticated signaling (KEP-1174 security)
		tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
		} else {
			req.Header.Set("Authorization", "Bearer dev-sandbox-gateway-token")
		}

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("signaling callout to manager endpoint failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("manager endpoint returned error status %s for Sandbox %s: %s", resp.Status, key, string(body))
		}
		klog.Infof("Manager endpoint accepted thaw request: %s for Sandbox %s", resp.Status, key)

		klog.Infof("Awaiting upstream pod connectivity for %s from Informer cache...", key)

		var targetHost string
		var ready bool

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-sfCtx.Done():
				return nil, fmt.Errorf("timed out waiting for sandbox %s container readiness: %w", key, sfCtx.Err())
			case <-ticker.C:
				podIP, containerPort := s.getPodEndpointFromK8s(namespace, sandboxID)
				if podIP != "" {
					port := containerPort
					if reqPort > 0 {
						port = reqPort
					}
					candidateHost := fmt.Sprintf("%s:%d", podIP, port)

					// Verify direct TCP socket connectivity to pod IP
					conn, dialErr := net.DialTimeout("tcp", candidateHost, 100*time.Millisecond)
					if dialErr == nil {
						conn.Close()
						targetHost = candidateHost
						ready = true
						break
					}
				}
			}
			if ready {
				break
			}
		}

		if !ready || targetHost == "" {
			return nil, fmt.Errorf("upstream endpoint for Sandbox %s failed to become ready", key)
		}

		s.mu.Lock()
		s.warmCache[key] = targetHost
		s.mu.Unlock()

		klog.Infof("Sandbox %s is now WARM and READY at %s", key, targetHost)
		return targetHost, nil
	})

	if err != nil {
		return "", err
	}
	if resStr, ok := res.(string); ok && resStr != "" {
		return resStr, nil
	}
	return "", fmt.Errorf("unexpected empty targetHost for sandbox %s", key)
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	defer klog.Flush()

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	managerEndpoint := os.Getenv("MANAGER_ENDPOINT")
	if managerEndpoint == "" {
		managerEndpoint = defaultManagerEndpoint
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		klog.Fatalf("Failed to listen on port %s: %v", port, err)
	}

	grpcServer := grpc.NewServer()
	calloutServer := NewCalloutServer(ctx, managerEndpoint)
	extproc.RegisterExternalProcessorServer(grpcServer, calloutServer)
	reflection.Register(grpcServer)

	klog.Infof("Starting sandbox-gateway callout service on port %s (Manager: %s)", port, managerEndpoint)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			klog.Fatalf("gRPC server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	klog.Info("Shutting down sandbox-gateway callout service gracefully...")
	grpcServer.GracefulStop()
}

