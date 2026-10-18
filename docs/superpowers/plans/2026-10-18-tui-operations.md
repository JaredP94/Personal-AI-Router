<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Terminal Interface Operations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The terminal interface can list and delete models, change an engine's port, update an engine, control engines on other cluster nodes, and show which node served a workload — each offered only where the engine actually supports it, and every destructive action confirmed.

**Architecture:** engine-manager derives each engine's effective capabilities from its manifest, platform and ownership, and returns them inside every `EngineStatus`. Because `engine:remote-get-installed` returns the peer's own `EngineStatus` list, a peer's capabilities arrive with no new remote method. The TUI and the desktop both gate actions on those capabilities; the desktop's hardcoded copies of the same facts are deleted. All TUI operations reuse existing broker methods.

**Tech Stack:** Go 1.25 (engine-manager, TUI on Bubble Tea), TypeScript/React/Vitest (desktop).

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §9 row "TUI operation gaps" (owner decision: fix now). Source map: `.superpowers/sdd/known-limitations-explore/tui.md` (git-ignored scratch; read it for file:line anchors and per-method params/results).

## Global Constraints

- Branch `release/known-limitations` (continuing after the GPU plan); signed-off commits (`git commit -s`); two-line SPDX header on every new file.
- No new npm or Go dependencies.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports across directories; static imports only; renderer never imports `src/electron/`; unused exports fail `npm run dead-code:check`.
- Go owns capability policy. TUI and desktop read `capabilities`; neither keeps an engine table of what is supported.
- Never offer, send, or implement oMLX pull or delete. Never route around engine-manager action validation.
- Remote control uses only the existing `engine:remote-*` methods keyed by `node` = peer `hostUuid`. No remote uninstall, update, persistent port change, or restart method is added.
- Destructive actions use the confirmation prompt from the worker-health plan (`services/nvpair-tui/ui/confirm.go`, `newConfirm(title, detail, onYes)`): default cancel, target frozen when the prompt opens, no RPC until `y`.
- TUI call lifetimes: `longOpTimeout = 31 * time.Minute` for install, update, pull (local and remote) and remote install; `lifecycleOpTimeout = 11 * time.Minute` for start, stop, restart, set-port, model delete (local and remote); the existing 35 s for everything else.
- JSON-RPC payload changes update producer, broker relay, every consumer, tests and docs together; regenerate `desktop/docs/services-api.md` with `npm run service-contracts:write`.
- `services/versions.json` per the branch rule (no second bump of an already-bumped component unless severity rises).
- Built-in inference submission stays out of scope; keep that bullet in the docs.

### Capability contract (`EngineStatus.capabilities`)

`{"list_models": bool, "pull_model": bool, "delete_model": bool, "delete_restarts_engine": bool, "set_port": bool, "install": bool, "uninstall": bool}`

| Field | True when |
| --- | --- |
| `list_models`, `pull_model`, `delete_model` | the effective manifest declares that action for this host's platform |
| `delete_restarts_engine` | `delete_model` is declared with `restart_after: true` |
| `set_port` | this platform's runtime start argv, CLI args or env reference `{port}`, and `engine:set-port` would not reject the engine as externally adopted |
| `install` | the effective manifest has a platform block for this host OS/arch |
| `uninstall` | `install` is true, an uninstall is declared, and the engine is not an externally adopted install |

A status without `capabilities` (an older peer) is treated by every client as all-false.

## Review Focus

1. **oMLX never shows pull or delete**, locally or on a peer, and its port is not editable (its start command has no `{port}`). Tasks 1, 2 and 5.
2. **A failed uninstall never proceeds to install** during update; the user sees which phase failed. Task 6.
3. **A confirmation acts on what it showed.** If the selection, target node, or model list changes while a prompt is open, `y` still acts on the frozen target. Tasks 5 and 6.
4. **A slow remote pull or install is not reported as failed** at 35 s; its completion is the RPC reply, not a progress event. Task 4.
5. **A workload whose destination node has left the cluster** still shows an identifier, and a failover updates the destination. Task 3.

---

### Task 1: engine-manager reports effective capabilities

**Files:** `services/nvpair-engine-manager/{executor.go,status.go,registry.go}`, new `services/nvpair-engine-manager/capabilities.go` (+ `capabilities_test.go`), `services/nvpair-engine-manager/README.md`, `services/tests/remote_engine_test.go`

**Interfaces (produced):** `type Capabilities struct` with the seven fields above (snake_case JSON); `EngineStatus` gains `Capabilities Capabilities \`json:"capabilities"\``; `func capabilitiesFor(m *Manifest, platform string, adopted bool) Capabilities` (adapt parameter types to the registry's real types). Every path that returns `EngineStatus` (get-installed, status, lifecycle results, set-port, remote-get-installed via the peer's own response) carries it.

- [ ] **Step 1:** Tests against the bundled manifests: Ollama → list/pull/delete true, `delete_restarts_engine` false, `set_port` true; LM Studio → list/pull/delete true, `delete_restarts_engine` true, `set_port` true; oMLX on darwin/arm64 → list true, pull false, delete false, `set_port` false, install true; oMLX on linux/amd64 → install false, uninstall false; externally adopted Ollama → `set_port` false, `uninstall` false. Extend `remote_engine_test.go` to assert the peer's capabilities arrive in `engine:remote-get-installed`. Run `go test ./...` in engine-manager and `services/tests` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...` in engine-manager and `services/tests` — PASS.
- [ ] **Step 4: Commit** — `feat(engine-manager): report each engine's effective capabilities`

### Task 2: Desktop gates on reported capabilities

**Files:** `desktop/src/shared/types/engines.ts` (status type), bridge parsing in `desktop/src/electron/service-bridge/{modular-supervisor.ts,modular-state.ts}`, `desktop/src/ui/constants/engine-capabilities.ts`, `desktop/src/ui/types/engine-manifest.ts`, `desktop/src/ui/components/ModelManager/{ModelManager.tsx,ModelRow.tsx}`, `desktop/src/ui/components/BackendRow/{BackendRow.tsx,PortsSection.tsx}`, `desktop/src/ui/stores/pending-actions.store.ts`, tests, regenerated `desktop/docs/services-api.md`

**Interfaces:** `type EngineCapabilityFlags = { listModels: boolean; pullModel: boolean; deleteModel: boolean; deleteRestartsEngine: boolean; setPort: boolean; install: boolean; uninstall: boolean }` parsed from the snake_case wire object (missing → all false), stored per node and engine with the engine status. Delete `hasDeleteModel`, `restartsOnModelDelete` and `hasEnginePort` from the static table and `EngineCaps`; their consumers read the reported flags for the node the row belongs to. Other static UI metadata stays.

- [ ] **Step 1:** Vitest: parser maps all seven flags and defaults missing to false; oMLX row with `deleteModel: false` shows no delete; LM Studio delete requires confirmation because `deleteRestartsEngine` is true; server-port field hidden when `setPort` is false; remote node row uses that node's flags. Run `npm run test:unit -- capabilities ModelRow BackendRow` — FAIL.
- [ ] **Step 2:** Implement; `npm run service-contracts:write`.
- [ ] **Step 3:** `npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check && npm run service-contracts:check` — PASS.
- [ ] **Step 4: Commit** — `feat(desktop): gate engine actions on reported capabilities`

### Task 3: TUI shows which node a workload went to

**Files:** `services/nvpair-tui/ui/workloads.go` (+ `workloads_test.go`), `services/nvpair-tui/ui/nodes.go` if a name lookup helper belongs there

**Interfaces:** Decode `scheduledOn` from workload snapshots and `workloads:upsert`. New column `DESTINATION` after the origin column: node friendly name from discovery/cluster identity when known, otherwise the first 8 characters of the UUID, otherwise `—`. A later upsert with a different `scheduledOn` replaces it.

- [ ] **Step 1:** Fake-broker tests: snapshot with known destination shows its name; unknown UUID shows 8-char prefix; absent shows `—`; failover upsert changes it; node removed from discovery keeps the prefix. Run `cd services/nvpair-tui && go test ./ui/...` — FAIL.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): show each workload's destination node`

### Task 4: TUI engine target selector and remote engine status

**Files:** `services/nvpair-tui/ui/{engines.go,recovery.go,nodes.go,rpccmd.go}`, tests

**Interfaces:**
- Engines view keeps `engineStatus` with `Capabilities` (decode the wire object; missing → all false).
- `n` cycles the target: `This node`, then each cluster member with a `hostUuid` (from discovery/cluster state already held by the TUI). The view title shows the target. For a peer, the table comes from `engine:remote-get-installed {node}` (refreshed on target switch and after each remote action); errors (unknown peer, trust failure, offline) are shown in the status line, not as an empty table.
- Remote actions available: start `s`, stop `x`, install `i` (`start: true`); restart, uninstall, update and port editing are hidden for a peer.
- `rpccmd.go` gains a call helper taking an explicit timeout; long and lifecycle operations use the Global Constraints values.

- [ ] **Step 1:** Fake-broker tests: `n` switches to a peer and calls `engine:remote-get-installed` with its `hostUuid`; `s` on a peer sends `engine:remote-start {node,engine}`; `r` and `u` on a peer send nothing; a held `engine:remote-install` reply after 40 s of fake time is still treated as pending, and the reply completes it; a trust error is displayed. Run — FAIL.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): control engines on cluster peers`

### Task 5: TUI model inventory, delete and capability-gated pull

**Files:** `services/nvpair-tui/ui/{engines.go,models.go (new),nodes.go}`, tests

**Interfaces:**
- `m` toggles a models pane for the selected engine. Local: `engine:models` (`modelsByEngine[engine]`, `loadedByEngine[engine]`); an absent key shows "Model list unavailable (engine stopped or unsupported)", an empty list shows "No models". Peer: the peer's `modelsByEngine`/`loadedByEngine` from discovery (decode them in `nodes.go`). Refresh after pull/delete and on `engine:models-changed`.
- `d` on a model (only when `capabilities.delete_model`): confirmation naming node, engine and model; when `delete_restarts_engine`, the detail adds "This restarts <engine>; running inference will be interrupted." Local: `engine:action {engine, action:"delete_model", params}` with the existing per-engine param key (Ollama `name`, otherwise `model`); peer: `engine:remote-delete-model {node,engine,model}`.
- `p` (pull) only when `capabilities.pull_model`, otherwise the key does nothing and help omits it.

- [ ] **Step 1:** Fake-broker tests: pane lists models and marks loaded ones; absent vs empty messages; oMLX status (pull/delete false) — `p` and `d` send nothing and are absent from help; LM Studio delete prompt contains the restart sentence; `d` then `y` after the list refreshed still deletes the originally selected model; peer delete uses the remote method. Run — FAIL.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): list, delete and pull models where the engine supports it`

### Task 6: TUI engine port, update, and confirmed destructive actions

**Files:** `services/nvpair-tui/ui/{engines.go,cluster.go,manualnodes.go}`, tests

**Interfaces:**
- `e` (local, `capabilities.set_port`): port prompt (1–65535, integer) then confirmation ("<engine> will restart on port <n> if it is running.") then `engine:set-port {engine,port}`; the returned status replaces the row; errors are shown.
- `U` update (local, `install` and `uninstall`): confirmation ("Update reinstalls <engine>: it is uninstalled, then installed and started. Inference stops meanwhile, and files the engine's uninstaller removes are deleted."); then `engine:uninstall {engine}`, and only on success `engine:install {engine, start:true}`; status line names the failing phase.
- Confirmation added to existing direct actions: `u` uninstall, cluster member remove, cluster leave (`L`), manual node remove.

- [ ] **Step 1:** Fake-broker tests: invalid port (0, 70000, `abc`) never sends; valid port sends exactly one `engine:set-port` after `y`; oMLX `e` does nothing; `e` and `U` with a peer target send nothing; update with failing uninstall sends no install and shows "uninstall failed"; update success sends uninstall then install in order; each newly confirmed action sends nothing on `n`/Esc and one RPC on `y`. Run — FAIL.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): change engine ports, update engines, and confirm destructive actions`

### Task 7: Versions, documentation and full verification

**Files:** `services/versions.json`; `docs/terminal-interface.mdx` (key bindings, Engines and Workloads sections, "What the Terminal Interface Cannot Do"), `services/nvpair-tui/README.md`, `services/nvpair-engine-manager/README.md`, `docs/known-issues.mdx` ("The Terminal Interface Cannot Do Everything…"), `desktop/docs/services-parity.md` if it lists capability flags, `docs/release-readiness.mdx`

- [ ] **Step 1:** Bump `nvpair-engine-manager` MINOR and `nvpair-tui` MINOR per the branch rule; product per the rule.
- [ ] **Step 2:** Document the new keys and gating. The remaining "cannot" list: send inference requests; uninstall, update or change the port of an engine on another node; pull or delete oMLX models (the engine offers no such action). Update the known issue to match. Add a "Terminal interface operations" entry to release readiness ("implemented, pending release testing").
- [ ] **Step 3:** `make check`; `cd desktop && npm run dead-code:check`; `go test -race ./...` in engine-manager, TUI and `services/tests` — PASS. `node scripts/spdx-headers.mjs` — clean.
- [ ] **Step 4: Commit** — `docs: document terminal interface operations`
