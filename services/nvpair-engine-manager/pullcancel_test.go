// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCancelAcceptedBeforeConfirmedAndKeepsCache(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"completed.gguf", "downloading_model.gguf.part", "sha256-test-partial"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("shared bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	started, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	events, unsub := ex.progress.subscribe("ollama")
	defer unsub()
	go func() {
		_, err := ex.trackedPull(context.Background(), "ollama", "model", "first", true, func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(stopping)
			<-release
			return nil, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pull did not start")
	}
	if ok := ex.CancelModelPull("ollama", "model", "first"); !ok {
		t.Fatal("cancel was not accepted")
	}
	select {
	case <-stopping:
	case <-time.After(time.Second):
		t.Fatal("cancel not delivered")
	}
	select {
	case <-done:
		t.Fatal("confirmed stop before adapter returned")
	default:
	}
	for draining := true; draining; {
		select {
		case ev := <-events:
			if ev.Stage == "cancelled" {
				t.Fatal("premature confirmation")
			}
		default:
			draining = false
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pull did not settle")
	}
	confirmed := false
	for draining := true; draining; {
		select {
		case ev := <-events:
			if ev.Stage == "cancelled" && ev.OpID == "first" && ev.Model == "model" {
				confirmed = true
			}
		default:
			draining = false
		}
	}
	if !confirmed {
		t.Fatal("no confirmed cancellation")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 3 {
		t.Fatalf("cache changed: %v %v", entries, err)
	}
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil || string(b) != "shared bytes" {
			t.Fatalf("cache data changed: %s %v", entry.Name(), err)
		}
	}
}

func TestCancelOperationIDCannotStopReplacement(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ex.trackedPull(ctx, "ollama", "same", "new", true, func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	<-started
	if ex.CancelModelPull("ollama", "same", "old") {
		t.Fatal("stale cancellation accepted")
	}
	select {
	case <-done:
		t.Fatal("replacement stopped")
	default:
	}
	if !ex.CancelModelPull("ollama", "same", "new") {
		t.Fatal("current cancellation refused")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("current cancellation did not stop")
	}
}
