// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"nvpair-tui/rpc"
	"strings"
	"testing"
)

const llamaPeerInventory = `[{"hostUuid":"peer-uuid","name":"GPU-box","trusted":true,"identityVerification":"verified","peerRecordId":"proof","modelsByEngine":{"llamacpp":["served-only"]},"downloadedByEngine":{"llamacpp":[{"id":"ggml-org/Demo:Q4_K_M","file":"cache.gguf","size":42,"sha256":"verified","owned":true},{"id":"manual.gguf","file":"manual.gguf","size":24,"owned":false}]}}]`

func TestLlamaPeerInstalledListAndDelete(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	modelEngine(v, "llamacpp")
	v.table.SetCursor(len(v.order) - 1)
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(llamaPeerInventory)}})
	if cmd := v.Update(engineKey("m")); cmd != nil {
		t.Fatal("peer list must use discovery, not local cache RPC")
	}
	if view := v.View(); !strings.Contains(view, "ggml-org/Demo:Q4_K_M") || !strings.Contains(view, "manual.gguf") || strings.Contains(view, "served-only") || strings.Contains(view, "unavailable; served") {
		t.Fatalf("wrong peer list: %s", view)
	}
	v.Update(engineKey("d"))
	if !v.confirm.open() {
		t.Fatal("owned stopped peer cache row cannot be deleted")
	}
	reply := engineRequest(t, v.Update(engineKey("y")), c, "engine:remote-delete-model", `{"node":"peer-uuid","engine":"llamacpp","model":"ggml-org/Demo:Q4_K_M"}`, `{"ok":true}`)
	refresh := v.Update(reply)
	v.Update(engineRequest(t, refresh, c, "discovery:get-nodes", `null`, `{"nodes":[{"hostUuid":"peer-uuid","trusted":true,"identityVerification":"verified","peerRecordId":"proof","downloadedByEngine":{"llamacpp":[]}}]}`))
	if !strings.Contains(v.View(), "No models") {
		t.Fatalf("delete retained rows: %s", v.View())
	}
}

func TestLlamaPeerDeleteRunningUnownedAndPreviewGuards(t *testing.T) {
	v, c := engineWire(t)
	switchEnginePeer(t, v, c)
	modelEngine(v, "llamacpp")
	v.table.SetCursor(len(v.order) - 1)
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(llamaPeerInventory)}})
	v.Update(engineKey("m"))
	e := v.byName["llamacpp"]
	e.Running = true
	v.merge(e)
	v.Update(engineKey("d"))
	if v.confirm.open() || !strings.Contains(v.status, "Stop llama.cpp") {
		t.Fatalf("missing running refusal: %s", v.status)
	}
	e.Running = false
	v.merge(e)
	v.models.table.SetCursor(1)
	v.Update(engineKey("d"))
	if v.confirm.open() || !strings.Contains(v.status, "PAIR-owned") {
		t.Fatalf("unrecorded file deletable: %s", v.status)
	}
	unverified := strings.ReplaceAll(llamaPeerInventory, `"identityVerification":"verified"`, `"identityVerification":"unverified"`)
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: json.RawMessage(unverified)}})
	if strings.Contains(v.modelsView(), "ggml-org/Demo") || strings.Contains(v.modelsView(), "manual.gguf") {
		t.Fatal("preview acquired cache inventory")
	}
}
