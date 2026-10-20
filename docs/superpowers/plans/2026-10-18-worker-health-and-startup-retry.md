<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Worker Health and Startup Retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The broker detects a worker that is alive but no longer answering, reports every worker's real state to the desktop and terminal interface, and retries optional workers whose first launch failed, using the same bounded budget it already uses for crashes.

**Architecture:** Every worker answers a standard `worker/health` request from its control loop. The broker supervisor probes each worker at a low frequency with a deadline the transport actually honours, publishes a per-worker state, and restarts only the workers whose teardown is known to be bounded and child-free. Initial spawn failures enter the supervisor's existing backoff loop; a missing default binary is a distinct state that never spins. Clients read `workers:get-status`, receive `workers:update` snapshots, and request `workers:restart`.

**Tech Stack:** Go 1.25 (broker, workers, shared), Bubble Tea (TUI), TypeScript/React/Vitest (desktop).

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §9 rows "Worker alive but wedged" and "Optional worker initial spawn failure" (owner decision: fix now). Source map: `.superpowers/sdd/known-limitations-explore/supervision.md` (git-ignored scratch; read it for file:line anchors).

## Global Constraints

- Branch `release/known-limitations` from `main` @ `40a8367`; signed-off commits (`git commit -s`); two-line SPDX header on every new file.
- Relay protocol stays v2. The broker stamps new notifications like every other notification; no protocol bump, no new required envelope field.
- No new npm or Go dependencies.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports across directories; static imports only; renderer never imports `src/electron/`; unused exports fail `npm run dead-code:check`.
- Go owns health, retry and restart policy. Desktop and TUI render broker-reported state and relay commands; no TypeScript or TUI-side policy.
- No legacy fallbacks: delete `startOptionalWorker`'s "continue without it" path and the TUI's crash-error-derived health table once replaced.
- JSON-RPC changes update the producer, broker, desktop bridge (`desktop/src/electron/service-bridge/`), TUI, tests and docs in the same change; regenerate `desktop/docs/services-api.md` with `npm run service-contracts:write`, never by hand.
- `services/versions.json`: additive IPC is MINOR, fixes are PATCH. A component whose version on this branch already differs from `main` is not bumped again unless the severity rises.
- Tests use fakes, injected timings, or the existing `services/tests` cross-process harness; no real engines.
- Say "engine", not "backend", in new copy. Never log prompts, responses, PINs or keys.
- Invalid explicit worker paths stay fatal. Scanner initial spawn failure stays fatal.
- Restart budget is unchanged and shared: `defaultRestartPolicy` (1 s → 16 s backoff, 5 attempts per unhealthy streak, 60 s healthy reset). Initial-spawn retries and hang restarts consume the same budget as crashes.
- Health probing: method `worker/health`, result `{"ok":true}`, default interval 30 s, deadline 5 s. `degraded` after 2 consecutive failed probes; a hang restart after 3 consecutive failed probes, for eligible workers only; any successful probe returns the worker to `running` and clears the failure count.
- Hang auto-restart eligibility (Ruling, below): eligible — `scanner`, `errors`, `settings`, `node-info`, `workload-manager`, `manual-nodes`, `cluster-manager`, `scheduler`. Never auto-restarted on hang — `engine-manager`, `proxy`, `lmstudio-proxy`, `omlx-proxy` (they report `degraded` and accept an explicit restart).
- Healthy reset (attempt counter to 0, crash error cleared) requires 60 s uptime **and** at least one successful health probe since that spawn.

**Ruling recorded with this plan:** engine-manager's process-mode engine stop waits without bound and a restart can orphan engines (macOS reclaim fails closed); the proxies serve inference over HTTP independently of their stdio control loop, so a silent control loop does not mean inference is broken and a restart would cut live streams. The spec allows auto-restart "only after worker-specific bounded teardown/orphan handling is verified", which holds today only for the eight child-free workers. Cost if wrong: four workers need a manual restart from the UI when hung.

## Review Focus

1. **A legitimately busy worker is not restarted.** An engine install or model pull running for 30 minutes, or an active inference stream, must never make engine-manager or a proxy restart; a cluster pairing exchange shorter than 60 s must not make cluster-manager restart. Tests in Tasks 3 and 4.
2. **A blocked stdin pipe cannot defeat a deadline.** A worker that stops reading stdin must fail the probe within its 5 s deadline, not hang the prober. Task 1.
3. **A missing binary does not spin.** A default worker binary that is absent stays `missing` with no retries until an explicit restart finds it. Task 4.
4. **A deterministically hanging worker exhausts the budget** and stops being restarted rather than looping forever. Task 3.
5. **Clients after reconnect see the current state**, not a stale or empty list: `workers:get-status` is part of epoch recovery in both clients. Tasks 5 and 6.

---

### Task 1: Deadlines that the transport honours, and the shared health contract

**Files:** `services/shared/jsonrpc/peer.go`, `services/shared/jsonrpc/peer_test.go`, new `services/shared/workerhealth/workerhealth.go` (+ `_test.go`), `services/shared/applog/stdinrpc.go` (+ test)

**Interfaces:**
- `package workerhealth`: `const Method = "worker/health"`; `type Result struct { OK bool \`json:"ok"\` }`; `func OK() Result`.
- `Peer.Call(ctx, method, params, result)` and `Peer.RelayRequest` return `ctx.Err()` when the context ends while the request write is still blocked; the write no longer runs before the context is observed. `Notify` keeps its signature and gains no deadline.
- `applog` StdinRPC answers an id-bearing `worker/health` request with `{"ok":true}` (it already owns a response writer for `log/set-level`).

- [ ] **Step 1:** Write `TestCallDeadlineWithBlockedWriter` (peer over an `io.Pipe` whose reader never reads; `Call` with a 100 ms context returns `context.DeadlineExceeded` within 1 s) and `TestStdinRPCAnswersWorkerHealth`. Run `cd services/shared && go test ./jsonrpc/... ./applog/...` — FAIL.
- [ ] **Step 2:** Implement: run the write so the call can return on `ctx.Done()` while the write is blocked; the blocked write unblocks when the peer's writer is closed. Add `workerhealth`. Handle the method in StdinRPC.
- [ ] **Step 3:** `cd services/shared && go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `fix(jsonrpc): honour call deadlines while the request write is blocked`

### Task 2: Every worker answers `worker/health` from its control loop

**Files:** the JSON-RPC dispatch of all 12 workers (anchors in the source map §2 table): `nvpair-node-scanner/scanner.go`, `nvpair-node-info/main.go` (via StdinRPC from Task 1), `nvpair-workload-manager/manager.go`, `nvpair-errors/manager.go`, `nvpair-engine-manager/manager.go`, `nvpair-manual-nodes/manager.go`, `nvpair-node-settings/manager.go`, `nvpair-cluster-manager/manager.go`, `nvpair-job-scheduler/manager.go`, `ollama-proxy/proxy.go`, `lmstudio-proxy/proxy.go`, `omlx-proxy/proxy.go`; one test per module beside its dispatch tests.

**Interfaces:** Consumes `workerhealth.Method`/`workerhealth.OK()`. Each worker answers **inline on its stdin dispatch loop** (never in a goroutine, never behind an engine operation lock), so a reply proves the control loop is responsive.

- [ ] **Step 1:** In each module add `TestWorkerHealth` driving the existing dispatch entry point with an id-bearing `worker/health` request and asserting result `{"ok":true}` with the same id. Run each module's `go test ./...` — FAIL.
- [ ] **Step 2:** Add the case to each dispatch switch.
- [ ] **Step 3:** Each module: `go test -race ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(workers): answer worker/health from every control loop`

### Task 3: Supervisor retries initial failures, detects hangs, and reports state

**Files:** `services/nvpair-ui-broker/supervisor.go`, `services/nvpair-ui-broker/supervisor_test.go`

**Interfaces (produced, used by Task 4):**
- `type WorkerState string` with values `"running"`, `"degraded"`, `"restarting"`, `"exhausted"`, `"missing"`.
- `type WorkerStatus struct { Name string \`json:"name"\`; State WorkerState \`json:"state"\`; Attempts int \`json:"attempts"\`; PID int \`json:"pid,omitempty"\`; LastError string \`json:"lastError,omitempty"\`; RestartOnHang bool \`json:"restartOnHang"\`; Since time.Time \`json:"since"\` }`.
- `type hangPolicy struct { probe func(ctx context.Context) error; interval, deadline time.Duration; degradeAfter, restartAfter int; restart bool }` — `restartAfter` is ignored when `restart` is false.
- `supervisor` gains `onState func(WorkerStatus)`, `hang hangPolicy`, and:
  - `StartRetrying()` — for optional workers: a failed first spawn reports `restarting` with the error, then enters the existing backoff loop under the same budget; exhausting it reports `exhausted`. `Start()` (synchronous, error-returning) stays for the scanner.
  - `Retry()` — from `exhausted`, resets the budget and spawns immediately; from `running`/`degraded`, performs the existing requested restart (not counted as a crash).
- A hang restart stops the old handle through the existing bounded `Stop` escalation, counts as a crash attempt (reason `"unresponsive"` via `onCrash`), and follows the crash backoff.
- The 60 s healthy reset also requires one successful probe since the spawn (Global Constraints).

- [ ] **Step 1:** Write tests with fake handles and millisecond-scale policies: `TestInitialSpawnFailureRetriesWithinBudget` (spawn fails twice then succeeds → `restarting`, `restarting`, `running`; attempts 2), `TestInitialSpawnFailureExhausts` (always fails → `exhausted` after initial + 5 attempts, no further spawns), `TestRetryFromExhausted`, `TestHangDegradesThenRestarts` (probe fails → `degraded` after 2, restart after 3, old handle stopped, new handle running), `TestHangWithoutRestartOnlyDegrades` (restart=false: `degraded`, never stopped; probe recovers → `running`), `TestBusyProbeSuccessKeepsRunning` (probe succeeds while handle runs indefinitely → no restart), `TestRepeatedHangExhausts`, `TestHealthyResetRequiresProbe` (60 s-equivalent uptime without a successful probe does not reset attempts). Run `go test ./... -run 'Supervisor|Spawn|Hang|Retry|Healthy'` in `services/nvpair-ui-broker` — FAIL.
- [ ] **Step 2:** Implement in `supervisor.go`. Keep `defaultRestartPolicy` values unchanged.
- [ ] **Step 3:** `go test -race ./...` in `services/nvpair-ui-broker` — PASS.
- [ ] **Step 4: Commit** — `feat(broker): retry failed initial spawns and detect hung workers`

### Task 4: Broker wiring, worker status RPCs, and cross-process proof

**Files:** `services/nvpair-ui-broker/{broker.go,main.go,broker_errors.go,nodeinfo.go,workloadmanager.go,rpcworker.go,proxy.go,discovery.go,errors.go,clustermanager.go}`, new `services/nvpair-ui-broker/workers.go` (+ `workers_test.go`), `services/tests/broker_supervision_test.go`, `services/tests/main_test.go`

**Interfaces:**
- Consumes Task 3's `WorkerStatus`, `StartRetrying`, `Retry`, `hangPolicy`.
- Every handle type exposes a health call that sends `worker/health` with the 5 s deadline. node-info and workload-manager readers currently drop responses; give them response correlation so the probe can complete (reuse `jsonrpc.Peer` routing where the reader allows it).
- `workers.go`: a registry holding one `WorkerStatus` per worker for all 12 names, in startup order. A worker whose default binary did not resolve is `missing` with `lastError` naming the expected path.
- Broker methods: `workers:get-status` (no params) → `{"workers": WorkerStatus[]}`; `workers:restart` `{"name": string}` → `WorkerStatus` (unknown name → `-32602`; `missing` re-runs default path resolution and starts the worker if the binary is now present, otherwise returns the unchanged `missing` status; `scanner` restart is allowed only when running). Notification `workers:update` `{"workers": WorkerStatus[]}` — a full snapshot on every change, relayed unconditionally like `errors:update`.
- Broker flag `--worker-health-interval` (duration, default `30s`, minimum `1s`) for diagnostics and cross-process tests; deadline stays 5 s, capped at the interval.
- Crash reports keep `supervisor:subprocess-crashed:<name>`; a hang restart reports message `subprocess <name> stopped responding`.
- Replace `startOptionalWorker` and the inline auxiliary start branches with `StartRetrying`; keep the existing port/alias ownership gates for Ollama, LM Studio and oMLX on exhaustion.

- [ ] **Step 1:** Unit tests in `workers_test.go`: snapshot lists 12 names in order; `missing` → restart with binary present → `running`; unknown name → `-32602`; every state change emits exactly one `workers:update`. Cross-process tests (Unix only, skip on Windows with a named reason) in `services/tests/broker_supervision_test.go`, broker started with `--worker-health-interval 1s`: `TestHungSchedulerRestarts` (SIGSTOP the scheduler PID → `degraded` then `restarting` then `running` with a new PID; old PID reaped), `TestHungEngineManagerOnlyDegrades` (SIGSTOP → `degraded`, PID unchanged for 10 s; SIGCONT → `running`), `TestMissingWorkerThenRestart` (start without the manual-nodes binary → `missing`; copy it in; `workers:restart` → `running`). Add `omlx-proxy` to the `services/tests` build inventory. Run — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `cd services/nvpair-ui-broker && go test -race ./...`; `cd services/tests && go test -race ./...` — PASS (record any skips by name).
- [ ] **Step 4: Commit** — `feat(broker): report worker health and accept explicit worker restarts`

### Task 5: Desktop shows worker state and offers restart

**Files:** `desktop/src/electron/service-bridge/{modular-supervisor.ts,modular-state.ts}`, `desktop/src/shared/types/` (new `workers.ts`), `desktop/src/shared/types/ipc-channels.ts`, preload bridge, `desktop/src/ui/api/pair-api.ts`, new `desktop/src/ui/stores/workers.store.ts`, new `desktop/src/ui/components/ServiceSettings/WorkersCard.tsx`, `ServiceSettings.tsx`, tests under `desktop/tests/`, regenerated `desktop/docs/services-api.md`

**Interfaces:**
- `type WorkerState = 'running' | 'degraded' | 'restarting' | 'exhausted' | 'missing'`; `type WorkerStatus = { name: string; state: WorkerState; attempts: number; pid?: number; lastError?: string; restartOnHang: boolean; since: string }`. Unknown `state` strings from the broker are rejected by the parser (the row is dropped and logged), not coerced.
- Bridge: hydrate `workers:get-status` on every new epoch alongside the other domain snapshots; replace state on `workers:update`; `restartWorker(name)` calls `workers:restart`.
- Settings → Service gains a **Workers** card: one row per worker with name, state badge, attempts, last error, and **Restart**. Restarting `engine-manager` asks for confirmation ("Engines PAIR started may keep running or stop; running inference may be interrupted."); restarting a proxy asks for confirmation ("Running inference through this proxy will be interrupted."). `degraded` rows whose `restartOnHang` is false show "Not responding — restart manually".

- [ ] **Step 1:** Vitest: parser accepts/rejects; epoch change re-hydrates; `workers:update` replaces the list; card renders each state; engine-manager restart requires confirmation and sends exactly one `workers:restart` on confirm, none on cancel. Run `npm run test:unit -- workers` — FAIL.
- [ ] **Step 2:** Implement; `npm run service-contracts:write`.
- [ ] **Step 3:** `npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check && npm run service-contracts:check` — PASS.
- [ ] **Step 4: Commit** — `feat(desktop): show worker health and restart workers from Settings`

### Task 6: TUI overview from worker status, with a shared confirmation prompt

**Files:** `services/nvpair-tui/ui/{health.go,health_test.go,recovery.go,model.go}`, new `services/nvpair-tui/ui/confirm.go` (+ `confirm_test.go`)

**Interfaces (produced, reused by the TUI operations plan):**
- `type confirmPrompt struct` with `func newConfirm(title, detail string, onYes tea.Cmd) *confirmPrompt`; while open it captures input through the existing `inputCapturer` path, so `q`, Tab and other global keys cannot bypass it; `y` runs `onYes`, `n`/Esc cancels; default is cancel; the target is frozen at creation.
- Overview reads `workers:get-status` in the epoch recovery path and applies `workers:update`; shows all 12 workers with state, attempts and last error. Delete the hardcoded nine-row table and crash-error-derived health.
- `R` on the selected worker opens a confirmation, then calls `workers:restart`.

- [ ] **Step 1:** Tests with the existing fake-broker harness (`ui/model_test.go`): recovery hydrates 12 rows; `workers:update` replaces rows; `R` then `n` sends no RPC; `R` then `y` sends exactly one `workers:restart` with the name captured at prompt time even if selection changed; `q` while the prompt is open does not quit. Run `cd services/nvpair-tui && go test ./ui/...` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...` in `services/nvpair-tui` — PASS.
- [ ] **Step 4: Commit** — `feat(tui): show worker health from the broker and restart workers`

### Task 7: Versions, documentation and full verification

**Files:** `services/versions.json`; docs listed in the source map §6 (`services/readme.md`, `AGENTS.md`, `docs/architecture.mdx`, `docs/known-issues.mdx`, `docs/building.mdx`, `docs/overview.mdx`, `docs/developing.mdx`, `services/nvpair-ui-broker/README.md`, every worker README's method list, `desktop/docs/architecture.md`, `desktop/docs/services-backend.md`, `services/nvpair-tui/README.md`, `docs/terminal-interface.mdx`); `docs/release-readiness.mdx`

- [ ] **Step 1:** Bump: `nvpair-ui-broker` MINOR, `nvpair-tui` MINOR, each of the 12 workers MINOR (new IPC method), `product`/`installer` MINOR (following the Global Constraints rule for already-bumped components).
- [ ] **Step 2:** Update docs to describe the verified behaviour only: worker health probing (30 s, 5 s, degraded after 2, restart after 3 for the eight eligible workers), initial-spawn retry under the shared budget, `missing` state, the new RPCs, and that engine-manager and proxies are never auto-restarted on a hang. Keep the 15 s heartbeat / 45 s silence text as a separate layer. Remove the "Worker Hangs …" known-issue text that is no longer true; keep any residual limitation (Windows cross-process proof skipped) stated plainly. Add a "Worker health" entry to `docs/release-readiness.mdx` marked "implemented, pending release testing".
- [ ] **Step 3:** From the repo root `make check`; `cd desktop && npm run dead-code:check`; `go test -race ./...` in every changed services module and `services/tests` — PASS. `node scripts/spdx-headers.mjs` — clean.
- [ ] **Step 4: Commit** — `docs: document worker health and startup retry`
