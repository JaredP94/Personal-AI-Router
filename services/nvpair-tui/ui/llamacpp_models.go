// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	settings "nvpair-shared/enginesettings"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

var (
	engCatalogKey       = key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "catalog / installed"))
	engCatalogSearchKey = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search catalog"))
	engSelectModelKey   = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "startup model"))
	engRouterKey        = key.NewBinding(key.WithKeys("0"), key.WithHelp("0", "router mode"))
	llamaModelOption    = regexp.MustCompile(`(?:^|\s)(?:--model|-m)(?:=|\s+)(?:"[^"]*"|'[^']*'|[^\s]+)`)
)

type llamaModel struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type llamaModelsMsg struct {
	node       string
	generation uint64
	catalog    bool
	models     []llamaModel
	err        error
}

type llamaSettingsMsg struct {
	node, file, label string
	generation        uint64
	snapshot          settings.Snapshot
	err               error
}

type llamaPreviewMsg struct {
	node, label string
	generation  uint64
	request     settings.Request
	preview     settings.Preview
	err         error
}

func (v *enginesView) loadLlamaModelsCmd() tea.Cmd {
	v.models.generation++
	node, generation, catalog := v.target, v.models.generation, v.models.catalog
	method := "engine:action"
	params := map[string]any{"engine": "llamacpp", "action": "list_downloaded"}
	timeout := 15 * time.Second
	if catalog {
		method, params, timeout = "engine:model-catalog", map[string]any{"query": v.models.query}, 30*time.Second
	}
	return callWithTimeout(v.client, method, params, timeout, func(msg *rpc.Message, err error) tea.Msg {
		var result struct {
			Models []llamaModel `json:"models"`
		}
		if err == nil {
			err = decodeParams(msg.Result, &result)
		}
		return llamaModelsMsg{node: node, generation: generation, catalog: catalog, models: result.Models, err: err}
	})
}

func (v *enginesView) refreshLlamaRows() {
	selected := v.selectedModel()
	models := append([]llamaModel(nil), v.models.llama...)
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	rows := make([]table.Row, 0, len(models))
	for _, model := range models {
		state := "installed"
		if v.models.catalog {
			state = strings.SplitN(model.ID, "/", 2)[0]
		}
		if !v.models.catalog && model.SHA256 == "" {
			state = "unmanaged"
		}
		rows = append(rows, table.Row{model.ID, state})
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

func (v *enginesView) selectedLlamaEntry() (llamaModel, bool) {
	row := v.models.table.SelectedRow()
	if len(row) == 0 {
		return llamaModel{}, false
	}
	for _, model := range v.models.llama {
		if model.ID == row[0] {
			return model, true
		}
	}
	return llamaModel{}, false
}

func (v *enginesView) selectLlamaStartup(router bool) tea.Cmd {
	if v.selectedEngine() != "llamacpp" || v.target != "" || !v.byName["llamacpp"].Capabilities.ListModels {
		return nil
	}
	file, label := "", "No model (router mode)"
	if !router {
		if !v.models.open {
			return nil
		}
		model, ok := v.selectedLlamaEntry()
		if !ok || v.models.catalog {
			return nil
		}
		file, label = model.File, model.ID
	}
	v.models.selectionGeneration++
	generation, node := v.models.selectionGeneration, v.target
	v.status = "Reading startup settings…"
	return call(v.client, "engine:get-settings", map[string]string{"engine": "llamacpp"}, func(msg *rpc.Message, err error) tea.Msg {
		var snapshot settings.Snapshot
		if err == nil {
			err = decodeParams(msg.Result, &snapshot)
		}
		return llamaSettingsMsg{node: node, generation: generation, file: file, label: label, snapshot: snapshot, err: err}
	})
}

func (v *enginesView) previewLlamaStartup(msg llamaSettingsMsg) tea.Cmd {
	if msg.node != v.target || msg.generation != v.models.selectionGeneration || v.selectedEngine() != "llamacpp" {
		return nil
	}
	if msg.err != nil {
		v.status = msg.err.Error()
		return nil
	}
	if !msg.snapshot.Editable || msg.snapshot.Format != "pair-arguments-v1" {
		v.status = "Startup settings are not editable on this device."
		return nil
	}
	config := msg.snapshot.Settings
	config.LaunchText = strings.TrimSpace(llamaModelOption.ReplaceAllString(config.LaunchText, ""))
	if msg.file != "" {
		config.LaunchText = strings.TrimSpace(config.LaunchText + " --model " + strconv.Quote(msg.file))
	}
	request := settings.Request{Engine: "llamacpp", ExpectedRevision: msg.snapshot.Revision, Settings: config}
	return call(v.client, "engine:preview-settings", request, func(reply *rpc.Message, err error) tea.Msg {
		var preview settings.Preview
		if err == nil {
			err = decodeParams(reply.Result, &preview)
		}
		return llamaPreviewMsg{node: msg.node, label: msg.label, generation: msg.generation, request: request, preview: preview, err: err}
	})
}

func (v *enginesView) confirmLlamaStartup(msg llamaPreviewMsg) tea.Cmd {
	if msg.node != v.target || msg.generation != v.models.selectionGeneration || v.selectedEngine() != "llamacpp" {
		return nil
	}
	err := msg.err
	if err == nil {
		err = msg.preview.ValidationError()
	}
	if err == nil && msg.preview.Revision != msg.request.ExpectedRevision {
		err = fmt.Errorf("Settings changed elsewhere; select the startup model again.")
	}
	if err != nil {
		v.status = err.Error()
		return nil
	}
	request := msg.request
	request.Settings, request.RequestID = msg.preview.Settings, downloadID()
	detail := msg.label
	if msg.preview.Restart {
		detail += "\nThis restarts llama.cpp and interrupts inference."
	}
	cmd := callWithTimeout(v.client, "engine:apply-settings", request, 14*time.Minute, func(reply *rpc.Message, err error) tea.Msg {
		if err == nil {
			var receipt struct {
				Phase string `json:"phase"`
			}
			err = decodeParams(reply.Result, &receipt)
			if err == nil && receipt.Phase != "succeeded" {
				err = fmt.Errorf("Settings %s; reload settings before retrying.", receipt.Phase)
			}
		}
		return engineOpMsg{node: msg.node, what: "startup model", engine: "llamacpp", err: err}
	})
	v.status = ""
	v.confirm = newConfirm("Choose startup model?", detail, cmd)
	return nil
}
