// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
)

// Pause exactly between probe state commit and wire publication, rather than
// relying on scheduler timing, a slow HTTP response, or sleeps.
type publicationLogGate struct {
	entered, release chan struct{}
	once             sync.Once
}

func (g *publicationLogGate) Enabled(context.Context, slog.Level) bool { return true }
func (g *publicationLogGate) Handle(_ context.Context, r slog.Record) error {
	engineUp := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "ollama_up" {
			engineUp = a.Value.Bool()
		}
		return true
	})
	if r.Message == "manual node state changed" && engineUp {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return nil
}
func (g *publicationLogGate) WithAttrs([]slog.Attr) slog.Handler { return g }
func (g *publicationLogGate) WithGroup(string) slog.Handler      { return g }

type publicationCapture struct {
	*captureRW
	mu       sync.Mutex
	statuses []ManualNodeStatus
	pair     chan struct{}
	once     sync.Once
}

func (c *publicationCapture) Write(p []byte) (int, error) {
	var frame struct {
		Method string           `json:"method"`
		Params ManualNodeStatus `json:"params"`
	}
	if err := json.Unmarshal(p, &frame); err == nil && (frame.Method == "node/updated" || frame.Method == "node/discovered") {
		c.mu.Lock()
		c.statuses = append(c.statuses, frame.Params)
		c.mu.Unlock()
		if frame.Params.Kind == "pair" {
			c.once.Do(func() { close(c.pair) })
		}
	}
	return c.captureRW.Write(p)
}

func TestEnginePublicationCannotFollowPairReplacement(t *testing.T) {
	m, _, rt := newTestManager(t)
	capture := &publicationCapture{captureRW: newCaptureRW(), pair: make(chan struct{})}
	m.codec = NewCodec(capture)
	gate := &publicationLogGate{entered: make(chan struct{}), release: make(chan struct{})}
	priorLogger := slog.Default()
	slog.SetDefault(slog.New(gate))
	t.Cleanup(func() { slog.SetDefault(priorLogger) })
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.release) }) }
	t.Cleanup(unblock)
	engine := ManualEntry{Kind: "engine", Name: "overlay", Address: "overlay.example"}
	m.nodes[nodeID(engine)] = &trackedNode{entry: engine}
	rt.set(http.MethodGet, "overlay.example:11434", "/", func(*http.Request) (*http.Response, error) { return httpJSON(200, `{}`) })
	rt.set(http.MethodGet, "overlay.example:11434", "/api/tags", func(*http.Request) (*http.Response, error) { return httpJSON(200, `{"models":[{"name":"bare"}]}`) })
	probed := make(chan struct{})
	go func() { m.probeNode(engine); close(probed) }()
	select {
	case <-gate.entered:
	case <-m.ctx.Done():
		t.Fatal("probe never reached publication boundary")
	}
	pair := engine
	pair.Kind = "pair"
	replaced := make(chan struct{})
	// On the old implementation the state lock is already released: force the
	// new preview onto the wire before allowing the old engine update through.
	// With serialized publication, replacement must follow that old update.
	if m.mu.TryLock() {
		m.mu.Unlock()
		m.addNode(pair)
		close(replaced)
		select {
		case <-capture.pair:
		case <-m.ctx.Done():
			t.Fatal("replacement preview missing")
		}
		unblock()
	} else {
		go func() { m.addNode(pair); close(replaced) }()
		unblock()
	}
	for _, done := range []<-chan struct{}{probed, replaced, capture.pair} {
		select {
		case <-done:
		case <-m.ctx.Done():
			t.Fatal("publication interleaving did not finish")
		}
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	seenPair := false
	for _, s := range capture.statuses {
		if s.Kind == "pair" {
			seenPair = true
		}
		if seenPair && s.Kind == "engine" {
			t.Fatalf("obsolete engine restored bypass after PAIR replacement: %+v", capture.statuses)
		}
	}
	if !seenPair {
		t.Fatal("PAIR replacement was not published")
	}
}

func TestKindReplacementWithdrawsBeforeNewProbe(t *testing.T) {
	m, _, rt := newTestManager(t)
	capture := &publicationCapture{captureRW: newCaptureRW(), pair: make(chan struct{})}
	m.codec = NewCodec(capture)
	peer := newInventoryPeer(t, m, rt, "overlay", "peer")
	pair := peer.entry
	pair.Kind = "pair"
	pair.Name = "overlay"
	engine := pair
	engine.Kind = "engine"
	m.nodes[nodeID(engine)] = &trackedNode{entry: engine, status: ManualNodeStatus{Kind: "engine", IdentityVerification: "manual", ID: nodeID(engine), Address: engine.Address, OllamaUp: true, OllamaModels: []string{"bare"}}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rt.set(http.MethodGet, "overlay:14318", "/v1/node-info", func(*http.Request) (*http.Response, error) { <-release; return httpJSON(503, `{}`) })
	m.addNode(pair)
	select {
	case <-capture.pair:
	default:
		t.Fatal("kind changed but the old engine route remains until a probe finishes")
	}
}
