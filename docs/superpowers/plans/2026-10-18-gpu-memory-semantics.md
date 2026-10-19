<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# GPU Memory Semantics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every GPU PAIR reports says what kind of memory it has (dedicated, unified, shared, or unavailable), and an unknown reading is shown as unknown rather than as a measured zero.

**Architecture:** node-info publishes a `memory_kind` per adapter and emits a numeric reading only when it was measured (a measured zero is emitted; an unknown is omitted). Shared Go records carry the same fields through scanner, manual nodes and broker unchanged. The desktop keeps unknown values as `null` from parsing through history and charts, and labels memory by kind. No new driver support.

**Tech Stack:** Go 1.25 (node-info, shared noderec, scanner, manual-nodes, broker, scheduler), TypeScript/React/Vitest (desktop).

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §9 row "Misleading shared GPU memory / unavailable Linux metrics" (owner decision: fix now). Source map: `.superpowers/sdd/known-limitations-explore/gpu-and-path.md` §A (git-ignored scratch; read it for file:line anchors).

## Global Constraints

- Branch `release/known-limitations` (continuing after the worker-health plan); signed-off commits (`git commit -s`); two-line SPDX header on every new file.
- No new npm or Go dependencies. No new GPU driver or vendor support (no AMD/Intel sysfs collectors).
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports across directories; static imports only; renderer never imports `src/electron/`; unused exports fail `npm run dead-code:check`.
- Go owns classification. The desktop renders `memory_kind`; it never infers kind from GPU names.
- Never call system RAM dedicated VRAM. Never sum memory across adapters. Never render an unknown reading as `0`.
- No legacy fallbacks: delete the non-Apple macOS fallback that reports `Alloc system memory` as VRAM used, and the desktop's absent-to-zero coercion for GPU fields.
- Wire changes update the producer, every Go consumer, the desktop parser, tests and docs in the same change.
- `services/versions.json`: additive field is MINOR, fix is PATCH; a component already bumped on this branch (differs from `main`) is not bumped again unless the severity rises.
- Scheduler behaviour is unchanged: invalid or stale telemetry stays neutral pressure; utilization pressure uses the maximum over adapters with a measured utilization.

### Wire contract (node-info `/v1/node-info` `GPUs[]`, `noderec.GPUInfo`, manual-nodes `gpus[]`)

| Field | Meaning |
| --- | --- |
| `name` | string, always present |
| `memory_kind` | `"dedicated"` \| `"unified"` \| `"shared"` \| `"unavailable"`, always present |
| `vram_bytes` | capacity of this adapter's memory of that kind; omitted when unknown; for `unified` it is physical system memory; for `shared` it is the shared budget |
| `vram_used_bytes` | present iff measured (`0` allowed) |
| `shared_bytes`, `shared_used_bytes` | Windows `dedicated` adapters only: the shared-system-memory budget and its usage; omitted elsewhere and when unknown |
| `utilization_percent` | present iff measured (`0` allowed) |

Classification per platform:

| Source | `memory_kind` | Capacity / used |
| --- | --- | --- |
| macOS Apple GPU (AGX class or `Apple ` name) | `unified` | physical RAM / GPU-allocated (`Alloc system memory`) |
| macOS non-Apple Intel GPU (class, model, or entry name contains `Intel`, case-insensitive) | `shared` | `VRAM,totalMB` shared budget / `vramUsedBytes` or `inUseVidMemoryBytes` only; never `Alloc system memory`; otherwise used omitted |
| macOS other non-Apple GPU (AMD/NVIDIA discrete) | `dedicated` | `VRAM,totalMB` / `vramUsedBytes` or `inUseVidMemoryBytes` only; otherwise used omitted |
| Linux NVIDIA, numeric `memory.total` | `dedicated` | nvidia-smi |
| Linux NVIDIA, `[N/A]`/`Not Supported` total | `unified` | physical RAM / system used RAM |
| Linux NVIDIA, malformed total | `unavailable` | omitted |
| Linux names-only (ghw) and other OSes | `unavailable` | omitted; no utilization |
| Windows, `DedicatedVideoMemory > 0` | `dedicated` | dedicated / PDH `Dedicated Usage`; plus `shared_bytes` = `SharedSystemMemory`, `shared_used_bytes` = PDH `Shared Usage` |
| Windows, `DedicatedVideoMemory == 0` | `shared` | `SharedSystemMemory` / PDH `Shared Usage` |

## Review Focus

1. **An idle GPU still reads 0 %.** A measured idle utilization of `0` must reach the desktop as `0`, not as unknown. Tasks 1 and 4.
2. **One unknown adapter does not blank a known one.** On a two-GPU node where only one adapter reports utilization, the other shows unknown and the first shows its value. Tasks 1 and 4.
3. **Two adapters sharing system memory are never added together** anywhere a total is displayed. Task 5.
4. **An older peer without `memory_kind`** (mixed-version cluster) renders as `unavailable` memory rather than crashing or claiming VRAM. Tasks 3 and 4.
5. **Chart history with gaps** draws breaks, not drops to zero, and recovers when readings return. Task 5.

---

### Task 1: node-info publishes memory kind and measured-only readings

**Files:** `services/nvpair-node-info/{main.go,stats.go,ioreg_parse.go,stats_darwin.go,gpu_linux.go,stats_linux.go,gpu_other.go,stats_other.go}`, new platform-neutral `services/nvpair-node-info/gpu_classify.go` (+ `gpu_classify_test.go`), existing tests `stats_test.go`, `ioreg_parse_test.go`, `stats_linux_test.go`, `README.md`

**Interfaces (produced):**
- `type MemoryKind string` with constants `MemoryDedicated = "dedicated"`, `MemoryUnified = "unified"`, `MemoryShared = "shared"`, `MemoryUnavailable = "unavailable"`.
- `GPUInfo` becomes `{ Name string \`json:"name"\`; MemoryKind MemoryKind \`json:"memory_kind"\`; VRAMBytes *uint64 \`json:"vram_bytes,omitempty"\`; VRAMUsedBytes *uint64 \`json:"vram_used_bytes,omitempty"\`; SharedBytes *uint64 \`json:"shared_bytes,omitempty"\`; SharedUsedBytes *uint64 \`json:"shared_used_bytes,omitempty"\`; UtilizationPercent *uint32 \`json:"utilization_percent,omitempty"\` }`.
- Internal `gpuStat` keeps per-adapter validity for utilization and memory used (macOS currently discards per-record validity; keep it).
- Move the pure parsers that decide kind (ioreg normalization, nvidia-smi static and dynamic CSV parsing) into untagged files so their fixture tests run on every host; platform files keep only the command execution.

- [ ] **Step 1:** Fixture tests in `gpu_classify_test.go` and the existing parser tests: Apple M-series → `unified` with physical-RAM capacity; AMD discrete on macOS with no dedicated-used keys → `dedicated`, used omitted (not `Alloc system memory`); nvidia-smi discrete → `dedicated`; `[N/A]` total → `unified`; malformed total → `unavailable`; ghw names-only → `unavailable` with no utilization; two adapters where one reports utilization `0` and the other `[N/A]` → first has `utilization_percent: 0` in JSON, second omits it; JSON of a measured zero contains `"utilization_percent":0`. Run `cd services/nvpair-node-info && go test ./...` — FAIL.
- [ ] **Step 2:** Implement per the classification table.
- [ ] **Step 3:** `go test -race ./...`; `GOOS=linux go vet ./...`; `GOOS=windows go vet ./...` — PASS.
- [ ] **Step 4: Commit** — `feat(node-info): report GPU memory kind and only measured readings`

### Task 2: Windows shared memory budget and usage

**Files:** `services/nvpair-node-info/{gpu_windows.go,stats_windows.go}`, `stats_windows_test.go`, platform-neutral decode helpers in `gpu_classify.go` if needed

**Interfaces:** Consumes Task 1's `GPUInfo`. Publish `SharedSystemMemory` from the existing DXGI descriptor and read `\GPU Adapter Memory(*)\Shared Usage` in the existing PDH query, joined by the same LUID as `Dedicated Usage`. Classification per the table; never add dedicated and shared together.

- [ ] **Step 1:** Platform-neutral tests for the decode/classify helper: dedicated 8 GiB + shared 16 GiB → `dedicated` with `shared_bytes`; dedicated 0 + shared 16 GiB → `shared` with capacity 16 GiB and no `shared_bytes`; missing Shared Usage counter → `shared_used_bytes` omitted. Run — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...`; `GOOS=windows go vet ./...`; `GOOS=windows go test -c -o /dev/null ./...` — PASS. (Windows runtime behaviour is untested on this host; say so in the report.)
- [ ] **Step 4: Commit** — `feat(node-info): report Windows shared GPU memory separately`

### Task 3: Carry the contract through scanner, manual nodes, broker and scheduler

**Files:** `services/shared/noderec/noderec.go` (+ test), `services/nvpair-node-scanner/{scanner.go,daemon.go}` (+ tests), `services/nvpair-manual-nodes/manager.go` (+ `manager_test.go`), `services/nvpair-ui-broker/{discovery.go,manualnodes.go,relay/relay.go}` (+ tests), `services/nvpair-job-scheduler/telemetry.go` only if its reduction changes, `services/tests/scheduler_interop_test.go`

**Interfaces:** `noderec.GPUInfo` mirrors Task 1's fields and pointer types exactly (manual-nodes' duplicate type too). Utilization reduction for scheduling (scanner and manual nodes) takes the maximum over adapters with a non-nil utilization; if none, the existing validity rule applies. GPU change detection compares the new fields. A record without `memory_kind` decodes as `"unavailable"`.

- [ ] **Step 1:** Tests: round-trip of every field including measured zeros through noderec JSON; scanner reduction ignores nil utilization and keeps a measured `0`; manual-node equality notices a `memory_kind` change; a legacy record without `memory_kind` decodes as `unavailable`; interop test still passes. Run each module's `go test ./...` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `go test -race ./...` in shared, scanner, manual-nodes, broker, scheduler and `services/tests` — PASS.
- [ ] **Step 4: Commit** — `feat(discovery): carry GPU memory kind and measured readings end to end`

### Task 4: Desktop parses kind and keeps unknown readings as null

**Files:** `desktop/src/electron/service-bridge/modular-state.ts`, `desktop/src/shared/types/{hardware.ts,metrics.ts}`, `desktop/src/ui/stores/metrics.store.ts`, `desktop/src/ui/types/node-hardware.ts`, tests under `desktop/tests/`

**Interfaces:**
- `type GpuMemoryKind = 'dedicated' | 'unified' | 'shared' | 'unavailable'`.
- `GpuInfo = { id: string; name: string; memoryKind: GpuMemoryKind; vramTotal: number | null; sharedTotal: number | null }`.
- `GpuMetricValue.value: number | null` for utilization and memory-used percentage (percentage is `null` unless both used and capacity are known).
- Unknown or missing `memory_kind` parses as `'unavailable'`. Absent numeric fields stay `null`; delete `numberValue`'s zero coercion for GPU fields.
- Metrics history keeps `null` entries and is no longer zero-prefilled.

- [ ] **Step 1:** Vitest: parse each kind; absent `utilization_percent` → `null`, present `0` → `0`; legacy payload without `memory_kind` → `'unavailable'`; history appends `null`; initial history contains no fabricated zeros. Run `npm run test:unit -- modular-state metrics` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `npm run lint && npm run typecheck && npm run test:unit` — PASS.
- [ ] **Step 4: Commit** — `feat(desktop): keep GPU memory kind and unknown readings intact`

### Task 5: Desktop labels memory by kind and draws unknown as unknown

**Files:** `desktop/src/ui/components/NodeList/{NodeCardDetails.tsx,NodeChartLegend.tsx,NodePerformance.tsx}`, `desktop/src/ui/utils/build-radial-chart-metrics.ts`, `desktop/src/ui/components/{GpuRadialChartSvg.tsx,SvgLineChart.tsx}`, `desktop/src/ui/components/TrayApp/TrayNodeRow.tsx`, tests under `desktop/tests/`

**Interfaces:** Exact copy:
- Memory label by kind: `dedicated` → `VRAM`; `unified` → `Unified memory`; `shared` → `Shared memory`; `unavailable` → `Memory unavailable` (no used/total figures).
- Unified detail line: `<used> used of <total> unified memory` on every platform.
- Windows dedicated adapter with `sharedTotal`: an extra line `Shared memory <used> / <total>` (used shown as `—` when unknown).
- Unknown utilization: `Usage —`; radial ring shows an empty track and `—`; line charts break at `null` points.
- No component sums memory across adapters.

- [ ] **Step 1:** Component tests: each kind's label and figures; `Usage —` for null; legend for unavailable has no byte figures; line-chart path breaks at nulls (assert separate path segments); tray row uses the same labels. Run `npm run test:unit -- NodeChartLegend NodePerformance radial TrayNodeRow` — FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** `npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check` — PASS.
- [ ] **Step 4: Commit** — `feat(desktop): label GPU memory by kind and show unknown readings as unknown`

### Task 6: Versions, documentation and full verification

**Files:** `services/versions.json`; `docs/known-issues.mdx` ("Unified-Memory Systems Report Misleading GPU Memory"), `services/nvpair-node-info/README.md`, `desktop/docs/services-parity.md`, `docs/architecture.mdx` (where GPU fields are described), `docs/release-readiness.mdx`

- [ ] **Step 1:** Bump `nvpair-node-info` MINOR, `nvpair-node-scanner` MINOR, `nvpair-manual-nodes` MINOR, `nvpair-ui-broker` PATCH (or leave if already MINOR on this branch), `nvpair-job-scheduler` PATCH only if its compiled output changed; product per the rule.
- [ ] **Step 2:** Rewrite the known-issue section to the new behaviour: kinds per platform, unknown shown as `—`, Windows shared budget reported separately, Linux AMD/Intel still names-only (`Memory unavailable`) because driver support is out of scope. Document the wire contract in the node-info README. Add a "GPU memory semantics" entry to release readiness ("implemented, pending release testing; Windows runtime untested on this host").
- [ ] **Step 3:** `make check`; `cd desktop && npm run dead-code:check`; `go test -race ./...` in every changed module and `services/tests` — PASS. `node scripts/spdx-headers.mjs` — clean.
- [ ] **Step 4: Commit** — `docs: document GPU memory kinds`
