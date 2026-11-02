// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eapnoob"
)

// Skipping negotiation used to panic while holding the session lock, leaving
// a session expiry could never reclaim. It must now fail as a protocol error
// and release the pending-session slot.
func TestPairingInitialRejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	m := testManagerAt(t, t.TempDir(), 15211)
	env := &pairingEnvelope{InviteID: "skipped-negotiation", Phase: "initial"}

	discovery := httptest.NewRecorder()
	m.handlePairingInitial(discovery, env, []byte(`{"Type":1}`), "127.0.0.1")
	if discovery.Code != http.StatusOK {
		t.Fatalf("discovery status %d, want 200", discovery.Code)
	}
	if _, ok := m.getSession(env.InviteID); !ok {
		t.Fatal("discovery did not register a joiner session")
	}

	response := httptest.NewRecorder()
	m.handlePairingInitial(response, env, []byte(`{"Type":3,"PKs":{},"Ns":""}`), "127.0.0.1")
	if response.Code != http.StatusOK {
		t.Fatalf("key-exchange status %d, want 200 carrying an EAP-NOOB error", response.Code)
	}
	var reply pairingEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	message, err := base64.StdEncoding.DecodeString(reply.Msg)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	var failure struct {
		ErrorCode int `json:"ErrorCode"`
	}
	if err := json.Unmarshal(message, &failure); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if failure.ErrorCode != eapnoob.ErrStateMismatch {
		t.Fatalf("ErrorCode %d, want %d (state mismatch)", failure.ErrorCode, eapnoob.ErrStateMismatch)
	}
	if _, ok := m.getSession(env.InviteID); ok {
		t.Fatal("a terminal protocol error must release the pending session slot")
	}
}
