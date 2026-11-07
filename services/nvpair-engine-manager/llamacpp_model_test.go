// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLlamaModelPathValidation(t *testing.T) {
	install := filepath.Join(t.TempDir(), "llamacpp")
	cache := install + "-models"
	if err := os.MkdirAll(filepath.Join(cache, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(cache, "nested", "model.gguf")
	for _, path := range []string{model, filepath.Join(cache, "model.txt"), filepath.Join(cache, "file:model.gguf")} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	if err := os.WriteFile(outside, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cache, "escape.gguf")); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(model)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{model, filepath.Join("nested", "model.gguf")} {
		for _, name := range []string{"--model", "-m"} {
			got, err := normalizeLlamaOptions([]string{name, input}, cache)
			if err != nil || !reflect.DeepEqual(got, []string{"--model", canonical}) {
				t.Fatalf("%s: %v %v", input, got, err)
			}
		}
	}
	for _, input := range []string{outside, "../outside.gguf", "escape.gguf", "model.txt", "missing.gguf", "nested", "https://example.invalid/model.gguf", "file:model.gguf", "file://" + model} {
		if _, err := normalizeLlamaOptions([]string{"--model", input}, cache); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	if _, err := normalizeLlamaOptions([]string{"--model", model, "-m", model}, cache); err == nil {
		t.Fatal("duplicate aliases accepted")
	}
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	args := []string{"-m", filepath.Join("nested", "model.gguf")}
	p.Runtime.LaunchArgs = &args
	cmd, err := resolveProcessLaunch(p.Runtime, "/fixture/server", map[string]string{"install_dir": install, "host": "127.0.0.1", "port": "8082"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args[len(cmd.Args)-2:], []string{"--model", canonical}) {
		t.Fatalf("argv: %v", cmd.Args)
	}
}

func TestLlamaModelProbeIdentity(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	install := filepath.Join(t.TempDir(), "llamacpp")
	if err := os.MkdirAll(install+"-models", 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(install+"-models", "model.gguf")
	if err := os.WriteFile(model, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	model, err := filepath.EvalSymlinks(model)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--model", model}
	p.Runtime.LaunchArgs = &args
	rt := llamaRuntimeProbes(p.Runtime, install)
	if rt.Ready.JSONMatch.Field != "model_path" || rt.Ready.JSONMatch.Value != llamaModelDescriptorPath || rt.Health.JSONMatch.Value != llamaModelDescriptorPath {
		t.Fatalf("model probe: %+v", rt.Ready)
	}
	if p.Runtime.Ready.JSONMatch.Field != "role" {
		t.Fatal("mutated manifest probe")
	}
	p.Runtime.LaunchArgs = nil
	if llamaRuntimeProbes(p.Runtime, install).Ready.JSONMatch.Field != "role" {
		t.Fatal("router mode lost")
	}
}
