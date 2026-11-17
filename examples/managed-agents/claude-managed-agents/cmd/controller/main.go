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

// Command controller bridges Claude Managed Agents to Agent Sandbox: it polls
// the CMA work API, claims one warm sandbox per session through the Go SDK,
// notifies the in-sandbox runner of work items over a port-forward, and
// suspends idle sandboxes. Run it from a workstation with cluster access:
//
//	go run ./cmd/controller --namespace default --warmpool cma-runner-pool
package main

import (
	"context"
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

	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/controller"
	"sigs.k8s.io/agent-sandbox/examples/managed-agents/claude-managed-agents/internal/ports"
)

func main() {
	var (
		namespace     = flag.String("namespace", envOr("SANDBOX_NAMESPACE", "default"), "namespace of the warm pool and sandbox claims")
		warmPool      = flag.String("warmpool", "cma-runner-pool", "SandboxWarmPool to claim session sandboxes from")
		kubeconfig    = flag.String("kubeconfig", "", "path to the kubeconfig (default: standard loading rules)")
		kubeContext   = flag.String("context", "", "kubeconfig context to use (default: current context)")
		environmentID = flag.String("environment-id", os.Getenv("ANTHROPIC_ENVIRONMENT_ID"), "CMA self-hosted environment ID")
		idleSuspend   = flag.Duration("idle-suspend", 30*time.Second, "suspend a session sandbox after this much idle time")
		releaseAfter  = flag.Duration("release-after", 30*time.Minute, "delete a session's claim after this much idle time")
		handoffTTL    = flag.Int64("handoff-ttl", 120, "seconds the first heartbeat leases a work item for hand-off")
		pollInterval  = flag.Duration("poll-interval", time.Second, "delay between work polls when idle")
		baseURL       = flag.String("anthropic-base-url", os.Getenv("ANTHROPIC_BASE_URL"), "custom Anthropic API base URL")
		runnerPort    = flag.Int("runner-port", ports.RunnerHTTP, "port the in-sandbox runner listens on")
		runnerGRPC    = flag.Int("runner-grpc-port", ports.RunnerGRPCPlaceholder, "runner's idle SDK-tunnel gRPC placeholder port; must match the runner's --grpc-placeholder-addr and differ from --runner-port")
		cleanupOnExit = flag.Bool("cleanup-on-exit", true, "delete every claim this controller created on graceful shutdown")
		gcInterval    = flag.Duration("gc-interval", 5*time.Second, "how often the idle sweep runs")
		activeWindow  = flag.Duration("runner-active-window", 15*time.Minute, "how long an unconfirmed turn keeps suspend/release off after the controller stops observing it")
	)
	flag.Parse()

	log := slogr.NewLogr(slog.Default().Handler()).WithName("cma-controller")

	if *environmentID == "" {
		fmt.Fprintln(os.Stderr, "Error: ANTHROPIC_ENVIRONMENT_ID (or --environment-id) is required.")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	sdkClient, err := sdk.NewClient(ctx, sdk.Options{
		RestConfig:       restCfg,
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
	if *cleanupOnExit {
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			log.Info("Cleaning up claims created by this controller")
			sdkClient.DeleteAll(cctx)
		}()
	}

	poller, err := controller.NewPoller(controller.PollerConfig{
		EnvironmentID:     *environmentID,
		BaseURL:           *baseURL,
		HandoffTTLSeconds: *handoffTTL,
		PollInterval:      *pollInterval,
		Logger:            log.WithName("poller"),
	})
	if err != nil {
		log.Error(err, "Creating CMA poller failed")
		os.Exit(1)
	}

	factory := &controller.HandleFactory{
		Client:     sdkClient,
		Forwarder:  controller.NewPodPortForwarder(restCfg),
		Namespace:  *namespace,
		WarmPool:   *warmPool,
		RunnerPort: *runnerPort,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logger:     log.WithName("sessions"),
	}
	manager := controller.NewSessionManager(ctx, controller.SessionManagerConfig{
		Factory: factory.New,
		Logger:  log.WithName("sessions"),
		// Queued work items keep their hand-off lease until the runner's
		// worker takes over heartbeating; renew at half the TTL.
		Renewer:            poller,
		RenewInterval:      time.Duration(*handoffTTL) * time.Second / 2,
		RunnerActiveWindow: *activeWindow,
	})
	gc := controller.NewGC(manager, *idleSuspend, *releaseAfter, *gcInterval, log.WithName("gc"))
	go func() {
		if err := gc.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "GC loop exited")
		}
	}()

	log.Info("Controller started",
		"namespace", *namespace, "warmpool", *warmPool,
		"environment_id", *environmentID,
		"idle_suspend", *idleSuspend, "release_after", *releaseAfter)

	if err := poller.Poll(ctx, func(ctx context.Context, c controller.Claim) error {
		log.Info("Work item claimed",
			"work_id", c.Work.ID, "session_id", c.Work.Data.ID)
		return manager.Dispatch(controller.WorkEvent{
			SessionID:     c.Work.Data.ID,
			WorkID:        c.Work.ID,
			LastHeartbeat: c.LastHeartbeat,
		})
	}); err != nil && ctx.Err() == nil {
		log.Error(err, "Poll loop exited")
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
