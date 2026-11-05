// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
)

const pullOpTimeout = 32 * time.Minute
const cancelPullTimeout = 15 * time.Second

var engCancelPullKey = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel newest pull"))

type engineDownload struct {
	id, node, engine, model, status string
	canCancel                       bool
	percent                         int
}
type downloadProgress struct {
	OpID      string `json:"opId"`
	Node      string `json:"node"`
	Engine    string `json:"engine"`
	Model     string `json:"model"`
	Stage     string `json:"stage"`
	Message   string `json:"message"`
	Percent   int    `json:"percent"`
	CanCancel bool   `json:"canCancel"`
}
type downloadCancelMsg struct {
	id       string
	accepted bool
	err      error
}

func downloadID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (v *enginesView) downloadIndex(id string) int {
	for i, p := range v.downloads {
		if p.id == id {
			return i
		}
	}
	return -1
}
func (v *enginesView) retireDownload(index int) {
	v.downloads = append(v.downloads[:index], v.downloads[index+1:]...)
}
func (v *enginesView) applyDownloadProgress(p downloadProgress) tea.Cmd {
	i := v.downloadIndex(p.OpID)
	if i < 0 {
		return nil
	}
	active := &v.downloads[i]
	if active.node != p.Node || active.engine != p.Engine || active.model != p.Model {
		return nil
	}
	terminal := p.Stage == "done" || p.Stage == "error" || p.Stage == "cancelled"
	status := p.Stage
	if active.status == "canceling" && !terminal {
		status = "canceling"
	}
	active.status, active.percent, active.canCancel = status, p.Percent, p.CanCancel
	if p.Node == v.target {
		v.status = fmt.Sprintf("pull %s %s: %s", p.Engine, p.Model, status)
		if p.Stage == "cancelled" {
			v.status += ". Partial data is kept so the download can resume."
		}
		if p.Stage == "error" && p.Message != "" {
			v.status += ": " + p.Message
		}
	}
	if terminal {
		v.retireDownload(i)
		if p.Node == v.target {
			return v.loadModelsCmd()
		}
	}
	return nil
}
func (v *enginesView) cancelDownload() tea.Cmd {
	for i := len(v.downloads) - 1; i >= 0; i-- {
		p := v.downloads[i]
		if p.node != v.target || p.engine != v.selectedEngine() || !p.canCancel {
			continue
		}
		v.downloads[i].status = "canceling"
		v.status = fmt.Sprintf("Canceling %s %s...", p.engine, p.model)
		method := "engine:cancel-pull"
		params := map[string]string{"engine": p.engine, "model": p.model, "opId": p.id}
		if p.node != "" {
			method = "engine:remote-cancel-pull"
			params["node"] = p.node
		}
		return callWithTimeout(v.client, method, params, cancelPullTimeout, func(msg *rpc.Message, err error) tea.Msg {
			var result struct {
				Accepted bool `json:"accepted"`
			}
			if err == nil {
				err = decodeParams(msg.Result, &result)
			}
			return downloadCancelMsg{id: p.id, accepted: result.Accepted, err: err}
		})
	}
	v.status = "No cancellable download on this engine"
	return nil
}
func (v *enginesView) downloadsView() string {
	var lines []string
	for _, p := range v.downloads {
		if p.node != v.target {
			continue
		}
		line := fmt.Sprintf("%s %s: %s", p.engine, p.model, p.status)
		if p.status != "canceling" && p.percent > 0 {
			line += fmt.Sprintf(" (%d%%)", p.percent)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
