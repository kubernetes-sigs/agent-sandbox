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

// Command controller bridges Amazon Bedrock Managed Agents (self-hosted
// environments) to Agent Sandbox: it polls managed sessions, claims one
// warm sandbox per session through the Go SDK, attaches the session's
// executor in the in-sandbox runner over a port-forward, and suspends idle
// sandboxes. Run it from a workstation with cluster access and AWS
// credentials:
//
//	go run ./cmd/controller --namespace default --warmpool bma-runner-pool --region us-east-1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr/slogr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/bma"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/controller"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/bedrock-managed-agents/internal/ports"
)

func main() {
	var (
		namespace     = flag.String("namespace", envOr("SANDBOX_NAMESPACE", "default"), "namespace of the warm pool and sandbox claims")
		warmPool      = flag.String("warmpool", "bma-runner-pool", "SandboxWarmPool to claim session sandboxes from")
		region        = flag.String("region", envOr("BMA_REGION", envOr("AWS_REGION", "us-east-1")), "BMA region (signing region for the bedrock-mantle endpoint)")
		endpoint      = flag.String("endpoint", os.Getenv("BMA_ENDPOINT"), "BMA endpoint override (default: https://bedrock-mantle.<region>.api.aws)")
		kubeconfig    = flag.String("kubeconfig", "", "path to the kubeconfig (default: standard loading rules)")
		kubeContext   = flag.String("context", "", "kubeconfig context to use (default: current context)")
		idleSuspend   = flag.Duration("idle-suspend", 30*time.Second, "suspend a session sandbox after this much idle time")
		releaseAfter  = flag.Duration("release-after", 30*time.Minute, "delete a session's claim after this much idle time")
		pollInterval  = flag.Duration("poll-interval", 5*time.Second, "delay between session reconciliations")
		connectTO     = flag.Duration("connect-timeout", 4*time.Minute, "how long to wait for the executor to report attached")
		runnerPort    = flag.Int("runner-port", ports.RunnerHTTP, "port the in-sandbox runner listens on")
		runnerGRPC    = flag.Int("runner-grpc-port", ports.RunnerGRPCPlaceholder, "runner's idle SDK-tunnel placeholder port; must differ from --runner-port")
		cleanupOnExit = flag.Bool("cleanup-on-exit", true, "on graceful shutdown, delete the claims of sessions the platform reports inactive")
		gcInterval    = flag.Duration("gc-interval", 5*time.Second, "how often the idle sweep runs")
		activeWindow  = flag.Duration("runner-active-window", 15*time.Minute, "how long an unconfirmed turn keeps suspend/release off after the controller stops observing it")
	)
	flag.Parse()

	log := slogr.NewLogr(slog.Default().Handler()).WithName("bma-controller")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bmaClient, err := bma.NewClient(ctx, bma.Options{Region: *region, Endpoint: *endpoint})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: building the BMA client: %v\n", err)
		os.Exit(1)
	}
	api := controller.NewAWSAPI(bmaClient)

	// Shared platform-state check: GC uses it to veto suspensions, and the
	// shutdown cleanup uses it to keep claims of sessions that may still be
	// running a turn.
	sessionActive := func(ctx context.Context, sessionID string) (bool, error) {
		info, err := api.GetSession(ctx, sessionID)
		if errors.Is(err, controller.ErrSessionNotFound) {
			// Gone on the platform: nothing is running, GC will release.
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return info.ActiveTurn, nil
	}

	// Fail fast on a bad key instead of silently backing off forever.
	func() {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := api.ListManagedSessions(probeCtx); err != nil {
			if controller.IsUnauthorized(err) {
				fmt.Fprintln(os.Stderr, "Error: AWS credentials were rejected by BMA (401/403). Check the profile/role and its BMA permissions.")
				os.Exit(1)
			}
			log.Error(err, "Initial session list failed; starting anyway")
		}
	}()

	restCfg, err := buildRestConfig(*kubeconfig, *kubeContext)
	if err != nil {
		log.Error(err, "Building Kubernetes client config failed")
		os.Exit(1)
	}

	// The SDK manages claim lifecycle only; this controller never uses the
	// SDK's sandboxd data plane. RuntimeSandboxd keeps the SDK off the
	// sandbox-router (its tunnel goes straight to the pod through the
	// apiserver). The SDK requires its two tunnel ports to differ, so the
	// gRPC port points at the runner's idle placeholder listener.
	if *runnerGRPC == *runnerPort {
		fmt.Fprintln(os.Stderr, "Error: --runner-grpc-port must differ from --runner-port (the SDK rejects equal tunnel ports).")
		os.Exit(1)
	}
	// Shared with the SDK client: the handles use it for the direct Sandbox
	// CR access the SDK client does not expose (resume-before-adopt,
	// suspend-by-name).
	k8sHelper, err := sdk.NewK8sHelper(restCfg, log.WithName("k8s"))
	if err != nil {
		log.Error(err, "Creating Kubernetes helper failed")
		os.Exit(1)
	}
	sdkClient, err := sdk.NewClient(ctx, sdk.Options{
		RestConfig:       restCfg,
		K8sHelper:        k8sHelper,
		Namespace:        *namespace,
		Runtime:          sdk.RuntimeSandboxd,
		SandboxdRESTPort: *runnerPort,
		SandboxdGRPCPort: *runnerGRPC,
		Logger:           log.WithName("sdk"),
	})
	if err != nil {
		log.Error(err, "Creating sandbox SDK client failed")
		os.Exit(1)
	}
	factory := &controller.HandleFactory{
		Client:     sdkClient,
		K8s:        k8sHelper,
		Forwarder:  controller.NewPodPortForwarder(restCfg),
		Namespace:  *namespace,
		WarmPool:   *warmPool,
		RunnerPort: *runnerPort,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		API:        api,
		Logger:     log.WithName("sessions"),
	}
	manager := controller.NewSessionManager(ctx, controller.SessionManagerConfig{
		Factory:            factory.New,
		Logger:             log.WithName("sessions"),
		ConnectTimeout:     *connectTO,
		RunnerActiveWindow: *activeWindow,
		SessionActiveCheck: sessionActive,
	})
	if *cleanupOnExit {
		// ReleaseInactive keeps claims of active or unknown-state sessions:
		// deleting one mid-turn destroys a workspace the platform still
		// expects.
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			log.Info("Cleaning up claims of inactive sessions")
			manager.ReleaseInactive(cctx)
		}()
	}
	gc := controller.NewGC(manager, api, *idleSuspend, *releaseAfter, *gcInterval, log.WithName("gc"))
	go func() {
		if err := gc.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "GC loop exited")
		}
	}()

	reconciler := controller.NewReconciler(api, manager, *pollInterval, log.WithName("reconciler"))

	log.Info("Controller started",
		"namespace", *namespace, "warmpool", *warmPool,
		"poll_interval", *pollInterval, "connect_timeout", *connectTO,
		"idle_suspend", *idleSuspend, "release_after", *releaseAfter)

	if err := reconciler.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error(err, "Reconciler exited")
		os.Exit(1)
	}
	log.Info("Controller stopped")
}

// buildRestConfig resolves the kubeconfig the SDK and the port-forwarder share.
func buildRestConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
