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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// Sandbox manages the lifecycle of a single agent-sandbox instance.
// Operations are split across Commands and Files.
type Sandbox struct {
	k8s       *K8sHelper
	connector *connector
	commands  *Commands
	files     *Files
	opts      Options
	log       logr.Logger

	claimName   string
	sandboxName string
	podName     string
	annotations map[string]string

	lifecycleSem chan struct{}
	mu           sync.Mutex
	inflightOps  *sync.WaitGroup
	draining     bool // set by Close before drain; checked by trackOp
	openCancel   context.CancelFunc

	tracer           trace.Tracer
	traceServiceName string
	lifecycleSpan    trace.Span
	lifecycleCtx     context.Context
}

// Compile-time interface checks.
var (
	_ Handle = (*Sandbox)(nil)
	_ Info   = (*Sandbox)(nil)
)

// New creates a new Sandbox with the given options.
// Call Open() to create a sandbox and establish connectivity.
func New(ctx context.Context, opts Options) (*Sandbox, error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}

	k8s := opts.K8sHelper
	if k8s == nil {
		var err error
		k8s, err = NewK8sHelper(opts.RestConfig, opts.Logger)
		if err != nil {
			return nil, err
		}
	}

	tracer, svcName := newTracer(opts)

	// Select connection strategy based on options.
	var strategy ConnectionStrategy
	switch {
	case opts.APIURL != "":
		strategy = &DirectStrategy{URL: opts.APIURL}
	case opts.GatewayName != "":
		strategy = &gatewayStrategy{
			dynamicClient:    k8s.DynamicClient,
			gatewayName:      opts.GatewayName,
			gatewayNamespace: opts.GatewayNamespace,
			gatewayScheme:    opts.GatewayScheme,
			timeout:          opts.GatewayReadyTimeout,
			log:              opts.Logger,
			tracer:           tracer,
			svcName:          svcName,
		}
	default:
		strategy = &tunnelStrategy{
			coreClient:      k8s.CoreClient,
			discoveryClient: k8s.DiscoveryClient,
			restConfig:      k8s.RestConfig,
			namespace:       opts.Namespace,
			pfTimeout:       opts.PortForwardReadyTimeout,
			log:             opts.Logger,
			tracer:          tracer,
			svcName:         svcName,
		}
	}

	conn := newConnector(connectorConfig{
		Strategy:          strategy,
		Namespace:         opts.Namespace,
		ServerPort:        opts.ServerPort,
		RequestTimeout:    opts.RequestTimeout,
		PerAttemptTimeout: opts.PerAttemptTimeout,
		HTTPTransport:     opts.HTTPTransport,
		Log:               opts.Logger,
		Tracer:            tracer,
		TraceServiceName:  svcName,
	})

	// Wire tunnel's connector reference for death notifications.
	if ts, ok := strategy.(*tunnelStrategy); ok {
		ts.connector = conn
	}

	s := &Sandbox{
		k8s:              k8s,
		connector:        conn,
		opts:             opts,
		log:              opts.Logger,
		lifecycleSem:     make(chan struct{}, 1),
		inflightOps:      &sync.WaitGroup{},
		tracer:           tracer,
		traceServiceName: svcName,
	}

	errPrefix := s.errPrefix
	trackOp := s.trackOp
	getLifecycleCtx := func() context.Context {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.lifecycleCtx
	}

	s.commands = &Commands{
		connector:    conn,
		tracer:       tracer,
		svcName:      svcName,
		log:          opts.Logger,
		errPrefix:    errPrefix,
		trackOp:      trackOp,
		lifecycleCtx: getLifecycleCtx,
	}
	s.files = &Files{
		connector:    conn,
		tracer:       tracer,
		svcName:      svcName,
		log:          opts.Logger,
		maxDownload:  opts.MaxDownloadSize,
		maxUpload:    opts.MaxUploadSize,
		errPrefix:    errPrefix,
		trackOp:      trackOp,
		lifecycleCtx: getLifecycleCtx,
	}

	return s, nil
}

// Open creates a SandboxClaim and waits for the sandbox to become ready,
// then discovers the API URL based on the configured connection mode.
// On failure after claim creation, the claim is automatically deleted;
// if deletion also fails, call Close() to retry.
func (s *Sandbox) Open(ctx context.Context) (retErr error) {
	select {
	case s.lifecycleSem <- struct{}{}:
		defer func() { <-s.lifecycleSem }()
	case <-ctx.Done():
		return fmt.Errorf("sandbox[%s/%s]: open cancelled waiting for lifecycle lock: %w", s.opts.Namespace, s.ClaimName(), ctx.Err())
	}

	openCtx, openCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.openCancel = openCancel
	s.mu.Unlock()
	defer func() {
		openCancel()
		s.mu.Lock()
		s.openCancel = nil
		s.mu.Unlock()
	}()

	// Check connectivity outside s.mu to avoid nested lock ordering
	// (s.mu → c.mu). The lifecycleSem serializes all lifecycle operations,
	// so no concurrent mutation is possible between this check and the
	// state reads below.
	if s.connector.IsConnected() {
		return ErrAlreadyOpen
	}

	s.mu.Lock()
	if s.claimName != "" && s.sandboxName == "" {
		s.mu.Unlock()
		return ErrOrphanedClaim
	}
	reconnect := s.claimName != ""
	s.mu.Unlock()

	// Start lifecycle span.
	s.mu.Lock()
	if s.lifecycleSpan != nil {
		s.lifecycleSpan.End()
	}
	lifecycleParent := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(openCtx))
	s.lifecycleCtx, s.lifecycleSpan = s.tracer.Start(lifecycleParent, s.traceServiceName+".lifecycle")
	s.mu.Unlock()

	// Inject lifecycle span into openCtx so child operations inherit it.
	openCtx = trace.ContextWithSpan(openCtx, trace.SpanFromContext(s.lifecycleCtx))

	defer func() {
		if retErr != nil {
			s.mu.Lock()
			if s.lifecycleSpan != nil {
				recordError(s.lifecycleSpan, retErr)
				s.lifecycleSpan.End()
				s.lifecycleSpan = nil
				s.lifecycleCtx = nil
			}
			s.mu.Unlock()
		}
	}()

	if reconnect {
		return s.reconnect(openCtx)
	}

	// Create claim.
	name, err := s.k8s.createClaim(openCtx, s.opts.Namespace, s.opts.TemplateName, s.tracer, s.traceServiceName)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.claimName = name
	s.mu.Unlock()
	s.connector.SetIdentity(name)

	// Wait for sandbox.
	state, err := s.k8s.waitForSandboxReady(openCtx, name, s.opts.Namespace, s.opts.SandboxReadyTimeout, s.tracer, s.traceServiceName)
	if err != nil {
		return s.rollbackOpen(err)
	}
	s.setState(state)

	// Connect transport.
	if err := s.connector.Connect(openCtx); err != nil {
		return s.rollbackOpen(err)
	}

	return nil
}

func (s *Sandbox) reconnect(ctx context.Context) error {
	ctx, span := startSpan(ctx, s.tracer, s.traceServiceName, "reconnect")
	defer span.End()

	reconnCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var reconnCancel context.CancelFunc
		reconnCtx, reconnCancel = context.WithTimeout(ctx, s.opts.SandboxReadyTimeout)
		defer reconnCancel()
	}

	claimName := s.ClaimName()
	sandboxName := s.SandboxName()
	s.log.Info("reconnecting transport", "claim", claimName, "sandbox", sandboxName)

	if err := s.k8s.verifyClaimExists(reconnCtx, claimName, s.opts.Namespace, s.tracer, s.traceServiceName); err != nil {
		s.mu.Lock()
		if k8serrors.IsNotFound(err) {
			s.claimName = ""
			s.sandboxName = ""
			s.podName = ""
			s.annotations = nil
			s.mu.Unlock()
			retErr := fmt.Errorf("%w: %w", ErrSandboxDeleted, err)
			recordError(span, retErr)
			return retErr
		}
		s.sandboxName = ""
		s.podName = ""
		s.annotations = nil
		s.mu.Unlock()
		recordError(span, err)
		return fmt.Errorf("%w: %w", ErrOrphanedClaim, err)
	}

	state, err := s.k8s.verifySandboxAlive(reconnCtx, sandboxName, s.opts.Namespace, s.tracer, s.traceServiceName)
	if err != nil {
		s.mu.Lock()
		if k8serrors.IsNotFound(err) {
			s.sandboxName = ""
			s.podName = ""
			s.annotations = nil
		}
		// Non-NotFound: sandboxName preserved so the next Open() can re-verify
		// instead of forcing the caller to Close() a potentially healthy sandbox.
		s.mu.Unlock()
		retErr := fmt.Errorf("%w: %w", ErrOrphanedClaim, err)
		recordError(span, retErr)
		return retErr
	}
	s.setState(state)

	s.connector.SetIdentity(claimName)
	if err := s.connector.Connect(reconnCtx); err != nil {
		retErr := fmt.Errorf("sandbox: reconnect transport failed (sandbox is alive; retry Open): %w", err)
		recordError(span, retErr)
		return retErr
	}
	return nil
}

func (s *Sandbox) rollbackOpen(originalErr error) error {
	_ = s.connector.Close()

	s.mu.Lock()
	name := s.claimName
	s.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), s.opts.CleanupTimeout)
	defer cancel()

	cleanupErr := s.k8s.deleteClaim(cleanupCtx, name, s.opts.Namespace)

	s.mu.Lock()
	s.sandboxName = ""
	s.podName = ""
	s.annotations = nil
	if cleanupErr == nil {
		s.claimName = ""
	}
	s.mu.Unlock()

	if cleanupErr != nil {
		s.log.Error(cleanupErr, "orphaned claim during Open rollback, could not delete", "claim", name)
		return errors.Join(originalErr, fmt.Errorf("sandbox[%s/%s]: open rollback: %w", s.opts.Namespace, name, cleanupErr))
	}
	return originalErr
}

// Close deletes the SandboxClaim and cleans up resources.
func (s *Sandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.openCancel != nil {
		s.openCancel()
	}
	s.mu.Unlock()

	// Allow up to 2x CleanupTimeout for semaphore acquisition so Close can
	// wait for an in-progress Open that is itself running rollbackOpen with
	// a detached CleanupTimeout context.
	semCtx, semCancel := context.WithTimeout(context.Background(), 2*s.opts.CleanupTimeout)
	defer semCancel()
	select {
	case s.lifecycleSem <- struct{}{}:
		defer func() { <-s.lifecycleSem }()
	default:
		select {
		case s.lifecycleSem <- struct{}{}:
			defer func() { <-s.lifecycleSem }()
		case <-ctx.Done():
			return fmt.Errorf("sandbox[%s/%s]: close cancelled waiting for lifecycle lock: %w", s.opts.Namespace, s.ClaimName(), ctx.Err())
		case <-semCtx.Done():
			return fmt.Errorf("sandbox[%s/%s]: close timed out waiting for lifecycle lock", s.opts.Namespace, s.ClaimName())
		}
	}

	// Single deadline for all remaining cleanup (drain + delete).
	closeDeadline := time.Now().Add(s.opts.CleanupTimeout)

	// Tear down transport first so new operations fail fast with
	// ErrNotReady before we swap the WaitGroup. This closes a race
	// where an operation could slip through trackOp (seeing draining=false,
	// adding to the WG) and still reach a live transport.
	_ = s.connector.Close()

	// Atomically mark draining and swap the WaitGroup so trackOp cannot
	// admit new operations to the old (draining) WaitGroup after this point.
	s.mu.Lock()
	s.draining = true
	drainWG := s.inflightOps
	s.inflightOps = &sync.WaitGroup{}
	s.mu.Unlock()

	drainDone := make(chan struct{})
	go func() {
		drainWG.Wait()
		close(drainDone)
	}()
	drainBudget := time.Until(closeDeadline) / 2 // reserve half for claim deletion
	if drainBudget < 0 {
		drainBudget = 0
	}
	drainTimer := time.NewTimer(drainBudget)
	defer drainTimer.Stop()
	select {
	case <-drainDone:
	case <-drainTimer.C:
		s.log.Info("in-flight operations did not drain; proceeding with cleanup", "timeout", drainBudget)
	}

	// Delete claim using the remaining time budget.
	s.mu.Lock()
	name := s.claimName
	s.mu.Unlock()

	cleanupCtx, cancel := context.WithDeadline(context.Background(), closeDeadline)
	defer cancel()
	err := s.k8s.deleteClaim(cleanupCtx, name, s.opts.Namespace)

	s.mu.Lock()
	s.draining = false
	s.sandboxName = ""
	s.podName = ""
	s.annotations = nil
	if err != nil && s.claimName != "" {
		s.log.Error(err, "orphaned claim during Close, could not delete; retry Close() to clean up", "claim", s.claimName)
	} else {
		s.claimName = ""
	}
	s.mu.Unlock()

	// End lifecycle span.
	s.mu.Lock()
	if s.lifecycleSpan != nil {
		if err != nil {
			recordError(s.lifecycleSpan, err)
		}
		s.lifecycleSpan.End()
		s.lifecycleSpan = nil
		s.lifecycleCtx = nil
	}
	s.mu.Unlock()

	if err != nil {
		return fmt.Errorf("sandbox[%s/%s]: close: %w", s.opts.Namespace, name, err)
	}
	return nil
}

// Disconnect closes the transport connection without deleting the
// SandboxClaim. The sandbox stays alive on the server. Call Open()
// to reconnect. Disconnect is safe to call concurrently with Open;
// an in-progress Open is cancelled before the transport is torn down.
func (s *Sandbox) Disconnect(ctx context.Context) error {
	s.mu.Lock()
	if s.openCancel != nil {
		s.openCancel()
	}
	s.mu.Unlock()

	timer := time.NewTimer(s.opts.CleanupTimeout)
	defer timer.Stop()
	select {
	case s.lifecycleSem <- struct{}{}:
		defer func() { <-s.lifecycleSem }()
	case <-ctx.Done():
		return fmt.Errorf("sandbox[%s/%s]: disconnect cancelled waiting for lifecycle lock: %w", s.opts.Namespace, s.ClaimName(), ctx.Err())
	case <-timer.C:
		// openCancel() above signals a stuck Open to abort and release the
		// semaphore. We intentionally do NOT call connector.Close() here:
		// it would race with a just-succeeding Open() and tear down a live
		// transport, leaving the caller with a nil error from Open() but a
		// dead connection.
		return fmt.Errorf("sandbox[%s/%s]: disconnect timed out waiting for lifecycle lock", s.opts.Namespace, s.ClaimName())
	}

	// End lifecycle span since the session is being suspended.
	// A subsequent Open() (reconnect) will start a new span.
	s.mu.Lock()
	if s.lifecycleSpan != nil {
		s.lifecycleSpan.End()
		s.lifecycleSpan = nil
		s.lifecycleCtx = nil
	}
	s.mu.Unlock()

	return s.connector.Close()
}

// IsReady returns true if the sandbox is ready for communication.
func (s *Sandbox) IsReady() bool {
	return s.connector.IsConnected()
}

// Commands returns the command execution sub-object.
func (s *Sandbox) Commands() *Commands { return s.commands }

// Files returns the file operations sub-object.
func (s *Sandbox) Files() *Files { return s.files }

// Convenience aliases that delegate to sub-objects.

func (s *Sandbox) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error) {
	return s.commands.Run(ctx, command, opts...)
}
func (s *Sandbox) Write(ctx context.Context, path string, content []byte, opts ...CallOption) error {
	return s.files.Write(ctx, path, content, opts...)
}
func (s *Sandbox) Read(ctx context.Context, path string, opts ...CallOption) ([]byte, error) {
	return s.files.Read(ctx, path, opts...)
}
func (s *Sandbox) List(ctx context.Context, path string, opts ...CallOption) ([]FileEntry, error) {
	return s.files.List(ctx, path, opts...)
}
func (s *Sandbox) Exists(ctx context.Context, path string, opts ...CallOption) (bool, error) {
	return s.files.Exists(ctx, path, opts...)
}

// Info accessors.

func (s *Sandbox) ClaimName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimName
}

func (s *Sandbox) SandboxName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxName
}

func (s *Sandbox) PodName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.podName
}

func (s *Sandbox) Annotations() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.annotations == nil {
		return nil
	}
	cp := make(map[string]string, len(s.annotations))
	for k, v := range s.annotations {
		cp[k] = v
	}
	return cp
}

func (s *Sandbox) errPrefix() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimName != "" {
		return fmt.Sprintf("sandbox[%s/%s]", s.opts.Namespace, s.claimName)
	}
	return "sandbox"
}

// trackOp increments the in-flight counter so Close can drain before
// deleting the claim. The draining flag and WaitGroup swap under s.mu
// in Close guarantee mutual exclusion: trackOp either sees draining
// (no-op) or Close's drain captures the WaitGroup. The gap before
// SendRequest is safe because SendRequest re-checks baseURL per attempt.
func (s *Sandbox) trackOp() func() {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return func() {}
	}
	wg := s.inflightOps
	wg.Add(1)
	s.mu.Unlock()
	return wg.Done
}

func (s *Sandbox) setState(state *sandboxState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxName = state.SandboxName
	s.podName = state.PodName
	s.annotations = state.Annotations
}
