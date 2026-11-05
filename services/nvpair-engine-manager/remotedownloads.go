// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"nvpair-shared/noderec"
)

type remotePull struct {
	node   string
	cancel context.CancelFunc
}

// Serialize registration with full discovery snapshots, but release the lock
// before cancellation, transport work or progress callbacks.
func (m *Manager) trackRemotePull(ctx context.Context, id, node string) (context.Context, func(), error) {
	m.remotePullMu.Lock()
	peer, ok := m.peers.lookup(node)
	if !ok || !peer.verified {
		m.remotePullMu.Unlock()
		return nil, nil, fmt.Errorf("download peer is no longer verified")
	}
	if m.remotePulls == nil {
		m.remotePulls = make(map[string]*remotePull)
	}
	if m.remotePulls[id] != nil {
		m.remotePullMu.Unlock()
		return nil, nil, fmt.Errorf("download already active")
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &remotePull{node: node, cancel: cancel}
	m.remotePulls[id] = p
	m.remotePullMu.Unlock()
	return ctx, func() {
		cancel()
		m.remotePullMu.Lock()
		if m.remotePulls[id] == p {
			delete(m.remotePulls, id)
		}
		m.remotePullMu.Unlock()
	}, nil
}
func (m *Manager) updateDownloadPeers(nodes []noderec.DirectoryNode) {
	m.remotePullMu.Lock()
	m.peers.set(nodes)
	var cancel []context.CancelFunc
	for _, p := range m.remotePulls {
		peer, ok := m.peers.lookup(p.node)
		if !ok || !peer.verified {
			cancel = append(cancel, p.cancel)
		}
	}
	m.remotePullMu.Unlock()
	for _, stop := range cancel {
		stop()
	}
}
