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

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// AtomicTimeDuration is a wrapper around time.Duration that allows for concurrent updates and retrievals.
type AtomicTimeDuration struct {
	v uint64
}

// Seconds returns the duration in seconds as a float64.
func (s *AtomicTimeDuration) Seconds() float64 {
	v := atomic.LoadUint64(&s.v)
	d := time.Duration(v)
	return d.Seconds()
}

// IsEmpty returns true if the duration is zero.
func (s *AtomicTimeDuration) IsEmpty() bool {
	return atomic.LoadUint64(&s.v) == 0
}

// Set sets the duration to the given value.
func (s *AtomicTimeDuration) Set(d time.Duration) {
	atomic.StoreUint64(&s.v, uint64(d))
}

// String returns the duration as a string.
func (s *AtomicTimeDuration) String() string {
	v := atomic.LoadUint64(&s.v)
	d := time.Duration(v)
	return d.String()
}

// ChromeSandboxMetrics holds timing measurements for the chrome sandbox startup.
type ChromeSandboxMetrics struct {
	SandboxReady AtomicTimeDuration // Time for sandbox to become ready
	PodCreated   AtomicTimeDuration // Time for pod to be created
	PodScheduled AtomicTimeDuration // Time for pod to be scheduled
	PodRunning   AtomicTimeDuration // Time for pod to become running
	PodReady     AtomicTimeDuration // Time for pod to become ready
	ChromeReady  AtomicTimeDuration // Time for chrome to respond on debug port
	Total        AtomicTimeDuration // Total time from start to chrome ready
}

func chromeSandbox(namespace string) *sandboxv1alpha1.Sandbox {
	sandbox := &sandboxv1alpha1.Sandbox{}
	sandbox.Name = "chrome-sandbox"
	sandbox.Namespace = namespace
	sandbox.Spec.PodTemplate = sandboxv1alpha1.PodTemplate{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "chrome-sandbox",
					// might be nice to remove the IMAGE_TAG env var so this is easier to run from IDE
					Image:           fmt.Sprintf("kind.local/chrome-sandbox:%s", os.Getenv("IMAGE_TAG")),
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}
	return sandbox
}

// TestRunChromeSandbox tests that we can run Chrome inside a Sandbox,
// it also measures how long it takes for Chrome to start serving the CDP protocol.
func TestRunChromeSandbox(t *testing.T) {
	metrics := runChromeSandbox(framework.NewTestContext(t))
	t.Logf("Metrics: %+v", metrics)
}

// BenchmarkChromeSandboxStartup measures the time for Chrome to start in a sandbox.
// Run with: go test -bench=BenchmarkChromeSandboxStartup -benchtime=1x ./test/e2e/...
// Compare results with: benchstat old.txt new.txt
func BenchmarkChromeSandboxStartup(b *testing.B) {
	time.Sleep(2 * time.Second) // Give cluster a moment to settle, and to help us split the logs by time

	for b.Loop() {
		metrics := runChromeSandbox(framework.NewTestContext(b))
		// Report custom metrics in addition to the default ns/op
		b.ReportMetric(metrics.SandboxReady.Seconds(), "sandbox-ready-sec")
		b.ReportMetric(metrics.PodCreated.Seconds(), "pod-created-sec")
		b.ReportMetric(metrics.PodScheduled.Seconds(), "pod-scheduled-sec")
		b.ReportMetric(metrics.PodRunning.Seconds(), "pod-running-sec")
		b.ReportMetric(metrics.PodReady.Seconds(), "pod-ready-sec")
		b.ReportMetric(metrics.ChromeReady.Seconds(), "chrome-ready-sec")
	}
}

// runChromeSandbox runs the chrome sandbox test and returns timing metrics.
func runChromeSandbox(t *framework.TestContext) *ChromeSandboxMetrics {
	ctx := t.Context()

	// Set up a namespace with unique name to avoid conflicts
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("chrome-sandbox-test-%d", time.Now().UnixNano())
	t.MustCreateWithCleanup(ns)

	metrics := &ChromeSandboxMetrics{}
	startTime := time.Now()

	go func() {
		var lastValue *corev1.Pod

		gvr := corev1.SchemeGroupVersion.WithResource("pods")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *corev1.Pod) (bool, error) {
			t.Logf("Pod event %s %s/%s", event.Type, obj.Namespace, obj.Name)

			if lastValue != nil {
				diff := cmp.Diff(lastValue, obj)
				t.Logf("Pod diff: %s", diff)
			}
			lastValue = obj.DeepCopy()

			if metrics.PodCreated.IsEmpty() {
				metrics.PodCreated.Set(time.Since(startTime))
			}

			if metrics.PodRunning.IsEmpty() {
				if obj.Status.Phase == corev1.PodRunning {
					metrics.PodRunning.Set(time.Since(startTime))
				}
			}

			if metrics.PodScheduled.IsEmpty() {
				if obj.Spec.NodeName != "" {
					metrics.PodScheduled.Set(time.Since(startTime))
				}
			}
			return false, nil
		})
	}()

	go func() {
		var lastValue *sandboxv1alpha1.Sandbox

		gvr := sandboxv1alpha1.GroupVersion.WithResource("sandboxes")
		watchFilter := framework.WatchFilter{
			Namespace: ns.Name,
		}

		framework.MustWatch(t.ClusterClient, gvr, watchFilter, func(event watch.Event, obj *sandboxv1alpha1.Sandbox) (bool, error) {
			t.Logf("Sandbox event %s %s/%s", event.Type, obj.Namespace, obj.Name)

			if lastValue != nil {
				diff := cmp.Diff(lastValue, obj)
				t.Logf("Sandbox diff: %s", diff)
			}
			lastValue = obj.DeepCopy()

			return false, nil
		})
	}()

	sandboxObj := chromeSandbox(ns.Name)
	t.MustCreateWithCleanup(sandboxObj)

	t.MustWaitForObject(sandboxObj, predicates.ReadyConditionIsTrue)
	metrics.SandboxReady.Set(time.Since(startTime))

	podID := types.NamespacedName{
		Namespace: ns.Name,
		Name:      "chrome-sandbox",
	}
	podObj := &corev1.Pod{}
	podObj.Name = podID.Name
	podObj.Namespace = podID.Namespace

	t.MustWaitForObject(podObj, predicates.ReadyConditionIsTrue)
	metrics.PodReady.Set(time.Since(startTime))

	if err := waitForChromeReady(t, podID); err != nil {
		t.Fatalf("failed to wait for chrome ready: %v", err)
	}
	metrics.ChromeReady.Set(time.Since(startTime))
	metrics.Total.Set(time.Since(startTime))

	// Gather kubelet/containerd logs to understand timing between scheduling and running
	logOptions := framework.NodeLogOptions{}
	logOptions.ArtifactsDir = t.ArtifactsDir()
	logOptions.Since = startTime

	// Get latest pod object to get node name
	var latestPod corev1.Pod
	if err := t.ClusterClient.Get(ctx, types.NamespacedName{Namespace: podObj.Namespace, Name: podObj.Name}, &latestPod); err != nil {
		t.Fatalf("failed to get latest pod object: %v", err)
	}
	nodeName := latestPod.Spec.NodeName
	if nodeName == "" {
		t.Fatalf("pod not scheduled to a node, cannot get node logs")
	}
	t.MustGetNodeLogs(nodeName, logOptions)

	return metrics
}

func waitForChromeReady(tc *framework.TestContext, podID types.NamespacedName) error {
	tc.Helper()

	ctx := tc.Context()

	// Loop until we can query chrome for its version via the debug port
	pollDuration := 100 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled")
		default:
			// We have to port-forward in the loop because port-forward exits when it sees an error
			// https://github.com/kubernetes/kubectl/issues/1249
			portForwardCtx, portForwardCancel := context.WithCancel(ctx)
			if err := tc.PortForward(portForwardCtx, podID, 9222, 9222); err != nil {
				tc.Logf("failed to port forward: %s", err)
				portForwardCancel()
				time.Sleep(pollDuration)
				continue
			}

			u := "http://localhost:9222/json/version"
			info, err := getChromeInfo(ctx, u)
			portForwardCancel()
			if err != nil {
				tc.Logf("failed to get chrome info: %s", err)
				time.Sleep(pollDuration)
				continue
			}
			tc.Logf("Chrome is ready (%s). Response: %s", u, info)
			return nil
		}
	}
}

// getChromeInfo connects to the Chrome Debug Port and retrieves the version information.
// This is used to verify that Chrome is running inside the sandbox.
func getChromeInfo(ctx context.Context, u string) (string, error) {
	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Send the HTTP request
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error sending HTTP request to Chrome Debug Port: %w", err)
	}
	defer response.Body.Close()

	// Check for HTTP 200 OK
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("non-200 response from Chrome Debug Port: %d", response.StatusCode)
	}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body from Chrome Debug Port: %w", err)
	}

	return string(b), nil
}
