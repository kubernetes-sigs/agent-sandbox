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
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type streamingProcessService struct {
	processv1.UnimplementedProcessServiceServer
	start  func(*processv1.StartRequest, processv1.ProcessService_StartServer) error
	signal func(context.Context, *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error)
}

func (s *streamingProcessService) Start(req *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
	return s.start(req, stream)
}

func (s *streamingProcessService) SendSignal(ctx context.Context, req *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
	return s.signal(ctx, req)
}

func processTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newStreamingSandbox(t *testing.T, svc processv1.ProcessServiceServer) *Sandbox {
	t.Helper()
	sb := newReadySandboxdTestSandbox("http://unused")
	startFakeProcessServer(t, sb, svc)
	t.Cleanup(func() { require.NoError(t, sb.Disconnect(context.Background())) })
	return sb
}

func processInit() *processv1.StartResponse {
	return &processv1.StartResponse{Event: &processv1.StartResponse_Init{Init: &processv1.InitEvent{ProcessId: 42}}}
}

func processExit(code int32) *processv1.StartResponse {
	return &processv1.StartResponse{Event: &processv1.StartResponse_Exit{Exit: &processv1.ExitEvent{ExitCode: code}}}
}

func scriptedProcessService(events []*processv1.StartResponse, terminal error) *streamingProcessService {
	return &streamingProcessService{start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		for _, event := range events {
			if err := stream.Send(event); err != nil {
				return err
			}
		}
		return terminal
	}}
}

func TestCommandsStart_StreamsBeforeExit(t *testing.T) {
	ctx := processTestContext(t)
	finish := make(chan struct{})
	svc := &streamingProcessService{start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		if err := stream.Send(&processv1.StartResponse{Event: &processv1.StartResponse_Init{Init: &processv1.InitEvent{ProcessId: 42}}}); err != nil {
			return err
		}
		if err := stream.Send(&processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: []byte("early")}}); err != nil {
			return err
		}
		select {
		case <-finish:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		return stream.Send(&processv1.StartResponse{Event: &processv1.StartResponse_Exit{Exit: &processv1.ExitEvent{ExitCode: 7}}})
	}}
	sb := newReadySandboxdTestSandbox("http://unused")
	startFakeProcessServer(t, sb, svc)
	t.Cleanup(func() { require.NoError(t, sb.Disconnect(context.Background())) })
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	require.Equal(t, int32(42), handle.ProcessID())
	event, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStdout, event.Type)
	require.Equal(t, []byte("early"), event.Data)
	close(finish)
	event, err = handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventExit, event.Type)
	require.Equal(t, 7, event.ExitCode)
	_, err = handle.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestCommandsStart_DoesNotReplayWithServiceRetryPolicy(t *testing.T) {
	var starts atomic.Int32
	sb := newStreamingSandbox(t, &streamingProcessService{start: func(_ *processv1.StartRequest, _ processv1.ProcessService_StartServer) error {
		starts.Add(1)
		return status.Error(codes.Unavailable, "start failed before Init")
	}})
	conn, err := grpc.NewClient(sb.connector.grpcTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"methodConfig":[{"name":[{"service":"process.v1.ProcessService","method":"Start"}],"retryPolicy":{"MaxAttempts":4,"InitialBackoff":"0.001s","MaxBackoff":"0.001s","BackoffMultiplier":1,"RetryableStatusCodes":["UNAVAILABLE"]}}]}`))
	require.NoError(t, err)
	sb.connector.mu.Lock()
	sb.connector.grpcConn = conn
	sb.connector.mu.Unlock()
	ctx := processTestContext(t)
	// The control proves this connection's retry policy is actually active.
	raw, err := processv1.NewProcessServiceClient(conn).Start(ctx, &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"example"}}})
	require.NoError(t, err)
	_, err = raw.Recv()
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, int32(4), starts.Load())
	starts.Store(0)
	handle, err := sb.Commands().Start(ctx, "example")
	require.Nil(t, handle)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, int32(1), starts.Load(), "a failed non-idempotent Start must not be replayed")
	starts.Store(0)
	handle, err = sb.Commands().Start(ctx, "example", WithMaxAttempts(2))
	require.Nil(t, handle)
	require.ErrorContains(t, err, "WithMaxAttempts")
	require.Zero(t, starts.Load(), "explicit retry requests must be rejected before sending")
}

func TestCommandsStart_EnvAndWorkingDir(t *testing.T) {
	requests := make(chan *processv1.StartRequest, 1)
	svc := scriptedProcessService([]*processv1.StartResponse{processInit(), processExit(0)}, nil)
	start := svc.start
	svc.start = func(req *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		requests <- req
		return start(req, stream)
	}
	sb := newStreamingSandbox(t, svc)
	handle, err := sb.Commands().Start(processTestContext(t), "printf hello",
		WithEnv(map[string]string{"A": "one", "B": "old"}),
		WithEnv(map[string]string{"B": "two"}), WithWorkingDir("project"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	config := (<-requests).GetConfig()
	require.Equal(t, []string{"/bin/sh", "-c", "printf hello"}, config.GetCommand())
	require.Equal(t, map[string]string{"A": "one", "B": "two"}, config.GetEnvVars())
	require.Equal(t, "project", config.GetCwd())
	for _, key := range []string{"", "A=B"} {
		_, err := sb.Commands().Start(processTestContext(t), "example", WithEnv(map[string]string{key: "value"}))
		require.ErrorContains(t, err, "invalid env key")
	}
	require.Empty(t, requests, "invalid configuration must not reach the service")
}

func TestCommandsStart_InitializationFailures(t *testing.T) {
	tests := []struct {
		name   string
		events []*processv1.StartResponse
		err    error
		code   codes.Code
	}{
		{name: "missing Init"},
		{name: "empty Init", events: []*processv1.StartResponse{{Event: &processv1.StartResponse_Init{Init: &processv1.InitEvent{}}}}},
		{name: "output before Init", events: []*processv1.StartResponse{{Event: &processv1.StartResponse_Stdout{Stdout: []byte("wrong")}}}},
		{name: "exit before Init", events: []*processv1.StartResponse{processExit(0)}},
		{name: "empty event", events: []*processv1.StartResponse{{}}},
		{name: "RPC error", err: status.Error(codes.NotFound, "missing command"), code: codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newStreamingSandbox(t, scriptedProcessService(tt.events, tt.err))
			handle, err := sb.Commands().Start(processTestContext(t), "example")
			require.Nil(t, handle)
			if tt.err != nil {
				require.Equal(t, tt.code, status.Code(err))
			} else {
				require.ErrorIs(t, err, ErrProcessProtocol)
				if len(tt.events) == 0 {
					require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				}
			}
		})
	}
}

func TestProcessHandle_ProtocolAndStableTerminal(t *testing.T) {
	tests := []struct {
		name   string
		events []*processv1.StartResponse
		err    error
		want   error
	}{
		{name: "normal EOF", events: []*processv1.StartResponse{processExit(9)}, want: io.EOF},
		{name: "missing Exit", want: io.ErrUnexpectedEOF},
		{name: "repeated Init", events: []*processv1.StartResponse{processInit()}, want: ErrProcessProtocol},
		{name: "empty event", events: []*processv1.StartResponse{{}}, want: ErrProcessProtocol},
		{name: "duplicate Exit", events: []*processv1.StartResponse{processExit(0), processExit(0)}, want: ErrProcessProtocol},
		{name: "output after Exit", events: []*processv1.StartResponse{processExit(0), {Event: &processv1.StartResponse_Stdout{Stdout: []byte("late")}}}, want: ErrProcessProtocol},
		{name: "error after Exit", events: []*processv1.StartResponse{processExit(7)}, err: status.Error(codes.PermissionDenied, "terminal failure")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newStreamingSandbox(t, scriptedProcessService(append([]*processv1.StartResponse{processInit()}, tt.events...), tt.err))
			handle, err := sb.Commands().Start(processTestContext(t), "example")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			for {
				_, err = handle.Recv()
				if err != nil {
					break
				}
			}
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				if tt.want == io.ErrUnexpectedEOF {
					require.ErrorIs(t, err, ErrProcessProtocol)
				}
			} else {
				require.Equal(t, codes.PermissionDenied, status.Code(err))
			}
			terminal := err
			require.NoError(t, handle.Close())
			_, err = handle.Recv()
			require.Equal(t, terminal, err)
		})
	}
}

func TestProcessHandle_RawOutputChunks(t *testing.T) {
	sb := newStreamingSandbox(t, scriptedProcessService([]*processv1.StartResponse{
		processInit(),
		{Event: &processv1.StartResponse_Stdout{Stdout: []byte{0xf0, 0x9f}}},
		{Event: &processv1.StartResponse_Stderr{Stderr: []byte{0xff, '\n'}}},
		{Event: &processv1.StartResponse_Stdout{Stdout: []byte{0x98, 0x80}}},
		processExit(-1),
	}, nil))
	handle, err := sb.Commands().Start(processTestContext(t), "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	first, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStdout, first.Type)
	second, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, ProcessEventStderr, second.Type)
	require.Equal(t, []byte{0xff, '\n'}, second.Data)
	third, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, []byte{0x98, 0x80}, third.Data)
	require.Equal(t, []byte{0xf0, 0x9f}, first.Data, "previous chunks must remain valid")
	exit, err := handle.Recv()
	require.NoError(t, err)
	require.Equal(t, -1, exit.ExitCode, "do not convert signal exits to shell conventions")
	_, err = handle.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestCommandsStart_RuntimeAndConnectionGuards(t *testing.T) {
	legacy := newReadyTestSandbox("http://unused")
	_, err := legacy.Commands().Start(processTestContext(t), "example")
	require.ErrorIs(t, err, ErrUnsupportedByRuntime)
	direct := newReadySandboxdTestSandbox("http://unused")
	_, err = direct.Commands().Start(processTestContext(t), "example")
	require.ErrorIs(t, err, ErrUnsupportedByRuntime)
	notReady, _, _ := newTestSandbox(inClusterTestOpts())
	_, err = notReady.Commands().Start(processTestContext(t), "example")
	require.ErrorIs(t, err, ErrNotReady)
}

func TestCommandsStart_NoDefaultTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			deadline := make(chan bool, 1)
			svc := scriptedProcessService([]*processv1.StartResponse{processInit(), processExit(0)}, nil)
			start := svc.start
			svc.start = func(req *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
				_, hasDeadline := stream.Context().Deadline()
				deadline <- hasDeadline
				return start(req, stream)
			}
			sb := newStreamingSandbox(t, svc)
			handle, err := sb.Commands().Start(context.Background(), "example", WithTimeout(timeout), WithMaxAttempts(1))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			require.False(t, <-deadline, "RequestTimeout must not become a stream deadline")
			_, err = handle.Recv()
			require.NoError(t, err)
			_, err = handle.Recv()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func blockingProcessService() *streamingProcessService {
	return &streamingProcessService{start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		if err := stream.Send(processInit()); err != nil {
			return err
		}
		<-stream.Context().Done()
		return status.FromContextError(stream.Context().Err()).Err()
	}}
}

func TestProcessHandle_CancelAndCloseInterruptReceive(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			sb := newStreamingSandbox(t, blockingProcessService())
			ctx, cancel := context.WithCancel(processTestContext(t))
			defer cancel()
			handle, err := sb.Commands().Start(ctx, "example")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			result := make(chan error, 1)
			go func() { _, err := handle.Recv(); result <- err }()
			if action == "close" {
				require.NoError(t, handle.Close())
				require.NoError(t, handle.Close())
			} else {
				cancel()
			}
			select {
			case err := <-result:
				require.Equal(t, codes.Canceled, status.Code(err))
			case <-processTestContext(t).Done():
				t.Fatal("cancellation failed to interrupt Recv")
			}
			_, err = handle.Recv()
			require.Equal(t, codes.Canceled, status.Code(err))
		})
	}
}

func TestProcessHandle_TimeoutCoversEntireStream(t *testing.T) {
	for _, earlierCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "option deadline", true: "earlier caller deadline"}[earlierCaller], func(t *testing.T) {
			sb := newStreamingSandbox(t, blockingProcessService())
			ctx := processTestContext(t)
			timeout := 60 * time.Millisecond
			if earlierCaller {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
				timeout = time.Second
			}
			handle, err := sb.Commands().Start(ctx, "example", WithTimeout(timeout))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			_, err = handle.Recv()
			require.Equal(t, codes.DeadlineExceeded, status.Code(err), "timeout must remain active after Start returns")
		})
	}
}

func TestProcessHandle_SendSignalBoundTarget(t *testing.T) {
	finish := make(chan struct{})
	requests := make(chan *processv1.SendSignalRequest, 3)
	svc := &streamingProcessService{
		start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
			if err := stream.Send(processInit()); err != nil {
				return err
			}
			select {
			case <-finish:
				return stream.Send(processExit(0))
			case <-stream.Context().Done():
				return status.FromContextError(stream.Context().Err()).Err()
			}
		},
		signal: func(_ context.Context, req *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
			requests <- req
			return &processv1.SendSignalResponse{}, nil
		},
	}
	sb := newStreamingSandbox(t, svc)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	for _, tt := range []struct {
		signal ProcessSignal
		want   processv1.Signal
	}{
		{SignalSIGINT, processv1.Signal_SIGNAL_SIGINT},
		{SignalSIGTERM, processv1.Signal_SIGNAL_SIGTERM},
		{SignalSIGKILL, processv1.Signal_SIGNAL_SIGKILL},
	} {
		require.NoError(t, handle.SendSignal(ctx, tt.signal))
		req := <-requests
		require.Equal(t, int32(42), req.GetProcessId())
		require.Equal(t, tt.want, req.GetSignal())
	}
	close(finish)
	_, err = handle.Recv()
	require.NoError(t, err)
	_, err = handle.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestProcessHandle_SignalFailureDoesNotCancelStream(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			finish := make(chan struct{})
			var calls atomic.Int32
			svc := &streamingProcessService{
				start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
					if err := stream.Send(processInit()); err != nil {
						return err
					}
					select {
					case <-finish:
						if err := stream.Send(&processv1.StartResponse{Event: &processv1.StartResponse_Stdout{Stdout: []byte("still running")}}); err != nil {
							return err
						}
						return stream.Send(processExit(0))
					case <-stream.Context().Done():
						return status.FromContextError(stream.Context().Err()).Err()
					}
				},
				signal: func(_ context.Context, _ *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
					calls.Add(1)
					return nil, status.Error(code, "signal refused")
				},
			}
			sb := newStreamingSandbox(t, svc)
			ctx := processTestContext(t)
			handle, err := sb.Commands().Start(ctx, "example")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			require.Equal(t, codes.InvalidArgument, status.Code(handle.SendSignal(ctx, ProcessSignal("SIGUSR1"))))
			require.Zero(t, calls.Load())
			require.Equal(t, code, status.Code(handle.SendSignal(ctx, SignalSIGTERM)))
			require.Equal(t, int32(1), calls.Load())
			close(finish)
			event, err := handle.Recv()
			require.NoError(t, err)
			require.Equal(t, []byte("still running"), event.Data)
			_, err = handle.Recv()
			require.NoError(t, err)
			_, err = handle.Recv()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestProcessHandle_SignalContextIsIndependent(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	svc := &streamingProcessService{
		start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
			if err := stream.Send(processInit()); err != nil {
				return err
			}
			select {
			case <-finish:
				return stream.Send(processExit(0))
			case <-stream.Context().Done():
				return status.FromContextError(stream.Context().Err()).Err()
			}
		},
		signal: func(ctx context.Context, _ *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
			close(entered)
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		},
	}
	sb := newStreamingSandbox(t, svc)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	signalCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- handle.SendSignal(signalCtx, SignalSIGTERM) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("signal RPC did not start")
	}
	cancel()
	select {
	case err := <-result:
		require.Equal(t, codes.Canceled, status.Code(err))
	case <-ctx.Done():
		t.Fatal("signal RPC did not cancel")
	}
	close(finish)
	_, err = handle.Recv()
	require.NoError(t, err, "cancelling the signal must not cancel Start")
	_, err = handle.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestProcessHandle_SignalAfterExitOrCloseDoesNotSend(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "exit", true: "close"}[closed], func(t *testing.T) {
			var calls atomic.Int32
			svc := blockingProcessService()
			if !closed {
				svc = scriptedProcessService([]*processv1.StartResponse{processInit(), processExit(0)}, nil)
			}
			svc.signal = func(_ context.Context, _ *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
				calls.Add(1)
				return &processv1.SendSignalResponse{}, nil
			}
			sb := newStreamingSandbox(t, svc)
			ctx := processTestContext(t)
			handle, err := sb.Commands().Start(ctx, "example")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			want := codes.NotFound
			if closed {
				require.NoError(t, handle.Close())
				want = codes.Canceled
			} else {
				_, err = handle.Recv()
				require.NoError(t, err)
			}
			require.Equal(t, want, status.Code(handle.SendSignal(ctx, SignalSIGKILL)))
			require.Zero(t, calls.Load())
		})
	}
}

func TestProcessHandle_SignalDoesNotFollowReconnect(t *testing.T) {
	old := blockingProcessService()
	sb := newStreamingSandbox(t, old)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	var newTargetCalls atomic.Int32
	replacement := blockingProcessService()
	replacement.signal = func(_ context.Context, _ *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
		newTargetCalls.Add(1)
		return &processv1.SendSignalResponse{}, nil
	}
	startFakeProcessServer(t, sb, replacement)
	require.Error(t, handle.SendSignal(ctx, SignalSIGKILL), "the old connection has closed")
	require.Zero(t, newTargetCalls.Load(), "old daemon-local IDs must not be sent to a replacement target")
}

func newTracedProcessSandbox(t *testing.T, svc processv1.ProcessServiceServer) (*Sandbox, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	sb, err := New(context.Background(), Options{
		WarmPoolName: "test-pool", Namespace: "default", Runtime: RuntimeSandboxd,
		APIURL: "http://unused", Quiet: true, CleanupTimeout: 2 * time.Second,
		TracerProvider: tp, TraceServiceName: "process-test",
		K8sHelper: &K8sHelper{Log: logr.Discard()},
	})
	require.NoError(t, err)
	startFakeProcessServer(t, sb, svc)
	t.Cleanup(func() { require.NoError(t, sb.Disconnect(context.Background())) })
	return sb, recorder
}

func TestProcessHandle_IdleCancellationEndsSpan(t *testing.T) {
	sb, recorder := newTracedProcessSandbox(t, blockingProcessService())
	ctx, cancel := context.WithCancel(processTestContext(t))
	defer cancel()
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	require.Len(t, recorder.Started(), 1)
	require.Empty(t, recorder.Ended(), "Start must transfer the live operation to the handle")
	cancel()
	require.Eventually(t, func() bool { return len(recorder.Ended()) == 1 }, 300*time.Millisecond, time.Millisecond,
		"cancellation must release resources without another Recv or Close")
}

func TestCommandsStart_StartupTimeoutReleasesResources(t *testing.T) {
	entered := make(chan struct{})
	svc := &streamingProcessService{start: func(_ *processv1.StartRequest, stream processv1.ProcessService_StartServer) error {
		close(entered)
		<-stream.Context().Done()
		return status.FromContextError(stream.Context().Err()).Err()
	}}
	sb, recorder := newTracedProcessSandbox(t, svc)
	handle, err := sb.Commands().Start(processTestContext(t), "example", WithTimeout(100*time.Millisecond))
	require.Nil(t, handle, "a timed-out Init must not return a usable handle")
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	select {
	case <-entered:
	default:
		t.Fatal("the Start request must reach the service before its Init times out")
	}
	require.Len(t, recorder.Ended(), 1, "startup failure must end the transferred span")
	started := time.Now()
	require.NoError(t, sb.Close(processTestContext(t)))
	require.Less(t, time.Since(started), time.Second, "failed Start must release its in-flight count")
}

func TestProcessHandle_ConcurrentRecvIsRejected(t *testing.T) {
	sb := newStreamingSandbox(t, blockingProcessService())
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := handle.Recv()
			results <- err
		}()
	}
	close(start)
	select {
	case err := <-results:
		require.ErrorIs(t, err, ErrConcurrentRecv)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a concurrent Recv must fail without waiting for the stream")
	}
	require.NoError(t, handle.Close())
	select {
	case err := <-results:
		require.Equal(t, codes.Canceled, status.Code(err), "the admitted receiver must remain owned and cancellable")
	case <-ctx.Done():
		t.Fatal("Close failed to interrupt the admitted Recv")
	}
}

func TestProcessHandle_ParentAndIdleCleanup(t *testing.T) {
	for _, action := range []string{"close handle", "timeout", "parent close", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			sb, recorder := newTracedProcessSandbox(t, blockingProcessService())
			var opts []CallOption
			if action == "timeout" {
				opts = []CallOption{WithTimeout(60 * time.Millisecond)}
			}
			handle, err := sb.Commands().Start(processTestContext(t), "example", opts...)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			require.Empty(t, recorder.Ended())
			started := time.Now()
			switch action {
			case "close handle":
				require.NoError(t, handle.Close())
			case "parent close":
				require.NoError(t, sb.Close(processTestContext(t)))
				require.Less(t, time.Since(started), time.Second, "the tracked operation must drain, not consume its cleanup budget")
			case "disconnect":
				require.NoError(t, sb.Disconnect(processTestContext(t)))
			}
			require.Eventually(t, func() bool { return len(recorder.Ended()) == 1 }, 300*time.Millisecond, time.Millisecond,
				"cleanup must not need another Recv")
			require.NoError(t, handle.Close())
			require.Len(t, recorder.Ended(), 1, "cleanup must end the span exactly once")
		})
	}
}

func TestProcessHandle_NormalEOFCleanupDoesNotBecomeCanceled(t *testing.T) {
	for _, terminal := range []error{nil, status.Error(codes.Unavailable, "terminal failure")} {
		t.Run(map[bool]string{true: "EOF", false: "RPC error"}[terminal == nil], func(t *testing.T) {
			sb, recorder := newTracedProcessSandbox(t, scriptedProcessService([]*processv1.StartResponse{processInit(), processExit(0)}, terminal))
			handle, err := sb.Commands().Start(processTestContext(t), "example")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, handle.Close()) })
			_, err = handle.Recv()
			require.NoError(t, err)
			_, err = handle.Recv()
			wantStatus := otelcodes.Unset
			if terminal == nil {
				require.ErrorIs(t, err, io.EOF)
			} else {
				require.Equal(t, codes.Unavailable, status.Code(err))
				wantStatus = otelcodes.Error
			}
			require.Len(t, recorder.Ended(), 1)
			require.Equal(t, wantStatus, recorder.Ended()[0].Status().Code)
			require.NoError(t, handle.Close())
			_, repeated := handle.Recv()
			require.Equal(t, err, repeated, "internal context cancellation must not overwrite the observed result")
		})
	}
}

func TestProcessHandle_ConcurrentSignalRecvAndClose(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	svc := blockingProcessService()
	svc.signal = func(ctx context.Context, _ *processv1.SendSignalRequest) (*processv1.SendSignalResponse, error) {
		close(entered)
		select {
		case <-release:
			return &processv1.SendSignalResponse{}, nil
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	sb := newStreamingSandbox(t, svc)
	ctx := processTestContext(t)
	handle, err := sb.Commands().Start(ctx, "example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	received := make(chan error, 1)
	signalled := make(chan error, 1)
	go func() { _, err := handle.Recv(); received <- err }()
	go func() { signalled <- handle.SendSignal(ctx, SignalSIGTERM) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("signal did not enter the service")
	}
	require.NoError(t, handle.Close())
	close(release)
	select {
	case err := <-received:
		require.Equal(t, codes.Canceled, status.Code(err))
	case <-ctx.Done():
		t.Fatal("Close did not interrupt Recv")
	}
	select {
	case err := <-signalled:
		require.NoError(t, err, "an admitted signal RPC owns its own context")
	case <-ctx.Done():
		t.Fatal("signal did not finish independently")
	}
}
