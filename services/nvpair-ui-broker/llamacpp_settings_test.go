// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
)

func TestLlamaSettingsWithoutProxy(t *testing.T) {
	h := newSettingsHarness(t)
	ctx := context.Background()
	snapshot, err := h.b.getEngineSettings(ctx, settings.Request{Engine: "llamacpp"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Editable || snapshot.Settings.ProxyPort != 0 || snapshot.EffectiveProxyPort != 0 || h.b.settingsProxy("llamacpp") != nil {
		t.Fatalf("llama.cpp requires a proxy: %+v", snapshot)
	}
	request := settings.Request{Engine: "llamacpp", ExpectedRevision: snapshot.Revision, RequestID: settingsID(), Settings: snapshot.Settings}
	request.Settings.LaunchText = "--threads 4"
	preview, err := h.b.previewEngineSettings(ctx, request, "")
	if err != nil || len(preview.Errors) != 0 || preview.Rebind {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	receipt, err := h.b.applyEngineSettings(ctx, request, "")
	if err != nil || receipt.Phase != "succeeded" || h.applies.Load() != 1 {
		t.Fatalf("apply: %+v %v", receipt, err)
	}
	after, err := h.b.getEngineSettings(ctx, settings.Request{Engine: "llamacpp"}, "")
	if err != nil || after.Settings.LaunchText != "--threads 4" || !after.Running || after.EffectiveProxyPort != 0 {
		t.Fatalf("result: %+v %v", after, err)
	}
	if engineSettingsService("llamacpp") != "" {
		t.Fatal("lifecycle-only engine maps to a routed service")
	}
	// In particular a settings change must not unregister another engine's service.
	if engineSettingsService("omlx") != noderec.ServiceOMLX {
		t.Fatal("existing service changed")
	}
	token, err := h.b.prepareEngineStart(ctx, "llamacpp", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.b.authorizeEngineStart(settings.Request{Engine: "llamacpp", RequestID: token}); err != nil {
		t.Fatal(err)
	}
}

func TestLlamaSettingsPortReservations(t *testing.T) {
	h := newSettingsHarness(t)
	ctx := context.Background()
	s, err := h.b.getEngineSettings(ctx, settings.Request{Engine: "llamacpp"}, "")
	if err != nil {
		t.Fatal(err)
	}
	request := settings.Request{Engine: "llamacpp", ExpectedRevision: s.Revision, Settings: s.Settings}
	for _, port := range []int{1, 8081} {
		request.Settings.ProxyPort = port
		p, err := h.b.previewEngineSettings(ctx, request, "")
		if err == nil && len(p.Errors) == 0 {
			t.Fatal("proxy port accepted before proxy integration")
		}
	}
	request.Settings.ProxyPort = 0
	h.otherEnginePort.Store(int32(request.Settings.ServerPort))
	p, err := h.b.previewEngineSettings(ctx, request, "")
	if err == nil && !strings.Contains(p.Errors["ports"], "configured engine") {
		t.Fatalf("reservation not enforced: %+v %v", p, err)
	}
	h.otherEnginePort.Store(0)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request.Settings.ServerPort = listener.Addr().(*net.TCPAddr).Port
	p, err = h.b.previewEngineSettings(ctx, request, "")
	if err == nil && !strings.Contains(p.Errors["ports"], "in use") {
		t.Fatalf("occupied port accepted: %+v %v", p, err)
	}
}

func TestLlamaSettingsRecoveryWithoutProxy(t *testing.T) {
	h := newSettingsHarness(t)
	ctx := context.Background()
	s, err := h.b.getEngineSettings(ctx, settings.Request{Engine: "llamacpp"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := h.b.engineSettings["llamacpp"]
	r.Explicit = true
	r.Previous = s.Settings
	r.Snapshot.Phase = "applying"
	r.Snapshot.Settings.LaunchText = "--threads 2"
	if err := h.b.saveEngineSettingsLocked(); err != nil {
		t.Fatal(err)
	}
	if excluded := h.b.recoverEngineSettings(); len(excluded) != 0 {
		t.Fatalf("recovery blocked: %v", excluded)
	}
	if r.Snapshot.Phase != "succeeded" || r.Snapshot.Settings.ProxyPort != 0 {
		t.Fatalf("bad recovery: %+v", r.Snapshot)
	}
}
