// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
)

func TestLoadProfileValidation(t *testing.T) {
	for _, text := range []string{"--gpu max --context-length 200000 --parallel 2", "--offload-kv-cache", `--future-option "two words"`, ""} {
		args, err := validateLoadText(text)
		if err != nil {
			t.Fatalf("valid options rejected: %v", err)
		}
		normalized, _ := formatLaunchText(args)
		again, err := validateLoadText(normalized)
		if err != nil || !reflect.DeepEqual(args, again) {
			t.Fatal("literal round trip failed")
		}
	}
	for _, text := range []string{
		"--estimate-only", "--dry-run", "--input x", "--output x", "--include x", "--require x", "--import x", "--export x", "--mcp x", "--proxy x", "--socket x",
		"--host localhost", "--port=1234", "--server-port 1234", "--bind 127.0.0.1", "--address localhost", "--api-url x",
		"--model other", "--model-path x", "--identifier same", "--alias=x", "--name x", "--path x", "other/model", "-- other/model", "-- --identifier x",
		"-p 1234", "-m other", "-a other", "-c 1234", "-c=1234", "-abc", "--config x", "--preset x", "--lora x", "--adapter x", "--file x", "--prompt-template x", "--exec x", "--command x", "--script x", "--plugin x", "--help", "--version", "@args", "--future @args", "--future /tmp/file", "--future=../file", "--future=https://example.test/file", "--gpu max; touch x", "--future $(touch x)", "--future='$(touch x)'", "--future `touch x`", "--future='a;b'", "--identifierX x", "--HOST=x", "GPU=max --gpu max",
	} {
		if _, err := validateLoadText(text); err == nil {
			t.Errorf("unsafe input accepted: %q", text)
		}
	}
}

func TestLoadProfilePersistenceRevisionAndPrivatePermissions(t *testing.T) {
	e := &Executor{baseDir: filepath.Join(t.TempDir(), "private")}
	request := settings.Request{Engine: "lmstudio", Model: "owner/model", RequestID: "one", LoadText: "--gpu max", ExpectedRevision: 0}
	got, err := e.ApplyLoadProfile(context.Background(), request)
	if err != nil || got.Revision == 0 || got.LoadText != "--gpu max" {
		t.Fatalf("apply: %+v %v", got, err)
	}
	reloaded := &Executor{baseDir: e.baseDir}
	snap, err := reloaded.GetLoadProfile(context.Background(), request)
	if err != nil || snap.Revision != got.Revision || snap.LoadText != got.LoadText {
		t.Fatalf("reload: %+v %v", snap, err)
	}
	if _, err := e.ApplyLoadProfile(context.Background(), request); err != nil {
		t.Fatalf("retry: %v", err)
	}
	request.RequestID = "stale"
	if _, err := e.ApplyLoadProfile(context.Background(), request); err == nil {
		t.Fatal("stale write accepted")
	}
	request.ExpectedRevision = snap.Revision
	request.LoadText = "--identifier secret-value"
	if _, err := e.ApplyLoadProfile(context.Background(), request); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatal("unsafe error or accepted override")
	}
	info, err := os.Stat(filepath.Join(e.baseDir, "lmstudio-load-profiles.json"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	info, err = os.Stat(e.baseDir)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory: %v %v", info, err)
	}
}

func TestLoadProfileDeletePruneAndCorruption(t *testing.T) {
	e := &Executor{baseDir: t.TempDir()}
	for _, model := range []string{"keep", "gone"} {
		snap, _ := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: model})
		if _, err := e.ApplyLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: model, RequestID: model, ExpectedRevision: snap.Revision, LoadText: "--gpu max"}); err != nil {
			t.Fatal(err)
		}
	}
	generation := e.loadProfileGeneration()
	// An inventory started before a concurrent edit must not prune that edit.
	snap, _ := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "new"})
	if _, err := e.ApplyLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "new", RequestID: "new", ExpectedRevision: snap.Revision, LoadText: "--parallel 2"}); err != nil {
		t.Fatal(err)
	}
	if err := e.pruneLoadProfiles(context.Background(), []string{"keep"}, generation); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"keep": "--gpu max", "gone": "", "new": "--parallel 2"} {
		got, err := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: model})
		if err != nil || got.LoadText != want {
			t.Fatalf("prune %s: %+v %v", model, got, err)
		}
	}
	if err := e.deleteLoadProfile(context.Background(), "keep"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "keep"})
	if got.LoadText != "" {
		t.Fatal("delete retained profile")
	}
	if err := os.WriteFile(filepath.Join(e.baseDir, "lmstudio-load-profiles.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadProfileArgs(context.Background(), "keep"); err == nil {
		t.Fatal("corrupt configuration silently fell back")
	}
}
