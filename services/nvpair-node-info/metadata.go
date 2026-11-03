// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"nvpair-shared/applog"
	"nvpair-shared/noderec"
	"sync"
)

// peerMetadata is replaced atomically by our owning broker, never from the LAN.
type peerMetadata struct {
	mu    sync.RWMutex
	value noderec.PeerMetadataParams
}

func (m *peerMetadata) get() noderec.PeerMetadataParams {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.value
}
func (m *peerMetadata) decorate(data []byte, hostUUID string, verified bool) []byte {
	var response NodeInfoResponse
	if json.Unmarshal(data, &response) != nil {
		return nil
	}
	response.IdentityVerification = "unverified"
	if verified {
		response.IdentityVerification = "verified"
	}
	meta := m.get()
	if meta.HostUUID == hostUUID {
		response.PeerMetadata = &meta
	}
	out, _ := json.Marshal(response)
	return out
}

func (m *peerMetadata) handle(msg applog.StdinMessage, hostUUID string) {
	if msg.Method != noderec.MethodSetPeerMetadata {
		return
	}
	var value noderec.PeerMetadataParams
	if json.Unmarshal(msg.Params, &value) != nil || hostUUID == "" || value.HostUUID != hostUUID {
		return
	}
	m.mu.Lock()
	m.value = value
	m.mu.Unlock()
}
