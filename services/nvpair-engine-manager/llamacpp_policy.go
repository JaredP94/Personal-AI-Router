// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const llamaCPPOptionPolicy = "llamacpp-b12171"

func (p *EditableLaunch) llamaCPP() bool { return p != nil && p.OptionPolicy == llamaCPPOptionPolicy }

type llamaOption struct {
	names    string
	kind     string
	min, max float64
	choices  string
	group    string
}

// Deliberately closed policy, audited against ggml-org/llama.cpp tag b12171:
// common/arg.cpp (common_params_parser_init/common_params_parse_ex), and
// src/llama-chat.cpp (LLM_CHAT_TEMPLATES). Only contained GGUF model files;
// no URLs or free-form programs.
// Unlike open server launch text, this parser also exposes host tools, agents,
// tunnels, MCP and downloads. New upstream options must not become permissions.
var llamaInferenceOptions = []llamaOption{
	{names: "--model -m", kind: "model"},
	{names: "--ctx-size -c", kind: "int", min: 0, max: 1048576},
	{names: "--batch-size -b", kind: "int", min: 1, max: 1048576},
	{names: "--ubatch-size -ub", kind: "int", min: 1, max: 1048576},
	{names: "--threads -t", kind: "int", min: 1, max: 4096},
	{names: "--threads-batch -tb", kind: "int", min: 1, max: 4096},
	{names: "--gpu-layers --n-gpu-layers -ngl", kind: "gpu", min: 0, max: 1048576},
	{names: "--flash-attn -fa", kind: "enum", choices: "on off auto"},
	{names: "--cache-type-k -ctk", kind: "enum", choices: "f32 f16 bf16 q8_0 q4_0 q4_1 q5_0 q5_1 iq4_nl"},
	{names: "--cache-type-v -ctv", kind: "enum", choices: "f32 f16 bf16 q8_0 q4_0 q4_1 q5_0 q5_1 iq4_nl"},
	{names: "--parallel -np", kind: "int", min: 1, max: 1024},
	{names: "--predict --n-predict -n", kind: "int", min: -1, max: 1048576},
	{names: "--temp --temperature", kind: "float", min: 0, max: 10},
	{names: "--top-k", kind: "int", min: 0, max: 1048576},
	{names: "--top-p", kind: "float", min: 0, max: 1},
	{names: "--min-p", kind: "float", min: 0, max: 1},
	{names: "--chat-template", kind: "enum", choices: "chatml llama2 llama2-sys llama2-sys-bos llama2-sys-strip mistral-v1 mistral-v3 mistral-v3-tekken mistral-v7 mistral-v7-tekken phi3 phi4 falcon3 zephyr monarch gemma orion openchat vicuna vicuna-orca deepseek deepseek2 deepseek3 deepseek-ocr command-r llama3 chatglm3 chatglm4 glmedge minicpm exaone3 exaone4 exaone-moe rwkv-world granite granite-4.0 granite-4.1 gigachat megrez yandex bailing bailing-think bailing2 llama4 smolvlm hunyuan-moe gpt-oss hunyuan-dense hunyuan-vl kimi-k2 seed_oss grok-2 pangu-embedded solar-open"},
	{names: "--reasoning-format", kind: "enum", choices: "none auto deepseek deepseek-legacy"},
	{names: "--reasoning -rea", kind: "enum", choices: "on off auto"},
	{names: "--reasoning-effort", kind: "enum", choices: "default minimal low medium high xhigh max"},
	{names: "--reasoning-budget", kind: "int", min: -1, max: 1048576},
	{names: "--cont-batching -cb", kind: "switch", group: "batching"},
	{names: "--no-cont-batching -nocb", kind: "switch", group: "batching"},
	{names: "--kv-offload -kvo", kind: "switch", group: "kv-offload"},
	{names: "--no-kv-offload -nkvo", kind: "switch", group: "kv-offload"},
	{names: "--load-mode -lm", kind: "enum", choices: "auto none mmap mlock mmap+mlock dio"},
	{names: "--metrics", kind: "switch"},
	{names: "--slots", kind: "switch", group: "slots"},
	{names: "--no-slots", kind: "switch", group: "slots"},
}

func normalizeLlamaOptions(args []string, cache string) ([]string, error) {
	for _, arg := range args {
		for _, c := range arg {
			if c < 32 || c > 126 {
				return nil, fmt.Errorf("llama.cpp launch options must be ASCII")
			}
		}
	}
	result := []string{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, equals := strings.Cut(args[i], "=")
		var option *llamaOption
		for j := range llamaInferenceOptions {
			if slices.Contains(strings.Fields(llamaInferenceOptions[j].names), name) {
				option = &llamaInferenceOptions[j]
				break
			}
		}
		if option == nil {
			return nil, fmt.Errorf("option is not in the llama.cpp inference allowlist")
		}
		canonical := strings.Fields(option.names)[0]
		group := canonical
		if option.group != "" {
			group = option.group
		}
		if seen[group] {
			return nil, fmt.Errorf("repeated llama.cpp option %s", canonical)
		}
		seen[group] = true
		if option.kind == "switch" {
			if equals {
				return nil, fmt.Errorf("%s is a presence-only boolean switch", canonical)
			}
			result = append(result, canonical)
			continue
		}
		if !equals {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("%s needs a value", canonical)
			}
			value = args[i]
		}
		valid := false
		switch option.kind {
		case "model":
			path, err := llamaModelPath(cache, value)
			if err != nil {
				return nil, err
			}
			value, valid = path, true
		case "enum":
			valid = slices.Contains(strings.Fields(option.choices), value)
		case "gpu":
			valid = value == "auto" || value == "all"
			if valid {
				break
			}
			fallthrough
		case "int":
			n, err := strconv.ParseInt(value, 10, 32)
			valid = err == nil && float64(n) >= option.min && float64(n) <= option.max
		case "float":
			n, err := strconv.ParseFloat(value, 32)
			valid = err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= option.min && n <= option.max
		}
		if !valid {
			return nil, fmt.Errorf("invalid value for %s", canonical)
		}
		// b12171 accepts separate argv values, not equals syntax. Canonicalize both
		// editor forms here, preserving only validated values, never shell expansion.
		result = append(result, canonical, value)
	}
	return result, nil
}

// b12171 common/arg.cpp:common_params_apply_system_config unconditionally
// reads /etc/llama.cpp/config.ini; there is no disable-config switch. The user
// search in common/common.cpp:fs_get_config_directory uses XDG_CONFIG_HOME or
// HOME/.config. The child has PAIR-owned search paths and no inherited env.
// Refuse any file at the remaining paths, including unreadable/dangling links;
// never parse or log its contents. A false env value cannot undo reuse-port.
func llamaConfigPaths(env map[string]string) []string {
	return []string{"/etc/llama.cpp/config.ini", filepath.Join(env["XDG_CONFIG_HOME"], "llama.cpp", "config.ini")}
}

func rejectLlamaConfigFiles(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			if err != nil {
				return fmt.Errorf("cannot inspect llama.cpp configuration %s: %w", path, err)
			}
			return fmt.Errorf("cannot start llama.cpp while configuration file %s exists; remove it from the child search path", path)
		}
	}
	return nil
}

func validateLlamaOverrides(rt Runtime, cache string) error {
	if !rt.EditableLaunch.llamaCPP() {
		return nil
	}
	if rt.LaunchEnv != nil && len(*rt.LaunchEnv) != 0 {
		return fmt.Errorf("llama.cpp launch environment assignments are not allowed")
	}
	if rt.LaunchArgs != nil {
		_, err := normalizeLlamaOptions(*rt.LaunchArgs, cache)
		return err
	}
	return nil
}
