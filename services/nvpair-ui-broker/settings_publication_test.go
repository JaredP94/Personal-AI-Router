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

func TestSettingsProjectionBackpressureDoesNotHoldApplyMutex(t *testing.T) {
	sink := &blockedSettingsSink{entered: make(chan struct{}), release: make(chan struct{}), frames: make(chan []byte, 10)}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(sink.release) })
	b := &Broker{engineSettingsLoaded: true, engineSettings: map[string]*engineSettingsRecord{
		"ollama": {Snapshot: settings.Snapshot{Engine: "ollama", Epoch: "fixture"}},
	}}
	b.setEngineMgr(&rpcWorker{peer: NewPeer(NewCodec(sink))})
	captured := make(chan struct{})
	go func() {
		b.settingsApplyMu.Lock()
		b.engineConfigMu.Lock()
		b.publishSettingsLocked()
		b.engineConfigMu.Unlock()
		b.settingsApplyMu.Unlock()
		close(captured)
	}()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("worker projection did not block")
	}
	if !b.settingsApplyMu.TryLock() {
		t.Fatal("worker Notify retained Apply mutex")
	}
	b.settingsApplyMu.Unlock()
	<-captured
	// The shared worker's writer is backpressured, so validation fails safely,
	// but another engine can enter and leave Apply without waiting on Notify.
	done := make(chan error, 1)
	go func() {
		_, err := b.applyEngineSettingsWithinBudget(context.Background(), settings.Request{Engine: "lmstudio", RequestID: "other"}, "", settingsApplyTiming{total: 200 * time.Millisecond, execution: 100 * time.Millisecond, cleanup: 50 * time.Millisecond})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked validation succeeded")
		}
	case <-time.After(200 * time.Millisecond * 14 / 12):
		t.Fatal("worker Notify blocked another Apply")
	}
	b.engineConfigMu.Lock()
	b.publishSettingsLocked()
	b.engineConfigMu.Unlock()
	releaseOnce.Do(func() { close(sink.release) })
	var previous uint64
	for count := 0; count < 2; count++ {
		select {
		case frame := <-sink.frames:
			var msg struct {
				Method string              `json:"method"`
				Params []settings.Snapshot `json:"params"`
			}
			if err := json.Unmarshal(frame, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Method != "engine:settings-projection" || len(msg.Params) != 1 || msg.Params[0].Sequence <= previous {
				t.Fatal("worker projections out of order")
			}
			previous = msg.Params[0].Sequence
		case <-time.After(time.Second):
			t.Fatal("worker projection did not resume")
		}
	}
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
	// Verify the worker's full projections as well as desktop notifications.
	// The same single sender must not let a later sequence overtake a blocked one.
	previous = 0
	for {
		select {
		case msg := <-h.notifications:
			if msg.Method != "engine:settings-projection" {
				continue
			}
			var snapshots []settings.Snapshot
			if err := json.Unmarshal(msg.Params, &snapshots); err != nil {
				t.Fatal(err)
			}
			for _, projected := range snapshots {
				if projected.Engine != "lmstudio" {
					continue
				}
				if projected.Sequence <= previous {
					t.Fatal("worker projections arrived out of order")
				}
				previous = projected.Sequence
				if projected.RequestID == other.RequestID && projected.Phase == "succeeded" {
					return
				}
			}
		case <-time.After(time.Second):
			t.Fatal("final worker projection did not arrive")
		}
	}
}
