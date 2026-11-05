// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"

	settings "nvpair-shared/enginesettings"
	"reflect"
	"testing"
	"time"
)

// Identifiers are user-selected and reusable, not immutable instance identities.
// There need not be a poll between TTL expiry and the user's replacement load.
func TestLoadProfileExpiredInstanceReplacementIsKept(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--gpu max")
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capture+".models", []byte(`[{"identifier":"keep","modelKey":"keep","deviceIdentifier":null,"replacement":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--gpu off")
	if err := e.Restart(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	for _, row := range capturedProfileArgs(t, capture) {
		if row[0] == "unload" {
			t.Fatalf("unloaded the user's replacement: %v", row)
		}
	}
	if len(capturedProfileLoads(t, capture)) != 1 {
		t.Fatal("duplicated the user's replacement")
	}
	models := profileResidents(t, capture)
	if len(models) != 1 || string(models[0]["replacement"]) != "true" {
		t.Fatalf("user instance changed: %s", models)
	}
}

func loadProfileState(t *testing.T, e *Executor) string {
	t.Helper()
	result, err := e.loadProfileOperation(context.Background(), "get-load-profile", settings.Request{Engine: "lmstudio", Model: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		LoadState string `json:"loadState"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	return snap.LoadState
}

func TestLoadProfileResidentStateAndDisappearance(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--gpu max")
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	if got := loadProfileState(t, e); got != "loaded" {
		t.Fatalf("matching load state = %q", got)
	}
	saveTestProfile(t, e, "keep", "--gpu off")
	if got := loadProfileState(t, e); got != "reload-required" {
		t.Fatalf("changed profile state = %q", got)
	}
	t.Setenv("PAIR_TEST_PROFILE_MODE", "malformed-list")
	if got := loadProfileState(t, e); got != "unknown" {
		t.Fatalf("unreadable listing state = %q", got)
	}
	t.Setenv("PAIR_TEST_PROFILE_MODE", "")
	if err := os.WriteFile(capture+".models", []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadProfileState(t, e); got != "unloaded" {
		t.Fatalf("expired state = %q", got)
	}
	st, _ := e.state("lmstudio")
	st.mu.Lock()
	remembered := len(st.loadModels)
	st.mu.Unlock()
	if remembered != 0 {
		t.Fatal("expired instance retained its load observation")
	}
	// A later external load of the same identifier is unknown, not a PAIR load.
	if err := os.WriteFile(capture+".models", []byte(`[{"identifier":"keep","modelKey":"keep","deviceIdentifier":null}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadProfileState(t, e); got != "reload-required" {
		t.Fatalf("external replacement state = %q", got)
	}
}

func TestLoadProfileEngineStopClearsLoadObservation(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop("lmstudio"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.state("lmstudio")
	st.mu.Lock()
	remembered := len(st.loadModels)
	st.mu.Unlock()
	if remembered != 0 {
		t.Fatal("engine stop retained an instance load observation")
	}
}

func TestLoadProfileInventoryClearsExpiredLoadObservation(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.state("lmstudio")
	// The isolated HTTP fixture reports only llama3.2:1b, not the expired keep.
	st.manifest.Actions["loaded_models"] = Action{HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}, Result: &ActionResult{Array: "models", Field: "name"}}
	if _, err := e.modelsResult(ctx); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	remembered := len(st.loadModels)
	st.mu.Unlock()
	if remembered != 0 {
		t.Fatal("authoritative residency inventory retained expired load observation")
	}
}

func TestLoadProfileResidencyListingIsUnlockedAndFenced(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	ctx := context.Background()
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.state("lmstudio")
	// A late empty listing would erase the newer observation without the fence.
	if err := os.WriteFile(capture+".models", []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAIR_TEST_PROFILE_MODE", "hold-list")
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan settings.LoadProfileSnapshot, 1)
	go func() {
		snap := settings.LoadProfileSnapshot{Model: "keep"}
		e.observeLoadProfile(readCtx, &snap)
		finished <- snap
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(capture + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("residency listing did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !e.loadProfilesMu.TryLock() {
		t.Fatal("profile lock retained across listing")
	}
	e.loadProfilesMu.Unlock()
	if !st.mu.TryLock() {
		t.Fatal("state lock retained across listing")
	}
	st.revision++
	st.loadModels["keep"] = lmsLoadedModel{Identifier: "keep", Args: []string{"--gpu", "off"}}
	st.mu.Unlock()
	select {
	case snap := <-finished:
		if snap.LoadState != "unknown" {
			t.Fatalf("late snapshot committed: %+v", snap)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("residency listing exceeded deadline")
	}
	st.mu.Lock()
	_, retained := st.loadModels["keep"]
	st.mu.Unlock()
	if !retained {
		t.Fatal("late listing erased a newer load observation")
	}
}

func TestLoadProfileUnreadableListingKeepsReplacement(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	before := profileResidents(t, capture)
	saveTestProfile(t, e, "keep", "--gpu off")
	t.Setenv("PAIR_TEST_PROFILE_MODE", "malformed-list")
	if err := e.Restart(ctx, "lmstudio"); err == nil {
		t.Fatal("unreadable listing was not reported")
	}
	if !reflect.DeepEqual(before, profileResidents(t, capture)) {
		t.Fatal("unreadable listing mutated residents")
	}
	for _, row := range capturedProfileArgs(t, capture) {
		if row[0] == "unload" {
			t.Fatal("unloaded without readable listing")
		}
	}
}
