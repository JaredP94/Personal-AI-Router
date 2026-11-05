// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

type blockedSettingsSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	frames  chan []byte
}

func (s *blockedSettingsSink) Read([]byte) (int, error) { return 0, io.EOF }
func (s *blockedSettingsSink) Write(frame []byte) (int, error) {
	s.once.Do(func() { close(s.entered); <-s.release })
	s.frames <- append([]byte(nil), frame...)
	return len(frame), nil
}

func TestSettingsPublicationBackpressureDoesNotHoldApplyMutex(t *testing.T) {
	h := newSettingsHarness(t)
	_ = h.request(t)
	other := settings.Request{Engine: "lmstudio", RequestID: settingsID()}
	snapshot, err := h.b.getEngineSettings(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	other.Settings, other.ExpectedRevision = snapshot.Settings, snapshot.Revision
	sink := &blockedSettingsSink{entered: make(chan struct{}), release: make(chan struct{}), frames: make(chan []byte, 100)}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(sink.release) })
	captured := make(chan struct{})
	go func() {
		h.b.settingsApplyMu.Lock()
		h.b.engineConfigMu.Lock()
		h.b.codec = NewCodec(sink)
		h.b.publishSettingsLocked()
		h.b.engineConfigMu.Unlock()
		h.b.settingsApplyMu.Unlock()
		close(captured)
	}()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("notification did not block")
	}
	done := make(chan error, 1)
	go func() { _, err := h.b.applyEngineSettings(context.Background(), other, ""); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked notification retained settingsApplyMu")
	}
	<-captured
	releaseOnce.Do(func() { close(sink.release) })
	// Full projections can coalesce, but each engine must advance, never regress.
	var previous uint64
	for count := 0; count < 2; {
		select {
		case frame := <-sink.frames:
			var msg struct {
				Method string            `json:"method"`
				Params settings.Snapshot `json:"params"`
			}
			if err := json.Unmarshal(frame, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Method != "engine:settings-changed" || msg.Params.Engine != "lmstudio" {
				continue
			}
			if msg.Params.Sequence <= previous {
				t.Fatal("settings publication arrived out of order")
			}
			previous = msg.Params.Sequence
			count++
		case <-time.After(time.Second):
			t.Fatal("queued publication did not arrive")
		}
	}
}
