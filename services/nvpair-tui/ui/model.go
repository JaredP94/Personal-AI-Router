// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"
	"sync/atomic"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// chromeHeight is the number of rows the shell reserves outside the
// content area: one header line, one tab-bar line, one footer line.
const chromeHeight = 3

// Model is the root Bubble Tea model: a tab bar over a set of Views, a
// header showing broker status, and a footer of contextual help. It owns
// the broker notification loop and routes messages to the views.
type Model struct {
	client       *rpc.Client
	logCh        <-chan string
	epochChanges <-chan string
	keys         globalKeyMap
	help         help.Model

	views  []View
	active int

	width, height int

	ready         bool
	brokerVersion string
	epoch         string
	disconnected  bool
	showFullHelp  bool

	// epochReloading counts the snapshot fetches of the current epoch re-fetch
	// burst that the broker has not answered yet. While it is above zero a burst
	// is in flight and further epoch changes coalesce into it rather than
	// stacking new bursts, so the number of in-flight re-fetches stays bounded
	// by the whitelist however often the broker restarts.
	//
	// It is a pointer and an atomic because the count is retired on the
	// goroutines Bubble Tea runs the fetch commands on, while the guard is
	// consulted in Update, and Model itself is copied between Update calls.
	epochReloading *atomic.Int64
}

// New builds the root model over a connected broker client, the broker's
// captured stderr line channel, the protocol epochs the supervisor published
// for this connection, and the set of views (tabs) to present, in tab order.
func New(client *rpc.Client, logCh <-chan string, epochChanges <-chan string, views []View) Model {
	return Model{
		client:         client,
		logCh:          logCh,
		epochChanges:   epochChanges,
		keys:           newGlobalKeyMap(),
		help:           help.New(),
		views:          views,
		epochReloading: new(atomic.Int64),
	}
}

// Init starts each view and arms the broker notification, epoch and log loops.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{waitForNotification(m.client), waitForLog(m.logCh), waitForEpochChange(m.epochChanges)}
	for _, v := range m.views {
		if c := v.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.resizeViews()
		return m, nil

	case tea.KeyMsg:
		// A view editing a text field (e.g. a port or PIN entry) captures
		// all keys, so global bindings like tab/q don't steal characters
		// mid-input.
		if v := m.activeView(); v != nil {
			if ic, ok := v.(inputCapturer); ok && ic.CapturingInput() {
				return m, v.Update(msg)
			}
		}
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Help):
			m.showFullHelp = !m.showFullHelp
			m.resizeViews()
			return m, nil
		case key.Matches(msg, m.keys.NextTab):
			m.active = (m.active + 1) % len(m.views)
			return m, nil
		case key.Matches(msg, m.keys.PrevTab):
			m.active = (m.active - 1 + len(m.views)) % len(m.views)
			return m, nil
		}
		// Anything else is for the active view only.
		if v := m.activeView(); v != nil {
			return m, v.Update(msg)
		}
		return m, nil

	case NotificationMsg:
		if msg.Msg.Method == "app:ready" {
			m.ready = true
			m.brokerVersion = readyVersion(msg.Msg)
		}
		cmds := m.broadcast(msg)
		cmds = append(cmds, waitForNotification(m.client))
		return m, tea.Batch(cmds...)

	case DisconnectedMsg:
		m.disconnected = true
		return m, nil

	case EpochChangeMsg:
		// The broker announced a new protocol epoch: a different process is
		// answering now, so the rosters every view cached from the old one are
		// suspect. Re-read them, once, read-only.
		m.epoch = msg.Epoch
		if m.epochReloading.Load() == 0 {
			fetches := m.epochSnapshotCmds()
			// Bubble Tea only runs the command returned here, after Update has
			// returned, so arming the guard now cannot race with its own fetches.
			m.epochReloading.Store(int64(len(fetches)))
			return m, tea.Batch(append(fetches, waitForEpochChange(m.epochChanges))...)
		}
		// A burst is already in flight: coalesce. This epoch is recorded as the
		// newest one seen and the running burst covers it, so no second burst
		// stacks on top. Only the listener re-arms.
		return m, waitForEpochChange(m.epochChanges)

	case LogLineMsg:
		cmds := m.broadcast(msg)
		cmds = append(cmds, waitForLog(m.logCh))
		return m, tea.Batch(cmds...)

	case LogClosedMsg:
		return m, nil

	default:
		// Background work (RPC results, ticks, spinner frames) goes to
		// every view; each ignores messages it doesn't own.
		return m, tea.Batch(m.broadcast(msg)...)
	}
}

func (m Model) View() string {
	if m.width == 0 {
		return "starting..."
	}
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteByte('\n')
	b.WriteString(m.tabBarView())
	b.WriteByte('\n')
	if v := m.activeView(); v != nil {
		b.WriteString(v.View())
	}
	b.WriteByte('\n')
	b.WriteString(m.footerView())
	return b.String()
}

func (m Model) activeView() View {
	if m.active < 0 || m.active >= len(m.views) {
		return nil
	}
	return m.views[m.active]
}

// epochSnapshotCmds collects the read-only re-fetch burst for one epoch change:
// one command per whitelisted snapshot, contributed by the views that own it
// (see epochSnapshotRefresher). Each is wrapped so retiring it can release the
// in-flight guard, and each is contributed by the view that already decodes it
// on first load, so there is one path from a broker response to the screen.
func (m *Model) epochSnapshotCmds() []tea.Cmd {
	var cmds []tea.Cmd
	for _, v := range m.views {
		refresher, ok := v.(epochSnapshotRefresher)
		if !ok {
			continue
		}
		for _, cmd := range refresher.epochSnapshotCmds() {
			if cmd == nil {
				continue
			}
			cmds = append(cmds, m.retireOnReturn(cmd))
		}
	}
	return cmds
}

// retireOnReturn runs a burst command and decrements the in-flight guard when
// the broker has answered it — success or failure, since a reply of any kind
// means the snapshot was re-fetched against the current broker.
func (m *Model) retireOnReturn(cmd tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		defer m.epochReloading.Add(-1)
		return cmd()
	}
}

func (m *Model) broadcast(msg tea.Msg) []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.views))
	for _, v := range m.views {
		if c := v.Update(msg); c != nil {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

func (m *Model) resizeViews() {
	footerH := 1
	if m.showFullHelp {
		footerH = len(m.views) // rough room for the full help block
	}
	contentH := m.height - (chromeHeight - 1) - footerH
	if contentH < 1 {
		contentH = 1
	}
	for _, v := range m.views {
		v.SetSize(m.width, contentH)
	}
}

func (m Model) headerView() string {
	left := titleStyle.Render("NVPAIR TUI")
	var status string
	switch {
	case m.disconnected:
		status = statusErrStyle.Render("broker disconnected")
	case m.ready:
		status = statusOKStyle.Render(fmt.Sprintf("broker ready  v%s%s", m.brokerVersion, epochSuffix(m.epoch)))
	default:
		status = footerStyle.Render("connecting to broker...")
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(status)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + status
}

func (m Model) tabBarView() string {
	cells := make([]string, len(m.views))
	for i, v := range m.views {
		label := fmt.Sprintf("%d %s", i+1, v.Title())
		if i == m.active {
			cells[i] = tabActiveStyle.Render(label)
		} else {
			cells[i] = tabInactiveStyle.Render(label)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

func (m Model) footerView() string {
	global := []key.Binding{m.keys.NextTab, m.keys.PrevTab, m.keys.Help, m.keys.Quit}
	var viewKeys []key.Binding
	if v := m.activeView(); v != nil {
		viewKeys = v.Help()
	}
	if m.showFullHelp {
		return m.help.FullHelpView([][]key.Binding{global, viewKeys})
	}
	return m.help.ShortHelpView(append(viewKeys, global...))
}

// epochSuffix renders the broker's protocol epoch for the header, or nothing
// while no epoch is known yet. The header is where a restart that re-fetched
// every snapshot becomes visible without opening the logs, which is the whole
// point of tracking the epoch; the value is a random UUID and carries no prompt,
// message or credential material.
func epochSuffix(epoch string) string {
	const shown = 8
	if epoch == "" {
		return ""
	}
	if len(epoch) > shown {
		epoch = epoch[:shown]
	}
	return "  epoch " + epoch
}

func readyVersion(msg *rpc.Message) string {
	var p struct {
		Version string `json:"version"`
	}
	_ = decodeParams(msg.Params, &p)
	return p.Version
}
