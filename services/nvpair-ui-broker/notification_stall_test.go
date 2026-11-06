// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	serviceerrors "nvpair-shared/errors"
	"nvpair-ui-broker/relay"
)

func stalledNotificationCodec(t *testing.T) *Codec {
	t.Helper()
	local, remote := net.Pipe()
	c := NewCodec(local)
	t.Cleanup(func() { _ = remote.Close(); _ = c.Close() })
	return c
}

func returnsDespiteStalledReader(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stalled notification retained caller or its lock")
	}
}

func TestPeerMetadataStalledReaderReleasesStdinMu(t *testing.T) {
	np := &nodeInfoProcess{codec: stalledNotificationCodec(t)}
	b := &Broker{regCache: relay.NewRegistrationCache(), nodeID: "fixture"}
	b.setNodeInfo(np)
	returnsDespiteStalledReader(t, b.pushPeerMetadataToNodeInfo)
	if !np.stdinMu.TryLock() {
		t.Fatal("peer metadata retained stdinMu")
	}
	np.stdinMu.Unlock()
}

func TestErrorReportStalledReaderReturns(t *testing.T) {
	b := &Broker{}
	b.setErrors(&errorsProcess{peer: NewPeer(stalledNotificationCodec(t))})
	returnsDespiteStalledReader(t, func() { b.forwardErrorsReport(serviceerrors.ServiceError{ID: "fixture"}) })
}

func TestErrorClearStalledReaderReturns(t *testing.T) {
	b := &Broker{}
	b.setErrors(&errorsProcess{peer: NewPeer(stalledNotificationCodec(t))})
	returnsDespiteStalledReader(t, func() { b.forwardErrorsClear("fixture") })
}

func TestErrorReplayStalledReaderReleasesErrorMu(t *testing.T) {
	b := &Broker{ollamaHostAliasError: &serviceerrors.ServiceError{ID: "fixture"}}
	b.setErrors(&errorsProcess{peer: NewPeer(stalledNotificationCodec(t))})
	returnsDespiteStalledReader(t, b.replayOllamaHostAliasError)
	if !b.ollamaHostAliasErrorMu.TryLock() {
		t.Fatal("error replay retained error lock")
	}
	b.ollamaHostAliasErrorMu.Unlock()
}

func TestPublicPortAssignmentStalledReaderReturns(t *testing.T) {
	b := &Broker{codec: stalledNotificationCodec(t)}
	b.managedOllamaFacade.Store(true)
	id := json.RawMessage("1")
	// Exercise the actual public port-assignment rejection, not Codec directly.
	returnsDespiteStalledReader(t, func() {
		b.relayToEngineNow(&Message{
			JSONRPC: "2.0", ID: &id, Method: "engine:set-port",
			Params: json.RawMessage(`{"engine":"ollama","port":11434}`),
		})
	})
}
