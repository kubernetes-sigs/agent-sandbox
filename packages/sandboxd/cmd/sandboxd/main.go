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
// Both listeners always bind to 127.0.0.1; they are reachable outside the
// pod solely through explicit proxying (sandbox-router). SDKs discover the
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
	"strconv"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/server"
)

// Daemon-level timing knobs. Process-lifecycle grace periods live in one
// const block in pkg/server (they are internal to that package).
const (
	// defaultShutdownTimeout is the default for --shutdown-timeout.
	defaultShutdownTimeout = 10 * time.Second
	// defaultHTTPIdleTimeout is the default for --http-idle-timeout.
	defaultHTTPIdleTimeout = 60 * time.Second
	// readHeaderTimeout bounds reading request headers only. Body transfers
	// are deliberately unbounded: ReadTimeout/WriteTimeout would abort large
	// file PUT/GET streams, which this API exists to serve.
	readHeaderTimeout = 10 * time.Second

	// loopbackHost is the only interface sandboxd ever binds: the runtime
	// API must not be reachable from outside the pod without explicit
	// proxying (sandbox-router).
	loopbackHost = "127.0.0.1"
)

// config holds the daemon's flag-configurable settings.
type config struct {
	grpcPort          int
	restPort          int
	rootDir           string
	metadataEnvPrefix string
	shutdownTimeout   time.Duration
	httpIdleTimeout   time.Duration
	streamChunkSize   int
	printVersion      bool
}

func main() {
	var cfg config
	zapOpts := zap.Options{Development: false}

	flag.IntVar(&cfg.grpcPort, "grpc-port", 9090,
		"Port for the gRPC ProcessService. Binds to 127.0.0.1.")
	flag.IntVar(&cfg.restPort, "rest-port", 8080,
		"Port for the Filesystem & Runtime REST API. Binds to 127.0.0.1.")
	flag.StringVar(&cfg.rootDir, "root-dir", "/workspace",
		"Sandbox root directory that file operations and working directories are confined to. Created if missing.")
	flag.StringVar(&cfg.metadataEnvPrefix, "metadata-env-prefix", "SANDBOX_",
		"Only environment variables with this prefix are exposed on GET /v1/metadata.")
	flag.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", defaultShutdownTimeout,
		"Maximum time to wait for in-flight requests and child processes during graceful shutdown.")
	flag.DurationVar(&cfg.httpIdleTimeout, "http-idle-timeout", defaultHTTPIdleTimeout,
		"Close idle HTTP keep-alive connections after this duration.")
	flag.IntVar(&cfg.streamChunkSize, "stream-chunk-size", 0,
		"Buffer size in bytes for streaming process stdout/stderr chunks. 0 selects the default (4096).")
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

// validatePort rejects out-of-range port numbers before they reach
// net.Listen, where the error message would be less direct.
func validatePort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("--%s must be in range 1-65535 (got %d)", name, port)
	}
	return nil
}

func run(cfg *config, log logr.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := validatePort("grpc-port", cfg.grpcPort); err != nil {
		return err
	}
	if err := validatePort("rest-port", cfg.restPort); err != nil {
		return err
	}
	if cfg.grpcPort == cfg.restPort {
		return fmt.Errorf("--grpc-port and --rest-port must differ (both %d)", cfg.grpcPort)
	}

	// Create the sandbox root if missing so a template whose image lacks the
	// directory still starts. MkdirAll errors if the path exists as a file.
	if err := os.MkdirAll(cfg.rootDir, 0o755); err != nil {
		return fmt.Errorf("root dir %q: %w", cfg.rootDir, err)
	}

	srv, err := server.New(server.Options{
		RootDir:           cfg.rootDir,
		MetadataEnvPrefix: cfg.metadataEnvPrefix,
		StreamChunkSize:   cfg.streamChunkSize,
		Log:               log.WithName("rest"),
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	grpcAddr := net.JoinHostPort(loopbackHost, strconv.Itoa(cfg.grpcPort))
	restAddr := net.JoinHostPort(loopbackHost, strconv.Itoa(cfg.restPort))

	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %q: %w", grpcAddr, err)
	}
	restLis, err := net.Listen("tcp", restAddr)
	if err != nil {
		_ = grpcLis.Close()
		return fmt.Errorf("listen rest %q: %w", restAddr, err)
	}

	grpcServer := grpc.NewServer()
	srv.RegisterGRPC(grpcServer)
	reflection.Register(grpcServer)
	httpServer := &http.Server{
		Handler:           srv.RESTHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       cfg.httpIdleTimeout,
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
		// Shutdown is bounded by shutdownCtx; if it expires with connections
		// still open, force-close them so shutdown-timeout is a hard bound,
		// mirroring the gRPC GracefulStop/Stop fallback below.
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "rest server graceful shutdown incomplete; forcing close")
			_ = httpServer.Close()
		}

		grpcStopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(grpcStopped)
		}()
		select {
		case <-grpcStopped:
		case <-shutdownCtx.Done():
			log.Info("grpc graceful stop exceeded shutdown-timeout; forcing stop")
			grpcServer.Stop()
			<-grpcStopped
		}
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
