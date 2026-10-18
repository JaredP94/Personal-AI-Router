<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine Launch Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A user can run the CLI of any engine PAIR manages with `nvpair engine <engine> [args…]`, without knowing where it is installed and without PAIR changing their `PATH`.

**Architecture:** engine-manager gains two one-shot modes that resolve an engine's executable and environment from the same effective manifests it uses at runtime, then exit without starting the service. `nvpair-tui` (which the existing `nvpair` wrapper runs) gains an `engine` subcommand that asks its sibling engine-manager for the resolution and runs the engine CLI with it. No broker is started, so it is safe while the desktop app is running.

**Tech Stack:** Go 1.25 (engine-manager, nvpair-tui).

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §9 rows "Installed engines not on PATH" and (disposition only) "macOS unclustered LAN reachability" (owner decision: fix now). Source map: `.superpowers/sdd/known-limitations-explore/gpu-and-path.md` §B (git-ignored scratch; read it for file:line anchors).

## Global Constraints

- Branch `release/known-limitations` (continuing after the TUI operations plan); signed-off commits (`git commit -s`); two-line SPDX header on every new file.
- No new Go dependencies.
- Never modify `PATH` (shell profiles, registry, `/etc/paths*`) and never guess vendor paths outside the effective manifest. Resolution lives in engine-manager; nvpair-tui holds no engine table.
- The `engine` subcommand never starts a broker or any worker, and never opens the TUI.
- Resolution substitutes `{host}` with `127.0.0.1` and `{port}` with the engine's effective configured port (persisted override, else manifest default), so a client CLI talks to the PAIR-managed engine, not the proxy.
- An engine adopted as an external HTTP service with no filesystem executable is an error ("<engine> is running as an external service; PAIR has no executable for it"), never a guessed path.
- `services/versions.json` per the branch rule.

**Ruling recorded with this plan:** the spec's macOS unclustered-reachability item needs a controlled second machine to reproduce, and the spec forbids a guessed fix. It is classified for this release as **explicitly accepted and documented**, with the existing known-issue workaround (restart PAIR), and stays open in release readiness. Cost if wrong: one known issue ships unfixed, as it does today.

## Review Focus

1. **Arguments reach the engine untouched**, including flags that look like PAIR's own (`--version`, `-h`, `--`), quoted spaces, and the engine's exit code. Task 2.
2. **Running it beside the desktop app is safe**: no broker, no port binding, no settings write. Task 2.
3. **An engine that is not installed, or not supported on this platform**, prints a one-line reason and exits non-zero, rather than running something else. Tasks 1 and 2.
4. **Ollama's CLI targets the engine port** (`OLLAMA_HOST=127.0.0.1:<engine port>`), not the proxy's 11434, and Linux keeps `LD_LIBRARY_PATH`. Task 1.
5. **A user manifest override** (custom install path or port) is honoured exactly as the running service honours it. Task 1.

---

### Task 1: engine-manager resolves engine executables in one-shot mode

**Files:** `services/nvpair-engine-manager/main.go`, new `services/nvpair-engine-manager/resolve.go` (+ `resolve_test.go`), `services/nvpair-engine-manager/README.md`

**Interfaces (produced, used by Task 2):**
- `nvpair-engine-manager --list-engines` prints a JSON array `[{"engine": string, "display_name": string, "installed": bool, "supported": bool}]` and exits 0.
- `nvpair-engine-manager --resolve <engine>` prints `{"engine": string, "path": string, "env": {string: string}}` and exits 0; on failure prints one line to stderr and exits 2. `path` is the detected executable for process-mode engines (the same first match `Detect` stores) and the expanded runtime CLI for command-mode engines (`lms`, `omlx`); it must exist on disk. `env` is the platform's declared runtime env, resolved as in Global Constraints.
- Both modes read the same config root and bundled + user manifests as the service (no stdio transport, no HTTP listeners, no cluster identity) and accept the existing `--log-level` flag only.
- `func resolveEngine(reg *Registry, engine, goos, goarch string) (Resolution, error)` (adapt to the registry's real types) so tests can use a temp root and fake manifests.

- [ ] **Step 1:** Tests with a temp config root and fake manifests/binaries: process-mode engine installed → path and `OLLAMA_HOST=127.0.0.1:<port>` and `LD_LIBRARY_PATH` resolved; persisted port override changes `OLLAMA_HOST`; command-mode engine → CLI path; not installed → error "is not installed"; unsupported platform → error "is not supported on"; externally adopted service with no binary → the external-service error; unknown engine → "unknown engine". Test `--list-engines` output shape. Run `cd services/nvpair-engine-manager && go test ./...` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...`; `GOOS=windows go vet ./...`; `GOOS=linux go vet ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(engine-manager): resolve engine executables without starting the service`

### Task 2: `nvpair engine` runs an engine's CLI

**Files:** `services/nvpair-tui/main.go`, new `services/nvpair-tui/enginecmd.go` (+ `enginecmd_test.go`, `enginecmd_unix.go`, `enginecmd_windows.go`), `services/nvpair-tui/README.md`

**Interfaces:**
- If `os.Args[1] == "engine"`, nvpair-tui handles it before parsing TUI flags. `nvpair engine` (no engine) runs the sibling `nvpair-engine-manager --list-engines` and prints a table (ENGINE, NAME, INSTALLED, SUPPORTED) plus usage `nvpair engine <engine> [args...]`. `nvpair engine <engine> [args…]` runs `--resolve <engine>`, then executes `path` with every remaining argument unchanged and the current environment overlaid with `env`.
- Sibling resolution mirrors how nvpair-tui already finds its broker (same directory as the running executable; never `PATH`).
- Unix: replace the process (`syscall.Exec`). Windows: run the child with inherited stdio, forward Ctrl-C, and exit with its exit code.
- Errors from the resolver are printed as `nvpair: <message>` with exit code 2.

- [ ] **Step 1:** Tests that re-exec the test binary as a fake engine-manager and a fake engine (existing pattern in `supervisor_test.go`): arguments `--version`, `-h`, `--`, `"a b"` reach the engine exactly; env overlay applied; engine exit code 7 propagates; resolver error → stderr `nvpair: …`, exit 2; no-argument form prints the table; no broker process is spawned (the fake broker path is never executed). Run `cd services/nvpair-tui && go test ./...` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...`; `GOOS=windows go vet ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): run managed engine CLIs with nvpair engine`

### Task 3: Versions, documentation, and the release disposition of every known limitation

**Files:** `services/versions.json`; `docs/getting-started.mdx` ("Using an engine's own command line"), `docs/terminal-interface.mdx` (`nvpair` command section), `docs/known-issues.mdx` ("Engine Binaries Are Not on Your PATH", "macOS Can Stop Answering LAN Connections While Unclustered"), `services/nvpair-tui/README.md`, `services/nvpair-engine-manager/README.md`, `docs/release-readiness.mdx`

- [ ] **Step 1:** Bump `nvpair-engine-manager` MINOR and `nvpair-tui` MINOR per the branch rule; product per the rule.
- [ ] **Step 2:** Document `nvpair engine` as the primary way to run an engine CLI, keeping the full-path table as the fallback when `nvpair` is not on `PATH` (headless/source builds, unwritable `/usr/local/bin`). Replace the PATH known issue with that residual condition. Leave the macOS reachability known issue in place and state that it is accepted for this release pending reproduction on a controlled second machine.
- [ ] **Step 3:** Add a "Known Limitations (spec §9)" section to `docs/release-readiness.mdx` with one row per limitation and its disposition: worker hangs — implemented (auto-restart for eight child-free workers; engine-manager and proxies degraded + manual restart); optional initial spawn failure — implemented; macOS unclustered reachability — accepted and documented, needs controlled reproduction; GPU memory semantics — implemented (Linux AMD/Intel remain unavailable; Windows runtime untested on this host); TUI operations — implemented (remote uninstall/update/port and oMLX pull/delete unsupported by design); engine CLI access — implemented via `nvpair engine`. Each "implemented" row says "pending release testing".
- [ ] **Step 4:** `make check`; `cd desktop && npm run dead-code:check`; `go test -race ./...` in engine-manager, TUI and `services/tests` — PASS. `node scripts/spdx-headers.mjs` — clean.
- [ ] **Step 5: Commit** — `docs: document nvpair engine and the disposition of known limitations`
