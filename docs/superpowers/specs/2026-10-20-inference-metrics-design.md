<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Inference Prefill / Decode Metrics — Design (v1, with v2 scope)

- **Date:** 2026-10-20
- **Status:** proposed — awaiting owner approval
- **Purpose:** show, **per job** and **per node**, how much work went into prefill
  versus decode, and how fast each node is.
- **Explicitly not for scheduling.** The numbers are for display and diagnosis.
  Nothing in this design feeds the job scheduler's ranking.

> Revision note: this draft was reviewed by a fresh builder-pass reader, and the
> review changed several load-bearing decisions. The changes are summarized in
> §12 so a reviewer can see what moved and why.

---

## 1. Intent

A user running PAIR across one or more nodes wants to answer three questions
without leaving the app:

1. *What did this job actually cost?* — how many tokens were prefilled, how
   many were decoded, how long each phase took, how fast was the decode.
2. *Which node is fast?* — per node: prompt tokens/s, decode tokens/s, TTFT,
   and how much traffic it has served.
3. *Is something wrong?* — a node whose prefill rate collapsed, or a job whose
   decode rate is far below the node's usual, should be visible.

Today PAIR can answer none of these. It parses `usage.prompt_tokens` and
`usage.completion_tokens` out of engine responses and then discards everything
else the engine sends, including the phase timings oMLX already computes.

**The central finding of this design:** the engines already measure what we
need, and PAIR already receives it. The work is mostly parsing, wiring, and
counting — not new instrumentation.

---

## 2. Measurement inventory: what the engines already report

### 2.1 oMLX (the richest source)

oMLX's OpenAI-compatible responses carry a `usage` object with **per-request
phase timings**, in both the streaming and non-streaming paths.

From oMLX `main` (`omlx/server.py`, function `_usage_timing_fields`):

| Field | Meaning |
| --- | --- |
| `prompt_eval_duration` | prefill seconds |
| `generation_duration` | decode seconds |
| `prompt_tokens_per_second` | prefill tok/s, computed on `prompt − cached` |
| `generation_tokens_per_second` | decode tok/s |
| `time_to_first_token` | TTFT (s) |
| `total_time` | wall time of the request |
| `prompt_tokens_details.cached_tokens` | tokens reused from the engine's own KV prefix cache |
| `completion_tokens_details.reasoning_tokens` | reasoning tokens |

These are attached to the final response's `usage` (non-streaming) and to the
final `usage` chunk (streaming).

**Gating condition:** the streaming usage chunk is emitted only when the request
carries `stream_options.include_usage` (`omlx/server.py`, the
`if request.stream_options and request.stream_options.include_usage:` guard).
PAIR never sets it, so **on the streaming path oMLX currently sends no usage at
all** — the counts PAIR does parse today are absent for streaming requests
against oMLX. v1 must inject the flag.

oMLX also exposes a server-side admin API (same port, `admin_router` mounted
with prefix `/admin`, `omlx/server.py`), authenticated with the **main** API key:

- `GET /admin/api/stats?scope=session|alltime&model=` — `total_prompt_tokens`,
  `total_completion_tokens`, `total_cached_tokens`, `total_requests`,
  `avg_prefill_tps`, `avg_generation_tps`, `cache_efficiency`, `uptime_seconds`
- `GET /admin/api/usage?range=&model=&include_details=true` — hourly buckets
  with `prefill_seconds`, `generation_seconds`, `request_seconds`,
  `prefill_tps`, `generation_tps`, `cache_efficiency`
- `GET /admin/api/activity` — **live per-request rows** keyed by `request_id`
  (waiting / prefilling / generating, with `tokens_per_second` and queue
  position)

Every inference response also carries `X-oMLX-Request-ID`, sent before prefill
starts on the streaming path, so a client can correlate its own request with a
live `activity` row. **The admin API is not needed for v1.** It is v2 scope
(§6.1, §6.2).

### 2.2 Ollama

Ollama's final NDJSON chunk carries `prompt_eval_count`, `eval_count`, and
`prompt_eval_duration` / `eval_duration` (nanoseconds). PAIR parses the counts
and the phase durations, converting the durations to milliseconds, so an Ollama
job reports its own prefill and decode durations. No request injection is
needed — Ollama always includes them. Ollama reports no cached-token count and
no TTFT.

### 2.3 LM Studio

LM Studio's OpenAI-compatible surface reports `usage.prompt_tokens` and
`usage.completion_tokens`. It reports **no phase split**.

### 2.4 Capability matrix

"Measured" means the engine reports the value itself.

| Engine | Prefill tokens | Decode tokens | Prefill duration | Decode duration | TTFT | Cached tokens | Total duration |
| --- | --- | --- | --- | --- | --- | --- | --- |
| oMLX | usage | usage | measured (`prompt_eval_duration`) | measured (`generation_duration`) | measured (`time_to_first_token`) | measured (`prompt_tokens_details.cached_tokens`) | measured (`total_time`) |
| Ollama | measured (`prompt_eval_count`) | measured (`eval_count`) | measured (`prompt_eval_duration`, ns) | measured (`eval_duration`, ns) | not reported | not reported | not reported |
| LM Studio | measured (`usage.prompt_tokens`) | measured (`usage.completion_tokens`) | not reported | not reported | not reported | not reported | not reported |

Two of the three engines report a full phase split. LM Studio reports none, and
§4.1 explains why PAIR does not estimate them. Only oMLX reports a total
duration and a cached-token count; for the other two engines those two fields
are absent, and the desktop leaves their rows out rather than filling them
with a number the proxy measured itself (§4.1 rule 6).

### 2.5 Where PAIR throws the data away

- `services/{omlx,ollama,lmstudio}-proxy/telemetry.go` — the `streamChunk`
  struct parses only `usage.prompt_tokens` / `usage.completion_tokens` (and the
  Ollama counts). The other fields are in the same JSON object, and were
  unparsed until this change.
- The reader **already retains** the parsed counts in plain struct fields
  (`r.promptTokens`, `r.outputTokens`) and is installed on every inference
  response regardless of telemetry state (a disabled session uses
  `noop.NewTracerProvider`, so the span is non-nil and the reader is still
  attached). What is missing is that those values are only *consumed* inside
  `finalize()` under `span.IsRecording()`, so with telemetry disabled they are
  parsed and then discarded. **The work is to surface them, not to retain
  them.**
- `services/omlx-proxy/proxy.go` — the `Director` forwards the client's request
  body unchanged, so `stream_options.include_usage` is never added.

---

## 3. Goals, non-goals, anti-goals

### Goals

- Per **job**: prefill tokens, decode tokens, prefill ms, decode ms, TTFT,
  cached tokens, and the resulting rates, on the job card.
- Per **node**: the same totals and rates aggregated across the jobs that ran
  on that node, cluster-wide, on the node card.
- One measurement contract that is identical for all three engines, so the UI
  never branches on engine type.
- Only engine-measured values. Where an engine reports nothing, the UI says
  so — no estimates.
- No new worker, no new inter-node surface, no counting in the desktop tree.

### Non-goals

- Feeding the scheduler. The job scheduler's ranking stays as it is
  (GPU pressure, queue depth, affinity). §9.
- Replacing Phoenix / OpenTelemetry. The existing span attributes keep working;
  v2 adds parity attributes (§6.5).
- Surfacing engine-native cache efficiency in v1. oMLX's own KV prefix cache is
  a different mechanism from PAIR's routing affinity; conflating them would
  produce a number that means neither.
- A new metrics *service*. The proxy measures, the broker aggregates.

### Anti-goals

- **Do not** let the desktop count, filter, or aggregate across records, and
  do not let it own a windowed rate. Every count, filter, aggregate and
  windowed rate is computed by a Go service and delivered as a value. A
  per-record ratio of two reported values (§4.5) is display arithmetic, not
  counting.
- **Do not** merge engine-native totals and PAIR-derived totals into one field.
  Two sources of truth that disagree is worse than one.
- **Do not** invent a phase number the engine did not measure. §4.1.
- **Do not** introduce a second, parallel "routing metrics" implementation. The
  existing `nvpair-shared/routingmetrics` pattern is the model: the service that
  observes the event counts it once; every consumer displays.

---

## 4. Design

### 4.1 The measurement contract

One per-request measurement, three sources, defined once in a new shared
package `nvpair-shared/inferencemetrics`:

```go
// Measurement is what one inference request cost.
//
// Every field is a pointer: nil means "this engine did not report it", which
// is different from a reported zero (a completion with no output tokens is a
// real zero). Presence is the provenance: a value that is present was
// measured by the engine.
type Measurement struct {
    PrefillTokens *int
    DecodeTokens  *int
    CachedTokens  *int
    PrefillMs     *int64
    DecodeMs      *int64
    TtftMs        *int64
    TotalMs       *int64
}
```

**Rules**

1. **Tokens** come from the engine's usage block. If the engine reports no
   usage, the token fields are absent and the request contributes nothing to
   token totals.
2. **Durations are only ever engine-measured.** PAIR does not estimate a phase
   it cannot observe. (See the note below for why estimation was dropped.)
3. **Rates** are always `sum(tokens) / sum(seconds)` over a set of requests,
   never the mean of per-request rates. This matches oMLX's own weighting
   (`generation_tps = sum(output tokens) / sum(generation seconds)`).
4. **`cached_tokens`** is reported only by oMLX. Elsewhere it is absent and the
   UI omits it. It is *not* subtracted from prefill tokens: oMLX's own
   `prompt_tokens_per_second` already uses `prompt − cached`, and PAIR's
   `prefill_tokens` remains the full prompt count. The UI shows the cached
   count separately; it does not recompute oMLX's rate. The count is shown
   **per job**, where the engine reported one. The node card shows no
   cached-token row at all: the broker's node totals carry no counter for the
   requests that reported a cached count, so a node-level total would read as
   a measured zero on a node whose engine never measured one. Such a total
   needs a presence counter (`cachedRequests`) in the broker's totals, which
   the engine-cache work (§6.7) brings.
5. **Provenance is presence.** A present value was measured by the engine; an
   absent one means the engine does not report it. There is no separate
   "measured vs. estimated" label, because there are no estimates. A value the
   engine reported as **zero** is present and stays a zero: oMLX reports
   `cached_tokens: 0` on every request that missed its prefix cache, and that
   zero is a measurement the engine made.
6. **`totalMs` is engine-reported only.** The total duration the measurement
   carries is the one the engine reported (`usage.total_time`), which only
   oMLX reports. Ollama and LM Studio report no total time, so `totalMs` is
   always absent for them. The proxy's own wall-clock duration is **not** a
   substitute: it includes queueing, transport and client write time, so it
   would not mean the same thing as oMLX's engine-reported total, and a user
   comparing an Ollama job with an oMLX job would be comparing two different
   quantities under one label. The proxy's own duration stays where it
   belongs — the `proxy/request` event's `duration_ms`.

**Why estimation was dropped.** An earlier draft derived missing phase
durations from PAIR's own timings (`prefill ≈ TTFT`, `decode ≈ total − TTFT`).
That does not survive contact with the two request shapes:

- *Streaming:* the first bytes of an SSE response are frequently a role-only
  delta emitted immediately, so "time to first byte" is not "time to first
  token" and can be near zero while the engine is still prefilling. A derived
  prefill would be systematically too small.
- *Non-streaming:* the engine computes the whole response before sending
  headers, so TTFB ≈ total duration. A derived split degenerates to
  `prefill ≈ total, decode ≈ 0`, and the decode rate becomes a division by
  zero.

Neither shape yields a usable estimate, and a wrong number displayed as a
measurement is worse than an absent one. LM Studio therefore shows tokens and
says that the engine does not report a phase split. §6.9 covers getting LM
Studio real numbers instead.

The same reasoning settles the total duration. An earlier draft filled
`totalMs` for Ollama and LM Studio with the proxy's own measured request
duration, which is not an estimate of a phase but a different quantity
altogether: it covers everything the request did between the client's write
and the client's read, while oMLX's `usage.total_time` covers the engine's own
work. Rule 6 above is the ruling: the measurement carries engine-reported
values only, so `totalMs` is absent for the two engines that report no total
time.

### 4.2 Proxy changes (all three engines)

**a. Surface the measurement.**
`telemetryBodyReader` already parses the chunk stream and already retains the
counts in plain fields. v1 extends those fields to the phase values and
exposes the finished measurement to the caller after the response completes.
The span attributes in `finalize()` become an *additional* consumer of the
same values, not the only one. The measurement must be available whether or
not telemetry is enabled.

**b. Parse the engine's fields.** Extend the `streamChunk` struct in each
proxy's `telemetry.go`:

- oMLX: `prompt_eval_duration`, `generation_duration`, `time_to_first_token`,
  `total_time`, and `prompt_tokens_details.cached_tokens` (a nested object).
  The engine's own `prompt_tokens_per_second` / `generation_tokens_per_second`
  are deliberately **not** consumed: PAIR computes rates its own way (§4.1
  rule 3), and reading a second engine-computed rate would create a second
  source of truth for the same cell.
- Ollama: `prompt_eval_duration` and `eval_duration` on the chunk itself (not
  under `usage`), converted from nanoseconds to milliseconds.
- LM Studio: token counts only.

All timing fields are optional in the wire shape (oMLX serializes with
`exclude_none=True`, so absent fields are simply missing), which is why the
shared type uses pointers.

**c. Inject `stream_options.include_usage` for oMLX.**
In the request path, before forwarding: if the body is a JSON object on an
inference endpoint, the request is streaming, and the body does not already
carry `stream_options`, set `stream_options.include_usage = true`.

Constraints:

- The body is already buffered and re-parsed for failover replay
  (`bufferBodyAndModel` in `services/omlx-proxy/proxy.go`), so the injection
  happens once, on the buffered copy, and every failover attempt forwards the
  same body.
- `stream_options` is already in `nvpair-shared/prefixhash`'s ignore list, so
  the prefix-cache key is unaffected.
- Only the omlx-proxy injects. Ollama needs nothing; LM Studio's behaviour with
  the flag is unverified (§8).
- The client's own request body is not modified in place — the injection
  happens on the copy PAIR forwards.

**d. Emit the measurement on the terminal workload event.**
The per-job surface, and the only v1 wire change. The proxy's `Workload`
struct gains the measurement with the workload spec's camelCase convention and
nullable columns (pointer fields, as `startedAt` / `completedAt` / `error`
already are):

```go
PrefillTokens *int   `json:"prefillTokens,omitempty"`
DecodeTokens  *int   `json:"decodeTokens,omitempty"`
CachedTokens  *int   `json:"cachedTokens,omitempty"`
PrefillMs     *int64 `json:"prefillMs,omitempty"`
DecodeMs      *int64 `json:"decodeMs,omitempty"`
TtftMs        *int64 `json:"ttftMs,omitempty"`
TotalMs       *int64 `json:"totalMs,omitempty"`
```

They are attached to the **terminal** event (`workload:completed` /
`workload:errored`) only. A `workload:started` event has no measurements yet.
A failed request carries whatever was measured before the failure (often
nothing) and omits the rest.

**e. Rejections produce no workload events.** When no candidate node is
available, the proxy answers 502 and returns *before* the workload is created
(`services/omlx-proxy/proxy.go`, the `len(candidates) == 0` branch). So a
rejected request never reaches the accumulator, and unrouted traffic cannot
inflate per-node numbers. (Note that `proxy/request` *is* emitted on that path;
it is the workload stream, not the absence of an event, that keeps rejections
out of this feature.)

**f. Share one definition.**
The measurement shape is defined once in `nvpair-shared/inferencemetrics` and
embedded, not re-declared, in the places that carry it. The workload spec uses
camelCase and the proxy's `RequestEvent` uses snake_case, so a single tagged
struct cannot serve both; the shared package therefore provides the
measurement type plus a tagged **workload view** for v1. (A snake_case request
view is deferred to v2, where the `RequestEvent` field set is actually
consumed — see §6.5. Adding it in v1 would put a wire change in place with no
reader, and would mean three more MINOR service bumps for nothing.)

### 4.3 Per-node aggregation — the cluster question

This is the load-bearing part of the design, so it is worth being precise.

**Observation:** a request that enters node A's proxy and is served by node B
produces exactly one workload lifecycle event — from **node A's** proxy,
naming node B in `scheduledOn` (re-pointed at the commit point if failover
moved the request). The cluster ingress on node B
(`services/omlx-proxy/ingress.go`) forwards to the local engine and emits
**no** workload event. So the origin proxy is the single reporter for every
request, and the serving node is always named explicitly. **No double counting
is possible.**

**The problem:** the desktop's node list is cluster-wide, but each node runs
its own broker. A local broker sees (a) the traffic its own proxies handled,
and (b) remote traffic only insofar as a local client caused it. Traffic that
entered a remote node directly is invisible to the local broker.

**The solution — no new inter-node surface.** The workload lifecycle stream is
already cluster-wide: the proxy emits `workload:started` / `workload:completed`
to the local broker, which stamps the origin node id, forwards to the
workload-manager, and the workload-manager **broadcasts every lifecycle frame
to all pinned peers**. Peers relay what they receive back to their own broker
as `workloads:upsert`. Every broker therefore already receives every job in
the cluster, including the `scheduledOn` node that served it.

So: **each broker accumulates per-node metrics from the same cluster-wide
workload stream it already consumes.** The terminal workload event carries both
the measurement (§4.2d) and `scheduledOn` (the serving node), so every broker
can maintain per-node totals. No new inter-node method, no new trust boundary,
no new contract surface.

**Exactly-once accumulation.** The cluster re-delivers terminal events: the
workload-manager re-asserts each terminal workload on a heartbeat, and a newly
joined peer receives a backfill snapshot. Those arrive as `workloads:upsert`
and are absorbed by the store's merge, which returns `false` for a duplicate.
The accumulator is therefore fed **only when the store accepts the merge**
(`workloadstore.Store.Apply` → `true`), never on a duplicate, and **never on
an inferred event** (`ApplyInferred*`, the broker's node-loss sweep, which
synthesizes a terminal `failed` state for a departed node and carries no
measurement). Feeding the accumulator on every event instead of on accepted
merges would inflate per-node totals two- to threefold.

**Consequences to note**

- The broker's per-node **totals** are identical on every up-to-date member,
  which is what makes the number trustworthy when two members disagree on
  screen. A member running an older workload-manager strips the measurement
  from remote jobs (§4.8), so the identity claim holds only among up-to-date
  members.
- The **windowed rates** are computed over each broker's local arrival clock,
  never by comparing a remote event's timestamp to the local clock (the repo
  already defends against cross-node clock skew elsewhere, e.g.
  `clampActivityAge`). Two members can therefore hold slightly different
  sample sets at a window boundary and report slightly different instantaneous
  rates, while their totals agree exactly. This is the honest trade: a shared
  clock is not available, and a shared clock would be worse.
- The totals are *PAIR-routed* traffic. Traffic that bypasses PAIR entirely
  (a local app hitting the engine's loopback port directly) is not counted.
  Since all three engines bind `127.0.0.1`
  (`services/nvpair-engine-manager/manifests/*.json`), that is a small and
  well-understood gap, and it is the honest denominator for the number.
- The broker's workload store already persists job history to disk with a
  7-day / 10k-record cap, so per-job measurements inherit that retention.
  Per-node **totals** are kept in a separate monotonic accumulator, not
  re-derived from the catalog (the catalog is pruned; totals must not shrink).

### 4.4 Broker changes

**a. A per-node metrics store.** New package `nvpair-ui-broker/nodemetrics`,
following the `workloadstore` precedent: pure state + the merge rule, no I/O
in the state file, persistence and RPC wiring in separate files.

State per node id:

- totals since the broker started: requests, prefill tokens, decode tokens,
  cached tokens, prefill ms, decode ms, ttft ms (sums), and the count of
  requests that reported a TTFT
- a rolling window of recent completions (local arrival time, tokens,
  durations) used to compute current rates over the last 60 seconds
- a per-model breakdown of the same totals, keyed by the workload's `model`
  field. The workload event already carries the model, so the breakdown costs
  one map lookup per job; it is included in v1 because "this node is slow" is
  usually "this node is slow *for this model*", and the UI presents it behind
  a per-node disclosure that is collapsed by default.

**b. Fed by the workload stream.** The broker's existing workload-ingest path
applies the terminal event's measurement to the accumulator for `scheduledOn`,
but only on an accepted merge (§4.3).

**c. Exposed as a snapshot.** Following the relay-protocol-v2 rules
(snapshot-per-push; clients recover on epoch change, not by polling):

- invoke: `metrics:get-node-metrics` → the full per-node snapshot
- push: `metrics:node-metrics` → the full per-node snapshot (not a delta)
- subscribe: `metrics:subscribe` / `metrics:unsubscribe`, mirroring the
  existing `workloads:subscribe` gate so the push is opt-in

**d. Push cadence.** One push per completed inference request would put the
broker's write path under load for no benefit. The broker coalesces: at most
one snapshot push per second, carrying the newest state. The invoke method
returns the same snapshot for hydration on connect.

**e. Rates and aggregates.**

- Prefill tok/s = `sum(prefill tokens in window) / (sum(prefill ms in window) / 1000)`.
- Decode tok/s = `sum(decode tokens in window) / (sum(decode ms in window) / 1000)`.
- TTFT = `sum(ttft ms) / count(requests that reported a TTFT)` — a **mean**,
  deliberately different from the token rates, because averaging durations
  across requests is the meaningful aggregate while averaging rates is not.
  The denominator is requests that reported a TTFT, not all requests.
- A node with no traffic in the window appears with zeroed rates rather than
  being absent, so the UI can distinguish "idle" from "unknown".
- **Presence counters.** A rate of `0` is ambiguous on its own: an engine that
  reported no prefill duration looks identical to one that reported a
  zero-length prefill. The totals therefore carry, alongside the sums, the
  count of requests that reported each value — `tokenRequests`,
  `prefillRequests`, `decodeRequests`, `ttftRequests` — and the UI renders a
  phase as "not reported by this engine" when its counter is zero. This is
  the per-node counterpart of the per-job contract, where a nil pointer
  already means "not reported".

### 4.5 Desktop changes

- **Types:** `desktop/src/shared/types/metrics.ts` gains the per-node metrics
  type; `desktop/src/shared/types/workloads.ts` gains the optional per-job
  fields.
- **Channels:** `desktop/src/shared/types/ws-channels.ts` gains the invoke,
  push, and subscribe entries; `desktop/src/ui/api/pair-api.ts` gains the
  accessors; the bridge handler in
  `desktop/src/electron/service-bridge/empty-handlers.ts` answers
  `metrics:get-node-metrics` from the bridge's cached snapshot.
- **Store:** the bridge caches the pushed snapshot; the renderer reads it from
  `desktop/src/ui/stores/metrics.store.ts`, which already keys by node id and
  keeps a rolling series for hardware metrics.
- **What the desktop may compute.** The desktop never counts, never filters,
  never aggregates across records, and never owns a windowed rate — those are
  the service's, and the repo's own history (`routingmetrics`) is a warning
  about what happens when two places own them. It may render a **per-record
  ratio of two values the service reported** — one request's tokens over that
  request's duration — which is the same kind of arithmetic it already does
  when it shows memory usage as a percentage of the reported total. Every such
  ratio is **labelled with the window it covers**, so a 60-second windowed
  rate and a since-broker-start ratio are never shown under the same label.
- **Node card** (`desktop/src/ui/components/NodeList/NodeCardDetails.tsx`):
  a prefill/decode section — prefill tok/s, decode tok/s, TTFT and the
  prefill + decode token count. Every figure is
  labelled with the window it covers: the node's rates with the broker's
  60-second window, the totals and the per-model breakdown with the broker's
  start, the job card with the request.
  Absent phases are shown as "not reported by this engine" rather than as 0.
  The node card carries **no cached-token row**: only oMLX reports a cached
  count, and the node's totals hold no counter for the requests that reported
  one, so a node-level total would read as a measured zero on a node whose
  engine never measured one (rule 4).
  A per-node disclosure lists the per-model breakdown; it is collapsed by
  default.
- **Job card** (`desktop/src/ui/components/Workloads/WorkloadItemCard.tsx`):
  per-job prefill/decode tokens, the cached-token count, the phase durations,
  the total duration and the rates, shown once the job is terminal.
  The optional extras — the cached-token count and the total duration — are
  **omitted** when the engine did not report them: an engine that reports no
  cached count and no total time has no row for them, and no zero is shown in
  their place. A job with a partial measurement shows "not reported" for what
  the engine did not report; a job with no measurement at all shows no
  measurement block — a job that reported nothing (a failed job, or an engine
  that sent no usage block) has nothing to report and must not carry a block
  of "not reported" noise.
- **Relay:** the broker's push reaches the renderer through the existing
  bridge push path (`emitBridgePush`).

### 4.6 Privacy and policy compliance

- The measurement is metadata: counts and durations. No prompt, message,
  response body, token id, key, or header is included. It is safe to relay
  cluster-wide over the pinned mTLS channel, in the same envelope as the
  existing workload lifecycle fields.
- The oMLX API key is read from `~/.omlx/settings.json` by existing code
  (`readOMLXAPIKey`). It must never appear in a log line, a metric, or an
  error message. v1 does not add new uses of the key.
- No new standard log sink receives these numbers.

### 4.7 Versioning and contract bookkeeping

- `services/versions.json`: MINOR bumps for `omlx-proxy`, `ollama-proxy`,
  `lmstudio-proxy` (new fields on the workload lifecycle events),
  `nvpair-ui-broker` (new method, push, and subscribe), and
  `nvpair-workload-manager` (it must carry the new fields — see §4.8).
  `product` and `installer` take a MINOR bump.
- `desktop/docs/services-api.md` is regenerated with
  `npm run service-contracts:write`; never hand-edited.
- `npm run service-contracts:check` must pass.
- Every changed file carries the two-line SPDX header.

### 4.8 A trap worth naming

`services/nvpair-workload-manager/server.go` receives a peer's lifecycle frame,
parses it into the workload-manager's **typed** `Workload` struct, and
re-emits it to the local broker as `workloads:upsert`. A field the struct does
not declare is **dropped on that path**.

The asymmetry matters: the origin → peer direction preserves fields, because
`broadcastFrame` re-marshals the raw params. Only the peer → broker re-emit
drops them. So in a cluster where one member runs an older workload-manager,
that member keeps its own local jobs' numbers and silently loses every remote
job's numbers — the totals look plausible and are wrong, and the loss is
invisible from the originating node.

Two consequences:

1. The workload-manager's `Workload` struct must carry the new fields, and a
   peer-relay test must assert that a remote job's measurement survives the
   round trip.
2. The "identical on every member" guarantee is restated as **"identical among
   up-to-date members"**, and the mixed-version case is a known limitation
   (§8), not a bug in this design.

The broker itself is safe: it manipulates workload params as generic JSON
(`stampNodeID`, `nvpair-ui-broker/broker.go`) specifically so it does not
duplicate the Workload schema.

---

## 5. v1 deliverables

Ordered, each independently testable.

| # | Change | Where |
| --- | --- | --- |
| 1 | Shared measurement type + workload wire view | `services/shared/inferencemetrics/` (new module) |
| 2 | Surface the measurement outside the span-recording gate | `services/{omlx,ollama,lmstudio}-proxy/telemetry.go` |
| 3 | Parse each engine's phase fields | same three files |
| 4 | Inject `stream_options.include_usage` | `services/omlx-proxy/proxy.go` |
| 5 | Add the measurement to the proxy `Workload`, populated on the terminal event | same three files |
| 6 | Carry the new fields through the peer relay | `services/nvpair-workload-manager/workload.go` |
| 7 | Per-node metrics store | `services/nvpair-ui-broker/nodemetrics/` (new package) |
| 8 | Feed the store from the accepted workload merge; expose the snapshot method, push, and subscribe | `services/nvpair-ui-broker/broker.go` |
| 9 | Types, channels, bridge handler, renderer store | `desktop/src/shared/types/{metrics,workloads,ws-channels}.ts`, `desktop/src/ui/api/pair-api.ts`, `desktop/src/electron/service-bridge/{empty-handlers,modular-state}.ts`, `desktop/src/ui/stores/metrics.store.ts` |
| 10 | Render on the node card and the job card | `desktop/src/ui/components/NodeList/NodeCardDetails.tsx`, `desktop/src/ui/components/Workloads/WorkloadItemCard.tsx` |
| 11 | Versions, contract check, regenerated API doc | `services/versions.json`, `desktop/docs/services-api.md` |
| 12 | Documentation | `docs/architecture.mdx` (observability section), the affected service READMEs |

**Effort:** roughly 3–5 working days including tests, contract regeneration,
and version bumps. The per-job slice (items 1–6, 9, 10-job-card) is about a
day of that and is independently shippable.

**Not in v1:** the TUI. `services/nvpair-tui/ui/workloads.go` renders its own
workload table; the new fields are additive, so the TUI keeps working
unchanged. TUI parity is v2 (§6.6).

---

## 6. v2 scope — the full integration

v2 is where the engine-native surfaces get used, and where the display purpose
actually starts to pay off. Each item is independently valuable; they are
ordered by value-per-effort.

### 6.1 Engine-native enrichment (oMLX admin API)

Poll `GET /admin/api/stats` and `GET /admin/api/usage` on the node's own
oMLX server to obtain the engine's **own** view: totals including traffic
PAIR did not route, `cache_efficiency`, per-model breakdown, and hourly
history that survives process restarts.

- The **engine-manager** is the natural owner: it already owns the engine
  process, already polls its health, and already reads the oMLX API key
  (`services/nvpair-engine-manager/actions.go`).
- The numbers are presented as a **separate, labelled panel** ("oMLX
  self-report"), never merged into the PAIR-derived per-node numbers.
- Cluster reach (a node's self-report visible to other members) needs a new
  inter-node surface — the workload broadcast is already used for job metrics,
  and engine self-reports are node-scoped rather than job-scoped, so they need
  their own method. This is the most expensive item in v2.
- Applies to oMLX only. Ollama and LM Studio have no equivalent admin surface,
  so this panel is engine-scoped by construction.

### 6.2 Live job inspector

Poll `GET /admin/api/activity` to show, for in-flight jobs, the live queue
position, prefill progress, and current decode rate — before the job
completes. PAIR already receives `X-oMLX-Request-ID` on every response, so the
origin proxy can map its request id to the engine's request id and the job card
can show live state. This is a **separate feature** (live view), with its own
polling cost; it should not be bundled with the metrics work.

### 6.3 Trends and history

Today the broker's per-node metrics are cumulative totals plus a 60-second
window. v2 adds a longer series (hourly buckets, like oMLX's own usage
history) so the UI can show "this node's decode rate fell 40% this morning".
The desktop's existing metrics store already keeps a rolling 60-point series
for GPU/CPU/memory (`desktop/src/ui/stores/metrics.store.ts`) — the same
pattern, a different source and cadence.

### 6.4 Node comparison view

The payoff of the display purpose: "which node is fastest for model X", side
by side, with the measurement's provenance and window visible. This is a new
panel, not a change to the node card, because it compares rather than
describes.

### 6.5 Phoenix / OpenTelemetry parity

Two related additions:

- Emit the same numbers as span attributes (`pair.inference.prefill_ms`,
  `pair.inference.decode_ms`, `pair.inference.prefill_tokens`,
  `pair.inference.decode_tokens`) so the UI and Phoenix agree, and so a
  per-node slice in Phoenix is possible without a parent/child join (today the
  token counts live on the `pair.engine.dispatch` child span while
  `pair.request_id` lives on the parent `pair.proxy.inference` span). The
  existing `gen_ai.usage.*` attributes are unchanged.
- Add the measurement to the proxy's `RequestEvent` (snake_case view in the
  shared package) so a per-request record is available to anything that
  consumes the proxy event stream rather than the workload stream. This is
  where the snake_case wire view earns its place.

### 6.6 TUI parity

`services/nvpair-tui/ui/workloads.go` (job table) and `ui/nodes.go` (node
table) gain the same columns the desktop shows. The TUI is a first-class
surface in this repo, so a TUI that cannot see a number the desktop can is an
inconsistency worth closing.

### 6.7 Engine cache efficiency vs. routing affinity

oMLX reports its own KV prefix-cache reuse (`cached_tokens`,
`cache_efficiency`). PAIR separately tracks *routing* affinity (does a request
land on the node that already holds the prefix). v2 can show both side by
side, which is the only way to answer "is the cache helping, and is the
routing helping?" — they are different mechanisms with different effects.

The node-level cached total belongs here as well. Showing a node's cached
tokens needs a presence counter in the broker's totals — `cachedRequests`,
how many requests reported a cached count — because without it the total is
indistinguishable from a measured zero on an engine that reports no cached
count at all (rule 4). That counter is a wire-contract change and is
deliberately deferred to this section, where the engine-cache comparison is
built. Until then, the cached count is shown per job, where the engine
reported one.

### 6.8 Persistence across broker restarts

The per-node accumulator is in-memory in v1 and resets when the broker
restarts. The workload store already persists job history with a 7-day cap;
v2 persists the per-node totals on the same cadence so "since" means "since"
rather than "since the last broker start".

### 6.9 LM Studio phase metrics

LM Studio reports no phase split on the OpenAI-compatible surface PAIR fronts.
Its native REST surface does report tokens/second and time-to-first-token. v2
can either add a native-surface call for LM Studio, or accept that LM Studio
shows tokens only. Until then, the UI says "not reported by this engine" rather
than showing an estimate.

### v2 effort and dependencies

| Item | Effort | Depends on |
| --- | --- | --- |
| 6.5 Phoenix parity + `RequestEvent` view | 1–1.5 days | v1 measurement plumbing |
| 6.6 TUI parity | 1 day | v1 wire fields |
| 6.3 Trends and history | 2–3 days | v1 store, new bucketing |
| 6.2 Live job inspector | 3–4 days | v1, `X-oMLX-Request-ID` capture, polling policy |
| 6.1 Engine-native enrichment (local node) | 3–4 days | engine-manager polling, admin auth, panel |
| 6.1 Engine-native enrichment (cluster reach) | 3–5 days | new inter-node method, trust boundary, contracts |
| 6.4 Node comparison view | 2–3 days | v1 + 6.3 |
| 6.7 Cache affinity vs. engine cache | 1–2 days | 6.1 |
| 6.8 Persistence | 1–2 days | v1 store |
| 6.9 LM Studio phase metrics | 1–2 days | engine-specific call path |

---

## 7. Testing and verification

**Go unit tests (per proxy)**

- Usage parsing: each engine's field set, streaming and non-streaming, with
  absent optional fields (oMLX's `exclude_none=True` behaviour).
- **A reported zero survives.** oMLX reports `cached_tokens: 0` on every
  request that missed its prefix cache; that zero reaches the workload event
  as a measured zero, and a field the engine did not report at all stays
  absent. The token counts on the wire are pointer-typed for exactly this
  reason.
- The measurement is available when telemetry is disabled (the §4.2a bug this
  design exists to prevent).
- Injection: `stream_options.include_usage` is added for streaming oMLX
  requests, not added when the client already set `stream_options`, not added
  to non-inference endpoints, and the forwarded body stays valid JSON.
- Failover: the measurement comes from the **committed** candidate, not an
  earlier one that failed.
- The terminal workload event carries the measurement; the started event does
  not; a rejected request produces no workload event at all.
- **`totalMs` is engine-reported only.** oMLX's terminal event carries the
  engine's `usage.total_time`; Ollama's and LM Studio's carry no `totalMs` at
  all, because the proxy's own wall-clock duration is not a measurement of
  the engine (§4.1 rule 6).

**Broker tests**

- Per-node accumulation from local and peer-origin events.
- **Duplicate suppression:** a re-asserted terminal event (the workload
  manager's heartbeat) and a backfill snapshot do not inflate totals; the
  accumulator is fed only on accepted merges.
- An inferred node-loss failure contributes nothing.
- No double counting: a request that traverses a peer ingress is counted once.
- The rolling window expires old samples; rates go to zero, not negative.
- Two brokers fed the same event stream produce identical **totals**; their
  windowed rates may differ at a boundary by design (§4.3).

**The proxy → broker hop**

The one shape that catches a contract drift between two components is a
request driven through the whole path: proxy → workload event → broker →
per-node snapshot. v1 closes that contract **at both ends of the hop** rather
than in the cross-process suite:

- The oMLX proxy's end-to-end test
  (`services/omlx-proxy/cache_affinity_e2e_test.go`) drives the compiled
  service's stdio and HTTP boundaries against a fixture engine and asserts
  that the recorded `workload:completed` frame carries the measurement
  **inline in `workloadInfo`** — the field names and the values the engine
  reported.
- The broker's counterpart test
  (`services/nvpair-ui-broker/node_metrics_test.go`) feeds a **verbatim copy
  of that frame's `workloadInfo` JSON literal** into the broker's
  workload-ingest path and asserts the per-node total and the per-model
  breakdown.
- The two literals agree by eye, and that agreement is the contract.

**Why not `services/tests`.** The cross-process suite cannot run reliably in
the review environment: fourteen of its tests fail on that machine for an
environment reason (a running PAIR.app holds ports 11434/1234/14320), so a new
case there could not be verified. The hop is therefore verified where it can
be: on the producing side by the proxy's end-to-end test, on the consuming
side by the broker's ingest test.

**Desktop tests**

- Type mapping and store updates for the new channels.
- The node card and job card render a present measurement and an absent one
  distinctly (the latter must not read as 0).
- The optional extras — the job card's cached-token count and its total
  duration — are **omitted** when the engine reported none, and shown when it
  did, including a reported zero.
- The node card has no cached-token row: a node-level cached total would read
  as a measured zero on a node whose engine never reported a cached count.
- The node card says what its totals cover: they are the totals the broker
  has kept since it started, not the life of the engine.

**Live verification checklist** (manual, not in CI)

- oMLX: confirm the injected `stream_options.include_usage` produces the
  timing fields, and that the values match `/admin/api/stats` deltas.
- Ollama: confirm `prompt_eval_duration` / `eval_duration` arrive and convert
  correctly from nanoseconds.
- LM Studio: confirm whether its streaming responses include `usage` at all,
  and whether it honours `stream_options.include_usage` (§8, §6.9).

Live engine tests are deliberately not part of CI: they modify real system
state, which `AGENTS.md` warns against outside a controlled machine.

---

## 8. Risks and unknowns

| # | Risk | Impact | Handling |
| --- | --- | --- | --- |
| 1 | LM Studio's streaming responses may carry no `usage`; the flag's effect is unverified | LM Studio streaming jobs show no token counts | Verify live first. If unsupported, LM Studio contributes token counts only, and the UI says so (§6.9) |
| 2 | oMLX's `usage` chunk is absent unless the flag is injected | oMLX streaming jobs show nothing | v1 step 4 injects it; verify against a live engine |
| 3 | The workload-manager's typed `Workload` drops unknown fields on the peer → broker path | Remote jobs silently lose their numbers, asymmetrically | §4.8 — the manager's struct must carry the fields; covered by a peer-relay test |
| 4 | A member running an older workload-manager strips remote measurements | Per-node totals differ between members, silently | Restated guarantee (§4.3); documented limitation; no version gate in v1 |
| 5 | Duplicate terminal events (heartbeat re-assert, peer backfill) | Totals inflate 2–3× | Accumulator fed only on accepted merges (§4.3); covered by a test |
| 6 | The broker's node-loss sweep synthesizes terminal failures | Would count jobs that never ran | Inferred events are never fed to the accumulator (§4.3) |
| 7 | Proxy restarts | None | Totals live in the broker, which outlives proxies |
| 8 | Broker restart | Per-node totals reset | Labelled "since broker start"; v2 §6.8 persists them |
| 9 | Cross-node clock skew | Instantaneous rates could differ between members | Windowing uses each broker's local arrival clock, never a remote timestamp compared to the local clock; totals still agree exactly |
| 10 | Traffic that bypasses PAIR (direct to the engine's loopback port) | Per-node totals understate the engine's true load | All engines bind `127.0.0.1`; the gap is small, understood, and the honest denominator |

---

## 9. Explicitly out of scope

- **Scheduler consumption.** The job scheduler ranks on GPU pressure, queue
  depth, and affinity. Feeding it prefill/decode rates would require a
  freshness bound, a per-model rate table, and a decision about what a fast
  node is worth against a slow one — a separate design with a different risk
  profile, and it would change routing behaviour. Out of scope for v1 **and**
  v2.
- **Prompt or completion content.** Never.
- **A new metrics worker or service.**
- **Replacing the Phoenix path.** It stays.
- **Estimated (PAIR-derived) phase durations.** Deliberately dropped (§4.1).
- **Cross-engine normalization of "cached tokens"**, which only oMLX reports.

---

## 10. Decisions taken, and the one that is the owner's

**Taken (implementation calls, not design questions)**

- The measurement lives in a shared Go package and is embedded, not
  re-declared, in each wire struct — to stop the copies drifting.
- **Only engine-measured values are reported.** No estimates, no derived
  phases, no separate provenance label: presence is provenance.
- Rates are `sum/sum` over a window, matching oMLX's own weighting; TTFT is a
  mean over the requests that reported one.
- The broker aggregates; the desktop displays and never counts.
- The per-node accumulator is fed by the **existing** cluster-wide workload
  stream, so v1 needs no new inter-node surface.
- The accumulator is fed only on accepted merges, never on inferred events.
- Windowing uses the local arrival clock; totals are cluster-identical,
  instantaneous rates are best-effort per member.
- The `RequestEvent` field set is **not** changed in v1 — nothing consumes it
  yet. It arrives with v2 §6.5.
- The oMLX admin API is v2, not v1.
- The TUI is v2, not v1.
- Totals are "since broker start" in v1, labelled as such.

**Owner decisions (recorded 2026-10-20)**

- **The per-node view breaks down by model in v1**, behind a per-node
  disclosure that is collapsed by default. The workload event already carries
  `model`, so the breakdown is nearly free, and "this node is slow" is usually
  "this node is slow *for this model*". §4.4a and §4.5.
- **`totalMs` is engine-reported only** (§4.1 rule 6). The measurement type
  is defined as engine-measured and presence is the provenance, so a
  proxy-measured wall-clock duration does not belong in it: oMLX fills
  `totalMs` from `usage.total_time`, and for Ollama and LM Studio the field
  stays absent. The proxy's own duration stays in the `proxy/request` event's
  `duration_ms`.
- **The proxy → broker hop is verified at both ends** rather than in
  `services/tests` (§7): the oMLX proxy's end-to-end test asserts the
  measurement inline in the recorded `workload:completed` frame, and the
  broker's ingest test feeds a verbatim copy of that frame's `workloadInfo`
  literal into the workload-ingest path. The two literals agree by eye, and
  that agreement is the contract. `services/tests` cannot run reliably in the
  review environment (a running PAIR.app holds ports 11434/1234/14320).

---

## 11. Delivery order

1. Shared package + proxy parsing + surfacing (items 1–3), with unit tests.
2. oMLX injection (item 4) — verified live against a running oMLX.
3. Workload fields (item 5) + workload-manager carry-through (item 6) + the
   peer-relay test.
4. Broker store and snapshot (items 7–8).
5. Desktop wiring and rendering (items 9–10).
6. Versions, contracts, docs (items 11–12).
7. Ship v1. Open v2 against §6.

---

## 12. What the builder-pass review changed

Recorded so a reviewer can see what moved after the first draft.

| Finding | Change |
| --- | --- |
| PAIR-derived phase durations are unreliable in both request shapes (an SSE stream's first chunk is often a role-only delta; a non-streaming response's TTFB ≈ total) | §4.1 — estimation dropped entirely; the contract reports engine-measured values only |
| The reader already retains the parsed counts, and is installed regardless of telemetry state | §4.2a rewritten: the work is to *surface* the values, not retain them |
| `proxy/request` **is** emitted on the rejection path, so it is not the absence of an event that excludes unrouted traffic | §4.2e — the exclusion comes from the workload path, which never creates a workload for a rejected request |
| The workload-manager's field drop is one-sided (origin → peer preserves raw params; peer → broker re-emit drops them) | §4.8 rewritten, and the cluster-identity claim restated as "identical among up-to-date members" (§4.3) |
| Nothing in v1 consumes the `RequestEvent` measurement | §4.2f and §5 — the `RequestEvent` change moves to v2 §6.5, with its snake_case view |
| The cluster re-delivers terminal events (heartbeat re-assert, peer backfill) and the broker synthesizes inferred failures | §4.3 — the accumulator is fed only on accepted merges, never on inferred events; both are now tested (§7) |
| One Go struct cannot serve both the camelCase workload spec and the snake_case proxy event | §4.2f — the shared package exposes the measurement plus a tagged workload view |
| Cross-node clock skew breaks a shared 60-second window | §4.3 — windowing uses the local arrival clock; the cluster-agreement claim is narrowed to totals |
| A push per inference request would overload the broker's write path, and high-volume streams in this repo are subscribe-gated | §4.4 — coalesced to at most one push per second, behind `metrics:subscribe` |
| `services/tests` has no live-engine harness, and live engine tests modify real system state | §7 — the proxy → broker hop is closed at both ends (the oMLX proxy's end-to-end test and the broker's ingest test), and live checks are a manual checklist |
| The node card's TTFT aggregate was undefined | §4.4e — a mean over the requests that reported a TTFT |
| The renderer-side store for the new per-node metrics was not listed | §5 item 9 — `desktop/src/ui/stores/metrics.store.ts` named |
