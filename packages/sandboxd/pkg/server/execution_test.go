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

package server

import (
	"context"
	"net"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func TestExecuteWithLimitsOutput(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf XYZ >&2; exit 7"}},
		StdoutLimitBytes: proto.Uint64(3),
		StderrLimitBytes: proto.Uint64(3),
	})
	require.NoError(t, err)
	require.Equal(t, int32(7), resp.GetExitCode())
	require.Equal(t, "abc", string(resp.GetStdout()))
	require.Equal(t, "XYZ", string(resp.GetStderr()))
	require.True(t, resp.GetStdoutTruncated())
	require.False(t, resp.GetStderrTruncated())
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED, resp.GetTerminationReason())
	require.Positive(t, resp.GetElapsedDuration().AsDuration())
}

func TestExecuteWithLimitsOutputBoundaries(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	for _, tc := range []struct {
		name                       string
		out, errOut                *uint64
		wantOut, wantErr           string
		truncatedOut, truncatedErr bool
	}{
		{name: "unconfigured", wantOut: "abcdef", wantErr: "XYZ"},
		{name: "only stderr limited", errOut: proto.Uint64(2), wantOut: "abcdef", wantErr: "XY", truncatedErr: true},
		{name: "exact boundaries", out: proto.Uint64(6), errOut: proto.Uint64(3), wantOut: "abcdef", wantErr: "XYZ"},
		{name: "independent quotas", out: proto.Uint64(2), errOut: proto.Uint64(10), wantOut: "ab", wantErr: "XYZ", truncatedOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
				Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf XYZ >&2"}},
				StdoutLimitBytes: tc.out, StderrLimitBytes: tc.errOut,
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantOut, string(resp.GetStdout()))
			require.Equal(t, tc.wantErr, string(resp.GetStderr()))
			require.Equal(t, tc.truncatedOut, resp.GetStdoutTruncated())
			require.Equal(t, tc.truncatedErr, resp.GetStderrTruncated())
		})
	}
}

func TestExecuteWithLimitsDrainsOutput(t *testing.T) {
	client := newProcessClient(t, t.TempDir())
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "seq 1 50000; printf done >&2"}},
		StdoutLimitBytes: proto.Uint64(4), StderrLimitBytes: proto.Uint64(4),
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode())
	require.Equal(t, "1\n2\n", string(resp.GetStdout()))
	require.Equal(t, "done", string(resp.GetStderr()))
	require.True(t, resp.GetStdoutTruncated())
	require.False(t, resp.GetStderrTruncated())
}

func TestExecuteWithLimitsOldService(t *testing.T) {
	// Register only the old method table, as a pre-extension daemon would.
	desc := processv1.ProcessService_ServiceDesc
	desc.Methods = nil
	for _, method := range processv1.ProcessService_ServiceDesc.Methods {
		if method.MethodName != "ExecuteWithLimits" {
			desc.Methods = append(desc.Methods, method)
		}
	}
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&desc, NewProcessServer(t.TempDir(), nil, 0, logr.Discard()))
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = grpcServer.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///old-runtime",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(); grpcServer.Stop(); _ = lis.Close() })
	client := processv1.NewProcessServiceClient(conn)
	_, err = client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "touch should-not-exist"}},
		StdoutLimitBytes: proto.Uint64(4),
	})
	require.Equal(t, codes.Unimplemented, status.Code(err))
	resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test ! -e should-not-exist"}},
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode(), "unsupported method must not execute or fall back")
}
