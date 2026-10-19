// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// engineCapabilities is policy reported by Go for the engine's own host.
// Missing capabilities on an older peer decode to all-false.
type engineCapabilities struct {
	ListModels           bool `json:"list_models"`
	PullModel            bool `json:"pull_model"`
	DeleteModel          bool `json:"delete_model"`
	DeleteRestartsEngine bool `json:"delete_restarts_engine"`
	SetPort              bool `json:"set_port"`
	Install              bool `json:"install"`
	Uninstall            bool `json:"uninstall"`
}

// engineStatus mirrors the local or peer engine-manager's status snapshot.
type engineStatus struct {
	Engine       string             `json:"engine"`
	DisplayName  string             `json:"display_name"`
	Installed    bool               `json:"installed"`
	Running      bool               `json:"running"`
	Healthy      bool               `json:"healthy"`
	Port         int                `json:"port"`
	Capabilities engineCapabilities `json:"capabilities"`
}

// enginesView manages inference engines on the selected node. Local state is
// kept live by engine notifications; peers are refreshed through the existing
// remote status method on selection and after lifecycle replies. Model inventory
// uses normalized local snapshots or peer discovery, with capability-gated actions.
type enginesView struct {
	client      *rpc.Client
	table       table.Model
	order       []string
	byName      map[string]engineStatus
	status      string
	input       textinput.Model
	pulling     bool
	pullEngine  string
	pullNode    string
	editingPort bool
	portEngine  string
	models      modelsPane
	confirm     *confirmPrompt

	// Empty target means local; peers are always keyed by hostUuid, never name.
	target         string
	discovered     []availableNode
	members        []clusterNode
	identity       clusterIdentity
	loadGeneration uint64

	width, height int
}

type enginesLoadedMsg struct {
	node       string
	generation uint64
	engines    []engineStatus
	err        error
}

type engineOpMsg struct {
	node   string
	what   string
	engine string
	status *engineStatus
	err    error
}

var (
	engTargetKey    = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next node"))
	engStartKey     = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start"))
	engStopKey      = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop"))
	engRestartKey   = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart"))
	engInstallKey   = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install"))
	engUninstallKey = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "uninstall"))
	engPullKey      = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pull model"))
	engPortKey      = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "engine server port"))
	engUpdateKey    = key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "update (reinstall)"))
)

func newEnginesView(client *rpc.Client) *enginesView {
	ti := textinput.New()
	ti.Placeholder = "model name (e.g. llama3.2)"
	v := &enginesView{client: client, byName: map[string]engineStatus{}, input: ti}
	v.table = newTable(nil)
	v.models.table = newTable(nil)
	return v
}

func (v *enginesView) Title() string {
	if v.target == "" {
		return "Engines — This node"
	}
	return "Engines — " + nodeLabel(v.target, v.identity, v.discovered, v.members)
}

func (v *enginesView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "engine:subscribe", nil, func(_ *rpc.Message, _ error) tea.Msg { return nil }),
		v.loadCmd(),
	)
}

func (v *enginesView) loadCmd() tea.Cmd {
	v.loadGeneration++
	node, generation := v.target, v.loadGeneration
	method := "engine:get-installed"
	var params any
	if node != "" {
		method = "engine:remote-get-installed"
		params = map[string]string{"node": node}
	}
	return call(v.client, method, params, func(msg *rpc.Message, err error) tea.Msg {
		var r struct {
			Engines []engineStatus `json:"engines"`
		}
		if err == nil {
			err = decodeParams(msg.Result, &r)
		}
		return enginesLoadedMsg{node: node, generation: generation, engines: r.Engines, err: err}
	})
}

func (v *enginesView) SetSize(w, h int) {
	v.width, v.height = w, h
	const inst, run, heal, port = 10, 8, 8, 7
	// Bubbles pads every column on both sides. Reserve all ten cells so the
	// engine server port stays visible at ordinary terminal widths.
	name := clampWidth(w-inst-run-heal-port-10, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ENGINE", Width: name},
		{Title: "INSTALLED", Width: inst},
		{Title: "RUNNING", Width: run},
		{Title: "HEALTHY", Width: heal},
		{Title: "PORT", Width: port},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-2, 1))
	v.sizeModels(w, h)
}

func (v *enginesView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case discoverySnapshotMsg:
		v.discovered = msg.nodes
		v.peerModelsChanged()
		return nil
	case clusterNodesMsg:
		if msg.err == nil {
			v.members = msg.nodes
		}
		return nil
	case clusterIdentityMsg:
		if msg.err == nil {
			v.identity = msg.id
		}
		return nil
	case enginesLoadedMsg:
		if msg.node != v.target || msg.generation != v.loadGeneration {
			return nil
		}
		if msg.err != nil {
			v.status = "load engines failed: " + msg.err.Error()
			return nil
		}
		if v.status == "loading engines..." || strings.HasPrefix(v.status, "load engines failed:") {
			v.status = ""
		}
		v.order = nil
		v.byName = map[string]engineStatus{}
		for _, e := range msg.engines {
			v.merge(e)
		}
		v.refreshRows()
		if v.table.Cursor() < 0 {
			v.table.SetCursor(0)
		}
		v.refreshModels()
		if v.models.open && v.target == "" {
			return v.loadModelsCmd()
		}
		return nil

	case modelsLoadedMsg:
		if msg.node == v.target && msg.generation == v.models.generation {
			v.models.err = msg.err
			if msg.err == nil {
				v.models.inventory = msg.inventory
				if msg.node != "" {
					v.discovered = msg.nodes
				}
			} else {
				v.models.inventory = modelSnapshot{}
			}
			v.refreshModels()
		}
		return nil

	case modelOpMsg:
		return v.modelOperationDone(msg)

	case engineOpMsg:
		if msg.node != v.target {
			return nil
		}
		if msg.err != nil {
			v.status = fmt.Sprintf("%s %s failed: %s", msg.what, msg.engine, msg.err.Error())
		} else {
			v.status = fmt.Sprintf("%s %s ok", msg.what, msg.engine)
			if msg.status != nil && msg.status.Engine == msg.engine {
				v.merge(*msg.status)
			}
		}
		if msg.node != "" {
			return v.loadCmd()
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
		case "discovery:nodes-changed":
			var nodes []availableNode
			if decodeParams(msg.Msg.Params, &nodes) == nil {
				v.discovered = nodes
				v.peerModelsChanged()
			}
			return nil
		case "nodes:changed":
			var p struct {
				Nodes []clusterNode `json:"nodes"`
			}
			if decodeParams(msg.Msg.Params, &p) == nil {
				v.members = p.Nodes
			}
			return nil
		}
		// Local pushes must never overwrite a peer's authoritative table or
		// operation status. Remote progress is not a completion signal; only
		// the RPC reply completes the pending remote action.
		if v.target != "" {
			return nil
		}
		switch msg.Msg.Method {
		case "engine:models-changed":
			var p struct {
				Models modelSnapshot `json:"models"`
			}
			if decodeParams(msg.Msg.Params, &p) == nil {
				v.models.generation++
				v.models.err = nil
				v.models.inventory = p.Models
				v.refreshModels()
			}
		case "engine:state-changed":
			var e engineStatus
			_ = decodeParams(msg.Msg.Params, &e)
			if e.Engine != "" {
				v.merge(e)
			}
		case "engine:install-progress":
			var p struct {
				Engine  string `json:"engine"`
				Stage   string `json:"stage"`
				Percent int    `json:"percent"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.status = fmt.Sprintf("install %s: %s (%d%%)", p.Engine, p.Stage, p.Percent)
		case "engine:pull-progress":
			var p struct {
				Engine  string `json:"engine"`
				Stage   string `json:"stage"`
				Percent int    `json:"percent"`
				Message string `json:"message"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			// Terminal stages carry no meaningful percent (success is implicitly
			// 100%; error uses -1), so render them as outcomes rather than a
			// misleading "success (0%)". A late failure that arrives after the
			// synchronous call timed out still surfaces here.
			switch p.Stage {
			case "success":
				v.status = fmt.Sprintf("pull %s: done", p.Engine)
			case "error":
				detail := p.Message
				if detail == "" {
					detail = "failed"
				}
				v.status = fmt.Sprintf("pull %s failed: %s", p.Engine, detail)
			default:
				v.status = fmt.Sprintf("pull %s: %s (%d%%)", p.Engine, p.Stage, p.Percent)
			}
		}
		return nil

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *enginesView) CapturingInput() bool {
	return v.pulling || v.editingPort || v.confirm.open()
}

func (v *enginesView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.confirm.open() {
		return v.confirm.Update(msg)
	}
	if v.editingPort {
		switch msg.String() {
		case "enter":
			return v.submitPort()
		case "esc":
			v.editingPort = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	if v.pulling {
		switch msg.String() {
		case "enter":
			return v.submitPull()
		case "esc":
			v.pulling = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	if key.Matches(msg, engTargetKey) {
		return v.cycleTarget()
	}
	if key.Matches(msg, engModelsKey) {
		return v.toggleModels()
	}
	if key.Matches(msg, engDeleteModelKey) {
		return v.confirmDeleteModel()
	}
	if key.Matches(msg, engPortKey) {
		engine := v.selectedEngine()
		if v.target != "" || engine == "" || !v.byName[engine].Capabilities.SetPort {
			return nil
		}
		v.portEngine = engine
		v.editingPort = true
		v.input.Placeholder = "engine server port (1–65535)"
		v.input.SetValue(strconv.Itoa(v.byName[engine].Port))
		v.input.Focus()
		return textinput.Blink
	}
	if key.Matches(msg, engUpdateKey) {
		engine := v.selectedEngine()
		e := v.byName[engine]
		if v.target != "" || engine == "" || !e.Capabilities.Install || !e.Capabilities.Uninstall {
			return nil
		}
		v.confirm = newConfirm("Update engine?", fmt.Sprintf("Update reinstalls %s: it is uninstalled, then installed and started. Inference stops meanwhile, and files the engine's uninstaller removes are deleted.", engine), v.updateCmd(engine))
		return nil
	}
	if key.Matches(msg, engPullKey) {
		engine := v.selectedEngine()
		if engine == "" || !v.byName[engine].Capabilities.PullModel {
			return nil
		}
		v.pullEngine = engine
		v.pullNode = v.target
		v.pulling = true
		v.input.SetValue("")
		v.input.Placeholder = "model name (e.g. llama3.2)"
		v.input.Focus()
		return textinput.Blink
	}
	if cmd, handled := v.handleAction(msg); handled {
		return cmd
	}
	var cmd tea.Cmd
	if v.models.open {
		v.models.table, cmd = v.models.table.Update(msg)
	} else {
		v.table, cmd = v.table.Update(msg)
	}
	return cmd
}

// pullParams builds the engine:action{action:"pull_model"} params for a pull.
// The model name is sent under BOTH "name" and "model" — mirroring
// PullModelStream's own empty-params default — because the two engines key it
// differently: Ollama's pull_model is HTTP /api/pull (body key "name"), while
// LM Studio's is a CLI action `lms get {model}` resolved from the "model" key.
// Sending only one key silently no-ops the pull on the other engine.
func pullParams(engine, model string) map[string]any {
	return map[string]any{"engine": engine, "action": "pull_model", "params": map[string]string{"name": model, "model": model}}
}

// submitPull uses the node and engine captured when the name prompt opened.
// Progress is informational; the local or remote RPC reply reports completion
// with the long operation lifetime, including any terminal error.
func (v *enginesView) submitPull() tea.Cmd {
	v.pulling = false
	v.input.Blur()
	model := strings.TrimSpace(v.input.Value())
	engine, node := v.pullEngine, v.pullNode
	if model == "" || engine == "" {
		v.status = "model name required"
		return nil
	}
	v.status = fmt.Sprintf("pull %s: %s...", engine, model)
	method := "engine:action"
	params := pullParams(engine, model)
	if node != "" {
		method = "engine:remote-pull-model"
		params = map[string]any{"node": node, "engine": engine, "model": model}
	}
	return callWithTimeout(v.client, method, params, longOpTimeout, func(_ *rpc.Message, err error) tea.Msg {
		return modelOpMsg{node: node, action: "pull", model: model, engine: engine, err: err}
	})
}

// submitPort edits the engine server's persistent port, not a proxy listener.
// Capture the engine at editor creation and the port at confirmation creation.
func (v *enginesView) submitPort() tea.Cmd {
	port, err := strconv.Atoi(strings.TrimSpace(v.input.Value()))
	if err != nil || port < 1 || port > 65535 {
		v.status = "engine server port must be an integer in 1–65535"
		return nil
	}
	v.editingPort = false
	v.input.Blur()
	engine := v.portEngine
	cmd := callWithTimeout(v.client, "engine:set-port", map[string]any{"engine": engine, "port": port}, lifecycleOpTimeout, func(msg *rpc.Message, err error) tea.Msg {
		var status engineStatus
		if err == nil {
			err = decodeParams(msg.Result, &status)
		}
		return engineOpMsg{what: "set-port", engine: engine, status: &status, err: err}
	})
	v.status = ""
	v.confirm = newConfirm("Change engine server port?", fmt.Sprintf("%s will restart on port %d if it is running.", engine, port), cmd)
	return nil
}

// updateCmd is a destructive reinstall, not an in-place upgrade. One update
// deadline bounds the sequence; uninstall also retains its shorter lifecycle
// deadline. Do not dispatch install until uninstall has replied successfully.
func (v *enginesView) updateCmd(engine string) tea.Cmd {
	client := v.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), longOpTimeout)
		defer cancel()
		uninstallCtx, uninstallCancel := context.WithTimeout(ctx, lifecycleOpTimeout)
		_, err := client.Call(uninstallCtx, "engine:uninstall", map[string]string{"engine": engine})
		uninstallCancel()
		if err != nil {
			return engineOpMsg{what: "update", engine: engine, err: fmt.Errorf("uninstall failed: %w", err)}
		}
		msg, err := client.Call(ctx, "engine:install", map[string]any{"engine": engine, "start": true})
		var status engineStatus
		if err == nil {
			err = decodeParams(msg.Result, &status)
		}
		if err != nil {
			return engineOpMsg{what: "update", engine: engine, err: fmt.Errorf("install failed: %w", err)}
		}
		return engineOpMsg{what: "update", engine: engine, status: &status}
	}
}

func (v *enginesView) handleAction(msg tea.KeyMsg) (tea.Cmd, bool) {
	var method, what string
	switch {
	case key.Matches(msg, engStartKey):
		method, what = "engine:start", "start"
	case key.Matches(msg, engStopKey):
		method, what = "engine:stop", "stop"
	case key.Matches(msg, engRestartKey):
		method, what = "engine:restart", "restart"
	case key.Matches(msg, engInstallKey):
		method, what = "engine:install", "install"
	case key.Matches(msg, engUninstallKey):
		method, what = "engine:uninstall", "uninstall"
	default:
		return nil, false
	}
	engine := v.selectedEngine()
	if engine == "" {
		return nil, true
	}
	e := v.byName[engine]
	if (what == "install" && !e.Capabilities.Install) || (what == "uninstall" && !e.Capabilities.Uninstall) {
		return nil, true
	}
	node := v.target
	params := map[string]any{"engine": engine}
	timeout := callTimeout
	switch what {
	case "start", "stop", "restart", "uninstall":
		timeout = lifecycleOpTimeout
	case "install":
		timeout = longOpTimeout
	}
	if node != "" {
		if what != "start" && what != "stop" && what != "install" {
			return nil, true
		}
		method = "engine:remote-" + what
		params["node"] = node
		if what == "install" {
			params["start"] = true
		}
	}
	cmd := callWithTimeout(v.client, method, params, timeout, func(_ *rpc.Message, err error) tea.Msg {
		return engineOpMsg{node: node, what: what, engine: engine, err: err}
	})
	if what == "uninstall" {
		v.confirm = newConfirm("Uninstall engine?", fmt.Sprintf("Uninstall %s on This node? Inference stops, and files the engine's uninstaller removes are deleted.", engine), cmd)
		return nil, true
	}
	v.status = what + " " + engine + "..."
	return cmd, true
}

func (v *enginesView) selectedEngine() string {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.order) {
		return ""
	}
	return v.order[idx]
}

func (v *enginesView) merge(e engineStatus) {
	if _, ok := v.byName[e.Engine]; !ok {
		v.order = append(v.order, e.Engine)
	}
	v.byName[e.Engine] = e
	v.refreshRows()
}

func (v *enginesView) refreshRows() {
	rows := make([]table.Row, 0, len(v.order))
	for _, name := range v.order {
		e := v.byName[name]
		label := e.Engine
		if e.DisplayName != "" {
			label = e.DisplayName
		}
		port := "-"
		if e.Port != 0 {
			port = strconv.Itoa(e.Port)
		}
		rows = append(rows, table.Row{
			label,
			yesNo(e.Installed),
			yesNo(e.Running),
			yesNo(e.Healthy),
			port,
		})
	}
	v.table.SetRows(rows)
}

func (v *enginesView) View() string {
	out := v.table.View()
	if len(v.order) == 0 {
		out = footerStyle.Render("No engines known on this host.")
	}
	if v.models.open {
		out = v.modelsView()
	}
	if v.confirm.open() {
		out += "\n" + v.confirm.View()
	}
	if v.pulling {
		out += "\npull model: " + v.input.View()
	}
	if v.editingPort {
		out += "\nengine server port: " + v.input.View()
	}
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *enginesView) Help() []key.Binding {
	if v.confirm.open() {
		return v.confirm.Help()
	}
	bindings := []key.Binding{engTargetKey, engModelsKey, engStartKey, engStopKey}
	e := v.byName[v.selectedEngine()]
	if v.target == "" {
		bindings = append(bindings, engRestartKey)
	}
	if e.Capabilities.Install {
		bindings = append(bindings, engInstallKey)
	}
	if v.target == "" {
		if e.Capabilities.Uninstall {
			bindings = append(bindings, engUninstallKey)
		}
		if e.Capabilities.SetPort {
			bindings = append(bindings, engPortKey)
		}
		if e.Capabilities.Install && e.Capabilities.Uninstall {
			bindings = append(bindings, engUpdateKey)
		}
	}
	if e.Capabilities.PullModel {
		bindings = append(bindings, engPullKey)
	}
	if v.models.open && e.Capabilities.DeleteModel && v.selectedModel() != "" {
		bindings = append(bindings, engDeleteModelKey)
	}
	return bindings
}

func (v *enginesView) cycleTarget() tea.Cmd {
	// Membership keeps offline peers selectable so their transport/trust errors
	// remain visible. Discovery contributes paired peers only, not strangers.
	targets := []string{""}
	seen := map[string]bool{"": true, v.identity.NodeUUID: true}
	add := func(uuid string) {
		if !seen[uuid] {
			targets = append(targets, uuid)
			seen[uuid] = true
		}
	}
	for _, n := range v.discovered {
		if n.Trusted {
			add(n.HostUUID)
		}
	}
	for _, n := range v.members {
		add(n.NodeUUID)
	}
	next := ""
	for i, target := range targets {
		if target == v.target {
			next = targets[(i+1)%len(targets)]
			break
		}
	}
	v.target = next
	v.models.generation++
	v.models.inventory = modelSnapshot{}
	v.models.err = nil
	v.models.open = false
	v.refreshModels()
	v.order = nil
	v.byName = map[string]engineStatus{}
	v.refreshRows()
	v.table.SetCursor(0)
	v.status = "loading engines..."
	return v.loadCmd()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
