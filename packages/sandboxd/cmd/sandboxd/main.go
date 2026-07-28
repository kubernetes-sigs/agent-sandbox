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

// Binary sandboxd is the portable sandbox runtime daemon defined by
// KEP-539.2. It serves the hybrid runtime API from inside a sandbox pod:
//
//	gRPC  :9090  ProcessService    — streaming process execution
//	HTTP  :8080  FilesystemService — stateless file operations & probes
//
// Both listeners bind to localhost only; they are reachable outside the pod
// solely through explicit proxying (sandbox-router). SDKs discover the
// endpoints via the SANDBOXD_GRPC_ADDR / SANDBOXD_REST_ADDR environment
// variables on the workload container.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
)

// config holds the daemon's flag-configurable settings.
type config struct {
	grpcAddr          string
	restAddr          string
	rootDir           string
	metadataEnvPrefix string
	shutdownTimeout   time.Duration
	printVersion      bool
}

func main() {
	var cfg config
	zapOpts := zap.Options{Development: false}

	flag.StringVar(&cfg.grpcAddr, "grpc-addr", "127.0.0.1:9090",
		"Listen address for the gRPC ProcessService. Must stay on localhost per KEP-539.2.")
	flag.StringVar(&cfg.restAddr, "rest-addr", "127.0.0.1:8080",
		"Listen address for the Filesystem & Runtime REST API. Must stay on localhost per KEP-539.2.")
	flag.StringVar(&cfg.rootDir, "root-dir", "/workspace",
		"Sandbox root directory that file operations and working directories are confined to.")
	flag.StringVar(&cfg.metadataEnvPrefix, "metadata-env-prefix", "SANDBOX_",
		"Only environment variables with this prefix are exposed on GET /v1/metadata.")
	flag.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", 10*time.Second,
		"Maximum time to wait for in-flight requests and child processes during graceful shutdown.")
	flag.BoolVar(&cfg.printVersion, "version", false, "Print version information and exit.")
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if cfg.printVersion {
		fmt.Println(version.Print("sandboxd"))
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("sandboxd")

	if err := run(&cfg, log); err != nil {
		log.Error(err, "exited with error")
		os.Exit(1)
	}
}

func run(cfg *config, log logr.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if info, err := os.Stat(cfg.rootDir); err != nil {
		return fmt.Errorf("root dir %q: %w", cfg.rootDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("root dir %q is not a directory", cfg.rootDir)
	}

	srv := server.New(server.Options{
		RootDir:           cfg.rootDir,
		MetadataEnvPrefix: cfg.metadataEnvPrefix,
		Log:               log.WithName("rest"),
	})

	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %q: %w", cfg.grpcAddr, err)
	}
	restLis, err := net.Listen("tcp", cfg.restAddr)
	if err != nil {
		_ = grpcLis.Close()
		return fmt.Errorf("listen rest %q: %w", cfg.restAddr, err)
	}

	grpcServer := grpc.NewServer()
	srv.RegisterGRPC(grpcServer)
	httpServer := &http.Server{
		Handler:           srv.RESTHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := httpServer.Serve(restLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("rest server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")

		// Flip readiness first so Kubernetes stops routing, then end child
		// processes — that closes their Start streams, which is what allows
		// GracefulStop to complete.
		srv.SetReady(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
		defer cancel()
		srv.ShutdownProcesses(shutdownCtx)
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "rest server shutdown")
		}
		grpcServer.GracefulStop()
		return nil
	})

	log.Info("sandboxd listening",
		"version", version.Get().GitVersion,
		"sha", version.Get().GitSHA,
		"grpc", grpcLis.Addr().String(),
		"rest", restLis.Addr().String(),
		"rootDir", cfg.rootDir,
	)
	return g.Wait()
}
