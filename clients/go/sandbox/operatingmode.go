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
	"time"

	"go.opentelemetry.io/otel/trace"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// Suspend puts the sandbox into the suspended state: it sets the Sandbox's
// spec.operatingMode to Suspended (unless already set), closes the transport,
// and waits for the Suspended condition, which the controller sets once the
// backing pod has terminated. The claim and sandbox identity are preserved,
// so Resume brings the same sandbox back.
//
// Suspend is idempotent and also completes a suspension requested outside the
// SDK: it waits for the pod to terminate and closes a live transport instead
// of returning early.
// Pass WithTimeout to override Options.SandboxReadyTimeout as the wait budget.
func (s *Sandbox) Suspend(ctx context.Context, opts ...CallOption) error {
	timeout := s.suspendResumeTimeout(opts)

	// WithTimeout documents a total operation timeout, so the deadline
	// covers the semaphore wait, the read, the patch, and the condition
	// wait. Registering the cancel before acquisition lets Close and
	// Disconnect interrupt an operation still parked on the semaphore.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer s.registerLifecycleCancel(cancel)()

	if err := s.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer func() { <-s.lifecycleSem }()

	name := s.SandboxName()
	if name == "" {
		return fmt.Errorf("%s: %w: Suspend needs a sandbox resolved by Open", s.errPrefix(), ErrNotReady)
	}

	ctx, span := startSpan(ctx, s.tracer, s.traceServiceName, "suspend")
	defer span.End()

	cr, err := s.readSandboxCR(ctx, name)
	if err != nil {
		recordError(span, err)
		return err
	}
	if err := s.verifySandboxIdentity(cr); err != nil {
		recordError(span, err)
		return err
	}

	// Gate the wait on the generation the controller must observe. On the
	// patched path that is the generation the patch produced; when the
	// suspension was requested externally, the current generation.
	minGeneration := cr.Generation
	if cr.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		patched, err := s.k8s.patchSandboxOperatingMode(ctx, name, s.opts.Namespace, sandboxv1beta1.SandboxOperatingModeSuspended, cr.UID, s.tracer, s.traceServiceName)
		if err != nil {
			recordError(span, err)
			return s.operatingModeErr(err)
		}
		minGeneration = patched.Generation
	} else {
		s.log.V(1).Info("sandbox already suspended; completing suspension", "sandbox", name)
	}

	// The pod is going away either way, so tear the transport down before
	// waiting: a timed-out wait must not leave the caller holding a
	// connection to a dying pod.
	s.teardownAfterSuspend()

	if err := s.k8s.waitForSandboxSuspended(ctx, name, s.opts.Namespace, timeout, minGeneration, cr.UID, s.tracer, s.traceServiceName); err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: %w", s.errPrefix(), err)
	}
	s.setSuspendedLocally(true)
	s.log.Info("sandbox suspended", "sandbox", name)
	return nil
}

// Resume makes a suspended sandbox runnable again: it sets the Sandbox's
// spec.operatingMode back to Running, waits for the sandbox to become Ready,
// and re-establishes connectivity so the caller can keep using the same
// handle.
//
// Resume is idempotent. On a sandbox that is already running it does not
// patch: Resume waits for the sandbox to report Ready at its current
// generation and leaves a connected transport alone, unless the pod behind
// the sandbox changed — another client may have suspended and resumed it —
// in which case Resume rebuilds the transport. A handle that lost its
// transport always reconnects.
// Pass WithTimeout to override Options.SandboxReadyTimeout as the wait budget.
func (s *Sandbox) Resume(ctx context.Context, opts ...CallOption) error {
	timeout := s.suspendResumeTimeout(opts)

	// WithTimeout documents a total operation timeout, so the deadline
	// covers the semaphore wait, the read, the patch, the readiness wait,
	// and the reconnect. Registering the cancel before acquisition lets
	// Close and Disconnect interrupt an operation still parked on the
	// semaphore.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer s.registerLifecycleCancel(cancel)()

	if err := s.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer func() { <-s.lifecycleSem }()

	name := s.SandboxName()
	if name == "" {
		return fmt.Errorf("%s: %w: Resume needs a sandbox resolved by Open", s.errPrefix(), ErrNotReady)
	}

	ctx, span := startSpan(ctx, s.tracer, s.traceServiceName, "resume")
	defer span.End()

	cr, err := s.readSandboxCR(ctx, name)
	if err != nil {
		recordError(span, err)
		return err
	}
	if err := s.verifySandboxIdentity(cr); err != nil {
		recordError(span, err)
		return err
	}

	// Gate the readiness wait on the generation the controller must
	// observe. Without the floor, a Ready=True left over from before an
	// interrupted suspend would satisfy the wait while the controller is
	// still terminating the old pod.
	minGeneration := cr.Generation
	resuming := cr.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended
	if resuming {
		patched, err := s.k8s.patchSandboxOperatingMode(ctx, name, s.opts.Namespace, sandboxv1beta1.SandboxOperatingModeRunning, cr.UID, s.tracer, s.traceServiceName)
		if err != nil {
			recordError(span, err)
			return s.operatingModeErr(err)
		}
		minGeneration = patched.Generation
		// Drop any transport this handle still holds before waiting. When
		// the suspension came from another client, the old tunnel only
		// dies now, and a stale base URL would let a failed reconnect — or
		// a later Resume — report success without a working transport.
		_ = s.connector.Close()
	} else {
		s.log.V(1).Info("sandbox is already running", "sandbox", name)
	}

	state, err := s.k8s.waitForSandboxReady(ctx, name, s.opts.Namespace, timeout, minGeneration, cr.UID, sandboxv1beta1.SandboxOperatingModeRunning, s.tracer, s.traceServiceName)
	if err != nil {
		recordError(span, err)
		return fmt.Errorf("%s: %w", s.errPrefix(), err)
	}

	// The sandbox never left Running and the readiness wait confirms the
	// current generation: a connected transport still points at the same
	// pod, so there is nothing to rebuild.
	if !resuming && s.connector.IsConnected() && !s.podIdentityChanged(state) {
		s.log.V(1).Info("sandbox is running and connected", "sandbox", name)
		return nil
	}

	s.setState(state)

	// Start a fresh lifecycle span so operations after the resume have a
	// parent, mirroring what Open does for a new session.
	s.mu.Lock()
	if s.lifecycleSpan != nil {
		s.lifecycleSpan.End()
	}
	lifecycleParent := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx))
	s.lifecycleCtx, s.lifecycleSpan = s.tracer.Start(lifecycleParent, s.traceServiceName+".lifecycle")
	s.mu.Unlock()

	// Close first: on the pod-changed path the old transport is still up,
	// and reconnecting over it would leak the previous tunnel.
	_ = s.connector.Close()
	s.connector.SetIdentity(name)
	s.connector.SetPodIP(state.PodIP)
	if err := s.connector.Connect(ctx); err != nil {
		// Roll the lifecycle span back so a failed resume does not leave
		// one dangling.
		s.mu.Lock()
		if s.lifecycleSpan != nil {
			s.lifecycleSpan.End()
			s.lifecycleSpan = nil
			s.lifecycleCtx = nil
		}
		s.mu.Unlock()
		retErr := fmt.Errorf("%s: resume transport failed (sandbox is ready; retry Resume): %w", s.errPrefix(), err)
		recordError(span, retErr)
		return retErr
	}
	s.setSuspendedLocally(false)
	s.log.Info("sandbox resumed", "sandbox", name)
	return nil
}

// IsSuspended reports whether the Sandbox CR asks the controller to keep the
// sandbox suspended (spec.operatingMode is Suspended). It reflects the desired
// state, not whether the pod has finished terminating; Suspend waits for that.
func (s *Sandbox) IsSuspended(ctx context.Context) (bool, error) {
	name := s.SandboxName()
	if name == "" {
		return false, fmt.Errorf("%s: %w: IsSuspended needs a sandbox resolved by Open", s.errPrefix(), ErrNotReady)
	}
	cr, err := s.readSandboxCR(ctx, name)
	if err != nil {
		return false, err
	}
	if err := s.verifySandboxIdentity(cr); err != nil {
		return false, err
	}
	return cr.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended, nil
}

// podIdentityChanged reports whether state points at a different pod than
// the one this handle's transport was built for. The controller recreates
// the pod under the same name across a suspend/resume cycle, so the pod IP
// carries the change signal; a field left empty on either side proves
// nothing and is skipped.
func (s *Sandbox) podIdentityChanged(state *sandboxState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.PodName != "" && s.podName != "" && state.PodName != s.podName {
		return true
	}
	return state.PodIP != "" && s.podIP != "" && state.PodIP != s.podIP
}

// readSandboxCR reads the Sandbox CR. A deleted sandbox maps to
// ErrSandboxDeleted so callers can classify it with errors.Is, matching the
// reconnect path.
func (s *Sandbox) readSandboxCR(ctx context.Context, name string) (*sandboxv1beta1.Sandbox, error) {
	sb, err := s.k8s.getSandbox(ctx, name, s.opts.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s: %w: %w", s.errPrefix(), ErrSandboxDeleted, err)
		}
		return nil, fmt.Errorf("%s: failed to read sandbox %s: %w", s.errPrefix(), name, err)
	}
	return sb, nil
}

// verifySandboxIdentity fails when the CR read back is not the sandbox this
// handle opened: a same-name replacement must not be suspended or resumed.
// It mirrors claimIdentityFailure, which guards the claim the same way. An
// empty recorded UID (a handle that never went through Open) disables the
// check.
func (s *Sandbox) verifySandboxIdentity(cr *sandboxv1beta1.Sandbox) error {
	s.mu.Lock()
	want := s.sandboxUID
	s.mu.Unlock()
	if want == "" || cr.UID == want {
		return nil
	}
	return fmt.Errorf("%s: %w: sandbox %s was replaced (uid %s, want %s)", s.errPrefix(), ErrSandboxDeleted, cr.Name, cr.UID, want)
}

// operatingModeErr classifies a patch failure: a sandbox deleted between the
// read and the patch reports ErrSandboxDeleted like readSandboxCR does.
func (s *Sandbox) operatingModeErr(err error) error {
	if k8serrors.IsNotFound(err) {
		return fmt.Errorf("%s: %w: %w", s.errPrefix(), ErrSandboxDeleted, err)
	}
	return fmt.Errorf("%s: %w", s.errPrefix(), err)
}

// teardownAfterSuspend closes the transport and drops cached pod identity —
// the pod is terminated or terminating — while keeping the claim and sandbox
// names so Resume can restore the same sandbox. It is safe to call when the
// transport is already closed.
func (s *Sandbox) teardownAfterSuspend() {
	s.mu.Lock()
	if s.lifecycleSpan != nil {
		s.lifecycleSpan.End()
		s.lifecycleSpan = nil
		s.lifecycleCtx = nil
	}
	s.podName = ""
	s.podIP = ""
	s.serviceFQDN = ""
	s.mu.Unlock()

	_ = s.connector.Close()
}

// registerLifecycleCancel publishes the cancel function of a lifecycle
// operation so Close and Disconnect can interrupt it, and returns the
// function that removes it again. The lifecycle semaphore guarantees only
// one operation runs at a time, but more can be parked waiting for it, so
// the hooks live in a set instead of a single slot.
func (s *Sandbox) registerLifecycleCancel(cancel context.CancelFunc) func() {
	s.mu.Lock()
	if s.lifecycleCancels == nil {
		s.lifecycleCancels = make(map[int]context.CancelFunc)
	}
	id := s.lifecycleCancelSeq
	s.lifecycleCancelSeq++
	s.lifecycleCancels[id] = cancel
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.lifecycleCancels, id)
		s.mu.Unlock()
	}
}

// interruptLifecycle cancels every registered lifecycle context: the
// operation holding the semaphore and anything parked waiting for it.
func (s *Sandbox) interruptLifecycle() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.lifecycleCancels))
	for _, cancel := range s.lifecycleCancels {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// setSuspendedLocally records whether this handle has suspended the sandbox
// and not resumed it since. The managed Client uses it to tell a suspended
// handle from a stale one: both report IsReady() false, but only the stale
// one should be evicted and re-attached.
func (s *Sandbox) setSuspendedLocally(v bool) {
	s.mu.Lock()
	s.suspended = v
	s.mu.Unlock()
}

// suspendedLocally reports the flag set by Suspend and cleared by Resume
// and Open.
func (s *Sandbox) suspendedLocally() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// suspendResumeTimeout resolves the wait budget: WithTimeout when set,
// otherwise Options.SandboxReadyTimeout (matching the Python SDK's 180s
// default wait_timeout).
func (s *Sandbox) suspendResumeTimeout(opts []CallOption) time.Duration {
	var co callOptions
	for _, o := range opts {
		o(&co)
	}
	if co.timeout > 0 {
		return co.timeout
	}
	return s.opts.SandboxReadyTimeout
}

// acquireLifecycle takes the lifecycle semaphore, honoring ctx cancellation,
// so Suspend/Resume serialize against Open, Close, and Disconnect. The
// caller's WithTimeout budget covers this wait too: expiring here reports
// ErrTimeout like the condition waits do.
func (s *Sandbox) acquireLifecycle(ctx context.Context) error {
	select {
	case s.lifecycleSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s: %w: waiting for lifecycle lock: %v", s.errPrefix(), ErrTimeout, ctx.Err())
		}
		return fmt.Errorf("%s: operation cancelled waiting for lifecycle lock: %w", s.errPrefix(), ctx.Err())
	}
}
