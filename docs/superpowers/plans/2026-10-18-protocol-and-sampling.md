<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Relay Envelope Conformance and Scheduling-safe Sampling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The desktop rejects relay frames exactly as the Go codec does, and GPU sampling slows down when nobody needs fresh data without ever letting the scheduler act on stale readings.

**Architecture:** One shared JSONL conformance corpus defines the canonical relay-v2 envelope; the Go codec and a new desktop decoder both run it. Node-info sampling becomes lease-driven (interactive, scheduling, idle) with a tested freshness budget; telemetry carries a sample sequence number so repeated reads never re-weight the scheduler, and manual nodes deliver telemetry on the scanner's cadence with honest age.

**Tech Stack:** Go 1.25, TypeScript (Node 24 / Electron 42 `JSON.parse` reviver `context.source`, `JSON.rawJSON`), Vitest.

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §4 and §5 (owner decision: canonical-envelope enforcement).

## Global Constraints

- Branch `release/protocol-sampling` from `main` @ `28de23c`; signed-off commits (`git commit -s`); SPDX headers on new files.
- Relay protocol stays v2: valid traffic from conforming encoders is unchanged; no new required fields; no protocol version bump.
- No new npm or Go dependencies. No regex-based JSON parsing; the desktop scanner is a character-level tokenizer.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports; unused exports fail `dead-code:check`.
- No legacy fallbacks: delete the `broker:epoch` params fallback for envelope epochs and any code made unreachable.
- Scheduler freshness stays 10 s; never reset or under-report a sample's age to avoid neutral pressure.
- Windows keeps its existing 1 s collector; do not claim or add backoff there.
- JSON-RPC/notification changes update producer, broker relay, consumers, tests and docs together; regenerate `desktop/docs/services-api.md` with `npm run service-contracts:write`.
- Tests use virtual clocks or fakes; no real engines, no real mDNS except the existing cross-process suite.

## Review Focus

1. **A conforming frame must never be rejected**: every frame produced by the Go encoder (including large `seq` values up to 2^64−1 and escaped member names) decodes on both sides. Corpus rows in Task 1.
2. **Go and desktop disagree on nothing in the corpus**: duplicate or case-variant reserved members, `null` stamps, wrong types, numbers like `1e0`, `-1`, `1.0`, `18446744073709551616`. Tasks 1–2.
3. **A protocol mismatch tears the session down once**, fails pending calls, and never triggers the silence-reconnect retry. Task 3.
4. **Freshness budget holds across every phase**: in a virtual-time sweep, the scheduler never receives a sample older than 10 s while a scheduling lease is active, and an expired or failed sampler is reported stale, not fresh. Task 7.
5. **Repeated cached samples add no averaging weight**, but a new sample with the same utilization value still does. Task 5.

---

### Task 1: Canonical envelope in the Go codec and the shared corpus

**Files:** `services/shared/jsonrpc/{jsonrpc.go,envelope.go}`, new `services/shared/jsonrpc/canonical.go`, new `services/shared/jsonrpc/testdata/envelope-conformance.jsonl`, new `services/shared/jsonrpc/conformance_test.go`

**Interfaces:**
- `ReservedMembers = []string{"jsonrpc","id","method","params","result","error","epoch","seq"}`.
- `func checkCanonical(line []byte) error` — walks only the top-level object with `json.Decoder.Token` (member names are decoded, so escapes like `"s\u0065q"` count as `seq`); returns a `*DecodeError` if a reserved name appears twice, or if any top-level name matches a reserved name case-insensitively but not exactly. Non-object or invalid JSON is left to the existing unmarshal error. Called in `Codec.Read` before `json.Unmarshal`.
- Corpus format, one JSON object per line: `{"name": string, "frame": string, "expect": "accept"|"decode-error"|"mismatch", "kind": "notification"|"request"|"response"|null, "epoch": string|null, "seq": string|null}` — `seq` is the canonical decimal string of the decoded value when accepted. Corpus covers at least: stamped notification; request; response; error response; `epoch:""` + `seq:0` (accept); `seq` 2^53+1 and 2^64−1 (accept, exact strings); `seq` 2^64 (decode-error); `seq` `-1`, `1.0`, `1e0`, `"5"` (decode-error); `epoch` number (decode-error); notification missing stamps / `null` stamps (mismatch); request or response with stamps (mismatch); `id:null` + method + stamps (accept, notification); duplicate `seq`, duplicate `method`, `"SEQ"`, `"Method"` (decode-error); escaped `"s\u0065q"` (accept); nested `"seq"` inside `params` (accept, not counted); `jsonrpc:"1.0"` (decode-error); invalid JSON (decode-error).

- [ ] **Step 1:** Write the corpus and `TestEnvelopeConformanceCorpus` (each row through `Codec.Read`; assert error type or decoded kind/epoch/seq) — RED on the duplicate and case-variant rows.
- [ ] **Step 2:** Implement `checkCanonical`. **Step 3:** `cd services/shared && go test -race ./jsonrpc/...` and every module's `go test ./...` that imports the codec (all workers, broker, TUI) — PASS.
- [ ] **Step 4: Commit** — `feat(jsonrpc): reject duplicate and case-variant envelope members`

### Task 2: Desktop relay envelope decoder

**Files:** new `desktop/src/electron/service-bridge/relay-envelope.ts`, new `desktop/tests/modular/relay-envelope.test.ts`

**Interfaces:**
- `type RelayFrame = { kind: 'message'; message: JsonRpcMessage; epoch?: string; seq?: bigint } | { kind: 'decode-error'; reason: string } | { kind: 'mismatch'; reason: string }`
- `function decodeRelayFrame(line: string): RelayFrame` — (1) a top-level member-name scanner (string- and escape-aware, depth-tracking, decodes `\uXXXX` in names) applies the same duplicate/case-variant rule as Task 1; (2) `JSON.parse` with a reviver that captures the source text of the top-level `seq` via `context.source`; (3) `seq` must match `^(0|[1-9][0-9]*)$` and be ≤ 2^64−1, converted with `BigInt`; `epoch` must be a string; `null` stamps count as absent; (4) `jsonrpc` must equal `"2.0"`; (5) notification (`id` absent or `null`, `method` non-empty) without both stamps → `mismatch`; any other frame with a non-null stamp → `mismatch`.
- `function encodeSeq(seq: bigint): string` for outbound use (exact decimal through `JSON.rawJSON`).
- At module load, assert the runtime supports reviver `context.source` and `JSON.rawJSON`; throw a clear error otherwise (no fallback).
- `JsonRpcMessage.seq` becomes `bigint` where it is surfaced.

- [ ] **Step 1:** `relay-envelope.test.ts` reads `../services/shared/jsonrpc/testdata/envelope-conformance.jsonl` and asserts every row's `expect`, `kind`, `epoch` and `seq` (as string) — RED.
- [ ] **Step 2:** Implement. **Step 3:** Corpus passes; plus a fuzz-style test generating 500 random conforming notifications with random large `seq` values encoded by `encodeSeq` and decoded losslessly.
- [ ] **Step 4:** `npm run typecheck && npm run lint && npm run test:unit` from `desktop/`. **Commit** — `feat(desktop): decode relay frames with the Go codec's rules`

### Task 3: Use the decoder in the subprocess bridge and tear down on mismatch

**Files:** `desktop/src/electron/service-bridge/{json-rpc-subprocess.ts,modular-supervisor.ts}`, tests `desktop/tests/modular/{json-rpc-subprocess.test.ts,silence-reconnect.test.ts}` (+ any fake broker emitting unstamped notifications)

**Interfaces:**
- `handleStdout`: redacted log emission unchanged; `decodeRelayFrame(line)`; `decode-error` → drop (log the reason at warn, redacted) and do **not** re-arm the silence deadline; `message` → arm deadline and route as today with `seq: bigint`; `mismatch` → once per session: reject all pending calls with `JsonRpcProtocolMismatchError`, stop delivering frames and writing, emit `'protocol-mismatch'` `{ source, reason }`.
- Outbound `notify` keeps a `bigint` counter and writes `seq` with `encodeSeq`.
- Remove the `broker:epoch` params fallback in both the subprocess and `modular-supervisor.ts` `handleNotification` (envelope epoch only).
- `modular-supervisor.ts`: on the broker's `'protocol-mismatch'`, run the owned stop path and call `onBrokerCrash` with a protocol-mismatch reason; do **not** consume `silenceRetryAvailable` or schedule a reconnect.
- Update every test fake to emit stamped notifications.

- [ ] **Step 1: Failing tests:** malformed frame does not reset the silence deadline; a mismatch rejects pending calls, emits once, stops further delivery; supervisor reports a crash without a reconnect attempt; large `seq` round-trips through `notify`.
- [ ] **Step 2:** Implement. **Step 3:** `npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check` PASS.
- [ ] **Step 4: Commit** — `fix(desktop): enforce relay envelopes and stop cleanly on protocol mismatch`

### Task 4: Honest manual-node telemetry age

**Files:** `services/nvpair-manual-nodes/manager.go`, `manager_test.go`

**Interfaces:** `probeNode` records the time the node-info response arrived; when publishing a status with `TelemetryValid`, `MSSince = info.MSSince + elapsed-since-response` (saturating, never decreasing). Invalid telemetry keeps `MSSince = 0`.

- [ ] **Step 1:** `TestManualTelemetryAgeIncludesProbeDuration` — fake node-info reports `msSince: 1000`; engine probes take a virtual 2.5 s; published `MSSince` ≥ 3500 — RED. **Step 2:** implement (inject a clock if needed). **Step 3:** `go test -race ./...` PASS.
- [ ] **Step 4: Commit** — `fix(manualnodes): include probe time in telemetry age`

### Task 5: Sample identity so repeated reads add no averaging weight

**Files:** `services/nvpair-node-info/{stats.go,stats_darwin.go,stats_linux.go,stats_windows.go,main.go}`, `services/shared/noderec/noderec.go`, `services/nvpair-node-scanner/daemon.go`, `services/nvpair-manual-nodes/manager.go`, `services/nvpair-ui-broker/{telemetry.go,manualnodes.go}`, `services/nvpair-job-scheduler/telemetry.go`, tests in each

**Interfaces:**
- `statsSnapshot.GPUSampleSeq uint64` — incremented by each collector whenever it publishes a new usable GPU sample (Windows included); `/v1/node-info` adds `"sampleSeq": uint64` (omitted when 0).
- `noderec.NodeTelemetry` gains field `SampleSeq uint64` with JSON name `sampleSeq` (omitempty); scanner and manual nodes copy it through; the broker cache preserves it.
- Scheduler `applyTelemetryAt`: when the incoming sample is fresh, `SampleSeq != 0`, and it equals the stored `SampleSeq` for that host, update freshness bookkeeping only (age/validity) and leave EWMA and pressure unchanged. A different `SampleSeq` (or 0, from older peers) updates EWMA as today.

- [ ] **Step 1: Failing tests:** scheduler `TestRepeatedSampleAddsNoWeight` (same seq twice at 100% after one at 0% → EWMA equals the single-update value) and `TestNewSampleSameValueStillWeighs`; node-info returns increasing `sampleSeq` per new collection and the same value for repeated reads; scanner/manual/broker pass it through.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` in every touched module PASS.
- [ ] **Step 4: Commit** — `feat(telemetry): carry GPU sample identity to the scheduler`

### Task 6: Manual-node telemetry on the scanner's cadence

**Files:** `services/nvpair-manual-nodes/manager.go` (+ tests), `services/nvpair-ui-broker/{broker.go,manualnodes.go}` (+ tests), `services/nvpair-manual-nodes/README.md`, `services/nvpair-ui-broker/README.md`

**Interfaces:**
- A per-node telemetry loop at 2 s ± 250 ms (deterministic per-node offset, same rule as the scanner's `telemetryIntervalForNode`) performs only the node-info request (`X-NVPAIR-Poll: scheduling`, see Task 7) and emits notification `node/telemetry` `{"id","hostUuid","gpuUtilizationPct","telemetryValid","msSince","sampleSeq"}` with age measured as in Task 4. The 10 s full probe keeps topology/engines; it no longer feeds scheduler telemetry.
- The broker ingests `node/telemetry` into the manual telemetry source (same key rules as today) without touching the discovery store or proxies, and drops it for nodes it does not know.

- [ ] **Step 1: Failing tests:** manual-nodes emits `node/telemetry` every ~2 s for a reachable node and none for an unreachable one; broker ingests it into the scheduler feed with no `discovery:nodes-changed` emission. **Step 2:** implement. **Step 3:** both modules `go test -race ./...` PASS.
- [ ] **Step 4: Commit** — `feat(manualnodes): deliver scheduling telemetry separately from topology probes`

### Task 7: Lease-driven sampling with a tested freshness budget

**Files:** new `services/nvpair-node-info/sampling.go` (+ `sampling_test.go`), `services/nvpair-node-info/{stats_darwin.go,stats_linux.go,main.go,main_test.go}`, `services/nvpair-ui-broker/{nodeinfo.go,workloadstore/store.go,broker.go}` (+ tests), `services/nvpair-node-scanner/daemon.go`, `services/nvpair-manual-nodes/manager.go`

**Interfaces:**
- Poll intents via `X-NVPAIR-Poll`: `foreground` (visible UI) renews the **interactive** lease; `scheduling` (scanner, manual nodes) renews the **scheduling** lease; `background` renews nothing and peeks; missing/unknown header behaves as `foreground`. Scanner and manual-node requests switch from `foreground` to `scheduling`.
- Broker → node-info notification `stats/set-demand` `{"localExecution": bool}`; the broker computes `localExecution` = at least one non-terminal workload with `ScheduledOn == self` (new `workloadstore.ActiveScheduledOn(node string) int`), sends it on change and on every node-info (re)start.
- `type SamplingPolicy` in `sampling.go` with constants: `InteractiveInterval = 2s`, `SchedulingInterval = 5s`, `IdleInterval = 10s`, `LeaseTTL = 6s`, `CollectionTimeout = 1s`; `func (p *SamplingPolicy) Interval(now time.Time) time.Duration` — interactive lease or `localExecution` → 2 s; else scheduling lease → 5 s; else 10 s. A renewal that raises the mode wakes the collector immediately.
- Darwin and Linux collectors use `SamplingPolicy.Interval` and run each GPU collection with a `CollectionTimeout` context; a timed-out collection publishes nothing (age keeps growing). Windows is unchanged.
- Budget proof test `TestSchedulingFreshnessBudget`: virtual clock, scheduling lease kept alive by reads every 2.25 s with 0.5 s request latency, collection taking up to 1 s, swept over every phase offset in 50 ms steps for 60 s → the age delivered to the scheduler is always ≤ 10 s. Companion test: with collection always timing out, delivered age grows past 10 s (reported stale, never reset).

- [ ] **Step 1: Failing tests:** policy table (each lease combination and expiry), wake-on-raise, budget sweep, timeout behaviour, broker `stats/set-demand` on workload transitions and node-info restart, `ActiveScheduledOn` counts executor not origin, scanner/manual send `scheduling`.
- [ ] **Step 2:** Implement. **Step 3:** `go test -race ./...` in node-info, broker, scanner, manual-nodes, scheduler PASS.
- [ ] **Step 4: Commit** — `feat(nodeinfo): lease-driven GPU sampling within the scheduler's freshness budget`

### Task 8: Versions, docs and full verification

**Files:** `services/versions.json`, `CHANGELOG.md` (Unreleased), `docs/architecture.mdx` (relay v2 section, sampling section), `docs/release-readiness.mdx` (mark both decisions implemented, pending release testing), `docs/known-issues.mdx` if affected, `docs/performance-release-summary.mdx` (state that idle sampling backoff is restored behind leases and must be re-profiled), `desktop/docs/{architecture.md,services-backend.md}`, component READMEs touched above, `AGENTS.md` if its relay/sampling summary changes.

- [ ] **Step 1:** Versions per `services/VERSIONING.md`, one bump per component relative to `28de23c`: `nvpair-node-info`, `nvpair-manual-nodes`, `nvpair-ui-broker` MINOR (new fields/notifications); `nvpair-node-scanner`, `nvpair-job-scheduler` PATCH; every other module that links the stricter codec PATCH; product/installer MINOR.
- [ ] **Step 2:** Docs describe only what the code does; remove statements that the desktop parser does not enforce envelopes or that unattended sampling backoff is deferred.
- [ ] **Step 3: Full verification**

```bash
node scripts/spdx-headers.mjs
for m in services/*/; do [ -f "$m/go.mod" ] && (cd "$m" && go test ./...) || exit 1; done
(cd services/shared && go test -race ./jsonrpc/...)
cd desktop && npm run lint && npm run typecheck && npm run test:unit \
  && npm run dead-code:check && npm run service-contracts:check && npm run licenses:check
```

- [ ] **Step 4: Commit** — `docs(release): document envelope conformance and lease-driven sampling`
