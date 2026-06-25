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

// Binary sandbox-router is the Go reimplementation of the Python sandbox
// reverse proxy. It preserves the X-Sandbox-* header contract used by
// existing clients and adds TLS, mTLS, metrics, tracing, and graceful
// shutdown for enterprise deployments.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
	"sigs.k8s.io/agent-sandbox/sandbox-router/cache"
	"sigs.k8s.io/agent-sandbox/sandbox-router/config"
	"sigs.k8s.io/agent-sandbox/sandbox-router/observability"
	"sigs.k8s.io/agent-sandbox/sandbox-router/proxy"
	"sigs.k8s.io/agent-sandbox/sandbox-router/server"
	"sigs.k8s.io/agent-sandbox/sandbox-router/tlsutil"
)

func main() {
	cfg := config.Defaults()
	zapOpts := zap.Options{Development: false}

	config.RegisterFlags(flag.CommandLine, &cfg, os.LookupEnv)
	zapOpts.BindFlags(flag.CommandLine)

	// Apply config-file values BEFORE flag.Parse so CLI flags take precedence.
	// The file path is pulled from --config / SANDBOX_ROUTER_CONFIG without
	// touching flag.Parse, so the rest of the args are still available below.
	if path := config.FileFromArgsAndEnv(os.Args[1:], os.Getenv); path != "" {
		cfg.ConfigFile = path
		if err := config.LoadFromFile(path, flag.CommandLine); err != nil {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
			os.Exit(2)
		}
	}
	flag.Parse()

	if cfg.PrintVersion {
		fmt.Println(version.Print("sandbox-router"))
		return
	}

	// Auto-enable tracing / OTel metrics when the corresponding OTLP
	// endpoint env var is set and the user didn't explicitly pass the
	// flag. Must run after flag.Parse so flag.Visit can tell us which
	// flags were set on the command line.
	config.ApplyPostParseEnvDefaults(flag.CommandLine, &cfg,
		func(k string) (string, bool) { return os.LookupEnv(k) })

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("sandbox-router")

	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid configuration")
		os.Exit(2)
	}

	if err := run(&cfg, log); err != nil {
		log.Error(err, "exited with error")
		os.Exit(1)
	}
}

func run(cfg *config.Config, log logr.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Tracing -----------------------------------------------------------
	if cfg.EnableTracing {
		initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, cleanup, err := asmetrics.SetupOTel(initCtx, "sandbox-router")
		cancel()
		if err != nil {
			return fmt.Errorf("setup otel: %w", err)
		}
		defer cleanup()
	}

	// --- Metrics -----------------------------------------------------------
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	if cfg.EnableOTelMetrics {
		initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		shutdown, err := observability.SetupOTLPMetrics(initCtx, "sandbox-router", reg)
		cancel()
		if err != nil {
			return fmt.Errorf("setup otel metrics: %w", err)
		}
		defer func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			_ = shutdown(shutCtx)
		}()
	}

	// --- TLS / cert reload --------------------------------------------------
	var tlsConfig *tls.Config
	if cfg.HTTPSAddr != "" {
		reloader, err := tlsutil.NewCertReloader(cfg.TLSCertFile, cfg.TLSKeyFile, log.WithName("tls"),
			func(ok bool, _ error) {
				outcome := "success"
				if !ok {
					outcome = "failure"
				}
				metrics.CertReloadsTotal.WithLabelValues(outcome).Inc()
			})
		if err != nil {
			return fmt.Errorf("cert reloader: %w", err)
		}
		if err := reloader.Start(ctx); err != nil {
			return fmt.Errorf("cert watcher: %w", err)
		}
		tlsConfig, err = tlsutil.BuildServerTLS(cfg, reloader)
		if err != nil {
			return fmt.Errorf("build TLS config: %w", err)
		}
	}

	// --- Kubernetes client (shared by cache + tokenreview) ----------------
	// Build once if either feature needs it so we don't load kubeconfig
	// twice. Nil when neither feature is on; helpers below handle that.
	var k8sClient kubernetes.Interface
	if cfg.CacheEnabled || cfg.AuthzMode == config.AuthzTokenReview {
		c, err := buildKubernetesClient(cfg.Kubeconfig)
		if err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		k8sClient = c
	}

	// --- Pod-IP cache (optional, KEP-NNNN fast path) ----------------------
	var podCache *cache.Cache
	if cfg.CacheEnabled {
		var err error
		podCache, err = cache.New(cache.Options{
			Client:    k8sClient,
			Log:       log.WithName("cache"),
			Namespace: cfg.CacheNamespace,
		})
		if err != nil {
			return fmt.Errorf("build pod cache: %w", err)
		}
		podCache.Start(ctx)
		// Block readiness on the initial LIST. Use a generous timeout
		// here so a slow API server doesn't make us flap, but bound it
		// so a misconfigured RBAC fails fast at startup rather than
		// silently serving DNS-only.
		syncCtx, syncCancel := context.WithTimeout(ctx, 60*time.Second)
		ok := podCache.WaitForSync(syncCtx)
		syncCancel()
		if !ok {
			return fmt.Errorf("pod cache failed initial sync (check RBAC for pods get/list/watch)")
		}
		log.Info("pod cache synced", "entries", podCache.Len(), "namespace", cfg.CacheNamespace)
	}

	// --- Authorization -----------------------------------------------------
	var authorizer authz.Authorizer = authz.AllowAll{}
	if cfg.AuthzMode == config.AuthzTokenReview {
		tr, err := authz.NewTokenReviewAuthorizer(authz.TokenReviewOptions{
			Client:         k8sClient,
			Log:            log.WithName("authz"),
			TTL:            cfg.AuthzTokenReviewTTL,
			CacheSize:      cfg.AuthzTokenReviewCacheSize,
			RequireToken:   cfg.AuthzTokenReviewRequireToken,
			Audiences:      cfg.AuthzTokenReviewAudiences,
			RequestTimeout: 0,
		})
		if err != nil {
			return fmt.Errorf("build tokenreview authorizer: %w", err)
		}
		authorizer = tr
	}

	// --- Proxy handler -----------------------------------------------------
	proxyOpts := proxy.Options{
		Config:     cfg,
		Metrics:    metrics,
		Propagator: otel.GetTextMapPropagator(),
		Logger:     log.WithName("proxy"),
		Authorizer: authorizer,
	}
	if podCache != nil {
		proxyOpts.Cache = podCache
	}
	handler := proxy.NewHandler(proxyOpts)

	// Top-level mux: /healthz reuses the probes implementation so the
	// Python router's contract (200 OK with {"status":"ok"}) is preserved.
	// All other paths fall through to the proxy.
	probes := server.NewProbes()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", probes.Healthz)
	mux.Handle("/", handler)

	// Wrap with observability middleware. Layering (outer → inner):
	//   tracing  — opens span, attaches per-request logger w/ trace_id
	//   access   — logs one line per request, reading the trace-aware logger
	//   metrics  — records inflight, request totals, durations
	//   mux      — /healthz fast-path, then proxy handler
	// Access logging skips /healthz, /readyz, /metrics so high-frequency
	// probes don't drown signal.
	var rootHandler http.Handler = mux
	rootHandler = metrics.Middleware(rootHandler)
	if cfg.AccessLog {
		rootHandler = observability.AccessLogMiddleware(
			log.WithName("access"),
			observability.SkipHealthAndMetrics,
		)(rootHandler)
	}
	rootHandler = observability.TracingMiddleware(
		otel.Tracer("sandbox-router"),
		otel.GetTextMapPropagator(),
		log,
	)(rootHandler)
	if cfg.MaxRequestBodyBytes > 0 {
		rootHandler = limitBody(rootHandler, cfg.MaxRequestBodyBytes)
	}

	// --- Server lifecycle --------------------------------------------------
	srv, err := server.New(server.Options{
		Log:             log.WithName("server"),
		Probes:          probes,
		ProxyHandler:    rootHandler,
		MetricsHandler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		HTTPAddr:        cfg.HTTPAddr,
		HTTPSAddr:       cfg.HTTPSAddr,
		MetricsAddr:     cfg.MetricsAddr,
		ProbeAddr:       cfg.ProbeAddr,
		TLSConfig:       tlsConfig,
		ShutdownTimeout: cfg.ShutdownTimeout,
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	log.Info("starting sandbox-router",
		"version", version.Get().GitVersion,
		"sha", version.Get().GitSHA,
		"http", cfg.HTTPAddr,
		"https", cfg.HTTPSAddr,
		"metrics", cfg.MetricsAddr,
		"probes", cfg.ProbeAddr,
		"mtls", cfg.MTLSMode,
		"tracing", cfg.EnableTracing,
		"otelMetrics", cfg.EnableOTelMetrics,
		"cache", cfg.CacheEnabled,
		"authz", cfg.AuthzMode,
	)
	return srv.Run(ctx)
}

// buildKubernetesClient returns a typed client built from kubeconfigPath
// when non-empty, or the in-cluster config (ServiceAccount token + the
// kubernetes.default API server) when empty. Mirrors clientcmd's
// standard precedence so operators can run the router locally with
// KUBECONFIG and in-cluster without any flag.
//
// When kubeconfigPath is empty we try rest.InClusterConfig() FIRST.
// clientcmd's loading rules don't fall through to in-cluster
// authentication on their own — they look at $KUBECONFIG, then
// ~/.kube/config, and error out if neither exists. In a Pod with only
// the ServiceAccount mount that path would fail, so we'd never get
// in-cluster mode despite the documented behavior. Only when
// InClusterConfig fails (we're not running in a Pod) do we fall back
// to clientcmd so the local-dev path keeps working.
func buildKubernetesClient(kubeconfigPath string) (kubernetes.Interface, error) {
	restConfig, err := loadRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	return kubernetes.NewForConfig(restConfig)
}

// loadRESTConfig is split out so the precedence logic is testable
// without spinning up a real client.
func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		// Explicit path overrides everything: respect what the
		// operator asked for, even if we happen to be in a Pod.
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.ExplicitPath = kubeconfigPath
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Only fall through to local-dev config when InClusterConfig
	// reports "not in a Pod". Any other error (broken SA token
	// mount, malformed CA file, etc.) means we ARE in a Pod but the
	// in-cluster credentials are unusable — silently switching to
	// ~/.kube/config in that case would either authenticate against
	// the wrong cluster or surface a confusing "no kubeconfig found"
	// error instead of the real cause. Fail loud.
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("in-cluster config failed: %w", err)
	}
	// Local dev fallback: $KUBECONFIG, then ~/.kube/config.
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
}

// limitBody applies http.MaxBytesReader to the inbound request body.
func limitBody(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}
