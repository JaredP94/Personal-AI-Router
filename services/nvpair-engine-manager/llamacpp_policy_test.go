// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLlamaInferenceAllowlist(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	cases := []struct{ names, value string }{
		{"--ctx-size -c", "4096"}, {"--batch-size -b", "512"}, {"--ubatch-size -ub", "128"},
		{"--threads -t", "4"}, {"--threads-batch -tb", "8"}, {"--gpu-layers --n-gpu-layers -ngl", "auto"},
		{"--flash-attn -fa", "on"}, {"--cache-type-k -ctk", "f16"}, {"--cache-type-v -ctv", "q8_0"},
		{"--parallel -np", "2"}, {"--predict --n-predict -n", "128"}, {"--temp --temperature", "0.7"},
		{"--top-k", "40"}, {"--top-p", "0.9"}, {"--min-p", "0.05"},
		{"--chat-template", "chatml"}, {"--reasoning-format", "deepseek"}, {"--reasoning -rea", "auto"},
		{"--reasoning-effort", "high"}, {"--reasoning-budget", "1024"},
	}
	for _, c := range cases {
		for _, name := range strings.Fields(c.names) {
			for _, args := range [][]string{{name, c.value}, {name + "=" + c.value}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					rt := p.Runtime
					rt.LaunchArgs = &args
					cmd, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"})
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(strings.Join(cmd.Args, " "), "=") {
						t.Fatal("equals syntax must be lowered for b12171")
					}
				})
			}
		}
	}
	for _, names := range []string{"--cont-batching -cb", "--no-cont-batching -nocb", "--kv-offload -kvo", "--no-kv-offload -nkvo", "--metrics", "--slots", "--no-slots"} {
		for _, name := range strings.Fields(names) {
			args := []string{name}
			rt := p.Runtime
			rt.LaunchArgs = &args
			if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}

func TestLlamaRejectsUncontrolledLaunch(t *testing.T) {
	p, _ := llamaManifest(t).PlatformFor("darwin", "arm64")
	flags := strings.Fields("--connect --connect-code --tools --tools-runtime -ag --agent --mcp-servers-config --mcp-servers-json --path --media-path --slot-save-path -hf -hfr --hf-repo -hff --hf-file -mu --model-url -dr --docker-repo -mmu --mmproj-url --spec-draft-hf -hfd -hfrd --hf-repo-draft --models-preset --api-prefix --reuse-port --unknown --model -m --chat-template-file")
	var inputs []string
	for _, flag := range flags {
		inputs = append(inputs, flag+" value", flag+"=value", flag+" value "+flag+" value", "-- "+flag+" value")
	}
	inputs = append(inputs, "--", "positional", "--threads 4 --threads 4", "-t 4 --threads 4", "--ctx-size -1", "--threads 4x", "--top-p NaN", "--top-p 1.1", "--chat-template /tmp/template", "--chat-template '{{evil}}'", "--threads ４", "--reasoning on/off", "--metrics=true")
	envs := strings.Fields("LLAMA_ARG_CONNECT LLAMA_ARG_CONNECT_CODE LLAMA_ARG_TOOLS LLAMA_ARG_TOOLS_RUNTIME LLAMA_ARG_AGENT LLAMA_ARG_MCP_SERVERS_CONFIG LLAMA_ARG_MCP_SERVERS_JSON LLAMA_ARG_STATIC_PATH LLAMA_ARG_SLOT_SAVE_PATH LLAMA_ARG_HF_REPO LLAMA_ARG_HF_FILE LLAMA_ARG_MODEL_URL LLAMA_ARG_DOCKER_REPO LLAMA_ARG_MMPROJ_URL LLAMA_ARG_SPEC_DRAFT_HF_REPO LLAMA_ARG_MODELS_PRESET LLAMA_ARG_API_PREFIX LLAMA_ARG_REUSE_PORT LLAMA_CACHE HF_TOKEN HF_ENDPOINT CUSTOM")
	for _, env := range envs {
		inputs = append(inputs, env+"=value", "-- "+env+"=value")
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			ex := settingsExecutor(t, true)
			settingsState(t, ex).plat.Runtime = p.Runtime
			req := settingsRequest(t, ex)
			req.Settings.LaunchText = input
			if len(previewSettings(t, ex, req).Errors) == 0 {
				t.Error("preview accepted unsafe input")
			}
			args, err := parseLaunchText(input)
			if err != nil {
				t.Fatal(err)
			}
			rt := p.Runtime
			rt.LaunchArgs = &args
			if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err == nil {
				t.Error("persisted arguments accepted unsafe input")
			}
		})
	}
	for _, env := range envs {
		rt := p.Runtime
		assignments := []string{env + "=value"}
		rt.LaunchEnv = &assignments
		if _, err := resolveProcessLaunch(rt, "/fixture/server", map[string]string{"host": "127.0.0.1", "port": "8082", "install_dir": "/fixture"}); err == nil {
			t.Errorf("persisted environment accepted %s", env)
		}
	}
}

// Every allowlisted name must exist in the pinned release: b12171 rejects
// unknown arguments, so a stale name would pass PAIR's validation and then
// stop llama-server from starting.
func TestLlamaAllowlistMatchesPinnedRelease(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "llamacpp-b12171-options.txt"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			known[line] = true
		}
	}
	for _, option := range llamaInferenceOptions {
		for _, name := range strings.Fields(option.names) {
			if !known[name] {
				t.Errorf("%s is not a llama-server b12171 option", name)
			}
		}
	}
	for _, mode := range strings.Fields("auto none mmap mlock mmap+mlock dio") {
		if _, err := normalizeLlamaOptions([]string{"--load-mode", mode}); err != nil {
			t.Errorf("load mode %s: %v", mode, err)
		}
	}
	for _, rejected := range [][]string{{"--load-mode", "lazy"}, {"--mlock"}, {"--mmap"}, {"--no-mmap"}} {
		if _, err := normalizeLlamaOptions(rejected); err == nil {
			t.Errorf("%v accepted", rejected)
		}
	}
}
