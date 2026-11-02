// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"time"
)

// installMarkerName records that this service, rather than the user, performed
// an engine's install.
//
// Ownership is otherwise unknowable for an engine whose vendor places it in a
// shared location. LM Studio's CLI lives in ~/.lmstudio whether PAIR or the LM
// Studio app put it there, and the oMLX app installs its CLI shim in ~/.omlx,
// beside the user's own settings and models. "The binary sits under our
// install directory" — the test that answers this for Ollama — is never true
// for these, so PAIR cannot otherwise tell an install it performed from one
// the user already had. Uninstall has to know: removing an engine the user
// installed themselves, with the data they built up in it, is not PAIR's call
// to make.
//
// The marker lives in the per-engine install directory, which is created for
// every install regardless of where the engine itself lands.
const installMarkerName = "installed-by-pair.json"

type installMarker struct {
	Engine      string `json:"engine"`
	InstalledAt string `json:"installed_at"`
}

func installMarkerPath(installDir string) string {
	if installDir == "" {
		return ""
	}
	return filepath.Join(installDir, installMarkerName)
}

// writeInstallMarker claims an install. Called only after an install this
// service actually performed and detected — never on the adoption paths, which
// return before any install work happens.
func writeInstallMarker(installDir, engine string) error {
	path := installMarkerPath(installDir)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(path, installMarker{
		Engine:      engine,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// clearInstallMarker drops the claim after an uninstall. An engine whose
// install directory is itself removed loses the marker with it; one installed
// by a vendor script elsewhere does not, and a stale claim would let PAIR
// remove a copy the user later installed themselves.
func clearInstallMarker(installDir string) {
	if path := installMarkerPath(installDir); path != "" {
		_ = os.Remove(path)
	}
}

func installedByPAIR(installDir string) bool {
	path := installMarkerPath(installDir)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
