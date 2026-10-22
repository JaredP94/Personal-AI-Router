<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Inference Metrics v2 — Engine-Native Surface and Live In-Flight Numbers

- **Date:** 2026-10-21
- **Status:** proposed — awaiting owner approval
- **Scope:** slice **A** (the engine-native surface) and slice **B** (live in-flight
  numbers). Slices C (history and comparison) and D (parity, persistence, LM Studio
  phases) are later work and are out of scope here.
- **Owner decision, 2026-10-21:** the live view shows **both** the PAIR-derived
  progress and the engine-native progress (option c).

This is the second half of the
[design](2026-10-20-inference-metrics-design.md) §6. It assumes v1 has shipped
(as `0.7.0-rc.3`).

---

## 1. Intent

v1 answers "what did this job cost, and what has this node done?" — but only
**after** the job ends, because the engine reports its numbers at the end of the
response. Two things are missing:

1. **The engine's own view of itself.** PAIR aggregates what it observes, which
   is not the same as what the engine knows: the engine knows its own cache
   efficiency, its own queue, and its own prefill progress. oMLX publishes all of
   it on an admin API that PAIR does not currently read.
2. **What a running job is doing right now.** v1 shows nothing until the job
   ends, so a job that is stuck in prefill looks identical to a job that is
   halfway through decoding.

The intent of v2-A+B is to close both, **without weakening the rule v1 was
built on**: a number PAIR does not measure is never shown as if it had been
measured.

## 2. What is available

**oMLX's admin API** (same server, same port as the inference API; the main API
key as a bearer token — sub keys are refused there):

| Endpoint | Content |
| --- | --- |
| `GET /admin/api/stats?scope=session\|alltime&model=` | Request and token counters, `avg_prefill_tps`, `avg_generation_tps`, `cache_efficiency`, loaded models. **Its payload also contains `api_key`** |
| `GET /admin/api/activity` | Live per-request rows: `waiting` (queue position, elapsed), `prefilling` (progress), `generating` (`generated_tokens`, `tokens_per_second`, `last_activity_age_seconds`), keyed by `request_id` |
| `GET /admin/api/usage?range=&model=` | Hourly history: `prefill_seconds`, `generation_seconds`, `prefill_tps`, `generation_tps`, `cache_efficiency` |

Every inference response carries `X-oMLX-Request-ID`, sent before prefill starts
on the streaming path, so a client can find its own request among the live rows.
oMLX also threads a client-supplied `X-Request-ID` into its preflight and log
correlation.

**What the live engine reports.** Live testing against **oMLX 0.7.2** found
three things the table above does not say:

- The **activity** payload's `active_models` is an object whose `models` key
  holds the array of models, with the engine's own `model_memory_used` and
  `model_memory_max` counters beside it. A live row carries `request_id`,
  `elapsed_seconds`, `generated_tokens`, `tokens_per_second`,
  `last_activity_age_seconds`, `prompt_tokens` and `max_tokens`; a queued
  request carries `queue_position` and `elapsed_seconds`.
- The **stats** payload matches what PAIR parses: `total_prompt_tokens`,
  `total_completion_tokens`, `total_cached_tokens`, `total_requests`,
  `avg_prefill_tps`, `avg_generation_tps` and `cache_efficiency` are all
  there, and the `api_key` field PAIR drops is present in the engine's
  payload, as expected.
- The **streaming** `usage` chunk carries only `prompt_tokens`,
  `completion_tokens`, `total_tokens`, `input_tokens`, `output_tokens`,
  `prompt_tokens_details.cached_tokens` and
  `completion_tokens_details.reasoning_tokens`. It carries **no**
  `prompt_eval_duration`, `generation_duration`, `time_to_first_token` or
  `total_time`; the engine's non-streaming response does carry those timings.
  A newer oMLX includes them in the streaming chunk; 0.7.2 does not.

So a streaming request against oMLX 0.7.2 gives the per-job measurement its
token counts and **not** its phase durations. That is the designed behaviour
rather than a gap to work around: an absent value stays absent, and the UI
says "not reported". The phase durations appear only on a newer oMLX, or on a
non-streaming request.

**PAIR's surfaces that already exist:**

- The **engine-manager's em surface** (`services/nvpair-engine-manager/httpserver.go`)
  serves one port with two personalities — plaintext restricted to loopback, and
  cluster mTLS restricted to pinned peers — and serves `/v1/models` on both. The
  **node-scanner already fetches every peer's `/v1/models`** over a per-peer mTLS
  pool (`services/nvpair-node-scanner/daemon.go`), and fetches its own node's over
  loopback.
- The **workload lifecycle is already cluster-wide**: the proxy emits
  `workload:started` / `workload:completed`, the broker stamps the origin and
  forwards to the **workload-manager**, which **broadcasts every frame to all
  pinned peers**; peers relay them back to their own broker as
  `workloads:upsert`, and every broker fans them out to its clients.
- The proxy's **telemetry reader already sees every streamed chunk** as it
  arrives.

**Consequence:** both slices need **no new inter-node mechanism and no new trust
boundary**. The engine-native surface rides the em surface the scanner already
fetches, and the live frames ride the workload broadcast the cluster already
uses.

## 3. The honesty problem, and the ruling

A live view wants "340 tokens so far, 64 tok/s". PAIR can count the chunks it
has received — but a chunk is not a token. Estimating tokens from chunks would
put an estimate on screen next to v1's exact numbers, and v1's whole discipline
is that presence means measured.

**Ruling:** the live view shows **facts PAIR measures exactly** and **numbers the
engine measures exactly**, and nothing else.

- PAIR-derived live facts (all three engines): elapsed time, time to first byte,
  time to first content, and the count of content chunks and response bytes
  received. All exact. **No token count, no rate** — PAIR cannot know either
  without estimating.
- Engine-native live numbers (oMLX only): the engine's own `generated_tokens`,
  `tokens_per_second`, prefill progress and queue position. Exact, because the
  engine measured them.
- A rejected alternative: estimate tokens from chunk count and label it "≈". It
  was rejected because it puts an estimate on the same card as v1's exact
  numbers, and a reader cannot tell which is which at a glance.

So the live job card shows, for an oMLX job, the engine's live token count and
rate; for an Ollama or LM Studio job, elapsed time and how much has streamed.
That is a real difference between engines, and the card says so rather than
papering over it.

## 4. Design

### 4.1 Slice A — the engine-native surface

**A1. A new endpoint on the engine-manager's em surface.**

`GET /v1/engine-stats` and `GET /v1/engine-activity`, registered on the same
listener as `/v1/models`. The stats path is cluster inventory and is served
behind the same two gates as `/v1/models` (plaintext → loopback only; TLS →
pinned peers). The activity path is this node's own live engine rows: only
this node's own proxy reads it, over loopback, so the TLS personality does
not serve the path at all — no peer reads another node's live engine rows.
The engine-manager already holds the local engine's URL and the oMLX API key
(`services/nvpair-engine-manager/actions.go`).

The response is **PAIR's own type, not oMLX's payload**. That is a security
decision as much as a stability one: oMLX's `/admin/api/stats` includes
`api_key`, and PAIR's type has no such field, so the key cannot leak by
accident. The type also drops `host`, `port`, `cli_prefix` and every other field
PAIR does not need.

```json
{
  "engine": "omlx",
  "supported": true,
  "totals": {
    "requests": 3270, "promptTokens": 212978711, "completionTokens": 2147906,
    "cachedTokens": 157045860, "cacheEfficiency": 73.7
  },
  "rates": { "prefillTokensPerSecond": 1553.2, "generationTokensPerSecond": 32.5 },
  "models": [{ "model": "mlx-community/Llama-3.2-3B-Instruct-4bit", "loaded": true }]
}
```

For an engine with no admin API (Ollama, LM Studio), the endpoint answers
`{"engine": "ollama", "supported": false}` — a uniform shape, so the consumer
does not branch on engine type. A failure to reach the engine is
`{"supported": false, "error": "..."}` with the error text sanitised, never a
crash and never a hang (bounded timeout).

**A2. The node-scanner fetches it.**

The scanner already fetches each peer's `/v1/models` over the mTLS pool and its
own node's over loopback. It fetches the engine-native view the same way, on a
**slower cadence** than the models sweep (the models sweep is frequent; engine
totals move slowly).

The result is carried the way `ModelsByEngine` and `LoadedByEngine` already are:
as an additive field on `noderec.DirectoryNode` (the payload the scanner
notifies to the broker with), which the broker copies onto its own node record
and the desktop renders. That is the verified path node inventory already takes
— engine-manager em surface → scanner → `noderec` → broker → desktop — so the
engine-native view needs no new channel, and it stays out of the mDNS TXT
record, which is deliberately small.

**A3. The desktop shows it, labelled.**

A "engine self-report" section on the node card, visually and verbally separate
from PAIR's own numbers, with a caption that says what it is: the engine's own
counters, which include traffic PAIR did not route. Never merged into the same
totals, never summed with PAIR's numbers.

### 4.2 Slice B — live in-flight numbers

**B1. PAIR-derived live facts, from the proxy.**

The telemetry reader already sees each chunk as it arrives. It counts, exactly:
content chunks received, response bytes received, the time to the first byte, the
time to the first content chunk, and the elapsed time. These ride the existing
`proxy/request-started` / `proxy/request` event pair — the proxy's own
notification channel to its broker — as an additive `progress` payload on the
started event and a periodic update.

**B2. Engine-native live activity, and the job-id join.**

The engine's live rows are keyed by the **engine's** request id; PAIR's jobs are
keyed by PAIR's own request id. The join:

- The origin proxy already reads the upstream response, so it sees
  `X-oMLX-Request-ID`.
- For a job served on **another** node, the origin proxy forwards to that node's
  cluster ingress. It adds one header — `X-NVPAIR-Request-Id: <PAIR job id>` —
  to that forwarded request. The serving node's ingress handler reads it.
- The serving node's proxy already sees the response header from its local
  engine, so it holds `engine request id ↔ PAIR job id` locally, and polls the
  engine's `/admin/api/activity` (or reads it through the em surface) to get the
  rows. It publishes the rows **keyed by PAIR's job id**, so no client-side
  join is needed.

**B3. How the live frames reach a desktop on another node.**

The workload lifecycle is already cluster-wide because the workload-manager
broadcasts every frame to all pinned peers. Live frames ride the same path:

```
origin proxy (or serving proxy)  →  workload-manager  →  pinned peers
        →  each peer's broker  →  clients
```

The workload-manager treats a progress frame as **pass-through, not lifecycle**:
it is not a state transition, so it must not be deduped as a duplicate of the
`workload:started` event that shares its id.

**B4. Rate limiting, at the source.**

The proxy emits at most **one progress frame per job every two seconds**, and
only for jobs still running. A cluster with twenty running jobs therefore
carries ten frames per second — bounded, and far below the lifecycle traffic a
busy cluster already produces.

**B5. The job card.**

A running job shows a live line: elapsed, what has arrived (chunks, bytes), and
— for oMLX — the engine's own token count, rate, and prefill progress. A job
whose engine reports nothing shows elapsed and streamed bytes, with a line that
says the engine does not report live progress. When the job ends, the card
switches to v1's final, exact numbers.

### 4.3 What crosses the cluster

| Frame | Contents | Never included |
| --- | --- | --- |
| `workload:progress` | PAIR job id, origin, engine, run id, node id, elapsed, chunks, bytes, and (oMLX) the engine's token count, rate, prefill progress | prompt text, response text, model prompt content, the oMLX API key |
| engine-native node record | totals, rates, cache efficiency, loaded model names | the API key (not in PAIR's type), prompt or response content, engine-internal paths |

The engine-native record includes **model names**, which is the same class of
data the em surface already publishes for `/v1/models` and which the node record
already carries today. The API key is the one thing that must never cross, and
PAIR's type makes that structural rather than a matter of remembering to strip
a field.

## 5. Wire contracts

**Engine-native node record** (additive to the node record the scanner
publishes):

```json
"engineNative": {
  "engine": "omlx",
  "supported": true,
  "totals": { "...": 0 },
  "rates": { "...": 0 },
  "models": [{ "model": "...", "loaded": true }],
  "observedAt": 1716998400000
}
```

A node that has **never produced** a view has none: its engine publishes no
admin API, or it could not be reached. A node whose **fetch fails** keeps the
view it had — the last-good rule the model list follows too — so a transient
engine outage never blanks a card. The stale `observedAt` is how a reader
sees it: it does not advance while the fetch fails, and the age of the stamp
is the signal that the view is no longer fresh.

**Progress frame** (a new workload-manager pass-through method):

```json
{
  "jobId": "17",
  "nodeId": "<the node serving the job>",
  "originatedFrom": "<the node the request entered the cluster on>",
  "engine": "omlx",
  "runId": "<the origin proxy's per-process run nonce>",
  "elapsedMs": 4200,
  "chunks": 34,
  "bytes": 1820,
  "ttfbMs": 260,
  "engineProgress": { "requestId": "...", "generatedTokens": 34, "tokensPerSecond": 62.1,
                      "prefillProgress": 0.62, "queuePosition": 0 }
}
```

`originatedFrom` is absent on a frame a proxy publishes — the proxy cannot
know it is the origin of a job it forwarded onward — and the origin's own
broker stamps it, exactly as it stamps `originatedFrom` onto a workload
lifecycle event. A frame that already names an origin is not re-stamped.

`engine` names the engine that serves the job and `runId` the origin proxy's
per-process run nonce. Both are part of the job's identity: every proxy
counts its own requests from 1, so an oMLX job "1" and an Ollama job "1" on
one node are two different jobs, and a proxy that restarts counts from 1
again. The broker keys a live view by `(originatedFrom, engine, jobId)` — the
serving node is deliberately not in the key — and a client looks the view up
by the same triple.

The `engineProgress` object is absent when the engine reports no live
progress. Every number in it is measured by the engine.

## 6. Testing

**Engine-manager**
- The endpoint returns PAIR's type, and the response contains **no API key** —
  a test asserts the serialized payload has no `api_key` field and no value
  matching the configured key.
- An engine with no admin API answers `supported: false`; an unreachable engine
  answers an error, not a hang and not a crash.
- The endpoint is refused for a non-loopback plaintext caller and for an unpinned
  TLS caller, exactly like `/v1/models`.

**Node-scanner**
- The peer fetch uses the existing mTLS pool and the pin gate; an unpinned peer
  yields no engine-native record rather than an error.

**Proxy**
- The live facts are exact: chunk and byte counts match the bytes the test
  engine sent; elapsed and ttfb are measured, not estimated.
- No token count and no rate appear in the PAIR-derived payload.

**Workload-manager / broker**
- A progress frame is broadcast to peers and relayed, and is **not** deduped
  against the lifecycle event that shares its job id.
- Rate limiting: twenty frames for one job in a second produce at most one
  emission per two-second window.
- The live view is keyed by `(originatedFrom, engine, jobId)`: an oMLX job "1"
  and an Ollama job "1" on one node keep separate views, and a frame that
  arrives after the job's terminal event does not resurrect a finished job.

**Desktop**
- A running oMLX job shows the engine's live token count and rate; a running
  Ollama job shows elapsed and streamed bytes and says the engine reports no
  live progress; neither shows an estimated token count.
- The engine-native section is labelled and separate from PAIR's own numbers.

## 7. Risks and unknowns

| # | Risk | Handling |
| --- | --- | --- |
| 1 | oMLX's admin API shape changes | PAIR owns the type and maps explicitly; an unmapped field is ignored, a missing field is absent. The endpoint reports `supported: false` rather than failing |
| 2 | The API key leaks through the engine's payload | Structurally impossible: PAIR's type has no such field, and a test asserts it |
| 3 | The scanner's sweep is too slow for live data | Engine-native *stats* are slow-moving and ride the existing sweep; *live* activity does not ride the scanner at all — it rides the workload broadcast |
| 4 | Live frames flood a busy cluster | Rate-limited at the source: one frame per job per two seconds |
| 5 | A peer's engine is older and has no engine-native view | The record is absent; the UI says "not reported" |
| 6 | The `X-NVPAIR-Request-Id` header is visible to the engine | It is a random per-request id, carries no content, and the engine already accepts an `X-Request-ID` for its own correlation |
| 7 | The proxy's chunk counter drifts from the engine's token count | They are different things and are labelled differently; neither is presented as the other |
| 8 | oMLX 0.7.2's streaming `usage` chunk reports no phase durations | The per-job measurement carries the token counts the engine does report and leaves the durations absent, so they read as "not reported" until a newer oMLX (or a non-streaming request) supplies them. Nothing is estimated in their place |

## 8. Out of scope

- Slices C (history and comparison) and D (Phoenix parity, TUI parity,
  persistence, LM Studio phase metrics).
- Any **estimate** of tokens or rates on the live card (§3).
- Using the engine-native view to change **scheduling** — it is display and
  diagnosis only, as in v1.
- Polling the engine's admin API from the desktop: the desktop never talks to
  an engine directly, and an engine's admin API is loopback-only by design.

## 9. Rulings

- **No estimates on the live card.** The PAIR-derived side reports only exact
  facts; the engine-native side reports the engine's own numbers. Chunk counts
  are not presented as tokens.
- **The engine-native surface rides the em surface the scanner already fetches**,
  rather than a new inter-node method. The cluster reach therefore costs a
  handler and a fetch, not a trust boundary.
- **The live frames ride the existing workload broadcast** as a pass-through
  method, rate-limited at the source, rather than a new cluster channel.
- **The engine-native numbers are never merged with PAIR's own.** They are a
  separate, labelled view, because they count traffic PAIR did not route.
- **The engine-native view carries no phase durations.** oMLX's stats endpoint
  has no duration fields — its speeds (`avg_prefill_tps`, `avg_generation_tps`)
  are what it reports, and those are what the view carries. The durations live
  in oMLX's *usage* endpoint, whose totals are scoped to a date range
  ("today" by default) rather than to the engine's life; mixing a day-scoped
  number into a session-scoped view would be a trap. The durations arrive with
  the trends work (slice C), where the hourly buckets are the natural source
  and the scope is explicit.
