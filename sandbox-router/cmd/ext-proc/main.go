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

// Binary ext-proc is the sandbox-router's ext_proc service. It runs a K8s
// informer over sandbox Pods, maintains a UID→PodIP cache, and serves
// Envoy's ext_proc v3 gRPC protocol. Envoy uses our header mutation to
// drive ORIGINAL_DST dispatch to the right sandbox Pod.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"sigs.k8s.io/agent-sandbox/sandbox-router/internal/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/internal/extproc"
)

// healthServiceName is the gRPC health-check service name Envoy uses to
// probe whether this replica is ready to serve. We keep it NOT_SERVING
// until the informer cache has synced, so Envoy's gRPC health check
// routes around freshly-started replicas that would otherwise miss cache
// lookups.
const healthServiceName = "envoy.service.ext_proc.v3.ExternalProcessor"

func main() {
	var (
		listenAddr    string
		namespace     string
		clusterDomain string
		syncTimeout   time.Duration
		shutdownGrace time.Duration
	)
	flag.StringVar(&listenAddr, "listen-address", ":50051",
		"Address for the ext_proc gRPC server.")
	flag.StringVar(&namespace, "namespace", "",
		"K8s namespace to watch for sandbox Pods. Empty means cluster-wide (the usual setting).")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local",
		"K8s cluster DNS suffix used for the DNS-form fallback when a UID is not cached.")
	flag.DurationVar(&syncTimeout, "informer-sync-timeout", 2*time.Minute,
		"Max time to wait for the initial Pod informer sync before failing readiness.")
	flag.DurationVar(&shutdownGrace, "shutdown-timeout", 30*time.Second,
		"Time budget for draining in-flight ext_proc streams on SIGTERM.")
	// Note: --kubeconfig is registered by controller-runtime's
	// client-go auth plugin import; ctrl.GetConfig() honors it
	// alongside KUBECONFIG and in-cluster config.

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("ext-proc")

	if err := run(log, runConfig{
		listenAddr:    listenAddr,
		namespace:     namespace,
		clusterDomain: clusterDomain,
		syncTimeout:   syncTimeout,
		shutdownGrace: shutdownGrace,
	}); err != nil {
		log.Error(err, "exited with error")
		os.Exit(1)
	}
}

type runConfig struct {
	listenAddr    string
	namespace     string
	clusterDomain string
	syncTimeout   time.Duration
	shutdownGrace time.Duration
}

func run(log logr.Logger, cfg runConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- K8s client -----------------------------------------------------
	// ctrl.GetConfig handles --kubeconfig flag, KUBECONFIG env, in-cluster
	// config, and ~/.kube/config in that order.
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("build kube REST config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	// --- Cache ----------------------------------------------------------
	cacheLog := log.WithName("cache")
	cch, err := cache.New(cache.Options{
		Client:    client,
		Log:       cacheLog,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	cch.Start(ctx)

	// --- gRPC server ----------------------------------------------------
	srv, err := extproc.NewServer(extproc.Options{
		Cache:         cch,
		ClusterDomain: cfg.clusterDomain,
		Log:           log.WithName("handler"),
	})
	if err != nil {
		return fmt.Errorf("extproc server: %w", err)
	}

	grpcSrv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcSrv, srv)

	// Health server — NOT_SERVING until informer.HasSynced(); Envoy's
	// gRPC health check sees this and routes around us until ready.
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus(healthServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)

	go func() {
		syncCtx, cancel := context.WithTimeout(ctx, cfg.syncTimeout)
		defer cancel()
		if cch.WaitForSync(syncCtx) {
			log.Info("informer synced; advertising READY")
			healthSrv.SetServingStatus(healthServiceName, healthpb.HealthCheckResponse_SERVING)
			healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		} else {
			log.Error(nil, "informer failed to sync within timeout; staying NOT_SERVING")
		}
	}()

	// --- Listen ---------------------------------------------------------
	lis, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.listenAddr, err)
	}
	log.Info("starting ext-proc",
		"address", lis.Addr().String(),
		"namespace", cfg.namespace,
		"clusterDomain", cfg.clusterDomain,
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcSrv.Serve(lis) }()

	// --- Wait for signal or serve error --------------------------------
	select {
	case <-ctx.Done():
		log.Info("shutdown initiated")
	case err := <-serveErr:
		return fmt.Errorf("grpc serve: %w", err)
	}

	// Flip health to NOT_SERVING immediately so Envoy stops routing new
	// streams here while the existing ones drain.
	healthSrv.SetServingStatus(healthServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	// Graceful shutdown: drain in-flight streams, then close listeners.
	// Bounded by shutdownGrace; if drain stalls, fall back to a hard stop.
	drained := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(cfg.shutdownGrace):
		log.Info("graceful shutdown timed out; forcing stop", "timeout", cfg.shutdownGrace)
		grpcSrv.Stop()
		<-drained
	}
	return nil
}
