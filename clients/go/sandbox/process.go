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
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// ProcessHandle owns one Start stream, not the Sandbox or shared connection.
// Consume its events through io.EOF or call Close when abandoning the stream.
type ProcessHandle struct {
	stream processv1.ProcessService_StartClient
	client processv1.ProcessServiceClient
	pid    int32
	ctx    context.Context
	prefix string
	span   trace.Span

	mu        sync.Mutex
	receiving bool
	exitSeen  bool
	terminal  error
	cleanup   func()
	once      sync.Once
}

// ProcessID returns the daemon-local virtual ID, not an OS PID.
// It cannot identify a process across daemon restarts.
func (h *ProcessHandle) ProcessID() int32 { return h.pid }

// SendSignal sends SIGINT, SIGTERM or SIGKILL to this process group using its
// original connection. Success means the signal was sent, not that the process
// exited. Its context controls only this RPC; failure does not cancel Start.
// Keep receiving events to observe the exit and the final RPC status.
func (h *ProcessHandle) SendSignal(ctx context.Context, signal ProcessSignal) error {
	var wireSignal processv1.Signal
	switch signal {
	case SignalSIGINT:
		wireSignal = processv1.Signal_SIGNAL_SIGINT
	case SignalSIGTERM:
		wireSignal = processv1.Signal_SIGNAL_SIGTERM
	case SignalSIGKILL:
		wireSignal = processv1.Signal_SIGNAL_SIGKILL
	default:
		return fmt.Errorf("%s: send signal: %w", h.prefix, status.Errorf(codes.InvalidArgument, "unsupported signal %q", signal))
	}
	h.mu.Lock()
	var err error
	switch {
	case h.terminal != nil && h.terminal != io.EOF:
		err = h.terminal
	case h.exitSeen:
		err = status.Error(codes.NotFound, "process already exited")
	case h.ctx.Err() != nil:
		err = status.FromContextError(h.ctx.Err()).Err()
	}
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%s: send signal: %w", h.prefix, err)
	}
	_, err = h.client.SendSignal(ctx, &processv1.SendSignalRequest{ProcessId: h.pid, Signal: wireSignal})
	if err != nil {
		return fmt.Errorf("%s: send signal: %w", h.prefix, err)
	}
	return nil
}

// Recv returns the next stdout, stderr or exit event. An exit event must be
// followed by io.EOF to confirm that the RPC also completed successfully.
// Nonzero exit codes are results; RPC and protocol failures remain errors.
// Only one Recv may be active; a second returns ErrConcurrentRecv without
// ending the stream. SendSignal and Close may run concurrently with Recv.
func (h *ProcessHandle) Recv() (*ProcessEvent, error) {
	h.mu.Lock()
	terminal := h.terminal
	if terminal != nil {
		h.mu.Unlock()
		return nil, terminal
	}
	if h.receiving {
		h.mu.Unlock()
		return nil, fmt.Errorf("%s: recv: %w", h.prefix, ErrConcurrentRecv)
	}
	h.receiving = true
	h.mu.Unlock()
	msg, err := h.stream.Recv()
	h.mu.Lock()
	h.receiving = false
	if h.terminal != nil {
		terminal = h.terminal
		h.mu.Unlock()
		return nil, terminal
	}
	var event *ProcessEvent
	if err == io.EOF {
		if !h.exitSeen {
			err = fmt.Errorf("%s: recv: %w: missing Exit: %w", h.prefix, ErrProcessProtocol, io.ErrUnexpectedEOF)
		}
	} else if err != nil {
		err = fmt.Errorf("%s: recv: %w", h.prefix, err)
	} else if h.exitSeen {
		err = fmt.Errorf("%s: recv: %w: event after Exit", h.prefix, ErrProcessProtocol)
	} else {
		switch ev := msg.GetEvent().(type) {
		case *processv1.StartResponse_Stdout:
			event = &ProcessEvent{Type: ProcessEventStdout, Data: bytes.Clone(ev.Stdout)}
		case *processv1.StartResponse_Stderr:
			event = &ProcessEvent{Type: ProcessEventStderr, Data: bytes.Clone(ev.Stderr)}
		case *processv1.StartResponse_Exit:
			if ev.Exit == nil {
				err = fmt.Errorf("%s: recv: %w: empty Exit", h.prefix, ErrProcessProtocol)
			} else {
				h.exitSeen = true
				event = &ProcessEvent{Type: ProcessEventExit, ExitCode: int(ev.Exit.GetExitCode())}
				h.span.SetAttributes(AttrExitCode.Int(event.ExitCode))
			}
		default:
			err = fmt.Errorf("%s: recv: %w: unexpected event", h.prefix, ErrProcessProtocol)
		}
	}
	if err != nil {
		h.terminal = err
	}
	h.mu.Unlock()
	if err != nil {
		h.release(err)
	} else if h.stream.Context().Err() != nil {
		h.observeCompletion()
	}
	return event, err
}

// Close cancels only this session and is idempotent. A nil error acknowledges
// local cleanup, not synchronous remote process termination. For graceful
// shutdown, send a signal before cancelling the stream instead.
func (h *ProcessHandle) Close() error {
	h.mu.Lock()
	if h.terminal == nil {
		err := status.FromContextError(h.ctx.Err()).Err()
		if err == nil {
			err = status.Error(codes.Canceled, "process handle closed")
		}
		h.terminal = fmt.Errorf("%s: recv: %w", h.prefix, err)
	}
	err := h.terminal
	h.mu.Unlock()
	h.release(err)
	return nil
}

func (h *ProcessHandle) release(err error) {
	h.once.Do(func() {
		if err != io.EOF {
			recordError(h.span, err)
		}
		h.cleanup()
	})
}

func (h *ProcessHandle) observeCompletion() {
	h.mu.Lock()
	receiving, terminal := h.receiving, h.terminal
	h.mu.Unlock()
	// Recv classifies its actual result before releasing. A normally finished
	// gRPC stream also cancels its context, which is not a command failure.
	if receiving {
		return
	}
	if terminal == nil {
		terminal = status.FromContextError(h.stream.Context().Err()).Err()
	}
	h.release(terminal)
}
