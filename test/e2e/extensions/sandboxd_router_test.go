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
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework/predicates"
)

// TestRunSandboxdViaGoRouter forwards only the router port. The second hop
// must resolve the controller-created Sandbox Service on the pod network.
// AllowAll and plaintext are confined to this isolated, non-public fixture;
// the public TLS example uses scoped tokens instead.
func TestRunSandboxdViaGoRouter(t *testing.T) {
	tc := framework.NewTestContext(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sandboxd-router-%d", time.Now().UnixNano())}}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))
	sb, err := sandboxFromManifest(fmt.Sprintf(sandboxdManifest, getImagePrefix(), getImageTag()))
	require.NoError(t, err)
	sb.Namespace = ns.Name
	sb.Spec.Service = new(true)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), sb))
	tc.MustWaitForObject(sb, predicates.ReadyConditionIsTrue)
	router := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-router", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: new(false),
			Containers: []corev1.Container{{
				Name: "router", Image: fmt.Sprintf("%ssandbox-router-go:%s", getImagePrefix(), getImageTag()),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"--cache-enabled=false", "--authz-mode=allow-all", "--grpc-proxy-timeout=0s"},
				SecurityContext: &corev1.SecurityContext{RunAsNonRoot: new(true), AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(8081)}}},
			}},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), router))
	tc.MustWaitForObject(router, predicates.ReadyConditionIsTrue)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	pfCtx, stop := context.WithCancel(t.Context())
	defer stop()
	require.NoError(t, tc.PortForward(pfCtx, types.NamespacedName{Namespace: ns.Name, Name: router.Name}, port, 8080))
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	require.NoError(t, err)
	defer conn.Close()
	client := processv1.NewProcessServiceClient(conn)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-sandbox-id", sb.Name, "x-sandbox-namespace", ns.Name, "x-sandbox-port", "9090"))
	resp, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "printf hello; printf warning >&2; exit 7"}}})
	require.NoError(t, err)
	require.Equal(t, "hello", string(resp.GetStdout()))
	require.Equal(t, "warning", string(resp.GetStderr()))
	require.EqualValues(t, 7, resp.GetExitCode())
	stream, err := client.Start(ctx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "printf ready; read line; printf 'input:%s' \"$line\""}}})
	require.NoError(t, err)
	init, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, init.GetInit())
	pid := init.GetInit().GetProcessId()
	var output strings.Builder
	for !strings.Contains(output.String(), "ready") {
		event, err := stream.Recv()
		require.NoError(t, err)
		output.Write(event.GetStdout())
	}
	_, err = client.WriteStdin(ctx, &processv1.WriteStdinRequest{ProcessId: pid, Payload: &processv1.WriteStdinRequest_Input{Input: []byte("router\n")}})
	require.NoError(t, err)
	exited := false
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		output.Write(event.GetStdout())
		if event.GetExit() != nil {
			exited = true
			require.Zero(t, event.GetExit().GetExitCode())
		}
	}
	require.True(t, exited)
	require.Equal(t, "readyinput:router", output.String())
	_, err = client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: pid, Signal: processv1.Signal_SIGNAL_SIGTERM})
	require.Equal(t, codes.NotFound, status.Code(err))

	// Cancel one live Start, then prove another RPC on this connection works.
	startCtx, startCancel := context.WithCancel(ctx)
	defer startCancel()
	live, err := client.Start(startCtx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "sleep 300"}}})
	require.NoError(t, err)
	_, err = live.Recv()
	require.NoError(t, err)
	startCancel()
	_, err = live.Recv()
	require.Equal(t, codes.Canceled, status.Code(err))
	_, err = client.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/true"}}})
	require.NoError(t, err)
	shortCtx, shortCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer shortCancel()
	_, err = client.Execute(shortCtx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", "sleep 300"}}})
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	_, err = client.Execute(metadata.NewOutgoingContext(ctx, metadata.Pairs("x-sandbox-port", "9090")), &processv1.ExecuteRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
