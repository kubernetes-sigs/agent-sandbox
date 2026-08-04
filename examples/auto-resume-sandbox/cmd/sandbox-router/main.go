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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoytypev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
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

// CalloutServer implements the Envoy ext_proc gRPC ExternalProcessor service,
// acting as the protocol-compliant ExternalProcessor for Gateway API Inference Extension (GAIE).
type CalloutServer struct {
	extproc.UnimplementedExternalProcessorServer
	managerEndpoint string
	namespace       string
	singleFlight    singleflight.Group
	httpClient      *http.Client
	podLister       corev1listers.PodLister
	informerSynced  cache.InformerSynced
	mu              sync.RWMutex
	warmCache       map[string]string // map[namespace/sandboxID] -> targetHost (IP:Port)
}

func NewCalloutServer(ctx context.Context, managerEndpoint string) (*CalloutServer, error) {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = os.Getenv("NAMESPACE")
	}
	if namespace == "" {
		namespace = os.Getenv("TENANT_NAMESPACE")
	}
	if namespace == "" {
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = strings.TrimSpace(string(data))
		}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load Kubernetes client config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Kubernetes clientset: %w", err)
	}

	var informerFactory informers.SharedInformerFactory
	if namespace != "" {
		informerFactory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Minute,
			informers.WithNamespace(namespace),
		)
	} else {
		informerFactory = informers.NewSharedInformerFactory(clientset, 30*time.Minute)
	}

	podInformer := informerFactory.Core().V1().Pods()
	podLister := podInformer.Lister()
	informerSynced := podInformer.Informer().HasSynced

	informerFactory.Start(ctx.Done())
	logger := klog.FromContext(ctx)
	logger.Info("Initializing Kubernetes Pod Informer for zero-latency lookups", "namespace", namespace)

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informerSynced) {
		return nil, fmt.Errorf("timed out waiting for Pod Informer cache sync in namespace %q after 30s", namespace)
	}
	logger.Info("Successfully synced Kubernetes Pod Informer cache", "namespace", namespace)

	return &CalloutServer{
		managerEndpoint: managerEndpoint,
		namespace:       namespace,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
			},
		},
		podLister:      podLister,
		informerSynced: informerSynced,
		warmCache:      make(map[string]string),
	}, nil
}

// Process handles bi-directional gRPC streaming with Envoy.
func (s *CalloutServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	logger := klog.FromContext(ctx)

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
			logger.V(4).Info("ext_proc gRPC stream closed", "error", err)
			return err
		}

		klog.Infof("Received ext_proc request type: %T", req.Request)
		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			klog.Infof("Handling RequestHeaders in ext_proc")
			resp, err := s.handleRequestHeaders(ctx, v.RequestHeaders)
			if err != nil {
				logger.Error(err, "Failed handling request headers")
				return err
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc response")
				return err
			}
		case *extproc.ProcessingRequest_RequestBody:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_RequestBody{
					RequestBody: &extproc.BodyResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc body response")
				return err
			}
		case *extproc.ProcessingRequest_ResponseHeaders:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extproc.HeadersResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc response headers response")
				return err
			}
		case *extproc.ProcessingRequest_ResponseBody:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_ResponseBody{
					ResponseBody: &extproc.BodyResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc response body response")
				return err
			}
		case *extproc.ProcessingRequest_RequestTrailers:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_RequestTrailers{
					RequestTrailers: &extproc.TrailersResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc request trailers response")
				return err
			}
		case *extproc.ProcessingRequest_ResponseTrailers:
			resp := &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_ResponseTrailers{
					ResponseTrailers: &extproc.TrailersResponse{},
				},
			}
			if err := stream.Send(resp); err != nil {
				logger.Error(err, "Failed sending ext_proc response trailers response")
				return err
			}
		default:
			logger.Info("Received unhandled ext_proc request type", "type", fmt.Sprintf("%T", req.Request))
		}
	}
}

func (s *CalloutServer) handleRequestHeaders(ctx context.Context, req *extproc.HttpHeaders) (*extproc.ProcessingResponse, error) {
	logger := klog.FromContext(ctx)
	headers := make(map[string]string)
	var headerNames []string

	for _, h := range req.Headers.Headers {
		key := strings.ToLower(h.Key)
		headerNames = append(headerNames, key)
		val := h.Value
		if val == "" && len(h.RawValue) > 0 {
			val = string(h.RawValue)
		}
		headers[key] = val
	}

	var sandboxID string
	var namespace string
	var reqPort int

	host := headers["host"]
	if host == "" {
		host = headers[":authority"]
	}
	if host != "" {
		hostOnly, portStr, err := net.SplitHostPort(host)
		if err != nil {
			hostOnly = host
		} else if p, pErr := strconv.Atoi(portStr); pErr == nil && p > 0 {
			reqPort = p
		}
		parts := strings.Split(hostOnly, ".")
		if len(parts) > 0 {
			sandboxID = parts[0]
			if len(parts) > 1 {
				namespace = parts[1]
			}
		}
	}

	if namespace == "" {
		namespace = s.namespace
	}
	if namespace == "" {
		namespace = "default"
	}

	if sandboxID == "" {
		return nil, fmt.Errorf("could not determine sandbox identity from request host header")
	}

	if logger.V(4).Enabled() {
		slices.Sort(headerNames)
		logger.V(4).Info("Derived routing metadata from request host",
			"sandboxID", sandboxID,
			"namespace", namespace,
			"reqPort", reqPort,
			"headerNames", strings.Join(headerNames, ","),
		)
	}

	logger.Info("Processing ext_proc callout for Sandbox", "namespace", namespace, "sandboxID", sandboxID)
	targetHost, err := s.ensureSandboxWarm(ctx, namespace, sandboxID, reqPort)
	if err != nil {
		logger.Error(err, "Failed ensuring sandbox warm", "namespace", namespace, "sandboxID", sandboxID)
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extproc.ImmediateResponse{
					Status: &envoytypev3.HttpStatus{
						Code: envoytypev3.StatusCode_ServiceUnavailable, // 503
					},
					Body: []byte("No ready sandbox endpoints available"),
				},
			},
		}, nil
	}

	// Respond to Envoy allowing request pipeline to unpause and proceed directly to target Pod IP / Host (EPP Protocol specification)
	return &extproc.ProcessingResponse{
		Response: &extproc.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{
					Status:          extproc.CommonResponse_CONTINUE,
					ClearRouteCache: false,
					HeaderMutation: &extproc.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "x-gateway-destination-endpoint",
									Value:    targetHost,
									RawValue: []byte(targetHost),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
						},
					},
				},
			},
		},
		ModeOverride: &extprocfilterv3.ProcessingMode{
			RequestHeaderMode:  extprocfilterv3.ProcessingMode_SEND,
			RequestBodyMode:    extprocfilterv3.ProcessingMode_NONE,
			ResponseHeaderMode: extprocfilterv3.ProcessingMode_SKIP,
			ResponseBodyMode:   extprocfilterv3.ProcessingMode_NONE,
		},
		DynamicMetadata: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"envoy.lb": {
					Kind: &structpb.Value_StructValue{
						StructValue: func() *structpb.Struct {
							s, _ := structpb.NewStruct(map[string]any{
								"x-gateway-destination-endpoint": targetHost,
							})
							return s
						}(),
					},
				},
			},
		},
	}, nil
}

func (s *CalloutServer) getPodEndpointFromK8s(ctx context.Context, namespace, sandboxID string, requestedPort int) (string, int) {
	if s.podLister == nil {
		return "", requestedPort
	}
	logger := klog.FromContext(ctx)
	// Direct in-memory lookup from Pod Informer Lister (Pod name matches Sandbox name)
	pod, err := s.podLister.Pods(namespace).Get(sandboxID)
	if err != nil || pod == nil {
		return "", requestedPort
	}

	port := requestedPort
	if port <= 0 && len(pod.Spec.Containers) > 0 && len(pod.Spec.Containers[0].Ports) > 0 {
		port = int(pod.Spec.Containers[0].Ports[0].ContainerPort)
	}
	if port <= 0 {
		port = 8080
	}

	if pod.Status.PodIP == "" {
		return "", port
	}

	logger.V(4).Info("Resolved Pod endpoint from Informer cache", "podIP", pod.Status.PodIP, "port", port, "namespace", namespace, "sandboxID", sandboxID)
	return pod.Status.PodIP, port
}

func (s *CalloutServer) ensureSandboxWarm(ctx context.Context, namespace, sandboxID string, requestedPort int) (string, error) {
	logger := klog.FromContext(ctx)

	_, targetPort := s.getPodEndpointFromK8s(ctx, namespace, sandboxID, requestedPort)
	if targetPort <= 0 {
		targetPort = 8080
	}
	cacheKey := fmt.Sprintf("%s/%s:%d", namespace, sandboxID, targetPort)
	thawKey := cacheKey

	s.mu.RLock()
	cachedHost := s.warmCache[cacheKey]
	s.mu.RUnlock()

	if cachedHost != "" {
		// Quick TCP probe to verify socket readiness; if pod was suspended externally, invalidate cache and trigger thaw
		if conn, err := net.DialTimeout("tcp", cachedHost, 100*time.Millisecond); err == nil {
			conn.Close()
			logger.V(4).Info("Sandbox is verified warm", "cacheKey", cacheKey, "targetHost", cachedHost)
			return cachedHost, nil
		}
		logger.Info("Sandbox warm cache TCP probe failed (suspended/terminated), invalidating cache", "cacheKey", cacheKey, "targetHost", cachedHost)
		s.mu.Lock()
		delete(s.warmCache, cacheKey)
		s.mu.Unlock()
	}

	// Use singleflight to collapse duplicate cold-start requests for the same sandbox into 1 resume signal
	res, err, _ := s.singleFlight.Do(thawKey, func() (any, error) {
		sfCtx, sfCancel := context.WithTimeout(ctx, 90*time.Second)
		defer sfCancel()

		logger.Info("Triggering cold-start thaw signal for Sandbox via Manager", "sandboxKey", thawKey, "managerEndpoint", s.managerEndpoint)

		// Send resume callout to Sandbox Suspension Manager
		payload := fmt.Sprintf(`{"namespace":"%s","sandboxName":"%s"}`, namespace, sandboxID)
		req, err := http.NewRequestWithContext(sfCtx, http.MethodPost, s.managerEndpoint, strings.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("creating http request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		// Attach Kubernetes Projected ServiceAccount Token for authenticated signaling (KEP-1174 security)
		tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			return nil, fmt.Errorf("reading ServiceAccount token from /var/run/secrets/kubernetes.io/serviceaccount/token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("signaling callout to manager endpoint failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("manager endpoint returned error status %s for Sandbox %s: %s", resp.Status, thawKey, string(body))
		}
		logger.Info("Manager endpoint accepted thaw request", "status", resp.Status, "sandboxKey", thawKey)

		logger.Info("Awaiting upstream pod connectivity from Informer cache", "sandboxKey", thawKey)

		var targetHost string
		var ready bool
		var resolvedPort int

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-sfCtx.Done():
				return nil, fmt.Errorf("timed out waiting for sandbox %s container readiness: %w", thawKey, sfCtx.Err())
			case <-ticker.C:
				podIP, port := s.getPodEndpointFromK8s(sfCtx, namespace, sandboxID, requestedPort)
				if podIP != "" {
					candidateHost := net.JoinHostPort(podIP, fmt.Sprintf("%d", port))

					// Verify direct TCP socket connectivity to pod IP
					conn, dialErr := net.DialTimeout("tcp", candidateHost, 100*time.Millisecond)
					if dialErr == nil {
						conn.Close()
						targetHost = candidateHost
						resolvedPort = port
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
			return nil, fmt.Errorf("upstream endpoint for Sandbox %s failed to become ready", thawKey)
		}

		if resolvedPort <= 0 {
			resolvedPort = 8080
		}
		cacheKey := fmt.Sprintf("%s/%s:%d", namespace, sandboxID, resolvedPort)

		s.mu.Lock()
		s.warmCache[cacheKey] = targetHost
		s.mu.Unlock()

		logger.Info("Sandbox is now WARM and READY", "cacheKey", cacheKey, "targetHost", targetHost)
		return targetHost, nil
	})

	if err != nil {
		return "", err
	}
	if resStr, ok := res.(string); ok && resStr != "" {
		return resStr, nil
	}
	return "", fmt.Errorf("unexpected empty targetHost for sandbox %s", cacheKey)
}

func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Agent Sandbox State Informer"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	return tls.X509KeyPair(certPEM, keyPEM)
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
	logger := klog.FromContext(ctx)

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		klog.Fatalf("Failed to listen on port %s: %v", port, err)
	}

	tlsCert, err := generateSelfSignedCert()
	if err != nil {
		klog.Fatalf("Failed to generate self-signed TLS cert: %v", err)
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"h2"},
	})
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	calloutServer, err := NewCalloutServer(ctx, managerEndpoint)
	if err != nil {
		klog.Fatalf("Failed to initialize sandbox-router callout server: %v", err)
	}
	extproc.RegisterExternalProcessorServer(grpcServer, calloutServer)
	reflection.Register(grpcServer)

	logger.Info("Starting sandbox-router callout service", "port", port, "managerEndpoint", managerEndpoint)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			klog.Fatalf("gRPC server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	logger.Info("Shutting down sandbox-router callout service gracefully")
	grpcServer.GracefulStop()
}
