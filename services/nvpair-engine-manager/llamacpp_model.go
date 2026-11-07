// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// llamaModelPath admits one existing regular GGUF inside the managed cache.
// Resolve both the cache and candidate before the containment check, including
// symlinks in intermediate directories. Return the canonical absolute path so
// the child never interprets a relative path against its working directory.
func llamaModelPath(cache, value string) (string, error) {
	fail := func() (string, error) {
		return "", fmt.Errorf("model must be an existing regular .gguf file inside the PAIR llama.cpp model cache")
	}
	if cache == "" || value == "" || strings.Contains(value, "://") {
		return fail()
	}
	root, err := filepath.Abs(cache)
	if err != nil {
		return fail()
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fail()
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fail()
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fail()
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(path), ".gguf") {
		return fail()
	}
	return path, nil
}

// b12171 /props reports role=router without a model, but model_path in
// single-model mode (server-context.cpp:get_res_props). Preserve identity
// checking in both modes, without mutating shared manifest probe pointers.
func llamaRuntimeProbes(rt Runtime, installDir string) Runtime {
	if !rt.EditableLaunch.llamaCPP() || rt.LaunchArgs == nil {
		return rt
	}
	args, err := normalizeLlamaOptions(*rt.LaunchArgs, installDir+"-models")
	if err != nil {
		return rt
	} // the launch path reports invalid saved settings
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--model" {
			continue
		}
		copyProbe := func(p *Probe) *Probe {
			if p == nil {
				return nil
			}
			copy := *p
			copy.JSONMatch = &ProbeJSONMatch{Field: "model_path", Value: args[i+1]}
			return &copy
		}
		rt.Ready, rt.Health = copyProbe(rt.Ready), copyProbe(rt.Health)
		break
	}
	return rt
}
