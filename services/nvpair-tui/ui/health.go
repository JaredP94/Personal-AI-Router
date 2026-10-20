// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// healthPollInterval is how often the Overview re-pings the broker for its
// version and uptime.
const healthPollInterval = 5 * time.Second

var healthRestartKey = key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart worker"))

// workerStatus mirrors the broker's WorkerStatus snapshot: the element of
// workers:get-status, of workers:restart, and of every workers:update push.
// The broker owns health, retry and restart policy; this view renders exactly
// what it reports and computes nothing of its own.
type workerStatus struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Attempts      int    `json:"attempts"`
	PID           int    `json:"pid"`
	LastError     string `json:"lastError"`
	RestartOnHang bool   `json:"restartOnHang"`
	Since         string `json:"since"`
}

type workersLoadedMsg struct {
	workers []workerStatus
	err     error
}

type workerRestartMsg struct {
	name string
	err  error
}

// healthView is the Overview tab: broker liveness/version/uptime from periodic
// pings, plus the lifecycle table the broker reports for every worker it
// supervises. The snapshot arrives from the root epoch recovery path
// (workers:get-status) and is kept current by workers:update.
type healthView struct {
	client *rpc.Client
	table  table.Model

	brokerVersion string
	uptime        time.Duration
	pingErr       error

	// workers is the broker's snapshot in its startup order. Both the recovery
	// read and a workers:update push replace it wholesale; a push is a full
	// snapshot, never a delta.
	workers []workerStatus
	status  string
	confirm *confirmPrompt

	width, height int
}

type healthTickMsg struct{}

type healthPingMsg struct {
	version string
	uptime  time.Duration
	err     error
}

func newHealthView(client *rpc.Client) *healthView {
	v := &healthView{client: client}
	v.table = newTable([]table.Column{
		{Title: "WORKER", Width: 20},
		{Title: "STATE", Width: 12},
		{Title: "ATTEMPTS", Width: 8},
		{Title: "LAST ERROR", Width: 32},
	})
	return v
}

func (v *healthView) Title() string { return "Overview" }

func (v *healthView) Init() tea.Cmd {
	return tea.Batch(v.pingCmd(), v.tickCmd())
}

func (v *healthView) pingCmd() tea.Cmd {
	return call(v.client, "ping", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return healthPingMsg{err: err}
		}
		var r struct {
			Version  string `json:"version"`
			UptimeMS int64  `json:"uptime_ms"`
		}
		_ = decodeParams(msg.Result, &r)
		return healthPingMsg{version: r.Version, uptime: time.Duration(r.UptimeMS) * time.Millisecond}
	})
}

func (v *healthView) tickCmd() tea.Cmd {
	return tea.Tick(healthPollInterval, func(time.Time) tea.Msg { return healthTickMsg{} })
}

func (v *healthView) SetSize(w, h int) {
	v.width, v.height = w, h
	// Each of the four columns has one cell of padding on either side.
	const name, state, attempts, padding = 20, 12, 8, 8
	detail := clampWidth(w-name-state-attempts-padding, 10)
	v.table.SetColumns([]table.Column{
		{Title: "WORKER", Width: name},
		{Title: "STATE", Width: state},
		{Title: "ATTEMPTS", Width: attempts},
		{Title: "LAST ERROR", Width: detail},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-3, 1))
}

// CapturingInput lets an open confirmation claim every key, so the root model
// routes q, Tab and Ctrl-C to it instead of the global bindings.
func (v *healthView) CapturingInput() bool { return v.confirm.open() }

func (v *healthView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case healthTickMsg:
		return tea.Batch(v.pingCmd(), v.tickCmd())

	case healthPingMsg:
		v.pingErr = msg.err
		if msg.err == nil {
			v.brokerVersion = msg.version
			v.uptime = msg.uptime
		}
		return nil

	case workersLoadedMsg:
		if msg.err != nil {
			v.status = "load worker status failed: " + msg.err.Error()
			return nil
		}
		v.setWorkers(msg.workers)
		return nil

	case workerRestartMsg:
		if msg.err != nil {
			v.status = fmt.Sprintf("restart %s failed: %s", msg.name, msg.err.Error())
		} else {
			// workers:restart returns once the restart is initiated; the
			// resulting state arrives as a workers:update push.
			v.status = fmt.Sprintf("restart %s requested", msg.name)
		}
		return nil

	case NotificationMsg:
		if msg.Msg.Method == "workers:update" {
			var r struct {
				Workers []workerStatus `json:"workers"`
			}
			if err := decodeParams(msg.Msg.Params, &r); err != nil {
				v.status = "worker status update ignored: " + err.Error()
				return nil
			}
			v.setWorkers(r.Workers)
		}
		return nil

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *healthView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.confirm.open() {
		return v.confirm.Update(msg)
	}
	if key.Matches(msg, healthRestartKey) {
		worker := v.selectedWorker()
		if worker == "" {
			return nil
		}
		// The restart command is built now, so confirming later restarts the
		// worker that was selected when the question was asked.
		v.confirm = newConfirm("Restart "+worker+"?", restartConsequence(worker), v.restartCmd(worker))
		return nil
	}
	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v *healthView) restartCmd(worker string) tea.Cmd {
	return call(v.client, "workers:restart", map[string]string{"name": worker}, func(_ *rpc.Message, err error) tea.Msg {
		return workerRestartMsg{name: worker, err: err}
	})
}

// restartConsequence is the confirmation detail for a worker whose restart
// interrupts work the operator cannot see from the process table. The wording
// matches the desktop Workers card, so both clients ask for the same reason.
func restartConsequence(worker string) string {
	switch worker {
	case "engine-manager":
		return "Engines PAIR started may keep running or stop; running inference may be interrupted."
	case "proxy", "lmstudio-proxy", "omlx-proxy":
		return "Running inference through this proxy will be interrupted."
	}
	return "The broker restarts it under its own retry policy."
}

func (v *healthView) selectedWorker() string {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.workers) {
		return ""
	}
	return v.workers[idx].Name
}

// setWorkers replaces the whole snapshot. A workers:update push is a full
// snapshot, and a recovery read is the broker's complete list, so neither is
// merged into what is already shown.
func (v *healthView) setWorkers(workers []workerStatus) {
	v.workers = workers
	v.refreshRows()
}

func (v *healthView) refreshRows() {
	rows := make([]table.Row, 0, len(v.workers))
	for _, w := range v.workers {
		rows = append(rows, table.Row{w.Name, w.State, strconv.Itoa(w.Attempts), w.LastError})
	}
	v.table.SetRows(rows)
}

func (v *healthView) View() string {
	var b strings.Builder
	if v.pingErr != nil {
		b.WriteString(statusErrStyle.Render("broker not responding: " + v.pingErr.Error()))
	} else {
		summary := fmt.Sprintf("broker v%s  up %s", v.brokerVersion, v.uptime.Round(time.Second))
		b.WriteString(statusOKStyle.Render(summary))
	}
	b.WriteString("\n\n")
	// Show the question instead of appending it below a full-height table,
	// where it would fall outside the terminal's content area.
	if v.confirm.open() {
		b.WriteString(v.confirm.View())
		return b.String()
	}
	if len(v.workers) == 0 {
		b.WriteString(footerStyle.Render("No worker status reported yet."))
	} else {
		b.WriteString(v.table.View())
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *healthView) Help() []key.Binding {
	if v.confirm.open() {
		return v.confirm.Help()
	}
	return []key.Binding{healthRestartKey}
}
