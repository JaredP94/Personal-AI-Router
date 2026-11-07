// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLlamaModelLaunchAlias(t *testing.T) {
	cache := t.TempDir()
	// Use the actual download/manifest path, not a synthetic ownership record.
	ex, recordedCache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "alias"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manual.gguf", "nested/manual.GGUF"} {
		writeSingleGGUF(t, filepath.Join(cache, name))
	}
	for _, tc := range []struct{ cache, selection, alias string }{
		{recordedCache, llamaTestID, llamaTestID},
		{recordedCache, llamaOwnedFilename(llamaTestID), llamaTestID},
		{recordedCache, filepath.Join(recordedCache, llamaOwnedFilename(llamaTestID)), llamaTestID},
		{cache, "manual.gguf", "manual"},
		{cache, "nested/manual.GGUF", "nested/manual"},
	} {
		t.Run(tc.selection, func(t *testing.T) {
			args, err := normalizeLlamaOptions([]string{"-m", tc.selection}, tc.cache)
			if err != nil {
				t.Fatal(err)
			}
			preview := append([]string(nil), args...)
			child, files, err := prepareLlamaModelArgs(args, tc.cache)
			for _, f := range files {
				defer f.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"--model", llamaModelDescriptorPath, "--alias", tc.alias}
			if !reflect.DeepEqual(child, want) || len(files) != 1 {
				t.Fatalf("launch = %v (%d descriptors), want %v", child, len(files), want)
			}
			if !reflect.DeepEqual(args, preview) || strings.Contains(strings.Join(args, " "), "/dev/fd") {
				t.Fatalf("mutated selection/preview: %v", args)
			}
		})
	}
	args := []string{"--threads", "4"}
	child, files, err := prepareLlamaModelArgs(args, cache)
	if err != nil || len(files) != 0 || !reflect.DeepEqual(child, args) {
		t.Fatalf("router mode changed: %v %v", child, err)
	}
}

func TestLlamaModelAliasKeepsCacheRelativeSettings(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	ex := settingsExecutor(t, false)
	st := settingsState(t, ex)
	st.plat.Runtime = p.Runtime
	cache := st.installDir + "-models"
	if err := os.MkdirAll(filepath.Join(cache, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	writeSingleGGUF(t, filepath.Join(cache, "nested", "manual.gguf"))
	req := settingsRequest(t, ex)
	req.Settings.LaunchText = "--model nested/manual.gguf"
	preview := previewSettings(t, ex, req)
	if len(preview.Errors) != 0 || preview.Settings.LaunchText != req.Settings.LaunchText {
		t.Fatalf("settings lost cache-relative selection: %+v", preview)
	}
	st.plat.Runtime.LaunchArgs = &preview.Args
	current, err := ex.LaunchSettings("fake")
	if err != nil || current.LaunchText != req.Settings.LaunchText {
		t.Fatalf("settings round trip: %+v %v", current, err)
	}
}

func TestLlamaModelAliasRejectsUnsafeFilename(t *testing.T) {
	cache, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"two,aliases.gguf", "white space.gguf", "tab\tname.gguf", "line\nname.gguf", "control\x7fname.gguf", "unicode-é.gguf", ".gguf"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(cache, name)
			writeSingleGGUF(t, path)
			if _, err := normalizeLlamaOptions([]string{"--model", path}, cache); err == nil {
				t.Errorf("selection accepted unsafe alias from %q", name)
			}
			_, files, err := prepareLlamaModelArgs([]string{"--model", path}, cache)
			for _, f := range files {
				_ = f.Close()
			}
			if err == nil {
				t.Fatalf("accepted unsafe model alias from %q", name)
			}
		})
	}
}

func TestLlamaAliasProtectedLaunchInputs(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	for _, input := range []string{"--alias other", "--alias=other", "-a other", "-a=other", "-aother", "-- --alias other", "LLAMA_ARG_ALIAS=other", "-- LLAMA_ARG_ALIAS=other"} {
		t.Run(input, func(t *testing.T) {
			ex := settingsExecutor(t, true)
			settingsState(t, ex).plat.Runtime = p.Runtime
			req := settingsRequest(t, ex)
			req.Settings.LaunchText = input
			if len(previewSettings(t, ex, req).Errors) == 0 {
				t.Fatal("preview accepted a user-owned alias")
			}
			args, err := parseLaunchText(input)
			if err != nil {
				t.Fatal(err)
			}
			rt := p.Runtime
			rt.LaunchArgs = &args
			if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err == nil {
				t.Fatal("saved launch accepted a user-owned alias")
			}
		})
	}
	if !slices.Contains(p.Runtime.EditableLaunch.ForbiddenEnv, "LLAMA_ARG_ALIAS") {
		t.Fatal("LLAMA_ARG_ALIAS must be explicitly PAIR-owned")
	}
	rt := p.Runtime
	env := []string{"LLAMA_ARG_ALIAS=other"}
	rt.LaunchEnv = &env
	if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err == nil {
		t.Fatal("saved launch environment accepted an alias")
	}
	for _, name := range []string{"--alias", "-a"} {
		if !slices.Contains(p.Runtime.EditableLaunch.ForbiddenFlags, name) {
			t.Errorf("%s must be explicitly PAIR-owned", name)
		}
	}
}

func TestLlamaModelAliasLengthBound(t *testing.T) {
	ok := strings.Repeat("a", llamaMaxAlias) + ".gguf"
	if alias, err := llamaFileAlias(ok); err != nil || len(alias) != llamaMaxAlias {
		t.Fatalf("alias at the limit rejected: %q %v", alias, err)
	}
	long := strings.Repeat("a", llamaMaxAlias+1) + ".gguf"
	if _, err := llamaFileAlias(long); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("alias over the limit accepted: %v", err)
	}
	nested := strings.Repeat("d/", 100) + strings.Repeat("m", 100) + ".gguf"
	if _, err := llamaFileAlias(nested); err == nil {
		t.Fatal("nested path producing an over-long alias accepted")
	}
}
