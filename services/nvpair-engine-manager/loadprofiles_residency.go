// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"runtime"
	"slices"
	"strconv"

	settings "nvpair-shared/enginesettings"
)

const loadProfileReloadRequired = "Loaded with different options — reload to apply"
const loadProfileUnverified = "Loaded options are unverified — reload to apply"

// Caller holds mu. These are observations of submitted tuning, not ownership
// records: a replacement can reuse the identifier between any two observations.
func forgetMissingLMSLoadsLocked(st *engineState, instances map[string]string) {
	for model, last := range st.loadModels {
		if instances[last.Identifier] != model {
			delete(st.loadModels, model)
		}
	}
}

// Observe only after releasing the profile-store lock. No state/settings mutex
// is retained across CLI I/O. A lifecycle revision fence prevents a late listing
// from erasing or publishing observations from a newer load/stop.
func (e *Executor) observeLoadProfile(ctx context.Context, snap *settings.LoadProfileSnapshot) {
	snap.LoadState = "unknown"
	snap.Reason = profiledListFailed
	if e.reg == nil {
		return
	}
	st, err := e.state("lmstudio")
	if err != nil {
		return
	}
	st.mu.Lock()
	revision, busy, port := st.revision, st.opDone != nil, st.port
	st.mu.Unlock()
	if busy {
		return
	}
	act := st.manifest.Actions["load_model"]
	argv, err := resolveArgs(act.Cmd, map[string]string{"model": snap.Model, "port": strconv.Itoa(port), "install_dir": st.installDir, "cli": controlCLIPath(st.plat, st.installDir, runtime.GOOS)})
	if err != nil || len(argv) == 0 {
		return
	}
	instances, err := e.listLMSInstances(ctx, argv[0])
	if err != nil || ctx.Err() != nil {
		return
	}
	args, err := validateLoadText(snap.LoadText)
	if err != nil {
		return
	}
	args = loadCLIArgs(args)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.revision != revision || st.opDone != nil || ctx.Err() != nil {
		return
	}
	forgetMissingLMSLoadsLocked(st, instances)
	snap.LoadState, snap.Reason = "unloaded", ""
	// Include external identifiers for this model, not just PAIR's preferred one.
	for identifier, model := range instances {
		if model != snap.Model {
			continue
		}
		last, known := st.loadModels[model]
		if !known || last.Identifier != identifier {
			snap.LoadState, snap.Reason = "reload-required", loadProfileUnverified
			return
		}
		if !slices.Equal(last.Args, args) {
			snap.LoadState, snap.Reason = "reload-required", loadProfileReloadRequired
			return
		}
		snap.LoadState = "loaded"
	}
}
