// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// modelSnapshot preserves map-key presence: a missing engine is unavailable,
// whereas a present empty list is a known empty inventory.
type modelSnapshot struct {
	ModelsByEngine map[string][]string `json:"modelsByEngine"`
	LoadedByEngine map[string][]string `json:"loadedByEngine"`
}

type modelsPane struct {
	open       bool
	table      table.Model
	inventory  modelSnapshot
	generation uint64
	err        error
}

type modelsLoadedMsg struct {
	node       string
	generation uint64
	inventory  modelSnapshot
	nodes      []availableNode
	err        error
}

type modelOpMsg struct {
	node, engine, model, action string
	err                         error
}

var (
	engModelsKey      = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "models"))
	engDeleteModelKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete model"))
)

func (v *enginesView) sizeModels(w, h int) {
	v.models.table.SetColumns([]table.Column{{Title: "MODEL", Width: clampWidth(w-12, 10)}, {Title: "STATE", Width: 8}})
	v.models.table.SetWidth(w)
	v.models.table.SetHeight(clampWidth(h-3, 1))
}

// Local inventory has a normalized RPC. Peer inventory is supplied only by
// discovery; never invent a remote list-models call or query the local engine
// for facts about a peer.
func (v *enginesView) loadModelsCmd() tea.Cmd {
	v.models.generation++
	node, generation := v.target, v.models.generation
	method := "engine:models"
	if node != "" {
		method = "discovery:get-nodes"
	}
	return call(v.client, method, nil, func(msg *rpc.Message, err error) tea.Msg {
		var inventory modelSnapshot
		var nodes []availableNode
		if err == nil {
			if node == "" {
				err = decodeParams(msg.Result, &inventory)
			} else {
				var r struct {
					Nodes []availableNode `json:"nodes"`
				}
				err = decodeParams(msg.Result, &r)
				if err == nil {
					nodes = r.Nodes
					inventory = peerInventory(node, nodes)
				}
			}
		}
		return modelsLoadedMsg{node: node, generation: generation, inventory: inventory, nodes: nodes, err: err}
	})
}

func peerInventory(node string, nodes []availableNode) modelSnapshot {
	for _, n := range nodes {
		if n.HostUUID == node {
			return modelSnapshot{ModelsByEngine: n.ModelsByEngine, LoadedByEngine: n.LoadedByEngine}
		}
	}
	return modelSnapshot{}
}

func (v *enginesView) peerModelsChanged() {
	if v.target != "" {
		v.models.generation++ // invalidate any getter issued before this full push
		v.models.err = nil
		v.models.inventory = peerInventory(v.target, v.discovered)
		v.refreshModels()
	}
}

func (v *enginesView) refreshModels() {
	selected := v.selectedModel()
	rows := []table.Row{}
	for _, name := range v.models.inventory.ModelsByEngine[v.selectedEngine()] {
		state := "—"
		for _, loaded := range v.models.inventory.LoadedByEngine[v.selectedEngine()] {
			if loaded == name {
				state = "loaded"
				break
			}
		}
		rows = append(rows, table.Row{name, state})
	}
	v.models.table.SetRows(rows)
	cursor := 0
	for i, row := range rows {
		if row[0] == selected {
			cursor = i
			break
		}
	}
	v.models.table.SetCursor(cursor)
}

func (v *enginesView) selectedModel() string {
	row := v.models.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (v *enginesView) toggleModels() tea.Cmd {
	v.models.open = !v.models.open
	if !v.models.open {
		return nil
	}
	v.models.err = nil
	if v.target != "" {
		v.models.inventory = peerInventory(v.target, v.discovered)
		v.refreshModels()
		return nil
	}
	v.refreshModels()
	return v.loadModelsCmd()
}

func (v *enginesView) modelsView() string {
	out := "Models — " + v.selectedEngine() + "\n"
	if v.models.err != nil {
		return out + statusErrStyle.Render("load models failed: "+v.models.err.Error())
	}
	models, ok := v.models.inventory.ModelsByEngine[v.selectedEngine()]
	if !ok {
		return out + footerStyle.Render("Model list unavailable (engine stopped or unsupported)")
	}
	if len(models) == 0 {
		return out + footerStyle.Render("No models")
	}
	return out + v.models.table.View()
}

func (v *enginesView) confirmDeleteModel() tea.Cmd {
	engine, model, node := v.selectedEngine(), v.selectedModel(), v.target
	e := v.byName[engine]
	if !v.models.open || model == "" || !e.Capabilities.DeleteModel {
		return nil
	}
	label := "This node"
	if node != "" {
		label = nodeLabel(node, v.identity, v.discovered, v.members)
	}
	detail := fmt.Sprintf("Node: %s; engine: %s; model: %s.", label, engine, model)
	if e.Capabilities.DeleteRestartsEngine {
		detail += "\nThis restarts " + engine + "; running inference will be interrupted."
	}
	method := "engine:action"
	paramKey := "model"
	if engine == "ollama" {
		paramKey = "name"
	}
	var params any = map[string]any{"engine": engine, "action": "delete_model", "params": map[string]string{paramKey: model}}
	if node != "" {
		method = "engine:remote-delete-model"
		params = map[string]string{"node": node, "engine": engine, "model": model}
	}
	// Creating the command performs no I/O. Its immutable inputs are the exact
	// node, engine and model named in the prompt, regardless of later pushes.
	cmd := callWithTimeout(v.client, method, params, lifecycleOpTimeout, func(_ *rpc.Message, err error) tea.Msg {
		return modelOpMsg{node: node, engine: engine, model: model, action: "delete", err: err}
	})
	v.confirm = newConfirm("Delete model?", detail, cmd)
	return nil
}

func (v *enginesView) modelOperationDone(msg modelOpMsg) tea.Cmd {
	if msg.node != v.target {
		return nil
	}
	switch {
	case msg.err == nil:
		v.status = fmt.Sprintf("%s %s %s ok", msg.action, msg.model, msg.engine)
	case msg.action == "delete" && strings.Contains(msg.err.Error(), fmt.Sprintf("action %q: engine %q failed to restart:", "delete_model", msg.engine)):
		// The existing action contract returns an error only after deletion has
		// succeeded in this branch. Do not tell the user to retry the deletion.
		v.status = fmt.Sprintf("%s %s: deleted, restart failed: %s", msg.engine, msg.model, msg.err)
	default:
		v.status = fmt.Sprintf("%s %s %s failed: %s", msg.action, msg.model, msg.engine, msg.err)
	}
	return v.loadModelsCmd()
}
