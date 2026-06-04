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
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	examplev1alpha1 "sigs.k8s.io/agent-sandbox/examples/managed-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/examples/managed-sandbox/controllers/managedsandbox"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var setupLog = ctrl.Log.WithName("setup")

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var poolAgentImage string
	var gatewayName string
	var gatewayNamespace string
	var gatewaySectionName string
	var concurrentWorkers int

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for the example controller.")
	flag.StringVar(&poolAgentImage, "pool-agent-image", "", "Container image for managed sandbox pool pods.")
	flag.StringVar(&gatewayName, "gateway-name", "", "Gateway resource name for optional HTTPRoutes. Empty disables HTTPRoute generation.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "Gateway namespace. Empty means same namespace as the ManagedSandbox.")
	flag.StringVar(&gatewaySectionName, "gateway-section-name", "", "Gateway listener sectionName.")
	flag.IntVar(&concurrentWorkers, "concurrent-workers", 1, "Max concurrent reconciles for the example controller.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if poolAgentImage == "" {
		setupLog.Error(nil, "--pool-agent-image is required")
		os.Exit(1)
	}
	if concurrentWorkers <= 0 {
		setupLog.Error(nil, "--concurrent-workers must be greater than 0")
		os.Exit(1)
	}

	scheme := runtimeScheme()
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "managed-sandbox-example.agents.x-k8s.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var gatewayParent *gwv1.ParentReference
	if gatewayName != "" {
		gatewayParent = &gwv1.ParentReference{Name: gwv1.ObjectName(gatewayName)}
		if gatewayNamespace != "" {
			ns := gwv1.Namespace(gatewayNamespace)
			gatewayParent.Namespace = &ns
		}
		if gatewaySectionName != "" {
			section := gwv1.SectionName(gatewaySectionName)
			gatewayParent.SectionName = &section
		}
	}

	if err := (&managedsandbox.ManagedSandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PodAgentImage: poolAgentImage,
		GatewayParent: gatewayParent,
	}).SetupWithManager(mgr, concurrentWorkers); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ManagedSandbox")
		os.Exit(1)
	}

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

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(examplev1alpha1.AddToScheme(scheme))
	utilruntime.Must(gwv1.AddToScheme(scheme))
	return scheme
}
