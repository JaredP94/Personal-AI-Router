<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Inference Metrics v2 — Engine-Native Surface and Live In-Flight Numbers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show each node's engine-native view (the engine's own counters, cache efficiency and loaded models), and show a running job's live progress — exact PAIR-derived facts for every engine, and the engine's own live numbers where the engine reports them.

**Architecture:** The engine-manager serves two new endpoints on the em surface it already exposes (`/v1/models`'s listener, same loopback / pinned-mTLS split), returning PAIR's own sanitized type — oMLX's admin API is called on loopback and mapped field by field, so the API key it returns cannot leak. The node-scanner fetches that view from every peer over the mTLS pool it already uses and carries it on `noderec.DirectoryNode`, which the broker copies onto its node record. Live progress rides two paths: the proxy's own notification channel (exact PAIR-derived facts) and the existing workload broadcast (the engine's own live numbers, keyed by PAIR's job id).

**Tech Stack:** Go 1.25 (engine-manager, node-scanner, ui-broker, workload-manager, omlx-proxy, `nvpair-shared`), TypeScript/React/Vitest (desktop).

**Spec:** [`docs/superpowers/specs/2026-10-21-inference-metrics-v2-design.md`](../specs/2026-10-21-inference-metrics-v2-design.md) — read §3 (the no-estimates ruling) and §4 before starting. The v1 spec it builds on is [`2026-10-20-inference-metrics-design.md`](../specs/2026-10-20-inference-metrics-design.md).

## Global Constraints

- Branch `feat/inference-metrics-v2` off `main`; signed-off commits (`git commit -s`); two-line SPDX header on every new file.
- No new npm or Go dependencies. **No new inter-node mechanism and no new trust boundary** — both slices ride surfaces that already exist (the engine-manager's em surface, the node-scanner's per-peer mTLS pool, the workload broadcast).
- **No estimates, anywhere.** The live card shows what PAIR measures exactly and what the engine measures exactly. A chunk count is never presented as a token count, and a rate is never derived from one.
- **The oMLX API key must never leave the node.** PAIR's response type has no field for it, and a test asserts the serialized payload contains no `api_key` field and no value equal to the configured key.
- Only engine-measured values are reported; a value nobody reported stays absent, never zero.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports across directories; static imports only; the renderer never imports `src/electron/`; unused exports fail `npm run dead-code:check`.
- Say "engine", never "backend", in user-facing copy and new code.
- Go tests run per component: `cd services/<component> && go test ./...`. Desktop tests: `cd desktop && npm run test:unit` (files in `desktop/tests/modular/*.test.ts`; a DOM environment is available by file via the `// @vitest-environment jsdom` docblock, as `inference-metrics-rendering.test.ts` does).
- Do not change any existing wire field name, JSON tag or type. Everything here is additive.
- `services/versions.json`: a new additive field or endpoint over IPC/HTTP is a MINOR bump for the affected component; the product takes a MINOR when any component does.

### Wire contracts

**Engine-native stats** — served by the engine-manager at `GET /v1/engine-stats`, carried on the node record as `engineNative`:

```json
{
  "engine": "omlx",
  "supported": true,
  "totals": {
    "requests": 3270, "promptTokens": 212978711, "completionTokens": 2147906,
    "cachedTokens": 157045860, "cacheEfficiency": 73.7
  },
  "rates": { "prefillTokensPerSecond": 1553.2, "generationTokensPerSecond": 32.5 },
  "models": [{ "model": "mlx-community/Llama-3.2-3B-Instruct-4bit", "loaded": true }],
  "observedAt": 1716998400000
}
```

`supported: false` for an engine with no admin API, and for a failed fetch (with a sanitised `error`). Every numeric field is omitempty — absent means "not reported".

**Engine-native activity** — served at `GET /v1/engine-activity`:

```json
{
  "engine": "omlx",
  "supported": true,
  "rows": [
    { "requestId": "...", "phase": "generating", "queuePosition": 0,
      "generatedTokens": 34, "tokensPerSecond": 62.1, "prefillProgress": 0.62,
      "elapsedSeconds": 4.2, "promptTokens": 1200 }
  ]
}
```

`phase` is one of `waiting`, `prefilling`, `generating`. Fields that do not apply to a phase are absent.

**Progress frame** — a new workload-manager pass-through method `workload:progress`:

```json
{
  "jobId": "17",
  "nodeId": "<the node serving the job>",
  "elapsedMs": 4200,
  "chunks": 34,
  "bytes": 1820,
  "ttfbMs": 260,
  "engine": { "requestId": "...", "generatedTokens": 34, "tokensPerSecond": 62.1,
              "prefillProgress": 0.62, "queuePosition": 0 }
}
```

The `engine` object is absent when the engine reports no live progress. Every number in it is measured by the engine; `chunks`, `bytes` and the timings are measured by PAIR.

## Review Focus

1. **The API key must not appear in any response, log, metric or node record.** The engine's own payload includes it; PAIR's type must not. Task 1 and Task 2.
2. **No estimate may reach the screen.** A chunk count must never be labelled or used as a token count, and no rate may be derived from one. Task 5 and Task 8.
3. **A progress frame must not be deduped as a duplicate of the lifecycle event that shares its job id** — it is not a state transition. Task 7.
4. **A live frame must reach a desktop on another node**, and a job served on a peer must be attributed to the serving node, not the originating one. Task 6 and Task 7.
5. **A phase nobody reported must render as "not reported", never as a zero**, on the live card as on the final one. Task 8.

---

### Task 1: the shared engine-native type and its parser

**Files:** `services/shared/enginenative/enginenative.go` (new), `services/shared/enginenative/enginenative_test.go` (new)

**Interfaces (produced):**
- `package enginenative` with:
  ```go
  type Stats struct {
      Engine         string   `json:"engine"`
      Supported      bool     `json:"supported"`
      Totals         Totals   `json:"totals,omitempty"`
      Rates          Rates    `json:"rates,omitempty"`
      Models         []Model  `json:"models,omitempty"`
      Error          string   `json:"error,omitempty"`
      ObservedAt     int64    `json:"observedAt,omitempty"`
  }
  type Totals struct {
      Requests         int     `json:"requests,omitempty"`
      PromptTokens     int     `json:"promptTokens,omitempty"`
      CompletionTokens int     `json:"completionTokens,omitempty"`
      CachedTokens     int     `json:"cachedTokens,omitempty"`
      CacheEfficiency  float64 `json:"cacheEfficiency,omitempty"`
  }
  type Rates struct {
      PrefillTokensPerSecond    float64 `json:"prefillTokensPerSecond,omitempty"`
      GenerationTokensPerSecond float64 `json:"generationTokensPerSecond,omitempty"`
  }
  type Model struct {
      Model  string `json:"model"`
      Loaded bool   `json:"loaded"`
  }
  type Activity struct {
      Engine    string       `json:"engine"`
      Supported bool         `json:"supported"`
      Rows      []ActivityRow `json:"rows,omitempty"`
      Error     string       `json:"error,omitempty"`
  }
  type ActivityRow struct {
      RequestID       string   `json:"requestId"`
      Phase           string   `json:"phase"`
      QueuePosition   int      `json:"queuePosition,omitempty"`
      GeneratedTokens int      `json:"generatedTokens,omitempty"`
      TokensPerSecond float64  `json:"tokensPerSecond,omitempty"`
      PrefillProgress float64  `json:"prefillProgress,omitempty"`
      ElapsedSeconds  float64  `json:"elapsedSeconds,omitempty"`
      PromptTokens    int      `json:"promptTokens,omitempty"`
  }
  func ParseStats(raw []byte) (Stats, error)
  func ParseActivity(raw []byte) (Activity, error)
  ```
  `ParseStats` and `ParseActivity` map oMLX's raw admin payloads onto these types **field by field**. They never copy a field they did not name — in particular `api_key`, `host`, `port` and `cli_prefix` are dropped by construction.
- The type carries **no phase durations**: oMLX's stats endpoint reports none (its speeds — `avg_prefill_tps`, `avg_generation_tps` — are what it does report), and the usage endpoint's durations are scoped to a date range rather than the engine's life. The durations arrive with the trends work, where the scope is explicit.

- [ ] **Step 1: Write the failing test**

`enginenative_test.go`:
```go
func TestParseStatsDropsTheAPIKey(t *testing.T) {
    raw := []byte(`{"total_prompt_tokens":120,"total_completion_tokens":34,"total_cached_tokens":90,` +
        `"total_requests":2,"avg_prefill_tps":100,"avg_generation_tps":50,"cache_efficiency":75,` +
        `"api_key":"sk-super-secret","host":"127.0.0.1","port":8000,"cli_prefix":"omlx"}`)
    s, err := ParseStats(raw)
    if err != nil { t.Fatal(err) }
    b, err := json.Marshal(s)
    if err != nil { t.Fatal(err) }
    if strings.Contains(string(b), "sk-super-secret") { t.Fatalf("the api key leaked: %s", b) }
    if strings.Contains(string(b), "api_key") { t.Fatalf("the api key field leaked: %s", b) }
    if s.Totals.PromptTokens != 120 { t.Fatalf("totals: %+v", s.Totals) }
    if s.Rates.GenerationTokensPerSecond != 50 { t.Fatalf("rates: %+v", s.Rates) }
}

func TestParseStatsReportsAbsentValuesAsZero(t *testing.T) {
    s, err := ParseStats([]byte(`{"total_requests":1}`))
    if err != nil { t.Fatal(err) }
    if s.Totals.PromptTokens != 0 { t.Fatalf("totals: %+v", s.Totals) }
    if s.Rates.PrefillTokensPerSecond != 0 { t.Fatalf("rates: %+v", s.Rates) }
}

func TestParseStatsRejectsGarbage(t *testing.T) {
    if _, err := ParseStats([]byte("not json")); err == nil { t.Fatal("expected an error") }
}
```
Add a `ParseActivity` test that maps one `generating` row and one `waiting` row from a payload shaped like oMLX's `active_models` rows, and asserts the phase and the numbers.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/shared && go test ./enginenative/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the package**

Create the two files with the SPDX header. Implement `ParseStats` and `ParseActivity` against oMLX's documented payload shapes: `ParseStats` reads `total_prompt_tokens`, `total_completion_tokens`, `total_cached_tokens`, `total_requests`, `avg_prefill_tps`, `avg_generation_tps`, `cache_efficiency`; `ParseActivity` reads the `active_models` rows, mapping each model's waiting / prefilling / generating entries into `ActivityRow`s with the phase names `waiting`, `prefilling`, `generating`. A payload that is not JSON is an error. A field that is absent is zero, and the JSON tags omit it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/shared && go test ./enginenative/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/shared/enginenative
git commit -s -m "feat(shared): add the engine-native view type and its parser"
```

---

### Task 2: the engine-manager serves the engine-native view

**Files:** `services/nvpair-engine-manager/httpserver.go`, `services/nvpair-engine-manager/enginenative.go` (new), `services/nvpair-engine-manager/enginenative_test.go` (new)

**Interfaces (consumes):** `enginenative.ParseStats`, `enginenative.ParseActivity` and the types from Task 1.

**Interfaces (produced):**
- `GET /v1/engine-stats` and `GET /v1/engine-activity` on the em surface, registered on **both** personalities exactly like `/v1/models` (plaintext → `loopbackOnly`, TLS → `requirePinnedPeer`).
- `func (e *Executor) EngineNativeStats(ctx context.Context) enginenative.Stats` and `func (e *Executor) EngineNativeActivity(ctx context.Context) enginenative.Activity` — look up the omlx engine's state (its port), call `http://127.0.0.1:<port>/admin/api/stats` and `.../admin/api/activity` with the oMLX API key, and map the responses through the shared package.

- [ ] **Step 1: Write the failing test**

`enginenative_test.go`:
```go
func TestEngineNativeStatsReportsTheEngineView(t *testing.T) {
    // A fake oMLX admin API on a loopback port that answers /admin/api/stats
    // with a payload including "api_key":"sk-secret".
    // Point the executor's omlx engine state at that port, call
    // EngineNativeStats, and assert: supported is true, the totals carry the
    // engine's numbers, and the serialized result contains neither "api_key"
    // nor "sk-secret".
}

func TestEngineNativeStatsReportsUnsupportedWhenTheEngineHasNoAdminAPI(t *testing.T) {
    // An executor with no omlx engine (or an ollama-only setup) answers
    // supported: false and no error.
}

func TestEngineNativeStatsSurvivesAnUnreachableEngine(t *testing.T) {
    // A port nothing listens on answers supported: false with a sanitised
    // error, and does not hang.
}
```
Reuse this package's existing test style for building an `Executor` (read `enginenative_test.go`'s neighbours: `executor_test.go`, `models_test.go`) rather than inventing a new harness.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/nvpair-engine-manager && go test ./... -run EngineNative`
Expected: FAIL — the functions do not exist.

- [ ] **Step 3: Implement**

- Add the two paths to the em surface's `plainMux` and `tlsMux`, using the same `loopbackOnly` / `requirePinnedPeer` gates as `modelsPath`. The handlers are GET-only and answer `Content-Type: application/json`.
- The executor methods: resolve the omlx engine's port from its `engineState`; if there is no omlx engine, answer `supported: false` with no error. Otherwise GET the admin endpoint on loopback with `Authorization: Bearer <key>` and a bounded timeout (a few seconds), parse through the shared package, and set `Engine: "omlx"`, `Supported: true` and `ObservedAt`. A transport error, a non-200, or an unparsable payload answers `supported: false` with a sanitised error string — never a panic, never a hang, and never the raw response body.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/nvpair-engine-manager && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-engine-manager
git commit -s -m "feat(engine-manager): serve the engine-native view on the em surface"
```

---

### Task 3: the node-scanner carries the engine-native view

**Files:** `services/shared/noderec/noderec.go`, `services/nvpair-node-scanner/daemon.go`, `services/nvpair-ui-broker/discovery.go`, plus their tests.

**Interfaces (consumes):** `enginenative.Stats` from Task 1.

**Interfaces (produced):**
- `noderec.DirectoryNode` gains `EngineNative *enginenative.Stats \`json:"engineNative,omitempty"\`` — additive, absent means "not fetched or not supported".
- The scanner fetches `/v1/engine-stats` from each peer over the mTLS pool and from its own node over loopback, on its own cadence, and folds the result into the directory node.
- The broker copies it onto its own node record alongside `ModelsByEngine` / `LoadedByEngine`.

- [ ] **Step 1: Write the failing test**

- In `services/shared/noderec`: a test that a directory node with an engine-native view marshals the `engineNative` field, and that a node without one omits it.
- In `services/nvpair-node-scanner`: a test that the scanner's peer fetch uses the same transport choice as the models fetch (loopback plaintext for self, pinned mTLS for a peer) and that a peer with no pin yields no engine-native view rather than an error.
- In `services/nvpair-ui-broker`: a test that the broker's node record carries the engine-native view through.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/shared && go test ./noderec/`, `cd services/nvpair-node-scanner && go test ./... -run EngineNative`, `cd services/nvpair-ui-broker && go test ./... -run EngineNative`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Add the field to `noderec.DirectoryNode` and copy it in the broker's node record (next to `ModelsByEngine` / `LoadedByEngine` in `discovery.go`).
- In the scanner, add a fetch beside the models fetch: reuse `modelsClient(ip, clusterUUID)` for the transport, GET `<peer>/v1/engine-stats`, parse with `enginenative.ParseStats`, and store it on the directory node. A failure leaves the field absent — a node with no engine-native view is not an error.
- Give it its own ticker, slower than the models refresh (the engine's totals move slowly), and a bounded timeout.

- [ ] **Step 4: Run the tests to verify they pass**

Run: the three commands above, then `cd services/shared && go test ./...`.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/shared/noderec services/nvpair-node-scanner services/nvpair-ui-broker
git commit -s -m "feat(discovery): carry the engine-native view on the node record"
```

---

### Task 4: the desktop shows the engine's own view

**Files:** `desktop/src/shared/types/metrics.ts`, `desktop/src/shared/types/nodes.ts` (or wherever `NodeItem` lives), `desktop/src/electron/service-bridge/modular-state.ts`, `desktop/src/ui/components/NodeList/NodeInferenceMetricsSection.tsx`, `desktop/src/ui/utils/inference-metrics.ts`, `desktop/tests/modular/inference-metrics.test.ts`

**Interfaces (produced):** the TypeScript mirror of the engine-native type, and a node-card section that renders it, clearly separated from PAIR's own numbers.

- [ ] **Step 1: Write the failing test**

In `desktop/tests/modular/inference-metrics.test.ts`:
```ts
it('labels the engine self-report separately from the router's own numbers', () => {
    const rows = buildEngineNativeRows({
        engine: 'omlx', supported: true,
        totals: { requests: 2, promptTokens: 120, completionTokens: 34, cachedTokens: 90,
            prefillSeconds: 1.25, generationSeconds: 4.5, cacheEfficiency: 75 },
        rates: { prefillTokensPerSecond: 100, generationTokensPerSecond: 50 },
        models: [{ model: 'm', loaded: true }]
    })
    expect(rows.length).toBeGreaterThan(0)
    expect(rows.every(r => r.source === 'engine')).toBe(true)
})

it('reports an engine with no native view as unsupported', () => {
    const rows = buildEngineNativeRows({ engine: 'ollama', supported: false })
    expect(rows).toEqual([])
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd desktop && npm run test:unit -- inference-metrics`
Expected: FAIL — `buildEngineNativeRows` is not exported.

- [ ] **Step 3: Implement**

- Add the TypeScript types mirroring the wire contract (every field optional; absent means "not reported").
- Parse the engine-native view in the bridge alongside the other node fields, and carry it on the node model.
- Add `buildEngineNativeRows` to `desktop/src/ui/utils/inference-metrics.ts`, returning rows tagged `source: 'engine'` so the section can be labelled and never merged with PAIR's own rows.
- Render the section in `NodeInferenceMetricsSection.tsx` under a heading that says it is the engine's own counters, which include traffic PAIR did not route. A node with no view renders nothing.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd desktop && npm run test:unit -- inference-metrics && npm run typecheck && npm run lint`
Expected: PASS / clean.

- [ ] **Step 5: Commit**

```bash
git add desktop
git commit -s -m "feat(desktop): show the engine's own counters on the node card"
```

---

### Task 5: the proxy measures live progress, exactly

**Files:** `services/omlx-proxy/telemetry.go`, `services/omlx-proxy/proxy.go`, `services/omlx-proxy/telemetry_test.go` (extend), `services/omlx-proxy/proxy_test.go` (extend)

**Interfaces (consumes):** nothing new.

**Interfaces (produced):**
- The telemetry reader counts **content chunks received** and **response bytes received**, and records the time to the first byte and the time to the first content chunk — all exact.
- The proxy emits a progress notification for an in-flight inference request, at most **one every two seconds per request**, carrying `jobId`, `nodeId`, `elapsedMs`, `chunks`, `bytes`, `ttfbMs` and nothing else. **No token count, no rate** — PAIR cannot know either without estimating.

- [ ] **Step 1: Write the failing test**

In `telemetry_test.go`: a test that drives the reader with a known SSE stream and asserts the chunk count and byte count match what the engine sent, and that the first-byte and first-content times are recorded. In `proxy_test.go`: a test that an in-flight inference request emits a progress notification, and that a burst of chunks within one window produces **one** notification, not one per chunk.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/omlx-proxy && go test ./... -run 'Progress|Chunk'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- In the reader, count content chunks (a chunk that carries any content) and total response bytes, and record the first-byte and first-content instants.
- In the proxy, emit the progress notification from the request's own goroutine, gated by a per-request last-emit timestamp so no more than one frame leaves every two seconds. The frame carries the job id, the serving node id, the elapsed time, the counts and the timing. It carries no token count and no rate.
- Emit the same shape from the ollama and lmstudio proxies in the same commit or a follow-up in the same PR — the frame is engine-agnostic.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/omlx-proxy && go test ./...` and the same for `ollama-proxy` and `lmstudio-proxy`.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/omlx-proxy services/ollama-proxy services/lmstudio-proxy
git commit -s -m "feat(proxies): report live progress for an in-flight request"
```

---

### Task 6: the engine's own live numbers, joined to PAIR's job

**Files:** `services/omlx-proxy/ingress.go`, `services/omlx-proxy/proxy.go`, `services/omlx-proxy/ingress_test.go` (extend), `services/omlx-proxy/proxy_test.go` (extend)

**Interfaces (produced):**
- When the proxy forwards a request to a **cluster peer**, it sends `X-NVPAIR-Request-Id: <PAIR job id>` on the forwarded request.
- The peer's ingress handler reads that header and keeps it with the request.
- The serving side reads `X-oMLX-Request-ID` from the engine's response and holds `engine request id ↔ PAIR job id` locally, so the engine's live rows can be published **already keyed by PAIR's job id**.

- [ ] **Step 1: Write the failing test**

- A test that a request forwarded to a peer carries the `X-NVPAIR-Request-Id` header with the proxy's own request id.
- A test that the ingress handler picks the header up and that the response's `X-oMLX-Request-ID` is associated with it.
- A test that a local request (no peer) associates the engine's request id with the job id in the same way.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/omlx-proxy && go test ./... -run 'RequestId|Ingress'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- In the proxy's Director, when the candidate is a cluster peer, set `X-NVPAIR-Request-Id` to the request id. It is a random per-request identifier and carries no content.
- In the ingress handler, read the header and keep it with the in-flight request; read `X-oMLX-Request-ID` from the engine's response headers and record the pairing. The mapping is in memory and dies with the request.
- Do not change the ingress's existing behaviour: it stays strictly terminal (a peer request is never re-routed onward) and the header is not a routing input.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/omlx-proxy && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/omlx-proxy
git commit -s -m "feat(omlx-proxy): carry the job id to the serving peer"
```

---

### Task 7: the live frames reach a desktop on any node

**Files:** `services/nvpair-workload-manager/manager.go`, `services/nvpair-workload-manager/workload.go`, `services/nvpair-ui-broker/broker.go`, plus their tests.

**Interfaces (consumes):** the progress frame from Task 5 and the engine-native live rows.

**Interfaces (produced):**
- The workload-manager treats `workload:progress` as a **pass-through**: it is broadcast to every pinned peer like a lifecycle frame, but it is **not** a state transition, so it is not deduped against the `workload:started` event that shares its job id, and it does not enter the workload catalog.
- The broker ingests a progress frame, keeps the live view for its job (a bounded map, oldest entries dropped), and pushes it to subscribed clients.

- [ ] **Step 1: Write the failing test**

- In the workload-manager: a test that a progress frame is broadcast to peers, and that two progress frames for the same job id are both broadcast (the dedup that applies to lifecycle events must not swallow them).
- In the broker: a test that a progress frame updates the live view for its job and that a terminal workload event for that job clears it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/nvpair-workload-manager && go test ./... -run Progress`, `cd services/nvpair-ui-broker && go test ./... -run Progress`
Expected: FAIL.

- [ ] **Step 3: Implement**

- In the workload-manager, add `workload:progress` to the methods that are relayed rather than treated as lifecycle events, and make sure it is excluded from the dedup key and the active-workload tracking. It must not create or update a catalog entry.
- In the broker, handle the relayed progress frame: keep the latest live view per job in a bounded map, and push it to the clients that subscribed to workloads. A terminal workload event removes the job's live view.

- [ ] **Step 4: Run the tests to verify they pass**

Run: the two commands above, then `cd services/nvpair-workload-manager && go test ./...` and `cd services/nvpair-ui-broker && go test ./...`.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-workload-manager services/nvpair-ui-broker
git commit -s -m "feat(cluster): relay live progress frames to every member"
```

---

### Task 8: the desktop shows a running job's progress

**Files:** `desktop/src/shared/types/workloads.ts`, `desktop/src/shared/types/ws-channels.ts`, `desktop/src/ui/api/pair-api.ts`, `desktop/src/electron/service-bridge/modular-state.ts`, `desktop/src/ui/components/Workloads/WorkloadItemCard.tsx`, `desktop/src/ui/utils/inference-metrics.ts`, `desktop/tests/modular/inference-metrics.test.ts`, `desktop/tests/modular/inference-metrics-rendering.test.ts`

**Interfaces (produced):** the live progress types and the running job card's live line.

- [ ] **Step 1: Write the failing test**

- A unit test that the live rows for a running job carry the elapsed time, the chunk and byte counts, and the engine's numbers when the engine reported them — and that **no row is ever a token count derived from chunks**.
- A rendering test that a running oMLX job shows the engine's live token count and rate, that a running Ollama job shows elapsed and streamed bytes and says the engine reports no live progress, and that a finished job still shows v1's exact final numbers.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd desktop && npm run test:unit -- inference-metrics`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Add the live progress types and the channel registration, the bridge's parse and push, and the store's live view (bounded).
- Render the live line on a running job's card: elapsed, what has arrived, and the engine's own live numbers when present. A phase the engine did not report reads "not reported" — never `0`.
- When the job becomes terminal, the card shows v1's final numbers, and the live line disappears.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd desktop && npm run test:unit && npm run typecheck && npm run lint && npm run dead-code:check`
Expected: PASS / clean.

- [ ] **Step 5: Commit**

```bash
git add desktop
git commit -s -m "feat(desktop): show live progress on a running job"
```

---

### Task 9: versions, contracts and documentation

**Files:** `services/versions.json`, `desktop/docs/services-api.md` (generated), `docs/architecture.mdx`, the affected service READMEs, `docs/release-readiness.mdx`

- [ ] **Step 1: Bump the versions**

MINOR bumps for the components whose compiled output changed: `nvpair-engine-manager`, `nvpair-node-scanner`, `nvpair-ui-broker`, `nvpair-workload-manager`, `omlx-proxy`, `ollama-proxy`, `lmstudio-proxy`. `product` and `installer` take a MINOR bump. Read the file first and bump whatever is there to its next MINOR.

- [ ] **Step 2: Regenerate the API doc and verify the contracts**

Run: `cd desktop && npm run service-contracts:write && npm run service-contracts:check`
Expected: the doc is regenerated and the check passes.

- [ ] **Step 3: Update the documentation**

- `docs/architecture.mdx`: the engine-native view and the live progress, and the no-estimates rule.
- The affected service READMEs: the two new engine-manager endpoints, the node record's new field, the progress frame.
- `docs/release-readiness.mdx`: this feature is implemented in source and pending live-engine and release testing, alongside the v1 entry.

- [ ] **Step 4: Run the full check**

Run: `make check`, then `make test`, then `cd desktop && npm run dead-code:check`.
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add services/versions.json desktop/docs docs
git commit -s -m "chore: version bump, contracts and docs for the v2 inference metrics"
```
