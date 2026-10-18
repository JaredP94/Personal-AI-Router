// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// TestHealthRendersBrokerReportedStateVerbatim: the Overview shows exactly
// what workers:get-status / workers:update reported. It invents no row for a
// worker the broker omitted, and it does not translate a state into its own
// opinion about liveness.
func TestHealthRendersBrokerReportedStateVerbatim(t *testing.T) {
	v := newHealthView(nil)
	v.setWorkers([]workerStatus{
		{Name: "scanner", State: "running", Attempts: 0, PID: 41},
		{Name: "proxy", State: "degraded", Attempts: 2, RestartOnHang: false, LastError: "control probe timed out"},
		{Name: "omlx-proxy", State: "missing", Attempts: 0, LastError: "binary not found: /opt/nvpair/omlx-proxy"},
	})
	v.SetSize(80, 21)

	rows := v.table.Rows()
	if len(rows) != 3 {
		t.Fatalf("rows %d, want 3: %v", len(rows), rows)
	}
	if got := rows[0][0]; got != "scanner" {
		t.Errorf("row 0 worker %q, want scanner", got)
	}
	if got := rows[1][1]; got != "degraded" {
		t.Errorf("row 1 state %q, want the broker's own 'degraded'", got)
	}
	if got := rows[1][2]; got != "2" {
		t.Errorf("row 1 attempts %q, want the broker's own 2", got)
	}
	if got := rows[1][3]; got != "control probe timed out" {
		t.Errorf("degraded worker lost its last error: %q", got)
	}
	if got := rows[2][3]; !strings.Contains(got, "binary not found") {
		t.Errorf("missing worker should show its last error, got %q", got)
	}
	for i, row := range rows {
		if row[1] == "ok" || row[1] == "DOWN" || row[1] == "up" {
			t.Errorf("row %d shows the view's own state %q instead of the broker's", i, row[1])
		}
	}
	if rendered := v.View(); !strings.Contains(rendered, "binary not found") {
		t.Errorf("last error is not visible at 80 columns: %q", rendered)
	}
}

// TestRestartConsequenceMatchesDesktopWording: restarting engine-manager or a
// proxy interrupts work the operator cannot see from the process table, so the
// confirmation must say so in the same words the desktop Workers card uses.
func TestRestartConsequenceMatchesDesktopWording(t *testing.T) {
	want := map[string]string{
		"engine-manager":  "Engines PAIR started may keep running or stop; running inference may be interrupted.",
		"proxy":           "Running inference through this proxy will be interrupted.",
		"lmstudio-proxy":  "Running inference through this proxy will be interrupted.",
		"omlx-proxy":      "Running inference through this proxy will be interrupted.",
		"scanner":         "The broker restarts it under its own retry policy.",
		"cluster-manager": "The broker restarts it under its own retry policy.",
	}
	for name, expected := range want {
		if got := restartConsequence(name); got != expected {
			t.Errorf("restartConsequence(%q) = %q, want %q", name, got, expected)
		}
	}
}

// A workers:update push replaces the whole list; a partial one is never merged
// into the previous rows.
func TestHealthWorkersUpdateReplacesRows(t *testing.T) {
	v := newHealthView(nil)
	v.setWorkers([]workerStatus{{Name: "scanner", State: "running"}, {Name: "proxy", State: "running"}})
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "workers:update", Params: json.RawMessage(
		`{"workers":[{"name":"scheduler","state":"exhausted","attempts":6,"lastError":"subprocess scheduler exited unexpectedly"}]}`,
	)}})
	rows := v.table.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows %d, want 1 after a full-snapshot push: %v", len(rows), rows)
	}
	if rows[0][0] != "scheduler" || rows[0][1] != "exhausted" {
		t.Errorf("row not replaced by the push: %v", rows[0])
	}
}

// R with no row selected must not open a prompt or send anything.
func TestHealthRestartWithoutSelectionIsANoOp(t *testing.T) {
	v := newHealthView(nil)
	if cmd := v.Update(keyRunes('R')); cmd != nil {
		t.Error("R with no selected worker issued a command")
	}
	if v.confirm.open() {
		t.Error("R with no selected worker opened a confirmation")
	}
}

func keyRunes(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestHealthConfirmationFitsAboveFullWorkerTable(t *testing.T) {
	v := newHealthView(nil)
	v.SetSize(80, 21)
	v.setWorkers(fixtureWorkers())
	v.table.SetCursor(5) // proxy
	v.Update(keyRunes('R'))
	rendered := v.View()
	if !strings.Contains(rendered, "Restart proxy?") || !strings.Contains(rendered, "Running inference through this proxy will be interrupted.") {
		t.Fatalf("confirmation missing target or warning: %q", rendered)
	}
	if lines := strings.Count(rendered, "\n") + 1; lines > 21 {
		t.Fatalf("confirmation falls below the available content area: %d lines", lines)
	}
}
