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
	ModelsByEngine         map[string][]string     `json:"modelsByEngine"`
	LoadedByEngine         map[string][]string     `json:"loadedByEngine"`
	DownloadedByEngine     map[string][]llamaModel `json:"downloadedByEngine"`
	DownloadErrorsByEngine map[string]string       `json:"downloadErrorsByEngine,omitempty"`
}

type modelsPane struct {
	loading             bool
	catalog             bool
	searching           bool
	query               string
	llama               []llamaModel
	selectionGeneration uint64
	open                bool
	table               table.Model
	inventory           modelSnapshot
	generation          uint64
	err                 error
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
	opID                        string
	err                         error
}

var (
	engModelsKey      = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "models"))
	engDeleteModelKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete model"))
)

func (v *enginesView) sizeModels(w, h int) {
	v.models.table.SetColumns([]table.Column{{Title: "MODEL", Width: clampWidth(w-16, 10)}, {Title: "STATE", Width: 12}})
	v.models.table.SetWidth(w)
	v.models.table.SetHeight(clampWidth(h-3, 1))
}

// Local inventory has a normalized RPC. Peer inventory is supplied only by
// discovery; never invent a remote list-models call or query the local engine
// for facts about a peer.
func (v *enginesView) loadModelsCmd() tea.Cmd {
	if v.selectedEngine() == "llamacpp" && (v.target == "" || v.models.catalog) {
		return v.loadLlamaModelsCmd()
	}
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
			inventory := modelSnapshot{ModelsByEngine: n.ModelsByEngine, LoadedByEngine: n.LoadedByEngine}
			if n.IdentityVerification == "verified" && n.Trusted && n.PeerRecordID != "" {
				inventory.DownloadedByEngine = n.DownloadedByEngine
				inventory.DownloadErrorsByEngine = n.DownloadErrorsByEngine
			}
			return inventory
		}
	}
	return modelSnapshot{}
}

func (v *enginesView) peerModelsChanged() {
	if v.selectedEngine() == "llamacpp" && v.models.catalog {
		return
	}
	if v.target != "" {
		v.models.generation++ // invalidate any getter issued before this full push
		v.models.err = nil
		v.models.inventory = peerInventory(v.target, v.discovered)
		v.refreshModels()
	}
}

func (v *enginesView) refreshModels() {
	if v.selectedEngine() == "llamacpp" {
		if v.target != "" && !v.models.catalog {
			v.models.llama = append([]llamaModel(nil), v.models.inventory.DownloadedByEngine["llamacpp"]...)
			for i := range v.models.llama {
				if !v.models.llama[i].Owned {
					v.models.llama[i].SHA256 = ""
				}
			}
		}
		v.refreshLlamaRows()
		return
	}
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
	if !v.byName[v.selectedEngine()].Capabilities.ListModels {
		return ""
	}
	row := v.models.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (v *enginesView) toggleModels() tea.Cmd {
	// A closed pane abandons its request; reopening peer inventory may not
	// issue another RPC, so neither loading nor a late catalog may survive.
	v.models.generation++
	v.models.loading = false
	v.models.llama = nil
	v.models.open = !v.models.open
	if !v.models.open {
		return nil
	}
	v.models.err = nil
	v.models.catalog = false
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
	if v.selectedEngine() == "llamacpp" && v.byName["llamacpp"].Capabilities.ListModels {
		if v.models.catalog {
			out = "Catalog — llama.cpp (single-file Q4_K_M; search: 100 characters / 50 results)\n"
		}
		if v.models.loading {
			return out + footerStyle.Render("Loading models…")
		}
		if v.models.err != nil {
			return out + statusErrStyle.Render(v.models.err.Error())
		}
		if !v.models.catalog && v.models.inventory.DownloadErrorsByEngine["llamacpp"] == "cache_unavailable" {
			return out + footerStyle.Render("Couldn't read llama.cpp's downloaded models")
		}
		if len(v.models.llama) == 0 {
			return out + footerStyle.Render("No models")
		}
		return out + v.models.table.View()
	}
	if !v.byName[v.selectedEngine()].Capabilities.ListModels {
		return out + footerStyle.Render("Model list unavailable (engine stopped or unsupported)")
	}
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
	if engine == "llamacpp" {
		if e.Running {
			v.status = "Stop llama.cpp before deleting a model."
			return nil
		}
		entry, ok := v.selectedLlamaEntry()
		if v.models.catalog || !ok || entry.SHA256 == "" {
			v.status = "Only PAIR-owned installed models can be deleted."
			return nil
		}
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
	if msg.action == "pull" && msg.opID != "" {
		i := v.downloadIndex(msg.opID)
		if i < 0 {
			return nil
		}
		if v.downloads[i].status == "canceling" {
			return nil
		}
		v.retireDownload(i)
	}
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
