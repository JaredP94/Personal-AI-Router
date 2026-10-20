// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelInventoryRequiresListCapability(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, wire := range []string{
			`{"engine":"older-peer"}`,
			`{"engine":"older-peer","capabilities":{"list_models":false,"delete_model":true}}`,
		} {
			v, c := engineWire(t)
			if remote {
				v.target = "peer-uuid"
			}
			var engine engineStatus
			if err := json.Unmarshal([]byte(wire), &engine); err != nil {
				t.Fatal(err)
			}
			v.merge(engine)
			for _, binding := range v.Help() {
				if binding.Help().Desc == "models" {
					t.Error("help offered model inventory without list_models")
				}
			}
			if remote {
				v.Update(discoverySnapshotMsg{nodes: []availableNode{{HostUUID: "peer-uuid", ModelsByEngine: map[string][]string{"older-peer": {"cached-model"}}}}})
				v.Update(engineKey("m"))
			} else {
				openLocalModels(t, v, c, `{"modelsByEngine":{"older-peer":["cached-model"]}}`)
			}
			if got := v.View(); strings.Contains(got, "cached-model") || !strings.Contains(got, "Model list unavailable (engine stopped or unsupported)") {
				t.Fatalf("unreported inventory displayed remote=%v: %s", remote, got)
			}
			if v.selectedModel() != "" {
				t.Fatal("unavailable cached inventory remains selectable")
			}
			if cmd := v.Update(engineKey("d")); cmd != nil || v.confirm.open() {
				t.Fatal("delete acted on an unavailable cached model")
			}
			// Capabilities can change while an already-open pane retains inventory.
			engine.Capabilities.ListModels = true
			v.merge(engine)
			v.refreshModels()
			if !strings.Contains(v.View(), "cached-model") {
				t.Fatal("supported inventory unavailable")
			}
			engine.Capabilities.ListModels = false
			v.merge(engine)
			if strings.Contains(v.View(), "cached-model") || v.selectedModel() != "" {
				t.Fatal("revoked list capability retained visible/selectable inventory")
			}
		}
	}
}
