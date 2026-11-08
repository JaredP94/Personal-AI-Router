// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"

	settings "nvpair-shared/enginesettings"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

type enginePortSnapshots map[string]settings.Snapshot

type enginePortsMsg struct {
	generation uint64
	snapshot   settings.Snapshot
	err        error
}

// The TUI's peer engine status has only one port, not a settings snapshot.
// Read local configured/effective pairs and keep them live via settings pushes.
func (v *enginesView) loadEnginePortsCmd() tea.Cmd {
	if v.target != "" {
		return nil
	}
	generation := v.loadGeneration
	var cmds []tea.Cmd
	for _, engine := range v.order {
		cmds = append(cmds, call(v.client, "engine:get-settings", map[string]string{"engine": engine}, func(msg *rpc.Message, err error) tea.Msg {
			var snapshot settings.Snapshot
			if err == nil {
				err = decodeParams(msg.Result, &snapshot)
			}
			return enginePortsMsg{generation: generation, snapshot: snapshot, err: err}
		}))
	}
	return tea.Batch(cmds...)
}

func (v *enginesView) receiveEnginePorts(snapshot settings.Snapshot) {
	// The broker uses the stable host UUID, as does local exclusion in cycleTarget.
	// Peer streams carry their own nodeId and independent epoch/sequence values.
	if v.identity.NodeUUID == "" || snapshot.NodeID != v.identity.NodeUUID {
		return
	}
	if v.enginePorts == nil {
		v.enginePorts = enginePortSnapshots{}
	}
	current, exists := v.enginePorts[snapshot.Engine]
	if exists && current.Epoch == snapshot.Epoch && current.Sequence >= snapshot.Sequence {
		return
	}
	v.enginePorts[snapshot.Engine] = snapshot
}

func (v *enginesView) enginePortNotes() string {
	if v.target != "" {
		return ""
	}
	snapshot, ok := v.enginePorts[v.selectedEngine()]
	if !ok {
		return ""
	}
	serverPort := 0
	if snapshot.Running {
		serverPort = snapshot.EffectiveServerPort
	}
	var out strings.Builder
	for _, port := range []struct {
		label                 string
		configured, effective int
	}{
		{"Server port", snapshot.Settings.ServerPort, serverPort},
		{"Proxy port", snapshot.Settings.ProxyPort, snapshot.EffectiveProxyPort},
	} {
		if port.configured > 0 && port.effective > 0 && port.configured != port.effective {
			fmt.Fprintf(&out, "\n%s: Running on port %d (saved setting: %d).", port.label, port.effective, port.configured)
		}
	}
	return out.String()
}
