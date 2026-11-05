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
	for _, text := range []string{"--gpu max --context-length 200000 --parallel 2", "--ttl 60", ""} {
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

func TestLoadProfileTuningAllowlist(t *testing.T) {
	for _, option := range []struct{ name, value string }{
		{"--gpu", "max"}, {"--gpu", "off"}, {"--gpu", "0.5"},
		{"--context-length", "200000"}, {"--parallel", "2"}, {"--ttl", "60"},
		{"--auto", "true"}, {"--auto", "false"},
		{"--speculative-draft-mtp", "true"}, {"--speculative-draft-mtp", "false"},
		{"--speculative-draft-max-tokens", "16"}, {"--speculative-draft-min-tokens", "0"},
		{"--speculative-draft-min-continue-probability", "0.25"},
	} {
		for _, separator := range []string{" ", "="} {
			text := option.name + separator + option.value
			if strings.HasPrefix(option.name, "--speculative-draft-m") && option.name != "--speculative-draft-mtp" {
				text = "--speculative-draft-mtp true " + text
			}
			if _, err := validateLoadText(text); err != nil {
				t.Errorf("rejected %s: %v", text, err)
			}
		}
	}
	for _, text := range []string{
		"--engine-cwd .", "--engine-cwd=..", "--no-engine-cwd true", "--no-engine-cwd=true", "--no-engine-cwd",
		"--future-option 3", "--future-option=3", "--offload-kv-cache true",
		"-c 1", "-c=1", "--context 1", "--", "model", "--gpu max model",
		"--gpu max --gpu=off", "--ttl 1 --ttl 2", "--auto true --auto false",
		"--gpu", "--gpu=", "--gpu --ttl 1", "--gpu MAX", "--gpu NaN", "--gpu Inf", "--gpu -0.1", "--gpu 1.1", "--gpu 0x1",
		"--context-length 0", "--context-length 1.5", "--context-length 2147483648", "--parallel -1", "--ttl 0", "--ttl 1e3", "--ttl +1",
		"--auto", "--auto yes", "--auto TRUE", "--auto true --gpu max", "--auto true --context-length 1",
		"--speculative-draft-mtp 1", "--speculative-draft-max-tokens 0", "--speculative-draft-min-tokens -1",
		"--speculative-draft-min-continue-probability 1.1", "--speculative-draft-min-continue-probability NaN",
		"--speculative-draft-max-tokens 4", "--speculative-draft-mtp false --speculative-draft-min-tokens 0",
		"--speculative-draft-mtp true --speculative-draft-min-tokens 3 --speculative-draft-max-tokens 2",
		"--gpu mаx", "--ttl １", "--ｔtl 1", "--ttl\u00a01", "--ttl '1", "--ttl 1; x",
	} {
		if _, err := validateLoadText(text); err == nil {
			t.Errorf("malformed/unsupported input accepted: %q", text)
		}
	}
	for _, option := range []string{"--engine-cwd", "--no-engine-cwd", "--future-option", "--ttl"} {
		_, err := validateLoadText(option + "=private-value")
		if err == nil || !strings.Contains(err.Error(), option) || strings.Contains(err.Error(), "private-value") {
			t.Errorf("error must name only the option %s: %v", option, err)
		}
	}
}

func TestLoadProfilePreviouslyAcceptedOptionReportedInvalid(t *testing.T) {
	e := &Executor{baseDir: t.TempDir()}
	path := filepath.Join(e.baseDir, "lmstudio-load-profiles.json")
	data := []byte(`{"revision":1,"profiles":{"m":{"revision":1,"loadText":"--engine-cwd ."}}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "invalid") || !strings.Contains(err.Error(), "--engine-cwd") {
		t.Fatalf("saved invalid profile was not reported: %v", err)
	}
	if _, err := e.loadProfileArgs(context.Background(), "m"); err == nil {
		t.Fatal("saved invalid profile applied silently")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("invalid profile silently dropped or rewritten")
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
