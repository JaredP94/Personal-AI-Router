// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestLlamaLifecycleNotice(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(100, 20)
	v.merge(engineStatus{Engine: "llamacpp", DisplayName: "llama.cpp", Installed: true, Running: true, Port: 8082})
	rendered := v.View()
	for _, notice := range []string{"llama.cpp", "8082", "not notarized", "does not serve requests through PAIR yet"} {
		if !strings.Contains(rendered, notice) {
			t.Fatalf("missing %s in %s", notice, rendered)
		}
	}
}

func TestLlamaUpdateKeepsInstalledTree(t *testing.T) {
	v, c := localEditableEngine(t)
	done := make(chan tea.Msg, 1)
	go func() { done <- v.updateCmd("llamacpp")() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if q.Method != "engine:install" {
		_ = c.RespondError(q.ID, -32000, "fixture refusal")
		<-done
		t.Fatalf("archive update first request = %s; must not uninstall", q.Method)
	}
	if err := c.RespondError(q.ID, -32000, "stop the engine before updating"); err != nil {
		t.Fatal(err)
	}
	result := <-done
	v.Update(result)
	if !strings.Contains(v.status, "stop the engine") {
		t.Fatalf("failure not surfaced: %s", v.status)
	}
}
