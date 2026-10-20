// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

var (
	confirmYesKey = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm"))
	confirmNoKey  = key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "cancel"))
)

// confirmPrompt is the shared gate in front of a destructive action. It is
// deliberately generic: the caller supplies the target's identity and the
// command to run, so the same prompt guards a worker restart, a model delete,
// an engine update, an uninstall or a cluster member removal.
//
// Three properties matter and are enforced here rather than by each caller:
//
//   - The action is frozen at creation. onYes is the command built from the
//     target the caller had selected when the prompt opened; answering the
//     prompt later cannot retarget it.
//   - The default is cancel. Only y runs anything; n, Esc and Enter cancel.
//     Other keys are swallowed without answering the question.
//   - While open, the prompt captures input through the view's inputCapturer
//     path. The root model routes keys to a capturing view before its own
//     global bindings, so q, Tab and Ctrl-C cannot quit or navigate past an
//     unanswered question.
type confirmPrompt struct {
	title  string
	detail string
	onYes  tea.Cmd
	done   bool
}

// newConfirm builds the prompt. Assigning the result to a view's field is
// what shows it: the prompt is open from the moment it is created and the view
// stops capturing once it has been answered. A view with no prompt in flight
// holds a nil *confirmPrompt, which is inert.
func newConfirm(title, detail string, onYes tea.Cmd) *confirmPrompt {
	return &confirmPrompt{title: title, detail: detail, onYes: onYes}
}

// open reports whether the prompt is still asking its question.
func (p *confirmPrompt) open() bool { return p != nil && !p.done }

// CapturingInput makes the prompt claim every key from the root model while it
// is open, so no global binding can answer it or bypass it.
func (p *confirmPrompt) CapturingInput() bool { return p.open() }

// Update answers the prompt. Only y produces a command, and answering closes
// the prompt so a repeated key cannot repeat the action.
func (p *confirmPrompt) Update(msg tea.Msg) tea.Cmd {
	if !p.open() {
		return nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "y":
		p.done = true
		return p.onYes
	case "n", "esc", "enter":
		p.done = true
	}
	return nil
}

// View renders the question over the caller's content, or nothing once it is
// answered.
func (p *confirmPrompt) View() string {
	if !p.open() {
		return ""
	}
	out := statusErrStyle.Render(p.title)
	if p.detail != "" {
		out += "\n" + p.detail
	}
	return out + "\n" + footerStyle.Render("y confirm / n, esc cancel")
}

// Help reports the prompt's bindings while it is open, so the footer shows how
// to answer instead of the underlying view's keys.
func (p *confirmPrompt) Help() []key.Binding { return []key.Binding{confirmYesKey, confirmNoKey} }
