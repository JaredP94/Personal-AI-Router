// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

func canMoveAdoptedEngine(rt Runtime) bool {
	return rt.modeOrDefault() == "command" && rt.Stop != nil && len(rt.Stop.Cmd) > 0
}

// SetPort persists an engine's chosen server port as a manifest override and
// applies it: the running session is bounced onto the new port (if running)
// and the cached port is updated so a later start/adopt uses it. Persistence
// is via the manifest (the single source of truth) — see persistPort — so
// the port survives a restart with no separate override store. Held under the
// engine's op lock so it can't interleave with another lifecycle op.
//
// A running, adopted process-mode engine is refused. An identified command-mode
// engine may be moved only when its manifest provides an official stop command.
func (e *Executor) SetPort(ctx context.Context, engine string, port int) (EngineStatus, error) {
	if port < 1 || port > 65535 {
		return EngineStatus{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if err := e.reservedPortError(port); err != nil {
		return EngineStatus{}, err
	}
	st, err := e.state(engine)
	if err != nil {
		return EngineStatus{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()

	st.mu.Lock()
	wasRunning := st.running
	adopted := st.adopted
	oldPort := st.port
	st.mu.Unlock()

	// Adopted process-mode engines and command-mode engines without an official
	// stop command remain externally managed. Refuse rather than killing an
	// unknown process or spawning a duplicate listener on the new port.
	if wasRunning && adopted && !canMoveAdoptedEngine(st.plat.Runtime) {
		return EngineStatus{}, fmt.Errorf("cannot change %s's port: it is running under external management (NVPAIR adopted it rather than starting it), so NVPAIR cannot move it — stop it in its own app first, then set the port", engine)
	}

	// Stop on the old port before switching, so a port-dependent stop (e.g.
	// a command-mode engine) targets the address it actually started on.
	if wasRunning {
		if err := e.doStop(st, engine); err != nil {
			return EngineStatus{}, err
		}
	}

	if err := e.persistPort(engine, port); err != nil {
		if !wasRunning {
			return EngineStatus{}, err
		}
		st.mu.Lock()
		st.port = oldPort
		if st.plat != nil {
			st.plat.Runtime.Port = oldPort
		}
		st.mu.Unlock()
		restartErr := e.doStart(ctx, st, engine, startOpts{})
		return EngineStatus{}, errors.Join(err, restartErr)
	}

	st.mu.Lock()
	st.port = port
	if st.plat != nil {
		st.plat.Runtime.Port = port
	}
	st.mu.Unlock()

	if wasRunning {
		// doStart re-reads st.port and emits engine:state-changed itself.
		if err := e.doStart(ctx, st, engine, startOpts{}); err != nil {
			return EngineStatus{}, err
		}
	} else {
		// No process to bounce, but the port changed — let subscribers see it.
		e.emitState(engine)
	}
	return e.snapshot(engine, st), nil
}

// persistPort merges runtime.port into the existing per-engine override,
// preserving all unrelated fields, including unknown fields. Restoring a
// bundled default removes the top-level and host-platform port pins; a
// port-only override is deleted. Writes remain atomic (tmp + rename).
func (e *Executor) persistPort(engine string, port int) error {
	if e.overrideDir == "" {
		return fmt.Errorf("no config directory available to persist the port")
	}
	if err := os.MkdirAll(e.overrideDir, 0o755); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}
	path := filepath.Join(e.overrideDir, engine+".json")
	// Raw messages preserve values without float64 round-tripping or dropping
	// fields unknown to this version of the manifest schema.
	m := map[string]json.RawMessage{}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read override: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(existing, &m); err != nil {
			return fmt.Errorf("parse override %s: %w", path, err)
		}
		if m == nil {
			return fmt.Errorf("parse override %s: expected an object", path)
		}
	}
	rt, err := overrideObject(m, "runtime")
	if err != nil {
		return fmt.Errorf("parse override %s: %w", path, err)
	}
	platforms, err := overrideObject(m, "platforms")
	if err != nil {
		return fmt.Errorf("parse override %s: %w", path, err)
	}
	manifest, _ := e.reg.Get(engine) // SetPort has already validated the engine.
	key, _ := manifest.platformKeyFor(runtime.GOOS, runtime.GOARCH)
	platform, err := overrideObject(platforms, key)
	if err != nil {
		return fmt.Errorf("parse override platform %s: %w", path, err)
	}
	platformRT, err := overrideObject(platform, "runtime")
	if err != nil {
		return fmt.Errorf("parse override platform %s: %w", path, err)
	}
	def, bundled := e.reg.bundledDefaultPort(engine)
	restore := bundled && def == port
	_, platformPinned := platformRT["port"]
	if platformPinned {
		if err := updatePortOverride(platform, platformRT, port, restore); err != nil {
			return err
		}
		if err := storeOverrideObject(platforms, key, platform); err != nil {
			return err
		}
		if err := storeOverrideObject(m, "platforms", platforms); err != nil {
			return err
		}
	}
	_, topPinned := rt["port"]
	if (!restore && !platformPinned) || (restore && topPinned) {
		if err := updatePortOverride(m, rt, port, restore); err != nil {
			return err
		}
	}
	if restore && (len(m) == 0 || (len(m) == 1 && m["engine"] != nil)) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove override: %w", err)
		}
		return nil
	}
	if _, ok := m["engine"]; !ok {
		m["engine"] = json.RawMessage(strconv.Quote(engine))
	}
	return writeJSONAtomic(path, m)
}

// overrideObject decodes only the object being edited, leaving all other raw
// values intact. Absent/null objects are allocated only in memory until changed.
func overrideObject(parent map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	object := map[string]json.RawMessage{}
	if raw, ok := parent[key]; ok {
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	return object, nil
}

func updatePortOverride(parent, rt map[string]json.RawMessage, port int, restore bool) error {
	if restore {
		delete(rt, "port")
	} else {
		rt["port"] = json.RawMessage(strconv.Itoa(port))
	}
	return storeOverrideObject(parent, "runtime", rt)
}

// storeOverrideObject is called only after changing a child, so pre-existing
// empty objects elsewhere are preserved rather than swept up during pruning.
func storeOverrideObject(parent map[string]json.RawMessage, key string, child map[string]json.RawMessage) error {
	if len(child) == 0 {
		delete(parent, key)
		return nil
	}
	raw, err := json.Marshal(child)
	if err != nil {
		return fmt.Errorf("marshal override %s: %w", key, err)
	}
	parent[key] = raw
	return nil
}

// writeJSONAtomic marshals v and writes it to path via a tmp file + rename so
// a crash mid-write can't leave a truncated manifest behind.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write override: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename override: %w", err)
	}
	return nil
}
