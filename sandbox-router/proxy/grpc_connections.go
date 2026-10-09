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

package proxy

import (
	"context"
	"net"
	"sync"
)

// grpcConnections owns the sockets independently of HTTP/2 stream teardown:
// CloseIdleConnections can skip a connection while cancellation is unwinding.
type grpcConnections struct {
	dial   func(context.Context, string, string) (net.Conn, error)
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	conns     map[*grpcUpstreamConn]struct{}
	closeOnce sync.Once
}

func newGRPCConnections(dial func(context.Context, string, string) (net.Conn, error)) *grpcConnections {
	ctx, cancel := context.WithCancel(context.Background())
	return &grpcConnections{
		dial: dial, ctx: ctx, cancel: cancel,
		conns: make(map[*grpcUpstreamConn]struct{}),
	}
}

func (c *grpcConnections) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if c.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Transport dials may outlive their initiating request. Give terminal
	// shutdown its own cancellation path without changing normal RPC budgets.
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	conn, err := c.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tracked := &grpcUpstreamConn{Conn: conn, owner: c}
	c.mu.Lock()
	// A dial can succeed concurrently with shutdown even after cancellation.
	// Do not let that late socket enter a pool that has already been closed.
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	c.conns[tracked] = struct{}{}
	c.mu.Unlock()
	return tracked, nil
}

func (c *grpcConnections) close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.cancel()
		conns := c.conns
		c.conns = nil
		c.mu.Unlock()
		// Closing a socket unblocks the transport readers/writers. Do not
		// wait for handlers or call Close while holding the ownership lock.
		for conn := range conns {
			_ = conn.Close()
		}
	})
}

type grpcUpstreamConn struct {
	net.Conn
	owner *grpcConnections
}

func (c *grpcUpstreamConn) Close() error {
	err := c.Conn.Close()
	c.owner.mu.Lock()
	delete(c.owner.conns, c)
	c.owner.mu.Unlock()
	return err
}
