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

package extensions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/clients/go/sandbox"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
)

// sandboxdTemplate returns a SandboxTemplate that runs sandboxd as the sole
// container, matching sandboxdManifest and the TypeScript SDK e2e fixture.
func sandboxdTemplate(ns, name string) *extensionsv1beta1.SandboxTemplate {
	template := &extensionsv1beta1.SandboxTemplate{}
	template.Name = name
	template.Namespace = ns
	template.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true),
				FSGroup:      new(int64(1000)),
			},
			Containers: []corev1.Container{{
				Name:            "sandboxd",
				Image:           fmt.Sprintf("%ssandboxd:%s", getImagePrefix(), getImageTag()),
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                new(int64(1000)),
					RunAsGroup:               new(int64(1000)),
					RunAsNonRoot:             new(true),
					AllowPrivilegeEscalation: new(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 9090}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/v1/health", Port: intstr.FromInt32(8080)},
					},
					PeriodSeconds:    1,
					FailureThreshold: 30,
					TimeoutSeconds:   2,
				},
			}},
		},
	}
	return template
}

// TestGoSDKSandboxdPortForward covers only what needs a real cluster: the
// Pod tunnel. Per-feature sandboxd behavior is covered by sandboxd's Go tests
// and the SDK unit tests.
func TestGoSDKSandboxdPortForward(t *testing.T) {
	tc := framework.NewTestContext(t)

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("go-sdk-sandboxd-%d", time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := sandboxdTemplate(ns.Name, "go-sdk-sandboxd-template")
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	warmPool := &extensionsv1beta1.SandboxWarmPool{}
	warmPool.Name = "go-sdk-sandboxd-warmpool"
	warmPool.Namespace = ns.Name
	warmPool.Spec.TemplateRef.Name = template.Name
	warmPool.Spec.Replicas = new(int32(1))
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))
	require.NoError(t, tc.WaitForWarmPoolReady(t.Context(), types.NamespacedName{Namespace: ns.Name, Name: warmPool.Name}))

	restConfig, err := clientcmd.BuildConfigFromFlags("", framework.GetKubeconfig())
	require.NoError(t, err)

	// RuntimeSandboxd must be explicit: the default legacy runtime would
	// port-forward to the sandbox-router instead of the sandbox pod.
	newClient := func() *sandbox.Client {
		c, err := sandbox.NewClient(t.Context(), sandbox.Options{
			RestConfig:   restConfig,
			Runtime:      sandbox.RuntimeSandboxd,
			Connectivity: sandbox.ConnectivityPortForward,
		})
		require.NoError(t, err)
		// Safety net if an assertion fails before the explicit Close below.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			c.DeleteAll(ctx)
		})
		return c
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	sb, err := newClient().CreateSandbox(ctx, warmPool.Name, ns.Name)
	require.NoError(t, err)
	claimName := sb.ClaimName()
	_, err = sb.Health(ctx)
	require.NoError(t, err)

	// REST (files) and gRPC (commands) reach the same Pod.
	require.NoError(t, sb.Write(ctx, "rest.txt", []byte("via REST")))
	res, err := sb.Run(ctx, "cat rest.txt; printf 'via gRPC' > grpc.txt")
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, "via REST", res.Stdout)
	got, err := sb.Read(ctx, "grpc.txt")
	require.NoError(t, err)
	require.Equal(t, "via gRPC", string(got))

	// A multi-megabyte stream survives the tunnel in both directions.
	data := make([]byte, 4<<20)
	_, err = rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, sb.WriteReader(ctx, "big.bin", bytes.NewReader(data)))
	var buf bytes.Buffer
	_, err = sb.ReadTo(ctx, "big.bin", &buf)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(data), sha256.Sum256(buf.Bytes()))

	// A fresh client re-attaches to the live claim through a new tunnel.
	require.NoError(t, sb.Disconnect(ctx))
	sb, err = newClient().GetSandbox(ctx, claimName, ns.Name)
	require.NoError(t, err)
	got, err = sb.Read(ctx, "rest.txt")
	require.NoError(t, err)
	require.Equal(t, "via REST", string(got))

	// Close deletes the SandboxClaim.
	require.NoError(t, sb.Close(ctx))
	require.Eventually(t, func() bool {
		err := tc.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: claimName}, &extensionsv1beta1.SandboxClaim{})
		return k8serrors.IsNotFound(err)
	}, time.Minute, defaultPollingInterval, "SandboxClaim %s should be deleted after Close", claimName)
}
