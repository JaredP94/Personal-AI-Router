// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func destructiveFixture(t *testing.T, kind string) (View, tea.KeyMsg, func(), string, string) {
	t.Helper()
	switch kind {
	case "member", "fallback":
		v := newClusterView(nil)
		v.SetSize(80, 21)
		n := clusterNode{ID: "original-id", Name: "Original member", NodeUUID: "original-uuid"}
		params := `{"nodeUuid":"original-uuid"}`
		if kind == "fallback" {
			n.NodeUUID = ""
			params = `{"nodeId":"original-id"}`
		}
		v.Update(clusterNodesMsg{nodes: []clusterNode{n}})
		return v, engineKey("r"), func() {
			v.Update(clusterNodesMsg{nodes: []clusterNode{{ID: "replacement-id", NodeUUID: "replacement-uuid"}}})
		}, "nodes:remove", params
	case "leave":
		v := newClusterView(nil)
		v.SetSize(80, 21)
		v.Update(clusterIdentityMsg{id: clusterIdentity{NodeUUID: "self", Name: "Original node", ClusterID: "original-cluster"}})
		return v, engineKey("L"), func() { v.Update(clusterNodesMsg{nodes: nil}) }, "cluster:leave", `null`
	case "manual":
		v := newManualView(nil)
		v.SetSize(80, 21)
		v.Update(manualNodesMsg{nodes: []manualNode{{ID: "original-id", Name: "Original manual", Address: "original-host"}}})
		return v, engineKey("r"), func() { v.Update(manualNodesMsg{nodes: nil}) }, "node/remove", `{"id":"original-id"}`
	}
	t.Fatal("unknown fixture")
	return nil, tea.KeyMsg{}, nil, "", ""
}

// Direct dispatch, reading the selection at y, missing input capture, or hiding
// the question after an async list refresh must each fail these tests.
func TestDestructiveActionsConfirmAndFreeze(t *testing.T) {
	for _, kind := range []string{"member", "fallback", "leave", "manual"} {
		t.Run(kind, func(t *testing.T) {
			view, k, replace, method, params := destructiveFixture(t, kind)
			transport, c := engineWire(t)
			switch v := view.(type) {
			case *clusterView:
				v.client = transport.client
			case *manualView:
				v.client = transport.client
			}
			if cmd := view.Update(k); cmd != nil {
				t.Fatal("destructive action sent without confirmation")
			}
			capture, ok := view.(inputCapturer)
			if !ok || !capture.CapturingInput() {
				t.Fatal("confirmation does not capture input")
			}
			if !strings.Contains(view.View(), "original-") {
				t.Fatalf("target not shown: %s", view.View())
			}
			before := view.View()
			for _, k := range []tea.KeyMsg{engineKey("q"), {Type: tea.KeyTab}, engineKey("r")} {
				if cmd := view.Update(k); cmd != nil {
					t.Fatal("prompt allowed unrelated action")
				}
			}
			replace()
			if !strings.Contains(view.View(), "original-") {
				t.Fatalf("frozen target lost: before=%s after=%s", before, view.View())
			}
			reply := engineRequest(t, view.Update(engineKey("y")), c, method, params, `{}`)
			// manual removal schedules an ordinary list refresh; not another removal.
			view.Update(reply)
			if cmd := view.Update(engineKey("y")); cmd != nil {
				t.Fatal("action repeated")
			}
			if capture.CapturingInput() {
				t.Fatal("answered prompt still captures")
			}
		})
	}
}

func TestDestructiveActionsDefaultCancel(t *testing.T) {
	for _, kind := range []string{"member", "leave", "manual"} {
		for _, k := range []tea.KeyMsg{engineKey("n"), {Type: tea.KeyEsc}, enterKey()} {
			view, open, _, _, _ := destructiveFixture(t, kind)
			if cmd := view.Update(open); cmd != nil {
				t.Fatalf("%s sent before y", kind)
			}
			if !view.(inputCapturer).CapturingInput() {
				t.Fatalf("%s prompt absent", kind)
			}
			if cmd := view.Update(k); cmd != nil {
				t.Fatal("cancel sent command")
			}
			if cmd := view.Update(engineKey("y")); cmd != nil {
				t.Fatal("cancelled confirmation sent command")
			}
			if view.(inputCapturer).CapturingInput() {
				t.Fatal("cancel did not close prompt")
			}
		}
	}
}

func TestClusterLeaveDoesNotLeaveReplacementCluster(t *testing.T) {
	view, k, _, _, _ := destructiveFixture(t, "leave")
	v := view.(*clusterView)
	if cmd := v.Update(k); cmd != nil {
		t.Fatal("leave sent before confirmation")
	}
	v.Update(clusterIdentityMsg{id: clusterIdentity{NodeUUID: "self", ClusterID: "different-cluster"}})
	if cmd := v.Update(engineKey("y")); cmd != nil {
		t.Fatal("confirmation left an unconfirmed cluster")
	}
	if !strings.Contains(v.View(), "cluster changed") {
		t.Fatalf("no identity-change explanation: %s", v.View())
	}
}
