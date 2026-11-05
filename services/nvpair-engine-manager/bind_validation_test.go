// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
)

func TestLiveLANBindSetting(t *testing.T) {
	var out bytes.Buffer
	ex := reconciledTestExecutor(NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir()))
	m := NewManager(NewCodec(&out), ex, nil)
	var result json.RawMessage
	var replyError string
	reads := 0
	m.settingsRelay.send = func(method string, params any) error {
		if method != "engine:settings-request" {
			t.Fatalf("unexpected method %s", method)
		}
		raw, _ := json.Marshal(params)
		var request struct{ ID, Method string }
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		if request.Method != "settings/get-engine-allow-lan-bind" {
			t.Fatalf("unexpected read %s", request.Method)
		}
		reads++
		reply, _ := json.Marshal(settingsReply{ID: request.ID, Result: result, Error: replyError})
		m.handleMessage(context.Background(), &Message{Method: "engine:settings-reply", Params: reply})
		return nil
	}
	for _, tc := range []struct {
		result, err string
		allowed     bool
	}{
		{`{"value":false}`, "", false},
		{`{"value":true}`, "", true},
		{`{"value":false}`, "", false},
		{`{}`, "", false},
		{`{"value":null}`, "", false},
		{`{"value":"true"}`, "", false},
		{`{"value":true}`, "unavailable", false},
	} {
		result, replyError = json.RawMessage(tc.result), tc.err
		out.Reset()
		id := json.RawMessage("1")
		m.runOp(context.Background(), &Message{ID: &id, Method: "engine:start", Params: json.RawMessage(`{"engine":"missing","bind":"0.0.0.0"}`)})
		denied := strings.Contains(out.String(), "bind must be a loopback address")
		if denied == tc.allowed {
			t.Errorf("result=%s err=%s allowed=%v response=%s", tc.result, tc.err, tc.allowed, out.String())
		}
	}
	if reads != 7 {
		t.Fatalf("read count=%d, want a live read each time", reads)
	}
	m.settingsRelay.send = func(string, any) error { return errors.New("broker unavailable") }
	if m.lanBindAllowed(context.Background()) {
		t.Fatal("transport error allowed LAN bind")
	}
}

func TestLANBindReadTimeoutFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		m := NewManager(NewCodec(&out), nil, nil)
		m.settingsRelay.send = func(string, any) error { return nil }
		if m.lanBindAllowed(context.Background()) {
			t.Fatal("timeout allowed LAN bind")
		}
		if len(m.settingsRelay.pending) != 0 {
			t.Fatal("timeout leaked pending read")
		}
	})
}

func TestLoopbackBindDoesNotReadSetting(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1", ""} {
		var out bytes.Buffer
		ex := reconciledTestExecutor(NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir()))
		m := NewManager(NewCodec(&out), ex, nil)
		m.settingsRelay.send = func(string, any) error { t.Fatal("loopback required setting lookup"); return nil }
		id := json.RawMessage("1")
		m.runOp(context.Background(), &Message{ID: &id, Method: "engine:start", Params: json.RawMessage(fmt.Sprintf(`{"engine":"missing","bind":%q}`, bind))})
		if !strings.Contains(out.String(), "unknown engine") {
			t.Fatalf("unexpected response %s", out.String())
		}
	}
}

func TestRunOpRejectsNonLoopbackBindByDefault(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "192.168.1.5", "::", "::ffff:192.168.1.5"} {
		var out bytes.Buffer
		ex := reconciledTestExecutor(NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir()))
		m := NewManager(NewCodec(&out), ex, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // An unavailable setting read must fail closed.
		id := json.RawMessage("1")
		m.runOp(ctx, &Message{ID: &id, Method: "engine:start", Params: json.RawMessage(fmt.Sprintf(`{"engine":"fake","bind":%q}`, bind))})
		if !strings.Contains(out.String(), "bind must be a loopback address") {
			t.Errorf("bind %s was not rejected: %s", bind, out.String())
		}
	}
}
