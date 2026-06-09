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
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"

	"crypto/tls"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/felixge/fgprof"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	extensionscontrollers "sigs.k8s.io/agent-sandbox/extensions/controllers"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
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
	var sandboxConcurrentWorkers int
	var sandboxClaimConcurrentWorkers int
	var sandboxWarmPoolConcurrentWorkers int
	var sandboxTemplateConcurrentWorkers int
	var sandboxWarmPoolMaxBatchSize int
	var enableWarmPoolEviction bool
	var printVersion bool
	var webhookPort int
	var webhookCertDir string
	var webhookServiceName string
	var webhookNamespace string

	flag.BoolVar(&printVersion, "version", false, "Print version information and exit.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the webhook server binds to.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "The directory that contains the certificates.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "agent-sandbox-webhook-service", "The name of the webhook service.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", "agent-sandbox-system", "The namespace of the webhook service.")
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
	flag.Float64Var(&kubeAPIQPS, "kube-api-qps", -1.0, "QPS limit for kube API client (default is -1 no rate limit-unlimited)")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 10, "Burst limit for kube API client")
	flag.IntVar(&sandboxConcurrentWorkers, "sandbox-concurrent-workers", 1, "Max concurrent reconciles for the Sandbox controller")
	flag.IntVar(&sandboxClaimConcurrentWorkers, "sandbox-claim-concurrent-workers", 1, "Max concurrent reconciles for the SandboxClaim controller")
	flag.IntVar(&sandboxWarmPoolConcurrentWorkers, "sandbox-warm-pool-concurrent-workers", 1, "Max concurrent reconciles for the SandboxWarmPool controller")
	flag.IntVar(&sandboxTemplateConcurrentWorkers, "sandbox-template-concurrent-workers", 1, "Max concurrent reconciles for the SandboxTemplate controller")
	flag.IntVar(&sandboxWarmPoolMaxBatchSize, "sandbox-warm-pool-max-batch-size", 300, "Max batch size for parallel sandbox creation and deletion in SandboxWarmPool controller. Default is 300.")
	flag.BoolVar(&enableWarmPoolEviction, "enable-warm-pool-eviction", true, "Mark pods created by a warm pool as ready-to-evict by default.")
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
	// A logical maximum (too much will create unnecessary load on the API server)
	totalWorkers := sandboxConcurrentWorkers + sandboxClaimConcurrentWorkers + sandboxWarmPoolConcurrentWorkers + sandboxTemplateConcurrentWorkers
	if totalWorkers > 1000 {
		setupLog.Info("Warning: total concurrent workers exceeds 1000, which could lead to resource exhaustion", "total", totalWorkers)
	}

	if kubeAPIBurst <= 0 {
		setupLog.Error(nil, "kube-api-burst must be greater than 0")
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

	// Importing net/http/pprof registers handlers on the global DefaultServeMux.
	// Reset it to avoid accidentally exposing pprof via any server that uses the default mux.
	http.DefaultServeMux = http.NewServeMux()

	scheme := controllers.Scheme
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	if extensions {
		utilruntime.Must(extensionsv1alpha1.AddToScheme(scheme))
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
			metricsOpts.ExtraHandlers["/debug/fgprof"] = fgprof.Handler()
		}
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = float32(kubeAPIQPS)
	restConfig.Burst = kubeAPIBurst

	// Create a temporary client to patch the CRDs and access Secrets
	tempClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create temporary client")
		os.Exit(1)
	}

	// Generate or load self-signed TLS certificates for the webhook server
	setupLog.Info("Preparing webhook certificates", "certDir", webhookCertDir)
	caPEM, err := generateWebhookCerts(ctx, tempClient, webhookCertDir, webhookServiceName, webhookNamespace, clusterDomain)
	if err != nil {
		setupLog.Error(err, "unable to prepare webhook certificates")
		os.Exit(1)
	}

	setupLog.Info("Patching CRDs with generated CA bundle")
	if err := patchCRDs(ctx, tempClient, caPEM, webhookServiceName, webhookNamespace); err != nil {
		setupLog.Error(err, "failed to patch CRDs with CA bundle")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        "a3317529.agent-sandbox.x-k8s.io",
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
			TLSOpts: []func(*tls.Config){
				func(cfg *tls.Config) {
					cfg.ClientAuth = tls.NoClientCert
				},
			},
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register the custom Sandbox metric collector globally.
	asmetrics.RegisterSandboxCollector(mgr.GetClient(), mgr.GetLogger().WithName("sandbox-collector"))

	if err = (&controllers.SandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Tracer:        instrumenter,
		ClusterDomain: clusterDomain,
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

		if err = (&extensionscontrollers.SandboxClaimReconciler{
			Client:              mgr.GetClient(),
			Scheme:              mgr.GetScheme(),
			WarmSandboxQueue:    warmSandboxQueue,
			Recorder:            mgr.GetEventRecorder("sandboxclaim-controller"),
			Tracer:              instrumenter,
			AllowedLabelDomains: allowedDomains,
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
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			MaxBatchSize:           sandboxWarmPoolMaxBatchSize,
			EnableWarmPoolEviction: enableWarmPoolEviction,
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
