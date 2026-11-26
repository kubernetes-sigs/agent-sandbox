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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// The helper enters the real main function in an isolated process, including
// flag parsing, listeners, policy wiring and shutdown.
func TestSandboxdPolicyProcess(t *testing.T) {
	if os.Getenv("SANDBOXD_POLICY_TEST_PROCESS") != "1" {
		return
	}
	var args []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("SANDBOXD_POLICY_TEST_ARGS")), &args))
	os.Args = append([]string{"sandboxd"}, args...)
	main()
	os.Exit(0)
}

func policyDaemonCommand(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxdPolicyProcess$")
	cmd.Env = append(os.Environ(), "SANDBOXD_POLICY_TEST_PROCESS=1", "SANDBOXD_POLICY_TEST_ARGS="+string(encoded))
	logs := new(bytes.Buffer)
	cmd.Stdout, cmd.Stderr = logs, logs
	return cmd, logs
}

func TestSandboxdExecutionPolicyFlags(t *testing.T) {
	grpcReservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer grpcReservation.Close()
	restReservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer restReservation.Close()
	grpcPort := grpcReservation.Addr().(*net.TCPAddr).Port
	restPort := restReservation.Addr().(*net.TCPAddr).Port
	cmd, logs := policyDaemonCommand(t,
		"--listen-host=127.0.0.1", "--grpc-port="+strconv.Itoa(grpcPort), "--rest-port="+strconv.Itoa(restPort),
		"--root-dir="+t.TempDir(), "--shutdown-timeout=3s",
		"--execute-default-time-limit=200ms", "--execute-max-time-limit=1s",
		"--execute-default-stdout-limit-bytes=3", "--execute-max-stdout-limit-bytes=8",
		"--execute-default-stderr-limit-bytes=2", "--execute-max-stderr-limit-bytes=8",
	)
	require.NoError(t, grpcReservation.Close())
	require.NoError(t, restReservation.Close())
	require.NoError(t, cmd.Start())
	waitDone := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(waitDone) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitDone:
		case <-time.After(6 * time.Second):
			_ = cmd.Process.Kill()
			<-waitDone
			t.Error("daemon did not stop within its shutdown budget")
		}
		if t.Failed() {
			t.Log(logs.String())
		} else {
			require.NoError(t, waitErr, logs.String())
		}
	})
	httpClient := &http.Client{Timeout: 200 * time.Millisecond}
	defer httpClient.CloseIdleConnections()
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get("http://127.0.0.1:" + strconv.Itoa(restPort) + "/v1/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond)
	conn, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(grpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := processv1.NewProcessServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf XYZ >&2"}}
	resp, err := client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: config})
	require.NoError(t, err)
	require.Equal(t, "abc", string(resp.GetStdout()))
	require.Equal(t, "XY", string(resp.GetStderr()))
	require.True(t, resp.GetStdoutTruncated())
	require.True(t, resp.GetStderrTruncated())
	resp, err = client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: config, StdoutLimitBytes: proto.Uint64(5), StderrLimitBytes: proto.Uint64(4)})
	require.NoError(t, err)
	require.Equal(t, "abcde", string(resp.GetStdout()))
	require.Equal(t, "XYZ", string(resp.GetStderr()))
	_, err = client.Execute(ctx, &processv1.ExecuteRequest{Config: config})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	legacy, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"printf", "ab"}}})
	require.NoError(t, err)
	require.Equal(t, "ab", string(legacy.GetStdout()))
	resp, err = client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf ok; exec sleep 30"}}})
	require.NoError(t, err)
	require.Equal(t, "ok", string(resp.GetStdout()))
	require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED, resp.GetTerminationReason())
	for _, req := range []*processv1.ExecuteWithLimitsRequest{
		{TimeLimit: durationpb.New(2 * time.Second)},
		{StdoutLimitBytes: proto.Uint64(9)},
		{StderrLimitBytes: proto.Uint64(9)},
	} {
		req.Config = &processv1.ProcessConfig{Command: []string{"touch", "should-not-exist"}}
		_, err := client.ExecuteWithLimits(ctx, req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	resp, err = client.ExecuteWithLimits(ctx, &processv1.ExecuteWithLimitsRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test ! -e should-not-exist"}}})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode())
}

func TestSandboxdExecutionPolicyInvalidFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--execute-default-time-limit=2s", "--execute-max-time-limit=1s"},
		{"--execute-default-stdout-limit-bytes=5", "--execute-max-stdout-limit-bytes=3"},
		{"--execute-default-stderr-limit-bytes=5", "--execute-max-stderr-limit-bytes=3"},
	} {
		cmd, logs := policyDaemonCommand(t, append([]string{"--root-dir=" + t.TempDir()}, args...)...)
		require.Error(t, cmd.Run())
		require.Contains(t, logs.String(), "invalid execution policy")
	}
}
