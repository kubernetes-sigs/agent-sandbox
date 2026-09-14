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

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/felixge/fgprof"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/events"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	extensionscontrollers "sigs.k8s.io/agent-sandbox/extensions/controllers"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	//+kubebuilder:scaffold:imports
)

var (
	setupLog = ctrl.Log.WithName("setup")
)

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var extensions bool
	var clusterDomain string
	var enableTracing bool
	var enablePprof bool
	var enablePprofDebug bool
	var pprofBlockProfileRate int
	var pprofMutexProfileFraction int
	var kubeAPIQPS float64
	var kubeAPIBurst int
	var apiConnections int
	var separateWatchConnection bool
	var sandboxConcurrentWorkers int
	var sandboxClaimConcurrentWorkers int
	var sandboxWarmPoolConcurrentWorkers int
	var sandboxTemplateConcurrentWorkers int
	var sandboxWarmPoolMaxBatchSize int
	var sandboxWarmPoolReplenishDelay time.Duration
	var sandboxWarmPoolMaxRefillRate float64
	var sandboxWriteBehindWindow time.Duration
	var sandboxWarmPoolReadinessGracePeriod time.Duration
	var sandboxWarmPoolUnschedulableRecheckInterval time.Duration
	var enableWarmPoolEviction bool
	var cacheLabelSelectors bool
	var printVersion bool
	var disableClaimEvents bool
	var disableClaimObservabilityAnnotations bool

	flag.BoolVar(&printVersion, "version", false, "Print version information and exit.")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "Kubernetes cluster domain for service FQDN generation")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "", "The namespace in which the leader election resource will be created.")
	flag.BoolVar(&extensions, "extensions", false, "Enable extensions controllers.")
	flag.BoolVar(&enableTracing, "enable-tracing", false, "Enable OpenTelemetry tracing via OTLP.")
	flag.BoolVar(&enablePprof, "enable-pprof", false,
		"Enable CPU profiling endpoint (/debug/pprof/profile) on the metrics server.")
	flag.BoolVar(&enablePprofDebug, "enable-pprof-debug", false,
		"Enable all pprof endpoints including sensitive ones (cmdline, symbol, heap, goroutine, etc). "+
			"Implies --enable-pprof. WARNING: May expose sensitive information and comes with performance overhead.")
	flag.IntVar(&pprofBlockProfileRate, "pprof-block-profile-rate", 1000000,
		"Block profile sampling rate for /debug/pprof/block when --enable-pprof-debug is set. "+
			"<=0 disables; 1 samples all blocking events; >=2 sets the rate in nanoseconds (e.g. 1000000 ~= 1ms).")
	flag.IntVar(&pprofMutexProfileFraction, "pprof-mutex-profile-fraction", 10,
		"Mutex contention sampling rate for /debug/pprof/mutex when --enable-pprof-debug is set. "+
			"<=0 disables; 1 samples all events; N>1 samples ~1/N events (e.g. 10 ~= 1/10, 100 ~= 1/100).")
	flag.Float64Var(&kubeAPIQPS, "kube-api-qps", -1.0, "Client-side QPS limit for the Kubernetes API client (default: -1, no client-side rate limiting)")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 10, "The maximum burst for client-side throttling of the Kubernetes API client.")
	flag.IntVar(&apiConnections, "api-connections", 1,
		"Number of independent HTTP/2 connections to the API server for non-watch traffic (writes, uncached reads, events, leader election). "+
			"The kube-apiserver caps concurrent in-flight requests per HTTP/2 connection (SETTINGS_MAX_CONCURRENT_STREAMS; 100 by default, "+
			"configurable server-side via --http2-max-streams-per-connection), "+
			"so a single connection bounds effective concurrency at the advertised limit regardless of worker count or QPS settings. "+
			"Values > 1 shard requests round-robin across that many dedicated connections, each dialed on first use (~N x per-connection limit ceiling). "+
			"Default 1 preserves the existing single-connection client.")
	flag.BoolVar(&separateWatchConnection, "separate-watch-connection", false,
		"Give the manager's informer cache (list/watch streams) a dedicated HTTP/2 connection to the API server. "+
			"Watch events arrive on existing long-lived streams, so this isolates their frames from TCP/connection-level queuing behind "+
			"bursts of request traffic on a shared connection (HTTP/2 stream prioritization does not help in practice). "+
			"The per-connection cap on concurrent request streams is addressed separately by --api-connections. "+
			"Default false preserves the existing shared-connection behavior.")
	flag.IntVar(&sandboxConcurrentWorkers, "sandbox-concurrent-workers", 100, "Max concurrent reconciles for the Sandbox controller")
	flag.IntVar(&sandboxClaimConcurrentWorkers, "sandbox-claim-concurrent-workers", 50, "Max concurrent reconciles for the SandboxClaim controller")
	flag.IntVar(&sandboxWarmPoolConcurrentWorkers, "sandbox-warm-pool-concurrent-workers", 1, "Max concurrent reconciles for the SandboxWarmPool controller")
	flag.IntVar(&sandboxTemplateConcurrentWorkers, "sandbox-template-concurrent-workers", 1, "Max concurrent reconciles for the SandboxTemplate controller")
	flag.IntVar(&sandboxWarmPoolMaxBatchSize, "sandbox-warm-pool-max-batch-size", 300, "Max batch size for parallel sandbox creation and deletion in SandboxWarmPool controller. Default is 300. Creates advance one observed batch per watch round-trip (the expectations gate waits for a batch's add events before issuing the next), so a large pool fills in about ceil(replicas/batchSize) round-trips; raising this trades round-trips for burst size and is safe at any value under the gate.")
	flag.DurationVar(&sandboxWarmPoolReplenishDelay, "sandbox-warm-pool-replenish-delay", 0,
		"How long the SandboxWarmPool controller defers creating replacement sandboxes after pool members drop out of the pool "+
			"(e.g. a burst of SandboxClaims adopting warm sandboxes), so the burst gets API server priority. "+
			"The hold re-arms while members keep dropping. 0 (default) replenishes immediately.")
	flag.Float64Var(&sandboxWarmPoolMaxRefillRate, "sandbox-warm-pool-max-refill-rate", 0,
		"Max rate (sandboxes/second, per pool) at which the SandboxWarmPool controller creates replacement sandboxes, "+
			"pacing refill into a smooth stream instead of full-deficit bursts that flood the write path and compete with claim adoption. "+
			"Composes with --sandbox-warm-pool-replenish-delay: the delay defers the start of refill, the rate shapes its flow. "+
			"0 (default) leaves refill unpaced (whole deficit per reconcile).")
	flag.DurationVar(&sandboxWarmPoolReadinessGracePeriod, "sandbox-warm-pool-readiness-grace-period", extensionscontrollers.DefaultWarmPoolReadinessGracePeriod, "How long a warm pool sandbox may stay non-Ready before the SandboxWarmPool controller considers it stuck and replaces it (or holds it, if its pod is unschedulable). Raise this for images with long initialization or clusters with slow node auto-provisioning. Must be a positive duration.")
	flag.DurationVar(&sandboxWarmPoolUnschedulableRecheckInterval, "sandbox-warm-pool-unschedulable-recheck-interval", extensionscontrollers.DefaultUnschedulableRecheckInterval, "Requeue interval at which the SandboxWarmPool controller re-checks a pool holding unschedulable sandboxes past the readiness grace period. Must be a positive duration.")
	flag.BoolVar(&enableWarmPoolEviction, "enable-warm-pool-eviction", true, "Mark pods created by a warm pool as ready-to-evict by default.")
	flag.BoolVar(&cacheLabelSelectors, "cache-label-selectors", false,
		"Scope the manager's Pod and Service informer caches to objects carrying the sandbox tracking label ("+
			controllers.SandboxNameHashLabel+"). The controller only ever creates/looks up Pods and Services it "+
			"labeled itself, so on shared or high-churn clusters this cuts informer list/watch volume, JSON decode "+
			"CPU, and cache memory from O(cluster) to O(sandboxes). CAVEAT: externally pre-provisioned resources "+
			"that rely on the "+sandboxv1beta1.SandboxAdoptableLabel+"=true adoption path MUST also carry the "+
			"tracking label (value = the owning sandbox's name hash) to remain visible to the controller when "+
			"this flag is enabled.")
	flag.BoolVar(&disableClaimEvents, "disable-claim-events", false,
		"Disable Kubernetes Event emission from the SandboxClaim controller (its Eventf calls become no-ops), "+
			"reducing API server writes during large claim bursts. Default false (events enabled).")
	flag.BoolVar(&disableClaimObservabilityAnnotations, "disable-claim-observability-annotations", false,
		"Skip persisting the SandboxClaim observability annotations (controller first-observed timestamp, trace context), "+
			"removing one API write per claim. The values are still stamped on the in-memory object, so startup-latency "+
			"metrics and trace propagation to the Sandbox keep working within the controller process. Costs the on-object "+
			"debugging breadcrumbs and, after a controller restart, the startup-latency metric for claims first observed "+
			"by the previous process. Default false (annotations persisted).")
	flag.DurationVar(&sandboxWriteBehindWindow, "sandbox-write-behind-window", 0,
		"Coalescing window for the Sandbox controller's recoverable metadata-only writes. 0 disables coalescing.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if printVersion {
		fmt.Println(version.Print("agent-sandbox-controller"))
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if sandboxWarmPoolReadinessGracePeriod <= 0 {
		setupLog.Error(nil, "--sandbox-warm-pool-readiness-grace-period must be a positive duration", "value", sandboxWarmPoolReadinessGracePeriod)
		os.Exit(1)
	}
	if sandboxWarmPoolUnschedulableRecheckInterval <= 0 {
		setupLog.Error(nil, "--sandbox-warm-pool-unschedulable-recheck-interval must be a positive duration", "value", sandboxWarmPoolUnschedulableRecheckInterval)
		os.Exit(1)
	}

	setupLog.Info("Concurrency settings",
		"sandbox", sandboxConcurrentWorkers,
		"sandboxClaim", sandboxClaimConcurrentWorkers,
		"sandboxWarmPool", sandboxWarmPoolConcurrentWorkers,
		"sandboxTemplate", sandboxTemplateConcurrentWorkers,
		"sandboxWarmPoolMaxBatchSize", sandboxWarmPoolMaxBatchSize,
	)

	// Validation checks for concurrency flags
	if sandboxConcurrentWorkers <= 0 || sandboxClaimConcurrentWorkers <= 0 || sandboxWarmPoolConcurrentWorkers <= 0 {
		setupLog.Error(nil, "concurrent workers must be greater than 0")
		os.Exit(1)
	}
	// Validation checks for sandboxWarmPoolMaxBatchSize (maximum batch size for sandbox creation and deletion in SandboxWarmPool controller)
	if sandboxWarmPoolMaxBatchSize <= 0 {
		setupLog.Error(nil, "sandbox-warm-pool-max-batch-size must be greater than 0")
		os.Exit(1)
	}
	// Fail fast on nonsensical refill rates: flag parsing accepts "NaN" and
	// "+Inf", and a negative rate would silently disable pacing (the
	// controller treats <= 0 as unpaced), which is confusing to debug.
	if math.IsNaN(sandboxWarmPoolMaxRefillRate) || math.IsInf(sandboxWarmPoolMaxRefillRate, 0) || sandboxWarmPoolMaxRefillRate < 0 {
		setupLog.Error(nil, "sandbox-warm-pool-max-refill-rate must be a finite value >= 0 (0 disables pacing)",
			"value", sandboxWarmPoolMaxRefillRate)
		os.Exit(1)
	}
	// 0 means "write-behind disabled"; a negative window is always a
	// misconfiguration, so fail fast instead of silently disabling.
	if sandboxWriteBehindWindow < 0 {
		setupLog.Error(nil, "sandbox-write-behind-window must be >= 0 (0 disables write-behind coalescing)",
			"value", sandboxWriteBehindWindow)
		os.Exit(1)
	}
	// A logical maximum (too much will create unnecessary load on the API server)
	totalWorkers := sandboxConcurrentWorkers + sandboxClaimConcurrentWorkers + sandboxWarmPoolConcurrentWorkers + sandboxTemplateConcurrentWorkers
	if totalWorkers > 1000 {
		setupLog.Info("Warning: total concurrent workers exceeds 1000, which could lead to resource exhaustion", "total", totalWorkers)
	}

	if kubeAPIBurst <= 0 {
		setupLog.Error(nil, "kube-api-burst must be greater than 0")
		os.Exit(1)
	}
	if apiConnections < 1 {
		setupLog.Error(nil, "api-connections must be greater than or equal to 1")
		os.Exit(1)
	}
	// Warning if the total number of workers exceeds the kube API burst limit
	if kubeAPIQPS > 0 && totalWorkers > kubeAPIBurst {
		setupLog.Info("Warning: Total concurrent workers exceeds the kube API burst limit. Workers may experience client-side throttling.",
			"totalWorkers", totalWorkers,
			"kubeAPIBurst", kubeAPIBurst,
		)
	}

	if enableLeaderElection && leaderElectionNamespace == "" {
		setupLog.V(1).Info("leader election is enabled (--leader-elect=true), but --leader-election-namespace is empty; attempting auto-detection")
	}

	ctx := ctrl.SetupSignalHandler()

	// Initialize Tracing Provider
	var instrumenter = asmetrics.NewNoOp()
	if enableTracing {
		var cleanup func()
		var err error
		// Use a timeout context for initialization to prevent blocking
		initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		instrumenter, cleanup, err = asmetrics.SetupOTel(initCtx, "agent-sandbox-controller")
		if err != nil {
			setupLog.Error(err, "unable to initialize tracing")
			os.Exit(1)
		}
		defer cleanup()
	}

	// Register client-go REST client metrics.
	// These metrics provide visibility into API server interactions.
	setupLog.Info("client-go REST client metrics enabled")
	metrics.RegisterRESTClientMetrics(
		metrics.MetricRequestLatency,
		metrics.MetricDNSResolutionLatency,
		metrics.MetricRequestSize,
		metrics.MetricResponseSize,
		metrics.MetricRateLimiterLatency,
		metrics.MetricRequestRetry,
	)

	// Importing net/http/pprof registers handlers on the global DefaultServeMux.
	// Reset it to avoid accidentally exposing pprof via any server that uses the default mux.
	http.DefaultServeMux = http.NewServeMux()

	scheme := controllers.Scheme
	if extensions {
		utilruntime.Must(extensionsv1beta1.AddToScheme(scheme))
	}

	metricsOpts := metricsserver.Options{
		BindAddress: metricsAddr,
	}
	if enablePprof || enablePprofDebug {
		setupLog.Info("pprof enabled", "debug", enablePprofDebug)
		metricsOpts.ExtraHandlers = map[string]http.Handler{
			"/debug/pprof/profile": http.HandlerFunc(pprof.Profile),
		}
		if enablePprofDebug {
			setupLog.Info("pprof debug endpoints enabled")
			if pprofBlockProfileRate < 0 {
				setupLog.Info("invalid pprof block profile rate; clamping to 0", "rate", pprofBlockProfileRate)
				pprofBlockProfileRate = 0
			}
			if pprofMutexProfileFraction < 0 {
				setupLog.Info("invalid pprof mutex profile fraction; clamping to 0", "fraction", pprofMutexProfileFraction)
				pprofMutexProfileFraction = 0
			}
			runtime.SetBlockProfileRate(pprofBlockProfileRate)
			runtime.SetMutexProfileFraction(pprofMutexProfileFraction)
			setupLog.Info("pprof sampling configured",
				"blockProfileRateNs", pprofBlockProfileRate,
				"mutexProfileFraction", pprofMutexProfileFraction,
			)
			metricsOpts.ExtraHandlers["/debug/pprof/"] = http.HandlerFunc(pprof.Index)
			metricsOpts.ExtraHandlers["/debug/pprof/cmdline"] = http.HandlerFunc(pprof.Cmdline)
			metricsOpts.ExtraHandlers["/debug/pprof/symbol"] = http.HandlerFunc(pprof.Symbol)
			metricsOpts.ExtraHandlers["/debug/pprof/heap"] = pprof.Handler("heap")
			metricsOpts.ExtraHandlers["/debug/pprof/goroutine"] = pprof.Handler("goroutine")
			metricsOpts.ExtraHandlers["/debug/pprof/allocs"] = pprof.Handler("allocs")
			metricsOpts.ExtraHandlers["/debug/pprof/block"] = pprof.Handler("block")
			metricsOpts.ExtraHandlers["/debug/pprof/mutex"] = pprof.Handler("mutex")
			metricsOpts.ExtraHandlers["/debug/pprof/trace"] = http.HandlerFunc(pprof.Trace)
			// Wrap fgprof handler with mutex to reject concurrent profiling requests,
			// aligning with pprof CPU profiling behavior.
			var fgprofMu sync.Mutex
			fgprofHandler := fgprof.Handler()
			metricsOpts.ExtraHandlers["/debug/fgprof"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !fgprofMu.TryLock() {
					http.Error(w, "Could not enable fgprof profiling: fgprof profiling already in use", http.StatusInternalServerError)
					return
				}
				defer fgprofMu.Unlock()
				fgprofHandler.ServeHTTP(w, r)
			})
		}
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = float32(kubeAPIQPS)
	restConfig.Burst = kubeAPIBurst

	// Optional API transport tuning (see transport.go). Order matters: the
	// dedicated watch client must be built before configureAPIConnections
	// installs its sharding WrapTransport on restConfig.
	var watchHTTPClient *http.Client
	if separateWatchConnection {
		var err error
		watchHTTPClient, err = newIsolatedHTTPClient(restConfig)
		if err != nil {
			setupLog.Error(err, "unable to build dedicated watch connection client")
			os.Exit(1)
		}
		setupLog.Info("informer cache list/watch traffic separated onto a dedicated HTTP/2 connection (--separate-watch-connection)")
	}
	if err := configureAPIConnections(restConfig, apiConnections); err != nil {
		setupLog.Error(err, "unable to configure API connections")
		os.Exit(1)
	}
	if apiConnections > 1 {
		setupLog.Info("API transport sharding enabled: non-watch API traffic distributed round-robin across independent HTTP/2 connections",
			"connections", apiConnections)
	}

	mgrOpts := buildManagerOptions(scheme, metricsOpts, probeAddr, enableLeaderElection, leaderElectionNamespace)
	// managedFields stripping, the Pod spec diet, and (optionally) the
	// tracking-label scoping; see buildCacheOptions for the rationale.
	cacheOpts, err := buildCacheOptions(cacheLabelSelectors)
	if err != nil {
		setupLog.Error(err, "unable to build cache options")
		os.Exit(1)
	}
	mgrOpts.Cache = cacheOpts
	if cacheLabelSelectors {
		setupLog.Info("informer caches for Pods and Services scoped to the sandbox tracking label (--cache-label-selectors)",
			"label", controllers.SandboxNameHashLabel)
	}
	if watchHTTPClient != nil {
		// The manager cache builds its list/watch REST clients from this
		// http.Client (RESTClientForConfigAndClient), bypassing restConfig's
		// WrapTransport, so watch streams stay off the write connections.
		mgrOpts.Cache.HTTPClient = watchHTTPClient
	}

	mgr, err := ctrl.NewManager(restConfig, mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register the custom Sandbox metric collector globally.
	asmetrics.RegisterSandboxCollector(mgr.GetClient(), mgr.GetLogger().WithName("sandbox-collector"))

	// RequeueAfter-based write deferral for the Sandbox controller's
	// recoverable metadata-only writes. Default (0) is fully synchronous:
	// the controller keeps its stock write path. No background goroutine is
	// involved; the workqueue's AddAfter provides the coalescing window.
	if sandboxWriteBehindWindow > 0 {
		setupLog.Info("Sandbox controller write deferral enabled (--sandbox-write-behind-window)",
			"window", sandboxWriteBehindWindow, "podPatchBound", "1s")
	}

	if err = (&controllers.SandboxReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          mgr.GetEventRecorder("sandbox-controller"),
		Tracer:            instrumenter,
		ClusterDomain:     clusterDomain,
		WriteBehindWindow: sandboxWriteBehindWindow,
	}).SetupWithManager(mgr, sandboxConcurrentWorkers); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}

	if extensions {
		warmSandboxQueue := queue.NewSimpleSandboxQueue()

		var allowedDomains []string
		configPath := "/etc/sandbox-config/allowed-label-domains"
		if data, err := os.ReadFile(configPath); err == nil {
			val := strings.TrimSpace(string(data))
			if val != "" {
				for _, d := range strings.FieldsFunc(val, func(c rune) bool {
					return c == ',' || c == '\n' || c == '\r'
				}) {
					d = strings.ToLower(strings.TrimSpace(d))
					if d != "" {
						allowedDomains = append(allowedDomains, d)
					}
				}
			}
		} else if !os.IsNotExist(err) {
			setupLog.Error(err, "failed to read configuration file", "path", configPath)
			os.Exit(1)
		}

		// Every Eventf site in the claim controller is nil-guarded on the
		// recorder, so a nil recorder cleanly disables event emission.
		var claimRecorder events.EventRecorder
		if disableClaimEvents {
			setupLog.Info("SandboxClaim controller event emission disabled (--disable-claim-events)")
		} else {
			claimRecorder = mgr.GetEventRecorder("sandboxclaim-controller")
		}

		if disableClaimObservabilityAnnotations {
			setupLog.Info("SandboxClaim observability annotation persistence disabled (--disable-claim-observability-annotations)")
		}

		if err = (&extensionscontrollers.SandboxClaimReconciler{
			Client:                          mgr.GetClient(),
			APIReader:                       mgr.GetAPIReader(),
			Scheme:                          mgr.GetScheme(),
			WarmSandboxQueue:                warmSandboxQueue,
			Recorder:                        claimRecorder,
			Tracer:                          instrumenter,
			AllowedLabelDomains:             allowedDomains,
			DisableObservabilityAnnotations: disableClaimObservabilityAnnotations,
		}).SetupWithManager(mgr, sandboxClaimConcurrentWorkers); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "SandboxClaim")
			os.Exit(1)
		}

		if err = (&extensionscontrollers.SandboxTemplateReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorder("sandboxtemplate-controller"),
			Tracer:   instrumenter,
		}).SetupWithManager(mgr, sandboxTemplateConcurrentWorkers); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "SandboxTemplate")
			os.Exit(1)
		}

		if err = (&extensionscontrollers.SandboxWarmPoolReconciler{
			Client:                       mgr.GetClient(),
			Scheme:                       mgr.GetScheme(),
			MaxBatchSize:                 sandboxWarmPoolMaxBatchSize,
			EnableWarmPoolEviction:       enableWarmPoolEviction,
			Recorder:                     mgr.GetEventRecorder("sandboxwarmpool-controller"),
			ReplenishDelay:               sandboxWarmPoolReplenishDelay,
			MaxRefillRate:                sandboxWarmPoolMaxRefillRate,
			ReadinessGracePeriod:         sandboxWarmPoolReadinessGracePeriod,
			UnschedulableRecheckInterval: sandboxWarmPoolUnschedulableRecheckInterval,
		}).SetupWithManager(mgr, sandboxWarmPoolConcurrentWorkers); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "SandboxWarmPool")
			os.Exit(1)
		}

	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
