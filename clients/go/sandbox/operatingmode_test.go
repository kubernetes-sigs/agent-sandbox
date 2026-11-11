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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace/noop"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
)

// seedGeneration is the metadata.generation the test CRs are seeded with.
// The fake clientset does not bump generation on patch, so patched objects
// keep this value and controller-simulated conditions use it as their
// ObservedGeneration.
const seedGeneration = int64(3)

// sandboxGVR is the Sandbox resource the typed fake clientset acts on.
// Seeding through NewSimpleClientset(cr) does not work for this kind: the
// tracker guesses the plural as "sandboxs", so objects must be added under
// the real GVR explicitly.
var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1beta1",
	Resource: "sandboxes",
}

// newOperatingModeCR builds a Sandbox CR seeded with an operating mode and
// status conditions for the suspend/resume tests.
func newOperatingModeCR(name string, mode sandboxv1beta1.SandboxOperatingMode, conditions ...metav1.Condition) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: seedGeneration, UID: types.UID("uid-" + name)},
		Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: mode},
		Status:     sandboxv1beta1.SandboxStatus{Conditions: conditions},
	}
}

// suspendedCond builds a Suspended condition the controller observed at the
// given generation.
func suspendedCond(observedGeneration int64) metav1.Condition {
	return metav1.Condition{
		Type:               string(sandboxv1beta1.SandboxConditionSuspended),
		Status:             metav1.ConditionTrue,
		Reason:             sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		ObservedGeneration: observedGeneration,
	}
}

// readyCond builds a Ready condition the controller observed at the given
// generation.
func readyCond(observedGeneration int64) metav1.Condition {
	return metav1.Condition{
		Type:               string(sandboxv1beta1.SandboxConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		ObservedGeneration: observedGeneration,
	}
}

// newSuspendTestSandbox builds an opened-looking Sandbox backed by a fake
// agents clientset. The sandbox never goes through Open: the identity fields
// an Open would have resolved are injected directly, and connected makes
// IsReady report true.
func newSuspendTestSandbox(t *testing.T, cr *sandboxv1beta1.Sandbox, connected bool) (*Sandbox, *fakeagents.Clientset) {
	t.Helper()
	agentsCS := fakeagents.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	if err := agentsCS.Tracker().Create(sandboxGVR, cr, cr.Namespace); err != nil {
		t.Fatalf("seed sandbox CR: %v", err)
	}
	opts := Options{
		Namespace:           "default",
		APIURL:              "http://localhost:9999",
		SandboxReadyTimeout: 5 * time.Second,
		Quiet:               true,
	}
	opts.setDefaults()
	opts.K8sHelper = &K8sHelper{
		AgentsClient: agentsCS.AgentsV1beta1(),
		Log:          logr.Discard(),
	}
	sb, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sb.sandboxName = cr.Name
	sb.sandboxUID = cr.UID
	// The controller names the pod after the sandbox, and extractState
	// falls back to the sandbox name when the pod-name annotation is
	// absent, so the cached name must match for Resume's pod-change check
	// to stay quiet on an unchanged pod.
	sb.podName = cr.Name
	if connected {
		sb.connector.SetIdentity(cr.Name)
		sb.connector.baseURL = "http://fake"
	}
	return sb, agentsCS
}

// hasPatchAction reports whether any patch was issued against the fake.
func hasPatchAction(cs *fakeagents.Clientset) bool {
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			return true
		}
	}
	return false
}

// getCR reads the current Sandbox CR from the fake.
func getCR(t *testing.T, cs *fakeagents.Clientset, name string) *sandboxv1beta1.Sandbox {
	t.Helper()
	cr, err := cs.AgentsV1beta1().Sandboxes("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sandbox CR: %v", err)
	}
	return cr
}

// waitForPatchAction blocks until the SDK has issued its patch, so a test
// only simulates the controller afterwards, the way the real sequence runs.
func waitForPatchAction(t *testing.T, cs *fakeagents.Clientset) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hasPatchAction(cs) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the SDK to patch the Sandbox CR")
}

// simulateController repeatedly applies mutate to the CR until stop closes,
// standing in for the controller observing the patch and updating status.
// The repeat hedges against the update landing between the wait's List and
// Watch calls, where a single event would be missed.
func simulateController(cs *fakeagents.Clientset, name string, mutate func(*sandboxv1beta1.Sandbox), stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx := context.Background()
				cr, err := cs.AgentsV1beta1().Sandboxes("default").Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return
				}
				mutate(cr)
				_, _ = cs.AgentsV1beta1().Sandboxes("default").Update(ctx, cr, metav1.UpdateOptions{})
			}
		}
	}()
}

// controllerStop returns a stop channel closed automatically at test cleanup,
// bounding the simulateController goroutine's lifetime (goleak checks this).
func controllerStop(t *testing.T) chan struct{} {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return stop
}

// assertNotCompleted fails if the operation finished before the simulated
// controller update, which would mean a stale condition satisfied the wait.
func assertNotCompleted(t *testing.T, errCh <-chan error, op string) {
	t.Helper()
	select {
	case err := <-errCh:
		t.Fatalf("%s returned before the controller observed the patch (err=%v); a stale condition must not satisfy the wait", op, err)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSandbox_Suspend_WaitsForControllerObservation(t *testing.T) {
	// A Suspended=True condition left over from a previous suspend cycle
	// (older ObservedGeneration) must not satisfy the wait.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, suspendedCond(seedGeneration-1))
	sb, cs := newSuspendTestSandbox(t, cr, true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background()) }()

	waitForPatchAction(t, cs)
	assertNotCompleted(t, errCh, "Suspend")

	simulateController(cs, cr.Name, func(cr *sandboxv1beta1.Sandbox) {
		cr.Status.Conditions = []metav1.Condition{suspendedCond(cr.Generation)}
	}, controllerStop(t))

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Suspend: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not return after the controller observed the suspension")
	}

	if got := getCR(t, cs, "sbx-1").Spec.OperatingMode; got != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Errorf("operatingMode = %q, want Suspended", got)
	}
	if sb.IsReady() {
		t.Error("transport is still connected after Suspend")
	}
	if sb.PodIP() != "" || sb.PodName() != "" {
		t.Errorf("stale pod identity after Suspend: podName=%q podIP=%q", sb.PodName(), sb.PodIP())
	}
	if sb.SandboxName() != "sbx-1" {
		t.Errorf("Suspend cleared the sandbox name: %q", sb.SandboxName())
	}
	// The managed Client relies on this flag to keep suspended handles
	// registered instead of evicting them as stale.
	if !sb.suspendedLocally() {
		t.Error("handle does not track the suspension after Suspend")
	}
}

func TestSandbox_Suspend_AlreadySuspended_CompletesSuspension(t *testing.T) {
	// The controller already observed the suspension, so Suspend only needs
	// to close the transport; it must not patch again.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended, suspendedCond(seedGeneration))
	sb, cs := newSuspendTestSandbox(t, cr, true)

	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend on suspended sandbox: %v", err)
	}
	if hasPatchAction(cs) {
		t.Error("Suspend patched an already suspended sandbox; want no patch")
	}
	if sb.IsReady() {
		t.Error("transport is still connected after suspending an externally suspended sandbox")
	}
}

func TestSandbox_Suspend_ExternallySuspended_WaitsForCondition(t *testing.T) {
	// spec.operatingMode is already Suspended but the controller has not
	// terminated the pod yet: Suspend must wait for the condition instead of
	// returning success early.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background()) }()

	assertNotCompleted(t, errCh, "Suspend")

	simulateController(cs, cr.Name, func(cr *sandboxv1beta1.Sandbox) {
		cr.Status.Conditions = []metav1.Condition{suspendedCond(cr.Generation)}
	}, controllerStop(t))

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Suspend: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not return after the controller observed the suspension")
	}
	if hasPatchAction(cs) {
		t.Error("Suspend patched an already suspended sandbox; want no patch")
	}
}

func TestSandbox_Suspend_Timeout(t *testing.T) {
	// No Suspended condition and no controller to add one: the wait must
	// give up within the timeout instead of blocking forever.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, _ := newSuspendTestSandbox(t, cr, true)

	err := sb.Suspend(context.Background(), WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Suspend error = %v, want ErrTimeout", err)
	}
}

func TestSandbox_Disconnect_InterruptsSuspend(t *testing.T) {
	// A suspend wait holds the lifecycle semaphore for up to its timeout.
	// Disconnect must be able to interrupt it through the cancel hook the
	// SDK registers, the same way it interrupts a stuck Open.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background(), WithTimeout(30*time.Second)) }()

	waitForPatchAction(t, cs)

	start := time.Now()
	if err := sb.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect during Suspend: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Suspend returned nil after being interrupted; want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect did not interrupt the suspend wait")
	}
	// Without the cancel hook the wait would run to its 30s timeout; the
	// interrupt must take effect immediately.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("interrupt took %v; Disconnect did not cancel the suspend wait", elapsed)
	}
}

func TestSandbox_Suspend_CanceledContextIsNotErrTimeout(t *testing.T) {
	// A caller cancellation (or a Disconnect interrupt) must surface
	// context.Canceled, not ErrTimeout: the wait did not run out of time.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(ctx, WithTimeout(30*time.Second)) }()

	waitForPatchAction(t, cs)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Suspend error = %v, want it to wrap context.Canceled", err)
		}
		if errors.Is(err, ErrTimeout) {
			t.Errorf("Suspend reported ErrTimeout for a cancelled context: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not return after cancellation")
	}
}

func TestSandbox_Resume_WaitsForControllerObservation(t *testing.T) {
	// A Ready=True left over from before the suspension (older observed
	// generation, e.g. an interrupted Suspend that never killed the pod
	// from the SDK's point of view) must not satisfy the readiness wait.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended, readyCond(seedGeneration-1))
	sb, cs := newSuspendTestSandbox(t, cr, false)
	sb.setSuspendedLocally(true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Resume(context.Background()) }()

	waitForPatchAction(t, cs)
	assertNotCompleted(t, errCh, "Resume")

	simulateController(cs, cr.Name, func(cr *sandboxv1beta1.Sandbox) {
		cr.Status.Conditions = []metav1.Condition{readyCond(cr.Generation)}
		cr.Status.PodIPs = []string{"10.0.0.5"}
		cr.Status.ServiceFQDN = "sbx-1.default.svc.cluster.local"
	}, controllerStop(t))

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not return after the sandbox became ready")
	}

	if got := getCR(t, cs, "sbx-1").Spec.OperatingMode; got != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Errorf("operatingMode = %q, want Running", got)
	}
	if !sb.IsReady() {
		t.Error("transport is not connected after Resume")
	}
	if sb.PodIP() != "10.0.0.5" {
		t.Errorf("PodIP = %q, want 10.0.0.5", sb.PodIP())
	}
	if sb.suspendedLocally() {
		t.Error("handle still tracks the suspension after Resume")
	}
}

func TestSandbox_Resume_Running_Connected_NoOp(t *testing.T) {
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	sb, cs := newSuspendTestSandbox(t, cr, true)

	if err := sb.Resume(context.Background()); err != nil {
		t.Fatalf("Resume on connected running sandbox: %v", err)
	}
	if hasPatchAction(cs) {
		t.Error("Resume patched a running sandbox; want no patch")
	}
	if !sb.IsReady() {
		t.Error("Resume dropped the transport on a no-op")
	}
}

func TestSandbox_Resume_Running_Disconnected_Reconnects(t *testing.T) {
	// The sandbox is already running but this handle lost its transport:
	// Resume must reconnect instead of returning nil and leaving a dead
	// handle.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	cr.Status.PodIPs = []string{"10.0.0.5"}
	sb, cs := newSuspendTestSandbox(t, cr, false)

	if err := sb.Resume(context.Background()); err != nil {
		t.Fatalf("Resume on disconnected running sandbox: %v", err)
	}
	if hasPatchAction(cs) {
		t.Error("Resume patched an already running sandbox; want no patch")
	}
	if !sb.IsReady() {
		t.Error("transport is not connected after Resume")
	}
	if sb.PodIP() != "10.0.0.5" {
		t.Errorf("PodIP = %q, want 10.0.0.5", sb.PodIP())
	}
}

func TestSandbox_Resume_Timeout(t *testing.T) {
	// Suspended with no Ready condition and no controller to add one: the
	// wait must give up within the timeout instead of blocking forever.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, _ := newSuspendTestSandbox(t, cr, false)

	err := sb.Resume(context.Background(), WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Resume error = %v, want ErrTimeout", err)
	}
}

func TestSandbox_SuspendResume_DeletedSandbox_ReturnsErrSandboxDeleted(t *testing.T) {
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)
	if err := cs.Tracker().Delete(sandboxGVR, "default", "sbx-1"); err != nil {
		t.Fatalf("delete seeded CR: %v", err)
	}

	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Suspend error = %v, want ErrSandboxDeleted", err)
	}
	if err := sb.Resume(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Resume error = %v, want ErrSandboxDeleted", err)
	}
	if _, err := sb.IsSuspended(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("IsSuspended error = %v, want ErrSandboxDeleted", err)
	}
}

func TestSandbox_SuspendResume_ReplacedSandbox_ReturnsErrSandboxDeleted(t *testing.T) {
	// The original sandbox was deleted and another took its name: mode
	// changes must not touch the replacement.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	sb, cs := newSuspendTestSandbox(t, cr, true)
	sb.sandboxUID = types.UID("uid-of-the-original")

	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Suspend error = %v, want ErrSandboxDeleted", err)
	}
	if err := sb.Resume(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Resume error = %v, want ErrSandboxDeleted", err)
	}
	if _, err := sb.IsSuspended(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("IsSuspended error = %v, want ErrSandboxDeleted", err)
	}
	if hasPatchAction(cs) {
		t.Error("mode change issued against a same-name replacement sandbox")
	}
}

func TestSandbox_Suspend_ReplacedDuringWait_ReturnsErrSandboxDeleted(t *testing.T) {
	// The original sandbox is deleted and a same-name replacement takes
	// over while the suspend wait runs. The replacement already reports
	// Suspended, but the wait must reject it on the UID instead of
	// reporting success for a sandbox this handle never opened.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	replacement := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended, suspendedCond(seedGeneration))
	replacement.UID = "uid-replacement"
	cs.PrependReactor("list", "sandboxes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &sandboxv1beta1.SandboxList{Items: []sandboxv1beta1.Sandbox{*replacement}}, nil
	})
	fw := watch.NewRaceFreeFake()
	defer fw.Stop()
	cs.PrependWatchReactor("sandboxes", func(ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	err := sb.Suspend(context.Background(), WithTimeout(5*time.Second))
	if !errors.Is(err, ErrSandboxDeleted) {
		t.Fatalf("Suspend error = %v, want ErrSandboxDeleted", err)
	}
}

func TestSandbox_Resume_ReplacedDuringWatch_ReturnsErrSandboxDeleted(t *testing.T) {
	// A same-name replacement shows up on the watch during the resume
	// wait, already Ready: the wait must fail instead of letting the
	// handle adopt the wrong sandbox.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, cs := newSuspendTestSandbox(t, cr, false)

	replacement := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	replacement.UID = "uid-replacement"
	replacement.Status.PodIPs = []string{"10.0.0.9"}

	fw := watch.NewRaceFreeFake()
	defer fw.Stop()
	cs.PrependWatchReactor("sandboxes", func(ktesting.Action) (bool, watch.Interface, error) {
		fw.Modify(replacement)
		return true, fw, nil
	})

	err := sb.Resume(context.Background(), WithTimeout(5*time.Second))
	if !errors.Is(err, ErrSandboxDeleted) {
		t.Fatalf("Resume error = %v, want ErrSandboxDeleted", err)
	}
}

func TestSandbox_Resume_Running_PodChanged_Reconnects(t *testing.T) {
	// Another client suspended and resumed the sandbox: the CR is running
	// and Ready at its current generation again, but a new pod backs it.
	// The still-connected transport points at the dead pod and must be
	// rebuilt instead of trusted.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	cr.Status.PodIPs = []string{"10.0.0.9"}
	sb, cs := newSuspendTestSandbox(t, cr, true)
	sb.podIP = "10.0.0.5" // the IP the live transport was built for

	if err := sb.Resume(context.Background()); err != nil {
		t.Fatalf("Resume on a running sandbox whose pod changed: %v", err)
	}
	if hasPatchAction(cs) {
		t.Error("Resume patched an already running sandbox; want no patch")
	}
	if sb.PodIP() != "10.0.0.9" {
		t.Errorf("PodIP = %q, want the replacement pod's 10.0.0.9", sb.PodIP())
	}
	if !sb.IsReady() {
		t.Error("transport is not connected after Resume rebuilt it")
	}
}

func TestSandbox_Resume_CanceledContextIsNotErrTimeout(t *testing.T) {
	// A caller cancellation (or a Disconnect interrupt) during the
	// readiness wait must surface context.Canceled, not ErrTimeout: the
	// wait did not run out of time.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, cs := newSuspendTestSandbox(t, cr, false)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sb.Resume(ctx, WithTimeout(30*time.Second)) }()

	waitForPatchAction(t, cs)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Resume error = %v, want it to wrap context.Canceled", err)
		}
		if errors.Is(err, ErrTimeout) {
			t.Errorf("Resume reported ErrTimeout for a cancelled context: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not return after cancellation")
	}
}

func TestSandbox_Resume_PatchClosesStaleTransport(t *testing.T) {
	// The handle was still connected when another client suspended the
	// sandbox. Once Resume patches the CR back to Running, the old
	// transport points at a dead pod and must go before the wait: a
	// readiness wait that then times out must not leave a handle
	// reporting itself ready.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, _ := newSuspendTestSandbox(t, cr, true)

	err := sb.Resume(context.Background(), WithTimeout(300*time.Millisecond))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Resume error = %v, want ErrTimeout", err)
	}
	if sb.IsReady() {
		t.Error("stale transport survived the resume patch; the handle reports itself ready against a dead pod")
	}
}

func TestSandbox_Close_ClearsSuspendedFlag(t *testing.T) {
	// A closed handle must not keep advertising itself as suspended: the
	// managed Client keeps suspended handles registered, so GetSandbox
	// would hand out a dead handle with no claim behind it.
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	setupWatchWithReactor(agentsCS, extensionsCS, readySandbox("sb"))

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	c.setSuspendedLocally(true)
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.suspendedLocally() {
		t.Error("handle is still tracked as suspended after Close")
	}
}

func TestOpen_Reconnect_ClearsSuspendedFlag(t *testing.T) {
	// The handle suspended the sandbox, the CR was resumed outside the
	// SDK, and Open re-attached the transport. A stale suspension flag
	// would make Client.GetSandbox hand out this handle after a later
	// Disconnect instead of re-attaching it.
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	sb := readySandbox("sb")
	setupWatchWithReactor(agentsCS, extensionsCS, sb)
	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, sb, nil
	})

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Disconnect(ctx); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	c.setSuspendedLocally(true)

	if err := c.Open(ctx); err != nil {
		t.Fatalf("reconnect Open: %v", err)
	}
	if c.suspendedLocally() {
		t.Error("handle is still tracked as suspended after a successful reconnect")
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSandbox_SuspendResume_RequireSandboxName(t *testing.T) {
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, _ := newSuspendTestSandbox(t, cr, false)
	sb.sandboxName = ""

	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("Suspend error = %v, want ErrNotReady", err)
	}
	if err := sb.Resume(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("Resume error = %v, want ErrNotReady", err)
	}
	if _, err := sb.IsSuspended(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("IsSuspended error = %v, want ErrNotReady", err)
	}
}

func TestClient_KeepsSuspendedHandles(t *testing.T) {
	// A suspended handle has no transport, so IsReady is false. The managed
	// Client must not treat it as stale: evicting it makes GetSandbox try to
	// re-Open a sandbox whose pod is gone, and pruning it makes DeleteAll
	// miss the claim.
	c, _ := newTestClient(t)
	key := Key{Namespace: "default", ClaimName: "suspended-claim"}

	sb := &Sandbox{log: logr.Discard(), opts: c.opts}
	sb.connector = &connector{}
	sb.claimName = key.ClaimName
	sb.sandboxName = "sbx-1"
	sb.setSuspendedLocally(true)
	c.mu.Lock()
	c.registry[key] = sb
	c.mu.Unlock()

	got, err := c.GetSandbox(context.Background(), key.ClaimName, key.Namespace)
	if err != nil {
		t.Fatalf("GetSandbox on a suspended handle: %v", err)
	}
	if got != sb {
		t.Error("GetSandbox did not return the tracked suspended handle")
	}

	keys := c.ListActiveSandboxes()
	if len(keys) != 1 || keys[0] != key {
		t.Errorf("ListActiveSandboxes = %v, want [%v] (suspended handle pruned)", keys, key)
	}
}

func TestSandbox_Suspend_TimeoutBoundsSemaphoreWait(t *testing.T) {
	// WithTimeout documents a total operation timeout, so it must also
	// bound the wait for the lifecycle semaphore, not just the condition
	// wait that starts after acquisition.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, _ := newSuspendTestSandbox(t, cr, true)

	sb.lifecycleSem <- struct{}{} // another lifecycle operation holds the lock
	defer func() { <-sb.lifecycleSem }()

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background(), WithTimeout(200*time.Millisecond)) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Suspend succeeded while the lifecycle lock was held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithTimeout did not bound the lifecycle semaphore wait")
	}
}

// waitForLifecycleCancel blocks until a lifecycle operation has registered
// its cancel hook, so a test can time an interrupt against an operation
// parked on the semaphore instead of racing a sleep.
func waitForLifecycleCancel(t *testing.T, sb *Sandbox) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sb.mu.Lock()
		registered := len(sb.lifecycleCancels) > 0
		sb.mu.Unlock()
		if registered {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("lifecycle operation did not register its cancel hook")
}

func TestSandbox_Disconnect_InterruptsSuspendInAcquisitionGap(t *testing.T) {
	// An interrupt that lands while Suspend is parked on the lifecycle
	// semaphore must not be lost. Registration is confirmed before the
	// interrupt runs, and the semaphore stays held until Suspend aborts,
	// so the test cannot pass by cancelling an already-active operation.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, _ := newSuspendTestSandbox(t, cr, true)

	sb.lifecycleSem <- struct{}{} // hold the lock so Suspend parks in acquisition
	defer func() {
		// Release on assertion failures too, so the parked goroutine can
		// finish instead of outliving the test.
		select {
		case <-sb.lifecycleSem:
		default:
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background(), WithTimeout(5*time.Second)) }()
	waitForLifecycleCancel(t, sb)

	discCh := make(chan error, 1)
	go func() { discCh <- sb.Disconnect(context.Background()) }()

	// The semaphore is still held here, so the only way Suspend can return
	// is by observing the interrupt.
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Suspend succeeded despite the interrupt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Suspend ignored an interrupt that arrived during semaphore acquisition")
	}

	<-sb.lifecycleSem // let Disconnect finish
	if err := <-discCh; err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

func TestSandbox_Suspend_ExternalModeFlipIsNotSuccess(t *testing.T) {
	// If another client flips operatingMode back to Running while the wait
	// is in flight, a Suspended=True condition stamped at the current
	// generation describes a suspension that is no longer wanted. The wait
	// must not report success.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background(), WithTimeout(time.Second)) }()

	waitForPatchAction(t, cs)
	simulateController(cs, cr.Name, func(cr *sandboxv1beta1.Sandbox) {
		cr.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
		cr.Status.Conditions = []metav1.Condition{suspendedCond(cr.Generation)}
	}, controllerStop(t))

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Suspend error = %v, want ErrTimeout after an external mode flip", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not return")
	}
}

func TestSandbox_Resume_ExternalModeFlipIsNotSuccess(t *testing.T) {
	// Symmetric case: another client re-suspends while Resume waits, and a
	// stale-but-current-generation Ready=True must not satisfy the wait.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, cs := newSuspendTestSandbox(t, cr, false)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Resume(context.Background(), WithTimeout(time.Second)) }()

	waitForPatchAction(t, cs)
	simulateController(cs, cr.Name, func(cr *sandboxv1beta1.Sandbox) {
		cr.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
		cr.Status.Conditions = []metav1.Condition{readyCond(cr.Generation)}
		cr.Status.PodIPs = []string{"10.0.0.5"}
	}, controllerStop(t))

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Resume error = %v, want ErrTimeout after an external mode flip", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not return")
	}
}

func TestPatchSandboxOperatingMode_UIDTestOp(t *testing.T) {
	// The patch itself must carry the identity check (JSON Patch test op on
	// metadata.uid) so a same-name replacement cannot be modified in the
	// window between the SDK's read and write.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	agentsCS := fakeagents.NewSimpleClientset() //nolint:staticcheck // TODO: regenerate clientsets with --with-applyconfig
	if err := agentsCS.Tracker().Create(sandboxGVR, cr, cr.Namespace); err != nil {
		t.Fatalf("seed sandbox CR: %v", err)
	}
	h := &K8sHelper{AgentsClient: agentsCS.AgentsV1beta1(), Log: logr.Discard()}
	tracer := noop.NewTracerProvider().Tracer("test")
	ctx := context.Background()

	if _, err := h.patchSandboxOperatingMode(ctx, "sbx-1", "default",
		sandboxv1beta1.SandboxOperatingModeSuspended, types.UID("someone-else"), tracer, "test"); err == nil {
		t.Fatal("patch carrying a foreign UID test op succeeded")
	}
	if got := getCR(t, agentsCS, "sbx-1").Spec.OperatingMode; got != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Errorf("operatingMode = %q after the rejected patch, want Running (replacement must stay untouched)", got)
	}

	patched, err := h.patchSandboxOperatingMode(ctx, "sbx-1", "default",
		sandboxv1beta1.SandboxOperatingModeSuspended, cr.UID, tracer, "test")
	if err != nil {
		t.Fatalf("patch with the opened UID: %v", err)
	}
	if patched.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Errorf("operatingMode = %q, want Suspended", patched.Spec.OperatingMode)
	}

	if _, err := h.patchSandboxOperatingMode(ctx, "sbx-1", "default",
		sandboxv1beta1.SandboxOperatingModeRunning, "", tracer, "test"); err != nil {
		t.Fatalf("patch without a recorded UID (unguarded): %v", err)
	}
}

func TestSandbox_IsSuspended(t *testing.T) {
	suspended := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, _ := newSuspendTestSandbox(t, suspended, false)
	got, err := sb.IsSuspended(context.Background())
	if err != nil {
		t.Fatalf("IsSuspended: %v", err)
	}
	if !got {
		t.Error("IsSuspended = false, want true")
	}

	running := newOperatingModeCR("sbx-2", sandboxv1beta1.SandboxOperatingModeRunning)
	sb2, _ := newSuspendTestSandbox(t, running, false)
	got, err = sb2.IsSuspended(context.Background())
	if err != nil {
		t.Fatalf("IsSuspended: %v", err)
	}
	if got {
		t.Error("IsSuspended = true, want false")
	}
}

func TestSandbox_SuspendResume_TerminatingSandbox_ReturnsErrSandboxDeleted(t *testing.T) {
	// Kubernetes retains objects during finalization with their conditions
	// still looking alive. A UID-matching sandbox that started terminating
	// must not be patched, waited on, or reported: the pod is going away
	// regardless of what operatingMode says.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning, readyCond(seedGeneration))
	now := metav1.Now()
	cr.DeletionTimestamp = &now
	sb, cs := newSuspendTestSandbox(t, cr, true)

	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Suspend error = %v, want ErrSandboxDeleted", err)
	}
	if err := sb.Resume(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("Resume error = %v, want ErrSandboxDeleted", err)
	}
	if _, err := sb.IsSuspended(context.Background()); !errors.Is(err, ErrSandboxDeleted) {
		t.Errorf("IsSuspended error = %v, want ErrSandboxDeleted", err)
	}
	if hasPatchAction(cs) {
		t.Error("mode change issued against a terminating sandbox")
	}
}

func TestSandbox_Resume_TerminatingDuringWait_ReturnsErrSandboxDeleted(t *testing.T) {
	// The sandbox starts terminating while the resume wait runs, and the
	// replacement state still satisfies the Ready predicate: the wait must
	// reject it instead of reporting success on a doomed sandbox.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeSuspended)
	sb, cs := newSuspendTestSandbox(t, cr, false)

	terminating := cr.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	terminating.Status.Conditions = []metav1.Condition{readyCond(seedGeneration)}

	cs.PrependReactor("list", "sandboxes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &sandboxv1beta1.SandboxList{Items: []sandboxv1beta1.Sandbox{*terminating}}, nil
	})
	fw := watch.NewRaceFreeFake()
	defer fw.Stop()
	cs.PrependWatchReactor("sandboxes", func(ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	err := sb.Resume(context.Background(), WithTimeout(5*time.Second))
	if !errors.Is(err, ErrSandboxDeleted) {
		t.Fatalf("Resume error = %v, want ErrSandboxDeleted", err)
	}
}

func TestSandbox_Suspend_TrackedDuringWaitAndAfterTimeout(t *testing.T) {
	// Suspend tears the transport down before the condition wait, so the
	// handle reports IsReady() false while the wait runs. The managed
	// Client evicts handles in that state unless the suspension flag is
	// already set, and eviction makes DeleteAll miss the claim — including
	// when the wait ends in a timeout.
	cr := newOperatingModeCR("sbx-1", sandboxv1beta1.SandboxOperatingModeRunning)
	sb, cs := newSuspendTestSandbox(t, cr, true)

	errCh := make(chan error, 1)
	go func() { errCh <- sb.Suspend(context.Background(), WithTimeout(2*time.Second)) }()

	waitForPatchAction(t, cs)
	deadline := time.Now().Add(2 * time.Second)
	for !sb.suspendedLocally() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !sb.suspendedLocally() {
		t.Error("handle is not tracked as suspended while the suspend wait runs")
	}

	// No controller will ever set the Suspended condition: the wait times
	// out, and the handle must stay tracked afterwards.
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Suspend error = %v, want ErrTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend did not time out")
	}
	if !sb.suspendedLocally() {
		t.Error("handle lost its suspension tracking after a timed-out Suspend")
	}
}

func TestOpen_Reconnect_ReplacedSandbox_ReturnsErrSandboxDeleted(t *testing.T) {
	// A same-name replacement appears while the handle is disconnected.
	// reconnect must not adopt it: setState would overwrite the recorded
	// UID and let a later Suspend/Resume operate on the replacement.
	opts := defaultTestOpts()
	c, agentsCS, extensionsCS := newTestSandbox(opts)
	sb := readySandbox("sb")
	sb.UID = "uid-original"
	setupWatchWithReactor(agentsCS, extensionsCS, sb)

	current := sb
	agentsCS.PrependReactor("get", "sandboxes", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, current, nil
	})

	ctx := context.Background()
	if err := c.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Disconnect(ctx); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	replacement := readySandbox("sb")
	replacement.UID = "uid-replacement"
	current = replacement

	if err := c.Open(ctx); !errors.Is(err, ErrSandboxDeleted) {
		t.Fatalf("reconnect Open error = %v, want ErrSandboxDeleted", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
