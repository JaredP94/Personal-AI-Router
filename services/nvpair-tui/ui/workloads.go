// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workload is the subset of the workload-manager's object the view shows.
type workload struct {
	ID             string `json:"id"`
	Model          string `json:"model"`
	Engine         string `json:"engine"`
	State          string `json:"state"`
	OriginatedFrom string `json:"originatedFrom"`
	ScheduledOn    string `json:"scheduledOn"`
	CreatedAt      int64  `json:"createdAt"` // Unix millis
}

// workloadsView renders an authoritative baseline plus live deltas.
// The root recovery owner replaces the baseline and reapplies concurrent pushes.
type workloadsView struct {
	client *rpc.Client
	table  table.Model
	order  []string
	byKey  map[string]workload
	status string

	// Name sources are replaced by the same recovery snapshots and live
	// notifications as the Nodes and Cluster tabs. Workloads retain the UUID.
	discovered []availableNode
	members    []clusterNode
	identity   clusterIdentity

	width, height int
}

type workloadsSubscribedMsg struct{ err error }

func newWorkloadsView(client *rpc.Client) *workloadsView {
	v := &workloadsView{client: client, byKey: map[string]workload{}}
	v.table = newTable(nil)
	return v
}

func (v *workloadsView) Title() string { return "Workloads" }

func (v *workloadsView) Init() tea.Cmd {
	return call(v.client, "workloads:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
		return workloadsSubscribedMsg{err: err}
	})
}

func (v *workloadsView) SetSize(w, h int) {
	v.width, v.height = w, h
	const engine, state, age = 10, 10, 6
	// Each of the seven cells has two columns of horizontal padding.
	available := w - engine - state - age - 14
	id := clampWidth(available/5, 8)
	origin := clampWidth(available/5, 8)
	destination := clampWidth(available/5, len("DESTINATION"))
	model := clampWidth(available-id-origin-destination, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ID", Width: id},
		{Title: "MODEL", Width: model},
		{Title: "ENGINE", Width: engine},
		{Title: "STATE", Width: state},
		{Title: "ORIGIN", Width: origin},
		{Title: "DESTINATION", Width: destination},
		{Title: "AGE", Width: age},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-1, 1))
}

func (v *workloadsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case discoverySnapshotMsg:
		v.discovered = msg.nodes
		v.refreshRows()
		return nil
	case clusterNodesMsg:
		if msg.err == nil {
			v.members = msg.nodes
			v.refreshRows()
		}
		return nil
	case clusterIdentityMsg:
		if msg.err == nil {
			v.identity = msg.id
			v.refreshRows()
		}
		return nil
	case workloadsSnapshotMsg:
		v.order = nil
		v.byKey = map[string]workload{}
		for _, w := range msg.workloads {
			v.upsert(w)
		}
		v.refreshRows()
		return nil
	case workloadsSubscribedMsg:
		if msg.err != nil {
			v.status = "workloads subscribe failed: " + msg.err.Error()
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
		case "discovery:nodes-changed":
			var nodes []availableNode
			if err := decodeParams(msg.Msg.Params, &nodes); err != nil {
				return nil
			}
			v.discovered = nodes
			v.refreshRows()
		case "nodes:changed":
			var p struct {
				Nodes []clusterNode `json:"nodes"`
			}
			if err := decodeParams(msg.Msg.Params, &p); err != nil {
				return nil
			}
			v.members = p.Nodes
			v.refreshRows()
		case "workloads:upsert":
			var p struct {
				WorkloadInfo workload `json:"workloadInfo"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.upsert(p.WorkloadInfo)
		case "workloads:remove":
			var p struct {
				WorkloadID     string `json:"workloadId"`
				OriginatedFrom string `json:"originatedFrom"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.remove(workloadKey(p.OriginatedFrom, p.WorkloadID))
		}
		return nil

	case tea.KeyMsg:
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

func (v *workloadsView) upsert(w workload) {
	key := workloadKey(w.OriginatedFrom, w.ID)
	if _, ok := v.byKey[key]; !ok {
		v.order = append(v.order, key)
	}
	v.byKey[key] = w
	v.refreshRows()
}

func (v *workloadsView) remove(key string) {
	if _, ok := v.byKey[key]; !ok {
		return
	}
	delete(v.byKey, key)
	for i, k := range v.order {
		if k == key {
			v.order = append(v.order[:i], v.order[i+1:]...)
			break
		}
	}
	v.refreshRows()
}

func (v *workloadsView) refreshRows() {
	rows := make([]table.Row, 0, len(v.order))
	for _, k := range v.order {
		w := v.byKey[k]
		rows = append(rows, table.Row{
			truncate(w.ID, 12),
			w.Model,
			w.Engine,
			w.State,
			v.nodeLabel(w.OriginatedFrom),
			v.nodeLabel(w.ScheduledOn),
			ageLabel(w.CreatedAt),
		})
	}
	v.table.SetRows(rows)
}

// nodeLabel resolves stable node UUIDs, never service IDs or addresses. A node
// leaving discovery or membership cannot erase a workload's routing attribution.
func (v *workloadsView) nodeLabel(uuid string) string {
	return nodeLabel(uuid, v.identity, v.discovered, v.members)
}

func (v *workloadsView) View() string {
	if v.status != "" {
		return statusErrStyle.Render(v.status)
	}
	if len(v.order) == 0 {
		return footerStyle.Render("No active workloads. Live cluster workloads will appear here as they run.")
	}
	return v.table.View()
}

func (v *workloadsView) Help() []key.Binding { return nil }

func workloadKey(origin, id string) string { return origin + "/" + id }
