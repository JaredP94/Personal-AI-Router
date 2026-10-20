// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "strings"

// Capabilities describes which engine operations this host supports for one
// engine, so a UI can offer exactly those operations without keeping its own
// table of engines. It is derived from the effective manifest (the bundled
// manifest with any per-user override merged on), the host's OS/architecture,
// and whether NVPAIR adopted the engine rather than starting it.
//
// A flag reports declared support, not readiness or a guarantee: the chosen
// port may be reserved, the engine may be stopped, and an operation can still
// fail. Clients must treat an absent capabilities object — an older peer that
// predates it — as all-false.
type Capabilities struct {
	ListModels           bool `json:"list_models"`
	PullModel            bool `json:"pull_model"`
	DeleteModel          bool `json:"delete_model"`
	DeleteRestartsEngine bool `json:"delete_restarts_engine"`
	SetPort              bool `json:"set_port"`
	Install              bool `json:"install"`
	Uninstall            bool `json:"uninstall"`
}

// capabilitiesFor derives the capability set for one platform block of m.
// adopted marks an engine NVPAIR found already serving rather than starting, so
// the engine stays externally managed.
//
// An engine with no block for this platform supports nothing here: get-installed
// still lists it, with every flag false, so a UI can say it is unavailable here
// rather than silently omitting it.
func capabilitiesFor(m *Manifest, platform string, adopted bool) Capabilities {
	p, ok := m.Platforms[platform]
	if !ok {
		return Capabilities{}
	}
	_, list := m.Actions["list_models"]
	_, pull := m.Actions["pull_model"]
	deleteAction, deleteModel := m.Actions["delete_model"]
	return Capabilities{
		ListModels:  list,
		PullModel:   pull,
		DeleteModel: deleteModel,
		// The runner already restarts the engine for a restart_after action, so
		// callers must warn that a delete interrupts inference on a running engine.
		DeleteRestartsEngine: deleteModel && deleteAction.RestartAfter,
		// Same two conditions engine:set-port enforces: the start inputs must
		// carry the port, and NVPAIR must be able to move an adopted engine.
		SetPort:   runtimeReferencesPort(p.Runtime) && (!adopted || canMoveAdoptedEngine(p.Runtime)),
		Install:   true,
		Uninstall: p.Uninstall != nil && !adopted,
	}
}

// runtimeReferencesPort reports whether a platform's startup inputs carry the
// port, so changing it can actually move the listener. Only startup inputs
// count: a port in a readiness probe or a stop command says nothing about where
// the engine binds. Bundled oMLX is the motivating case — its start command has
// no port input, so its port is not editable.
func runtimeReferencesPort(rt Runtime) bool {
	for _, arg := range rt.Args {
		if strings.Contains(arg, "{port}") {
			return true
		}
	}
	for _, cmd := range rt.Start {
		for _, arg := range cmd {
			if strings.Contains(arg, "{port}") {
				return true
			}
		}
	}
	for _, value := range rt.Env {
		if strings.Contains(value, "{port}") {
			return true
		}
	}
	return false
}
