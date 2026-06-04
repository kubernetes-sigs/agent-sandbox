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

package pool

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	podagentv1 "sigs.k8s.io/agent-sandbox/examples/managed-sandbox/clients/agentsandbox/podagent/v1"
)

// GRPCAgentClient is the production AgentClient that talks to a single
// pod-agent over gRPC. Use AgentClientPool when you need to address many
// pool pods from one controller.
type GRPCAgentClient struct {
	conn *grpc.ClientConn
	c    podagentv1.PodAgentServiceClient
}

// DialAgent opens a gRPC connection to the pod-agent at addr (host:port).
// The returned client must be closed with Close to release the connection.
//
// Transport security: MVP uses cleartext over the in-cluster network. mTLS
// will be added once the pool pod has a way to receive controller-issued
// certificates.
func DialAgent(ctx context.Context, addr string) (*GRPCAgentClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("pool: dial %s: %w", addr, err)
	}
	return &GRPCAgentClient{
		conn: conn,
		c:    podagentv1.NewPodAgentServiceClient(conn),
	}, nil
}

// Close releases the underlying gRPC connection.
func (g *GRPCAgentClient) Close() error { return g.conn.Close() }

func (g *GRPCAgentClient) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*SandboxHandle, error) {
	resp, err := g.c.CreateSandbox(ctx, &podagentv1.CreateSandboxRequest{
		SandboxUid:         req.UID,
		SandboxName:        req.Name,
		ImageReference:     req.ImageReference,
		WorkspaceMountPath: req.WorkspaceMountPath,
		SessionToken:       req.SessionToken,
	})
	if err != nil {
		return nil, err
	}
	return &SandboxHandle{UID: resp.GetSandboxUid(), State: stateFromProto(resp.GetState())}, nil
}

func (g *GRPCAgentClient) GetSandbox(ctx context.Context, uid string) (*SandboxState, error) {
	resp, err := g.c.GetSandbox(ctx, &podagentv1.GetSandboxRequest{SandboxUid: uid})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s := stateFromProto(resp.GetState())
	return &s, nil
}

func (g *GRPCAgentClient) DeleteSandbox(ctx context.Context, uid string) error {
	_, err := g.c.DeleteSandbox(ctx, &podagentv1.DeleteSandboxRequest{SandboxUid: uid})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	return nil
}

func (g *GRPCAgentClient) ListSandboxes(ctx context.Context) ([]SandboxState, error) {
	resp, err := g.c.ListSandboxes(ctx, &podagentv1.ListSandboxesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]SandboxState, 0, len(resp.GetSandboxes()))
	for _, s := range resp.GetSandboxes() {
		out = append(out, stateFromProto(s))
	}
	return out, nil
}

func stateFromProto(s *podagentv1.SandboxState) SandboxState {
	if s == nil {
		return SandboxState{}
	}
	out := SandboxState{
		UID:     s.GetSandboxUid(),
		Phase:   phaseFromProto(s.GetPhase()),
		Reason:  s.GetReason(),
		Message: s.GetMessage(),
	}
	if s.ExitCode != nil {
		v := s.GetExitCode()
		out.ExitCode = &v
	}
	return out
}

func phaseFromProto(p podagentv1.Phase) Phase {
	switch p {
	case podagentv1.Phase_PHASE_CREATING:
		return PhaseCreating
	case podagentv1.Phase_PHASE_RUNNING:
		return PhaseRunning
	case podagentv1.Phase_PHASE_STOPPING:
		return PhaseStopping
	case podagentv1.Phase_PHASE_STOPPED:
		return PhaseStopped
	case podagentv1.Phase_PHASE_FAILED:
		return PhaseFailed
	default:
		return PhaseUnknown
	}
}

// AgentClientPool keeps one GRPCAgentClient per pool pod, keyed by pod
// name. It is goroutine-safe.
type AgentClientPool struct {
	mu      sync.Mutex
	clients map[string]*cachedAgent
	// Port is the pod-agent gRPC port (default 7443).
	Port int32
}

type cachedAgent struct {
	client *GRPCAgentClient
	// addr is the host:port we dialed. Compared against the caller-
	// supplied address on every For() call: when a pod is replaced
	// (same name, new IP) we transparently close the old client and
	// redial. Without this, kubelet recreating the pod would leave the
	// controller talking to a defunct IP indefinitely.
	addr string
}

// For returns a client for the named pool pod, dialing it lazily. The
// pod's IP is read from the supplied resolver — typically the controller's
// cached client. If the cached client was for a different IP (pod
// replaced), we close it and dial the new address.
func (p *AgentClientPool) For(ctx context.Context, podName, podIP string) (AgentClient, error) {
	if podIP == "" {
		return nil, fmt.Errorf("pool: pod %q has no IP yet", podName)
	}
	port := p.Port
	if port == 0 {
		port = 7443
	}
	addr := fmt.Sprintf("%s:%d", podIP, port)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*cachedAgent{}
	}
	if existing, ok := p.clients[podName]; ok {
		if existing.addr == addr {
			return existing.client, nil
		}
		// Pod was replaced under the same name; drop the stale client.
		_ = existing.client.Close()
		delete(p.clients, podName)
	}
	cli, err := DialAgent(ctx, addr)
	if err != nil {
		return nil, err
	}
	p.clients[podName] = &cachedAgent{client: cli, addr: addr}
	return cli, nil
}

// Forget closes and removes a cached client for the named pod.
func (p *AgentClientPool) Forget(podName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cli, ok := p.clients[podName]; ok {
		_ = cli.client.Close()
		delete(p.clients, podName)
	}
}
