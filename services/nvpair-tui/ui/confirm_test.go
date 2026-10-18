// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The default is cancel: only y runs the command, and answering closes the
// prompt so a repeated key press cannot send the request twice.
func TestConfirmYesRunsTheCommandExactlyOnce(t *testing.T) {
	runs := 0
	p := newConfirm("Restart proxy?", "Running inference through this proxy will be interrupted.",
		func() tea.Msg { runs++; return nil })

	cmd := p.Update(keyRunes('y'))
	if runs != 0 {
		t.Fatal("the command ran while it was being returned instead of when it is executed")
	}
	if cmd == nil {
		t.Fatal("y returned no command")
	}
	if msg := cmd(); msg != nil {
		t.Errorf("unexpected message from the confirmed command: %#v", msg)
	}
	if runs != 1 {
		t.Fatalf("confirmed command ran %d times, want 1", runs)
	}
	if p.open() {
		t.Error("prompt still open after confirmation")
	}
	if cmd := p.Update(keyRunes('y')); cmd != nil || runs != 1 {
		t.Error("a second y re-ran a confirmed action")
	}
}

// n, Esc and Enter cancel without running anything; other keys are swallowed,
// so a stray character neither acts nor falls through to a view.
func TestConfirmCancelsWithoutRunningAnything(t *testing.T) {
	for _, k := range []tea.KeyMsg{keyRunes('n'), {Type: tea.KeyEsc}, {Type: tea.KeyEnter}} {
		runs := 0
		p := newConfirm("Delete model?", "detail", func() tea.Msg { runs++; return nil })
		if cmd := p.Update(k); cmd != nil {
			t.Errorf("%q produced a command from an open prompt", k)
		}
		if runs != 0 {
			t.Errorf("%q ran the action", k)
		}
		if p.open() {
			t.Errorf("%q did not cancel the prompt", k)
		}
	}
	// Keys that are neither an answer nor a cancel leave the question standing.
	for _, k := range []tea.KeyMsg{keyRunes('q'), keyRunes('R'), keyRunes('x'), {Type: tea.KeyTab}, {Type: tea.KeyCtrlC}} {
		runs := 0
		p := newConfirm("Delete model?", "detail", func() tea.Msg { runs++; return nil })
		if cmd := p.Update(k); cmd != nil {
			t.Errorf("%q produced a command from an open prompt", k)
		}
		if runs != 0 {
			t.Errorf("%q ran the action", k)
		}
		if !p.open() {
			t.Errorf("%q answered the prompt without y or n", k)
		}
	}
}

// A view with no prompt in flight holds a nil *confirmPrompt, which must be
// inert: it cannot capture input and renders nothing.
func TestConfirmAbsentPromptIsInert(t *testing.T) {
	var p *confirmPrompt
	if p.open() || p.CapturingInput() {
		t.Error("an absent prompt captured input")
	}
	if p.View() != "" {
		t.Errorf("an absent prompt rendered %q", p.View())
	}
	if cmd := p.Update(keyRunes('y')); cmd != nil {
		t.Error("an absent prompt answered a key")
	}
}

// While open the prompt must claim input through the inputCapturer path, so
// the root model routes q, Tab and every other global key to it.
func TestConfirmCapturesInputWhileOpen(t *testing.T) {
	var _ inputCapturer = newConfirm("t", "d", nil)

	p := newConfirm("Restart proxy?", "Running inference through this proxy will be interrupted.", nil)
	if !p.CapturingInput() {
		t.Fatal("an open prompt must capture input")
	}
	view := p.View()
	if !strings.Contains(view, "Restart proxy?") || !strings.Contains(view, "will be interrupted") {
		t.Errorf("prompt view missing its title or detail: %q", view)
	}
	if help := p.Help(); len(help) != 2 {
		t.Errorf("prompt help %v, want the confirm and cancel bindings", help)
	}
	p.Update(keyRunes('n'))
	if p.CapturingInput() {
		t.Error("a cancelled prompt still captures input")
	}
}

// A different host view can reuse the component without worker-specific state.
type confirmHostView struct{ *confirmPrompt }

func (v *confirmHostView) Title() string    { return "Other operation" }
func (v *confirmHostView) Init() tea.Cmd    { return nil }
func (v *confirmHostView) SetSize(int, int) {}

func TestConfirmWorksThroughAnotherViewsInputCapture(t *testing.T) {
	v := &confirmHostView{newConfirm("Remove selected node?", "Frozen node identity", func() tea.Msg { return "removed original node" })}
	m := New(nil, nil, []View{newHealthView(nil), v})
	m.active = 1
	for _, k := range []tea.KeyMsg{keyRunes('q'), {Type: tea.KeyTab}, {Type: tea.KeyCtrlC}, keyRunes('?')} {
		next, cmd := m.Update(k)
		var ok bool
		m, ok = next.(Model)
		if !ok {
			t.Fatalf("unexpected model %T", next)
		}
		if cmd != nil || !v.open() || m.active != 1 {
			t.Fatalf("global key %q bypassed another view's prompt", k)
		}
	}
	_, cmd := m.Update(keyRunes('y'))
	if cmd == nil || cmd() != "removed original node" || v.open() {
		t.Fatal("another view could not confirm its frozen action")
	}
}
