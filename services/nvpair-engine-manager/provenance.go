// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
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
	// Creates and Identity bind the claim to the exact thing the install put
	// in place (install.creates) by its file identity. A copy put there later
	// — by the user, or by a vendor updater that replaces the bundle — is a
	// different file, and the claim no longer covers it.
	Creates  string `json:"creates,omitempty"`
	Identity string `json:"identity,omitempty"`
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
// identity, when creates is set, is the identity of what the install created,
// taken as it created it; the marker is refused when it no longer matches.
func writeInstallMarker(installDir, engine, creates, identity string) error {
	path := installMarkerPath(installDir)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	marker := installMarker{
		Engine:      engine,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if creates != "" {
		if identity == "" || fileIdentity(expandPath(creates)) != identity {
			return fmt.Errorf("%s is no longer what the install created", creates)
		}
		marker.Creates = creates
		marker.Identity = identity
	}
	return writeJSONAtomic(path, marker)
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

// installedByPAIR reports whether a claim stands. With no creates (an engine
// whose install has nowhere a user copy could be, such as LM Studio's CLI),
// the marker alone is the claim. When the manifest declares creates, the
// marker must name that path and it must still be the very file the install
// created; a marker from before identities were recorded covers nothing.
func installedByPAIR(installDir, creates string) bool {
	path := installMarkerPath(installDir)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var marker installMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}
	if creates == "" {
		return true
	}
	if marker.Creates != creates || marker.Identity == "" {
		return false
	}
	id := fileIdentity(expandPath(marker.Creates))
	return id != "" && id == marker.Identity
}
