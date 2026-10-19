// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"
)

// maxSetupAttempts bounds connection setup per transport: the initial attempt
// plus retries driven by later inbound traffic.
const maxSetupAttempts = 3

type connectionSetupMsg struct{ err error }
type discoverySnapshotMsg struct{ nodes []availableNode }
type workloadsSnapshotMsg struct{ workloads []workload }
type recoveryResultMsg struct {
	generation uint64
	epoch      string
	results    []tea.Msg
	err        error
}

// recoveryRead fetches one domain's baseline and returns the message its views
// consume. A failed getter yields that domain's ordinary error message (which the
// views already treat as a no-op or a status line), or nil when the domain has
// none; it never aborts the rest of the burst.
type recoveryRead func() tea.Msg

func (m Model) setupConnection() tea.Cmd {
	return func() tea.Msg {
		methods := []string{"discovery:subscribe", "workloads:subscribe", "engine:subscribe", "proxy:subscribe", "lmstudio-proxy:subscribe", "omlx-proxy:subscribe"}
		for _, method := range methods {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			_, err := m.client.Call(ctx, method, nil)
			cancel()
			if err != nil {
				return connectionSetupMsg{err}
			}
		}
		return connectionSetupMsg{}
	}
}

func snapshotQuery[T any](client *rpc.Client, method string, convert func(T) tea.Msg, failed func(error) tea.Msg) recoveryRead {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		msg, err := client.Call(ctx, method, nil)
		if err == nil {
			var value T
			if err = decodeParams(msg.Result, &value); err == nil {
				return convert(value)
			}
			err = fmt.Errorf("decode %s snapshot: %w", method, err)
		}
		if failed == nil {
			return nil
		}
		return failed(err)
	}
}

func (m *Model) beginRecovery() tea.Cmd {
	if !m.connectionSetup || m.disconnected || m.epoch == "" || m.hydratedEpoch == m.epoch || m.recoveryActive {
		return nil
	}
	m.recoveryActive = true
	m.recoveryGeneration++
	m.recoveryPushes = map[string]NotificationMsg{}
	generation, epoch := m.recoveryGeneration, m.epoch
	client := m.client
	reads := []recoveryRead{
		snapshotQuery(client, "discovery:get-nodes", func(v struct {
			Nodes []availableNode `json:"nodes"`
		}) tea.Msg {
			return discoverySnapshotMsg{v.Nodes}
		}, nil),
		snapshotQuery(client, "workloads:get-initial", func(v struct {
			Workloads []workload `json:"workloads"`
		}) tea.Msg {
			return workloadsSnapshotMsg{v.Workloads}
		}, nil),
		snapshotQuery(client, "nodes:get-initial", func(v struct {
			Nodes []clusterNode `json:"nodes"`
		}) tea.Msg {
			return clusterNodesMsg{nodes: v.Nodes}
		}, func(err error) tea.Msg { return clusterNodesMsg{err: err} }),
		snapshotQuery(client, "cluster:get-node-id", func(v clusterIdentity) tea.Msg { return clusterIdentityMsg{id: v} },
			func(err error) tea.Msg { return clusterIdentityMsg{err: err} }),
		snapshotQuery(client, "errors:get-initial", func(v []svcerrors.ServiceError) tea.Msg {
			raw, _ := json.Marshal(v)
			return NotificationMsg{&rpc.Message{Method: "errors:update", Params: raw}}
		}, func(err error) tea.Msg { return errorsLoadedMsg{err: err} }),
		snapshotQuery(client, "workers:get-status", func(v struct {
			Workers []workerStatus `json:"workers"`
		}) tea.Msg {
			return workersLoadedMsg{workers: v.Workers}
		}, func(err error) tea.Msg { return workersLoadedMsg{err: err} }),
	}
	// Other actual cached getters consumed by tabs, once each. These commands
	// only decode; results are retained here, never applied off the update loop.
	// Their error results go to the owning view like any other result: an
	// optional worker that is unavailable is a steady state, not a dirty epoch.
	for _, view := range m.views {
		var commands []tea.Cmd
		switch v := view.(type) {
		case *enginesView:
			commands = append(commands, v.loadCmd())
		case *proxiesView:
			for i := range v.engines {
				commands = append(commands, v.statusCmd(i), v.nodesCmd(i), v.selectedCmd(i))
			}
		case *manualView:
			commands = append(commands, v.listCmd())
		case *settingsView:
			for i := range v.items {
				commands = append(commands, v.loadCmd(i))
			}
		}
		for _, cmd := range commands {
			reads = append(reads, recoveryRead(cmd))
		}
	}
	return func() tea.Msg {
		result := recoveryResultMsg{generation: generation, epoch: epoch}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		_, result.err = client.Call(ctx, "broker:resync", nil)
		cancel()
		// Only a failed resync leaves the epoch dirty for a retry on later traffic.
		// Each getter's outcome, success or error, is that domain's result.
		if result.err != nil {
			return result
		}
		for _, read := range reads {
			if msg := read(); msg != nil {
				result.results = append(result.results, msg)
			}
		}
		return result
	}
}

func proxyPush(method string) bool {
	return strings.HasPrefix(method, "proxy:") || strings.HasPrefix(method, "lmstudio-proxy:") || strings.HasPrefix(method, "omlx-proxy:")
}

func (m Model) recoveryResultSuperseded(result tea.Msg) bool {
	idx := -1
	switch result := result.(type) {
	case proxyStatusMsg:
		idx = result.idx
	case proxyNodesMsg:
		idx = result.idx
	case proxySelectedMsg:
		idx = result.idx
	}
	if idx < 0 {
		return false
	}
	prefixes := []string{"proxy:", "lmstudio-proxy:", "omlx-proxy:"}
	for _, push := range m.recoveryPushes {
		if strings.HasPrefix(push.Msg.Method, prefixes[idx]) {
			return true
		}
	}
	return false
}

// Only cached facts need replay; latest per identity bounds the journal and
// preserves workload removals rather than resurrecting a stale baseline row.
func recoveryPushKey(msg NotificationMsg) string {
	method := msg.Msg.Method
	// This classifies cached snapshots for replay, not inbound RPC methods.
	if method == "discovery:nodes-changed" || method == "nodes:changed" || method == "errors:update" || method == "cluster:identity-changed" || method == "workers:update" {
		return method
	}
	switch method {
	case "engine:state-changed":
		var e engineStatus
		_ = decodeParams(msg.Msg.Params, &e)
		return method + ":" + e.Engine
	case "workloads:upsert":
		var p struct {
			WorkloadInfo workload `json:"workloadInfo"`
		}
		_ = decodeParams(msg.Msg.Params, &p)
		return "workload:" + workloadKey(p.WorkloadInfo.OriginatedFrom, p.WorkloadInfo.ID)
	case "workloads:remove":
		var p struct {
			WorkloadID     string `json:"workloadId"`
			OriginatedFrom string `json:"originatedFrom"`
		}
		_ = decodeParams(msg.Msg.Params, &p)
		return "workload:" + workloadKey(p.OriginatedFrom, p.WorkloadID)
	}
	if proxyPush(method) {
		return method
	}
	return ""
}
