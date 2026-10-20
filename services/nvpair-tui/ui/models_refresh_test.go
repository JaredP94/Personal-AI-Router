// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

func TestModelConfirmSurvivesEngineRemoval(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	openLocalModels(t, v, c, modelInventory)
	v.Update(engineKey("d"))
	// Engine snapshots can change while a destructive confirmation is open.
	v.Update(enginesLoadedMsg{generation: v.loadGeneration, engines: nil})
	if !v.CapturingInput() || !strings.Contains(v.View(), "Delete model?") || !strings.Contains(v.View(), "alpha") {
		t.Fatalf("invisible frozen confirmation: %s", v.View())
	}
	engineRequest(t, v.Update(engineKey("y")), c, "engine:action", `{"engine":"lmstudio","action":"delete_model","params":{"model":"alpha"}}`, `null`)
	if cmd := v.Update(engineKey("y")); cmd != nil {
		t.Fatal("repeat confirm repeated delete")
	}
}

func TestModelInventoryRowsMarkOnlyLoadedModels(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	openLocalModels(t, v, c, modelInventory)
	// Check rendered rows, not the word "loaded" anywhere in the pane.
	for _, line := range strings.Split(v.View(), "\n") {
		if strings.Contains(line, "alpha") && strings.Contains(line, "loaded") {
			t.Fatalf("unloaded marked loaded: %s", line)
		}
		if strings.Contains(line, "beta") && !strings.Contains(line, "loaded") {
			t.Fatalf("loaded marker missing: %s", line)
		}
	}
}

func TestModelInventoryFailureAndRecovery(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	cmd := v.Update(engineKey("m"))
	if cmd == nil {
		t.Fatal("missing inventory getter")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RespondError(q.ID, -32000, "engine worker unavailable"); err != nil {
		t.Fatal(err)
	}
	v.Update(<-done)
	if !strings.Contains(v.View(), "engine worker unavailable") || strings.Contains(v.View(), "No models") {
		t.Fatalf("inventory error hidden: %s", v.View())
	}
	v.Update(engineKey("m"))
	openLocalModels(t, v, c, modelInventory)
	if strings.Contains(v.View(), "unavailable") || !strings.Contains(v.View(), "alpha") {
		t.Fatalf("inventory recovery failed: %s", v.View())
	}
}

func TestModelDeleteFailureDoesNotClaimDeletion(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	openLocalModels(t, v, c, modelInventory)
	v.Update(engineKey("d"))
	cmd := v.Update(engineKey("y"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RespondError(q.ID, -32000, "model not found"); err != nil {
		t.Fatal(err)
	}
	refresh := v.Update(<-done)
	v.Update(engineRequest(t, refresh, c, "engine:models", `null`, modelInventory))
	if !strings.Contains(v.View(), "model not found") || strings.Contains(v.status, "deleted") {
		t.Fatalf("delete failure misreported: %s", v.View())
	}
}

func TestModelPullFrozenTargetAndStaleReply(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	v.target = "peer-uuid"
	v.Update(engineKey("p"))
	v.input.SetValue("alpha")
	// Notifications and other async updates cannot retarget the input prompt.
	v.merge(engineStatus{Engine: "other"})
	v.table.SetCursor(1)
	cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next := v.Update(engineKey("n"))
	reply := engineRequest(t, cmd, c, "engine:remote-pull-model", `{"node":"peer-uuid","engine":"lmstudio","model":"alpha"}`, `{"opId":"p","result":null}`)
	if cmd := v.Update(reply); cmd != nil || strings.Contains(v.status, "ok") {
		t.Fatalf("stale operation affected another target: %s", v.status)
	}
	v.Update(engineRequest(t, next, c, "engine:remote-get-installed", `{"node":"offline-uuid"}`, peerEngines))
}

func TestModelStaleInventoryIgnoredAfterTargetCycle(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "lmstudio")
	old := v.Update(engineKey("m"))
	next := v.Update(engineKey("n"))
	// Return to local before the old local getter replies: node equality alone
	// is insufficient; this response belongs to the prior target lifetime.
	v.Update(engineKey("n"))
	local := v.Update(engineKey("n"))
	v.Update(engineRequest(t, old, c, "engine:models", `null`, modelInventory))
	v.Update(engineRequest(t, local, c, "engine:get-installed", `null`, `{"engines":[{"engine":"lmstudio"}]}`))
	if cmd := v.Update(engineKey("m")); cmd == nil {
		t.Fatal("local pane did not fetch inventory")
	}
	if strings.Contains(v.View(), "alpha") {
		t.Fatalf("stale target inventory retained: %s", v.View())
	}
	// Also ensure the abandoned peer getter cannot overwrite the local table.
	v.Update(engineRequest(t, next, c, "engine:remote-get-installed", `{"node":"peer-uuid"}`, peerEngines))
	if v.selectedEngine() != "lmstudio" {
		t.Fatal("stale peer getter replaced local engine")
	}
}

func TestModelPeerPullCompletesOnlyOnReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, c := engineWire(t)
		modelEngine(v, "lmstudio")
		v.target = "peer-uuid"
		v.Update(engineKey("p"))
		v.input.SetValue("alpha")
		cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		q, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:remote-progress", Params: json.RawMessage(`{"node":"peer-uuid","engine":"lmstudio","opId":"pull","stage":"success","percent":100}`)}})
		time.Sleep(40 * time.Second)
		synctest.Wait()
		select {
		case m := <-done:
			t.Fatalf("completed without reply: %#v", m)
		default:
		}
		if strings.Contains(v.status, "ok") || strings.Contains(v.status, "done") {
			t.Fatalf("progress completed pull: %s", v.status)
		}
		if err := c.Respond(q.ID, json.RawMessage(`{"opId":"pull","result":null}`)); err != nil {
			t.Fatal(err)
		}
		refresh := v.Update(<-done)
		if !strings.Contains(v.status, "ok") || refresh == nil {
			t.Fatalf("reply did not complete pull: %s", v.status)
		}
	})
}
