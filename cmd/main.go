/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spotahome/kooper/v2/controller"
	kooperlogrus "github.com/spotahome/kooper/v2/log/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/barney-s/agent-sandbox/api/v1alpha1"
	"github.com/barney-s/agent-sandbox/controllers"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(sandboxv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// Create a logger.
	logrusLog := logrus.New()
	logger := kooperlogrus.New(logrusLog.WithField("app", "sandbox-controller"))

	// Get kubernetes configuration.
	kcfg := ctrl.GetConfigOrDie()

	// Create a kubernetes clientset.
	k8scli, err := kubernetes.NewForConfig(kcfg)
	if err != nil {
		logger.Errorf("error creating kubernetes clientset: %s", err)
		os.Exit(1)
	}

	// Create a controller-runtime client.
	crCli, err := client.New(kcfg, client.Options{Scheme: scheme})
	if err != nil {
		logger.Errorf("error creating controller-runtime client: %s", err)
		os.Exit(1)
	}

	// Create the controller.
	cfg := controller.Config{
		Name:              "sandbox-controller",
		ConcurrentWorkers: 1,
		ResyncInterval:    30 * time.Second,
	}
	ctrl, err := controllers.NewSandboxController(cfg, k8scli, crCli, logger)
	if err != nil {
		logger.Errorf("error creating controller: %s", err)
		os.Exit(1)
	}

	// Start the controller.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		err := ctrl.Run(ctx)
		if err != nil {
			logger.Errorf("error running controller: %s", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)
	<-sigC

	// Stop the controller.
	logger.Infof("shutting down controller")
	cancel()
}
