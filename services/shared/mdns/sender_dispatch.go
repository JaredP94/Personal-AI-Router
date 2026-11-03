// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"net"
	"sync"
)

// Multiple browser/responder owners in a process bind the same source endpoint.
// Unicast UDP delivery chooses just one socket, unlike multicast delivery. Any
// reader therefore dispatches to every owner of that endpoint, letting each
// browser/responder select the DNS packets it understands.
var senderReceivers = struct {
	sync.RWMutex
	bySource map[senderKey]map[*Sender]*senderReceiver
}{bySource: make(map[senderKey]map[*Sender]*senderReceiver)}

type senderReceiver struct {
	mu      sync.Mutex
	active  bool
	owner   *Sender
	receive func([]byte, int, net.Addr)
}

func (s *Sender) registerReceiver(key senderKey, receive func([]byte, int, net.Addr)) {
	if receive == nil {
		return
	}
	receiver := &senderReceiver{active: true, owner: s, receive: receive}
	senderReceivers.Lock()
	owners := senderReceivers.bySource[key]
	if owners == nil {
		owners = make(map[*Sender]*senderReceiver)
		senderReceivers.bySource[key] = owners
	}
	owners[s] = receiver
	senderReceivers.Unlock()
}

// unregisterReceiver is called under the owner's lock, but never waits for a
// callback: callbacks can send packets themselves. Close waits only after
// releasing the owner lock. The receiver lock orders Add against deactivation,
// so no callback can start after Close begins waiting for the owner's group.
func (s *Sender) unregisterReceiver(key senderKey) {
	senderReceivers.Lock()
	owners := senderReceivers.bySource[key]
	receiver := owners[s]
	delete(owners, s)
	if len(owners) == 0 {
		delete(senderReceivers.bySource, key)
	}
	senderReceivers.Unlock()
	if receiver != nil {
		receiver.mu.Lock()
		receiver.active = false
		receiver.mu.Unlock()
	}
}

func dispatchSenderPacket(key senderKey, buf []byte, from net.Addr) {
	senderReceivers.RLock()
	receivers := make([]*senderReceiver, 0, len(senderReceivers.bySource[key]))
	for _, receiver := range senderReceivers.bySource[key] {
		receivers = append(receivers, receiver)
	}
	senderReceivers.RUnlock()
	for _, receiver := range receivers {
		receiver.mu.Lock()
		active := receiver.active
		if active {
			receiver.owner.callbacks.Add(1)
		}
		receiver.mu.Unlock()
		if active {
			receiver.receive(buf, key.index, from)
			receiver.owner.callbacks.Done()
		}
	}
}
