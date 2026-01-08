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
	"flag"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	extensionscontrollers "sigs.k8s.io/agent-sandbox/extensions/controllers"
	//+kubebuilder:scaffold:imports
)

var (
	setupLog = ctrl.Log.WithName("setup")
)

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var extensions bool
	var enablePprof bool
	var enablePprofDebug bool
	var pprofBlockProfileRate int
	var pprofMutexProfileFraction int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&extensions, "extensions", false, "Enable extensions controllers.")
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
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Importing net/http/pprof registers handlers on the global DefaultServeMux.
	// Reset it to avoid accidentally exposing pprof via any server that uses the default mux.
	http.DefaultServeMux = http.NewServeMux()

	scheme := controllers.Scheme
	if extensions {
		utilruntime.Must(extensionsv1alpha1.AddToScheme(scheme))
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
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a3317529.x-k8s.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}

	if extensions {
		if err = (&extensionscontrollers.SandboxClaimReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "SandboxClaim")
			os.Exit(1)
		}

		if err = (&extensionscontrollers.SandboxWarmPoolReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr); err != nil {
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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
