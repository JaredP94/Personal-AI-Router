// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

const peerEngines = `{"engines":[{"engine":"custom","display_name":"Peer engine","installed":true,"running":false,"healthy":false,"port":1234,"capabilities":{"list_models":true,"pull_model":false,"delete_model":false,"delete_restarts_engine":true,"set_port":false,"install":true,"uninstall":true}}]}`

func engineWire(t *testing.T) (*enginesView, *rpc.Codec) {
	t.Helper()
	cli, srv := net.Pipe()
	c := rpc.NewClient(cli, cli)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; cli.Close(); srv.Close() })
	v := newEnginesView(c)
	v.SetSize(80, 21)
	v.Update(clusterIdentityMsg{id: clusterIdentity{NodeUUID: "self"}})
	v.Update(discoverySnapshotMsg{nodes: []availableNode{{ID: "not-the-uuid", HostUUID: "peer-uuid", Name: "GPU-box", Trusted: true}, {HostUUID: "stranger", Name: "Not paired"}}})
	v.Update(clusterNodesMsg{nodes: []clusterNode{{NodeUUID: "self"}, {NodeUUID: "peer-uuid", Name: "Member name"}, {NodeUUID: "offline-uuid", Name: "Offline", State: "connected"}, {Name: "No UUID"}}})
	return v, rpc.NewCodec(srv)
}

func engineKey(k string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)} }

func engineRequest(t *testing.T, cmd tea.Cmd, codec *rpc.Codec, method string, params string, result string) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatalf("%s returned no command", method)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	q, err := codec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if q.Method != method {
		t.Fatalf("method=%s, want %s", q.Method, method)
	}
	var got, want map[string]any
	if err := json.Unmarshal(q.Params, &got); err != nil && params != "null" {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(params), &want); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("params=%s, want %s", a, b)
	}
	if err := codec.Respond(q.ID, json.RawMessage(result)); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func switchEnginePeer(t *testing.T, v *enginesView, c *rpc.Codec) {
	t.Helper()
	msg := engineRequest(t, v.Update(engineKey("n")), c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines)
	v.Update(msg)
	if !strings.Contains(v.Title(), "GPU-box") {
		t.Fatalf("target absent from title: %s", v.Title())
	}
	if len(v.order) != 1 || !strings.Contains(v.View(), "Peer engine") {
		t.Fatalf("peer table: %s", v.View())
	}
	if strings.Contains(v.status, "loading") {
		t.Fatalf("loaded peer still reports pending load: %s", v.status)
	}
}

func TestEnginePeerTargetAndActions(t *testing.T) {
	v, c := engineWire(t)
	// Identity notifications update only clusterId, not the local principal.
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "cluster:identity-changed", Params: json.RawMessage(`{"clusterId":"new-cluster"}`)}})
	switchEnginePeer(t, v, c)
	for _, k := range []string{"r", "u", "U", "e", "p"} {
		if cmd := v.Update(engineKey(k)); cmd != nil || v.CapturingInput() {
			t.Fatalf("peer key %s must not act or prompt", k)
		}
	}
	for _, b := range v.Help() {
		for _, k := range b.Keys() {
			if strings.Contains("ruUep", k) {
				t.Fatalf("unsupported peer help: %s", k)
			}
		}
	}
	for _, a := range []struct{ key, method string }{{"s", "engine:remote-start"}, {"x", "engine:remote-stop"}} {
		reply := engineRequest(t, v.Update(engineKey(a.key)), c, a.method, `{"node":"peer-uuid","engine":"custom"}`, `{"engine":"custom","running":true}`)
		refresh := v.Update(reply)
		v.Update(engineRequest(t, refresh, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines))
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:state-changed", Params: json.RawMessage(`{"engine":"local-only"}`)}})
	if len(v.order) != 1 {
		t.Fatalf("local notification leaked into peer: %v", v.order)
	}
	v.Update(engineRequest(t, v.Update(engineKey("n")), c, "engine:remote-get-installed", `{"node":"offline-uuid"}`, `{"engines":[]}`))
	v.Update(engineRequest(t, v.Update(engineKey("n")), c, "engine:get-installed", `null`, `{"engines":[{"engine":"local-only"}]}`))
	if !strings.Contains(v.Title(), "This node") || v.selectedEngine() != "local-only" {
		t.Fatalf("local cycle: %s %v", v.Title(), v.order)
	}
}

func TestEnginePeerTrustErrorAndStaleLoad(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	// Capture a request, switch to a different target, then deliver the old reply.
	old := v.loadCmd()
	next := v.Update(engineKey("n"))
	stale := engineRequest(t, old, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines)
	v.Update(stale)
	if len(v.order) != 0 {
		t.Fatalf("stale peer load populated new target: %v", v.order)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- next() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RespondError(q.ID, -32000, "peer is not trusted"); err != nil {
		t.Fatal(err)
	}
	v.Update(<-done)
	if !strings.Contains(v.View(), "peer is not trusted") {
		t.Fatalf("trust error hidden: %s", v.View())
	}
	v.Update(engineRequest(t, v.loadCmd(), c, "engine:remote-get-installed", `{"node":"offline-uuid"}`, peerEngines))
	if strings.Contains(v.View(), "not trusted") {
		t.Fatalf("recovered load still reports trust failure: %s", v.View())
	}
}

func TestEngineRemoteInstallRemainsPendingPast35Seconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, c := engineWire(t)
		switchEnginePeer(t, v, c)
		cmd := v.Update(engineKey("i"))
		if cmd == nil {
			t.Fatal("remote install returned no command")
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		q, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if q.Method != "engine:remote-install" || string(q.Params) != `{"engine":"custom","node":"peer-uuid","start":true}` {
			t.Fatalf("install=%s %s", q.Method, q.Params)
		}
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:remote-progress", Params: json.RawMessage(`{"node":"peer-uuid","engine":"custom","opId":"op","op":"install","stage":"success","percent":100}`)}})
		time.Sleep(40 * time.Second)
		synctest.Wait()
		select {
		case m := <-done:
			t.Fatalf("install completed before reply: %#v", m)
		default:
		}
		if strings.Contains(v.status, " ok") || strings.Contains(v.status, "done") {
			t.Fatalf("progress completed install: %s", v.status)
		}
		if err := c.Respond(q.ID, json.RawMessage(`{"opId":"op","status":{"engine":"custom","installed":true}}`)); err != nil {
			t.Fatal(err)
		}
		refresh := v.Update(<-done)
		if !strings.Contains(v.status, "install custom ok") {
			t.Fatalf("reply not terminal: %s", v.status)
		}
		v.Update(engineRequest(t, refresh, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines))
	})
}

func TestEnginePeerOldCapabilitiesAndFrozenAction(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	action := v.Update(engineKey("s"))
	next := v.Update(engineKey("n"))
	reply := engineRequest(t, action, c, "engine:remote-start", `{"node":"peer-uuid","engine":"custom"}`, `{"engine":"custom"}`)
	if cmd := v.Update(reply); cmd != nil || strings.Contains(v.status, " ok") {
		t.Fatalf("old target's action affected selected target: %s", v.status)
	}
	v.Update(engineRequest(t, next, c, "engine:remote-get-installed", `{"node":"offline-uuid"}`, `{"engines":[{"engine":"old-peer"}]}`))
	if cmd := v.Update(engineKey("i")); cmd != nil {
		t.Fatal("older peer install must be capability-gated")
	}
}

func TestEnginePeerActionErrorIsShownAndRefreshes(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	cmd := v.Update(engineKey("s"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RespondError(q.ID, -32000, "peer offline"); err != nil {
		t.Fatal(err)
	}
	refresh := v.Update(<-done)
	if !strings.Contains(v.View(), "peer offline") {
		t.Fatalf("action error hidden: %s", v.View())
	}
	v.Update(engineRequest(t, refresh, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines))
	if !strings.Contains(v.View(), "peer offline") {
		t.Fatalf("status refresh hid action error: %s", v.View())
	}
}

func TestEngineInstallUninstallCapabilities(t *testing.T) {
	for _, wire := range []string{`{"engine":"custom"}`, `{"engine":"custom","capabilities":{"install":false,"uninstall":false}}`} {
		v := newEnginesView(nil)
		v.SetSize(80, 21)
		var e engineStatus
		if err := json.Unmarshal([]byte(wire), &e); err != nil {
			t.Fatal(err)
		}
		v.merge(e)
		for _, k := range []string{"i", "u"} {
			if cmd := v.Update(engineKey(k)); cmd != nil {
				t.Fatalf("unsupported %s dispatched", k)
			}
		}
		for _, b := range v.Help() {
			for _, k := range b.Keys() {
				if k == "i" || k == "u" {
					t.Fatalf("unsupported help %s", k)
				}
			}
		}
	}
}

func TestEngineRootRecoveryRetainsPeerTarget(t *testing.T) {
	cli, srv := net.Pipe()
	client := rpc.NewClient(cli, cli)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); cli.Close(); srv.Close() })
	go client.Run(ctx)
	b := &recoveryBroker{conn: srv, codec: rpc.NewCodec(srv)}
	requests := make(chan *rpc.Message, 128)
	snapshots := map[string]json.RawMessage{
		"engine:get-installed":        json.RawMessage(`{"engines":[{"engine":"local-only"}]}`),
		"engine:remote-get-installed": json.RawMessage(peerEngines),
		"discovery:get-nodes":         json.RawMessage(`{"nodes":[{"hostUuid":"peer-uuid","name":"GPU-box","trusted":true}]}`),
		"nodes:get-initial":           json.RawMessage(`{"nodes":[{"nodeUuid":"peer-uuid","name":"Member name"}]}`),
		"cluster:get-node-id":         json.RawMessage(`{"nodeUuid":"self","name":"Local"}`),
	}
	go func() {
		for {
			q, err := b.codec.Read()
			if err != nil {
				return
			}
			requests <- q
			result := snapshots[q.Method]
			if result == nil {
				result = json.RawMessage(`{}`)
			}
			b.writes.Lock()
			err = b.codec.Respond(q.ID, result)
			b.writes.Unlock()
			if err != nil {
				return
			}
		}
	}()
	v := newEnginesView(client)
	l := newLoop(New(client, nil, []View{v}))
	l.exec(l.m.setupConnection())
	l.until(t, func() bool { return l.m.connectionSetup })
	b.push(t, "A", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.hydratedEpoch == "A" })
	if v.selectedEngine() != "local-only" || v.byName["local-only"].Capabilities.Install {
		t.Fatalf("local recovery or missing capabilities: %v", v.byName)
	}
	l.turn(t, engineKey("n"))
	l.until(t, func() bool { return v.selectedEngine() == "custom" })
	caps := v.byName["custom"].Capabilities
	if !caps.ListModels || caps.PullModel || caps.DeleteModel || !caps.DeleteRestartsEngine || caps.SetPort || !caps.Install || !caps.Uninstall {
		t.Fatalf("lost peer capabilities: %+v", caps)
	}
	b.push(t, "B", 1, "heartbeat", nil)
	l.until(t, func() bool { return l.m.hydratedEpoch == "B" })
	if v.selectedEngine() != "custom" || !strings.Contains(v.Title(), "GPU-box") {
		t.Fatalf("peer recovery: %s %v", v.Title(), v.order)
	}
	// Both the target switch and epoch recovery must query the peer, not local.
	peerReads, localReads := 0, 0
	for len(requests) > 0 {
		q := <-requests
		switch q.Method {
		case "engine:remote-get-installed":
			peerReads++
			if string(q.Params) != `{"node":"peer-uuid"}` {
				t.Fatalf("peer params: %s", q.Params)
			}
		case "engine:get-installed":
			localReads++
		}
	}
	if peerReads != 2 || localReads != 1 {
		t.Fatalf("engine recovery requests: peer=%d local=%d", peerReads, localReads)
	}
}

func TestEnginePeerNamesTrackValidNotifications(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	for _, wire := range []string{`{"bad":"shape"}`, `[{"hostUuid":"peer-uuid","name":"Corrupt"},{"name":true}]`} {
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(wire)}})
		if !strings.Contains(v.Title(), "GPU-box") {
			t.Fatalf("malformed names replaced target label: %s", v.Title())
		}
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[{"hostUuid":"peer-uuid","name":"Renamed","trusted":true}]`)}})
	if !strings.Contains(v.Title(), "Renamed") {
		t.Fatalf("rename not reflected: %s", v.Title())
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(`[]`)}})
	if !strings.Contains(v.Title(), "Member name") {
		t.Fatalf("membership name not retained: %s", v.Title())
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "nodes:changed", Params: json.RawMessage(`{"nodes":[]}`)}})
	if !strings.Contains(v.Title(), "peer-uui") {
		t.Fatalf("departed peer identifier lost: %s", v.Title())
	}
}

func TestEngineLocalActionsKeepWireContract(t *testing.T) {
	v, c := engineWire(t)
	v.Update(engineRequest(t, v.loadCmd(), c, "engine:get-installed", `null`, peerEngines))
	for _, a := range []struct{ key, method string }{{"s", "engine:start"}, {"x", "engine:stop"}, {"r", "engine:restart"}, {"i", "engine:install"}, {"u", "engine:uninstall"}} {
		v.Update(engineRequest(t, v.Update(engineKey(a.key)), c, a.method, `{"engine":"custom"}`, `{"engine":"custom"}`))
	}
	e := v.byName["custom"]
	e.Capabilities.PullModel = true
	v.merge(e)
	if v.Update(engineKey("p")) == nil || !v.CapturingInput() {
		t.Fatal("supported local pull prompt removed")
	}
}
