<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Relay Protocol v2 — push-primary with epoch-scoped resync

**Spec A of 4.** Native macOS re-implementation programme:

| Spec | Subject | Depends on |
|---|---|---|
| **A0** | Routing metrics: move counters from Electron into the proxies (correctness fix, wire-compatible, ships first) | — |
| **A (this)** | Wire protocol: envelope, epoch, resync, heartbeat | — |
| B | Telemetry push: retire the 2 s per-node HTTP poll, edge-triggered engine status | A |
| C | Native macOS app (SwiftUI) | A, B |
| D | Lifecycle: `launchd` supervision, Sparkle, Electron retirement | A |

## 1. Problem

The transport already pushes. `nvpair-ui-broker` fans out 69 consumed JSON-RPC
notifications across four subscription channels (`discovery:subscribe`,
`engine:subscribe`, `workloads:subscribe`, `proxy:subscribe`), and the desktop
renderer receives them on a `service-bridge:push` bus.

Four polling loops nonetheless still run *on top* of that push system:

| Loop | Location | Cadence | Stated reason |
|---|---|---|---|
| Node-info HTTP poll | `desktop/src/electron/service-bridge/node-info-poller.ts` | 2 000 ms visible / 30 000 ms idle, **one connect per remote node per tick** | hardware telemetry; "the single sanctioned HTTP exception" |
| Remote engine status | `modular-supervisor.ts:1050` | 20 000 ms | "poll clustered peers' authoritative engine status" |
| Pending-invite sweep | `modular-supervisor.ts:971` | **1 500 ms** | its own comment: cluster-manager already pushes `invite-canceled` / `invite-expired`; "this poll is the backstop that drops a resolved … invite **when a push is missed** (e.g. across a broker restart)" |
| Tray visibility retry | `desktop/src/electron/tray.ts:359` | 5 000 ms | retry until the status item exists |

Every one of these is compensation for the same defect: **a missed push is
undetectable.** `Codec.Notify` is fire-and-forget with no sequencing, so a
client that drops a frame — or reconnects to a restarted broker — cannot tell
"nothing happened" from "something happened and I did not hear it." The four
timers are four hand-rolled answers to one missing primitive.

## 2. Design

### 2.1 The load-bearing decision: snapshot-per-push

`relay.Directory` already states the invariant, for one channel only:

> `Subscriber` … "Consumers replace their set from it rather than applying
> deltas, so a dropped or reordered push can't leave them drifted — every push
> is the authoritative current list." (`relay/relay.go:88`)

Protocol v2 makes that **the** rule, broker-wide.

This is the decision that keeps v2 small. If pushes carry full state, then a
missed push is *self-healing* — the next push corrects it — and v2 needs no
replay buffer, no durable sequence numbers, and no per-channel catch-up logic.
Sequence numbers then serve one narrow purpose: telling the client **how long
since it last heard anything useful**, so a silently stalled channel can be
prodded, and so transport death is distinguishable from quietness.

The cost is payload size on wide channels. It is accepted deliberately: these
are LAN clusters of a handful of nodes, and `Directory.Apply` already
byte-compares projected state (`directoryNodeEqual`) so only real changes fan
out.

### 2.1.1 The rule applies downstream only

The audit (§6) shows the wire is already split into two halves with *different*
semantics, and both are correct:

| Half | Method family | Semantics | Evidence |
|---|---|---|---|
| **worker → broker** (upward) | `discovery:node-discovered`, `node/updated`, `node/removed`, `workloads:upsert/remove`, `errors:report/clear` | `DELTA` — one entity per frame | `nvpair-node-scanner/daemon.go:648`, `nvpair-workload-manager/manager.go:198` |
| **broker → client** (downward) | `discovery:nodes`, `errors:update` | `SNAPSHOT` — full set | `noderec.go:376` *"the broker relay -> subscriber push: the subscriber's full…"*; `broker/clustermanager.go:144`, `broker/proxy.go:186`, `nvpair-errors/manager.go:391` |

The broker is the delta→snapshot fold (`Directory.ApplySource`). **v2 does not
change the upstream half.** Deltas in, fold, snapshots out is the right shape
and is already built. §2.1 is an invariant on the client-facing surface only —
which is what deletes the four timers, since all four live on that surface.

### 2.2 Envelope

Two new top-level members on **notification frames only**. JSON-RPC 2.0
permits additional members; `Message` in `services/shared/jsonrpc/jsonrpc.go`
gains:

```go
type Message struct {
    JSONRPC string           `json:"jsonrpc"`
    ID      *json.RawMessage `json:"id,omitempty"`
    Method  string           `json:"method,omitempty"`
    Params  json.RawMessage  `json:"params,omitempty"`
    Result  json.RawMessage  `json:"result,omitempty"`
    Error   *RPCError        `json:"error,omitempty"`

    // Protocol v2: present on every notification, absent on requests and
    // responses. Their presence IS the version discriminator.
    Epoch *string `json:"epoch,omitempty"` // UUID v4, minted once per broker process
    Seq   *uint64 `json:"seq,omitempty"`  // monotonic per connection, starts at 1
}
```

- **`epoch`** — one UUID per broker process lifetime. Minted at broker start,
  never reused, never persisted. A new epoch means "every cached fact you hold
  about this broker is suspect."
- **`seq`** — a per-**connection** counter, not global and not durable. The
  broker assigns it when writing to a given client connection, starting at 1
  for that connection. Because a reconnect implies a fresh connection, and a
  fresh connection implies a fresh `seq` run, **reconnect automatically means
  resync** with no bookkeeping at all.

`epoch` and `seq` are stamped by the codec, not by callers. No worker computes
either; the broker stamps both on egress. Workers send notifications *upward*
to the broker, which re-derives and re-stamps for each downstream connection.

### 2.3 Version enforcement (clean cut)

`encoding/json` ignores unknown members, so a v1 client silently parses a
protocol v2 frame. Enforcement therefore lives on the **receive** path, in
`Codec.Read()`:

- A frame with `Method != ""` and no `ID` (a notification) **must** carry both
  `epoch` and `seq`. Absent either → `*DecodeError`, and the read loop treats
  it as **terminal**: close the connection, log `protocol mismatch: notification
  without epoch/seq`.
- A frame carrying `epoch`/`seq` on a *response* or *request* is likewise
  terminal — it means the peer misunderstands the envelope.
- The first frame from any peer must be `ready`, and `ready` must carry
  `"protocol": 2` in its params. `ReadyParams` gains that field.

This yields deterministic bidirectional failure with no negotiation branch:
a v1 broker never sends `epoch`, so a v2 worker refuses it; a v1 client never
sends `protocol: 2` in `ready`, so the broker refuses it. Per AGENTS.md — one
canonical path, no compatibility branch.

### 2.4 Client behaviour

Per connection the client holds `{ epoch, lastSeq }`.

| Observation | Action |
|---|---|
| `seq > lastSeq` | **Apply. Update `lastSeq`.** This is the only path that changes state, and it covers both contiguous delivery and gaps. |
| `epoch != held epoch` | Broker restarted. **Resync all.** Discard cached facts. |
| Heartbeat silence > `HEARTBEAT_TIMEOUT` | Connection presumed dead; reconnect → new connection → new `seq` run → resync all. |

**There is no gap-recovery path, and that is the point.** A gap means
notifications were dropped, but under §2.1 the frame that reveals the gap
*is itself a full snapshot*, applied at send time under the sender's lock — so
state is correct the moment it is applied and a gap cannot cause drift. v2
therefore does not branch on `seq` at all for state purposes.

`seq` survives for exactly two duties:

1. **Event-channel counters** (§2.7) — a gap there *is* lost data, because an
   event has no snapshot to fold into. Those channels resync their counters.
2. **Silence detection** — a channel that stays quiet while its neighbours skip
   ahead is a stalled producer, which is a bug worth logging even though state
   stays correct. `seq` is the only thing that makes that visible.

### 2.5 Resync

New broker method, replacing the four timers:

```
broker:resync  →  { epoch: string, protocol: 2, channels: { <name>: <revision> } }
```

`channels[name].revision` is a content hash of that channel's current snapshot.
The client compares each against the hash of the state it holds and re-invokes
the existing snapshot getter **only** for channels that differ:

| Channel | Snapshot getter (exists today) |
|---|---|
| discovery / nodes | `discovery:get-nodes`, `nodes:get-initial` |
| engines | `engines:get-initial` |
| cluster | `cluster:get-initial` |
| errors | `errors:get-initial` |
| workloads | `workloads:get-initial` |
| routing metrics | `metrics:get-routing` |
| routing metrics | `metrics:get-routing` — **no Go producer, see §6.2** |
| telemetry | `telemetry:get-status` |
| **node hardware telemetry** | **NONE — added by spec B** |
| **proxy request counters** | **NONE — added by spec B** |

`broker:resync` is the only new method. It is called on epoch change, on event
channel counter gaps (§2.7), and on connect — never on a timer.

### 2.6 Heartbeat

The broker sends `heartbeat` on every live client connection every
`HEARTBEAT_INTERVAL` (**15 s**). It carries `epoch` and `seq` like any
notification and, per §2.1, the full node-set snapshot for its filter. The
client **never** sends anything on a timer.

**Decided: 15 s, timeout at 3× interval (45 s to declare a connection dead).**
Rationale: after §2.1 the heartbeat is not load-bearing for correctness —
snapshots self-heal, so the heartbeat's only jobs are detecting a stalled
producer and keeping stateful middleboxes warm across the LAN. Transport death
is independently detectable from the failed `Read()`/`Done()` on the connection,
so the interval governs *cosmetic* staleness, not data loss, and 45 s is well
inside the range where the UI's existing disconnected-node affordance is the
honest display. A 5 s interval triples periodic wakeups on an always-on
tray-resident app to buy back nothing; 30 s makes a genuinely hung broker look
alive for over a minute. Both values are named constants in
`services/shared/jsonrpc` so tuning is a one-line change, not a hunt.

This is the asymmetry that pays for the battery goal: the server talks, the
client listens, and the client's only periodic work is a `DispatchSourceTimer`
that does nothing but compare two integers.

### 2.7 Event channels

`proxy/request`, `proxy/request-started`, `schedule:priority`, and `ready` are
intrinsically instantaneous — there is no state to snapshot. They still carry
`epoch`/`seq`. A gap on an event channel means **events were lost**, and they
cannot be recovered; so each event channel that feeds a *derived* value
(request counts, tokens/sec) declares a **counter snapshot** the client can
re-read. Which counters need to exist is enumerated by the notification audit
feeding spec B (§5).

## 3. Changes by component

| File / module | Change |
|---|---|
| `shared/jsonrpc/jsonrpc.go` | `Epoch`/`Seq` on `Message`; stamp on egress; terminal validation on `Read()` |
| `shared/jsonrpc/peer.go` | per-connection `seq` counter on the parent-side write path |
| `nvpair-ui-broker/relay/relay.go` | stamp on fan-out; expose `revision` per channel |
| `nvpair-ui-broker/broker.go` | mint `epoch`; `broker:resync`; heartbeat ticker; refuse `ready` without `protocol: 2` |
| `nvpair-ui-broker/supervisor.go` | stamp worker→broker notifications |
| All 11 workers | `ReadyParams.protocol` only. **No payload changes** — the upstream half stays `DELTA` (§2.1.1) |
| `desktop/src/electron/service-bridge/*` | delete 4 timers; add epoch tracking + resync (**transitional** — spec C replaces this layer) |
| `desktop/scripts/verify-service-contracts.ts` | understand `broker:resync` + `heartbeat` or `service-contracts:check` fails |
| `services/versions.json` | **breaking**: bump all 13 components + `installer` + `product` |
| `docs/services-api.md` | regenerate via `npm run service-contracts:write` (never by hand) |

## 4. Testing

- **Codec** — notification without `epoch`/`seq` → terminal `DecodeError`;
  `epoch`/`seq` on a response → terminal; monotonic stamping under concurrent
  writes.
- **Relay** — two subscribers on one connection get contiguous `seq`; a
  reconnect yields `seq` restarting at 1 and a fresh `epoch` only if the broker
  restarted.
- **Broker** — kill and restart the broker: client observes a new `epoch`,
  calls `broker:resync`, and converges with **no timer running**. Assert
  `broker:resync` call count is 0 while the connection is healthy.
- **Cross-process** (`services/tests`, real binaries) — v1 worker against v2
  broker must be refused at `ready`, deterministically, not silently.
- **Regression guards for the four deleted timers** — a pending invite that is
  resolved with its push suppressed must still clear — via the next snapshot
  push or an epoch resync — without a 1 500 ms sweep.
- Cadence assertion: no `time.Ticker` remains in the notification path except
  `heartbeat`.

## 5. Explicitly out of scope

- Replay buffers, durable sequence numbers, per-channel epochs, message
  retention — all unnecessary under §2.1, and all rejected deliberately.
- Telemetry collection moving into Go, GPU hysteresis, poll cadence → **spec B**.
- The native app itself → **spec C**. `launchd`, Sparkle → **spec D**.
- Windows/Linux build paths (retired by programme decision, not by this spec).

## 6. Audit findings (2026-10-04)

Inline audit of the ~160 `.Notify(` call sites after the subagent was lost to an
OAuth token expiry.

### 6.1 Snapshot channels already conform

`discovery:nodes`, `errors:update`, and the `proxy:*` / `workloads:*` relay
pushes already carry full sets (table in §2.1.1). The conversion work for
protocol v2 is therefore **stamping, not restructuring** — which is the single
largest cost reduction in this spec.

### 6.2 `metrics:*` has no Go producer — **RESOLVED by spec A0**

Historical finding (kept for provenance): `metrics:get-routing` resolved in
`desktop/src/electron/service-bridge/empty-handlers.ts:1090` to
`getModularBridgeState().getRoutingMetrics()`, and `metrics:routing-update` was
emitted at `modular-state.ts:2338`. Grepping `services/` returned nothing: there
was no Go implementation. The counters were accumulated in the Electron main
process, one increment per `proxy/request` notification where
`params.cache_affinity === true` — routing telemetry in the desktop tree, which
AGENTS.md forbids.

**Shipped as spec A0 (ships ahead of this spec, per §7.4):**

- `services/shared/routingmetrics` — the one filter (`Counts`) and the one
  accumulator (`Counters.Routed` / `Snapshot`), used by all three proxies so they
  cannot report different numbers.
- Each proxy stamps cumulative `requests` / `cache_affinity_hits` on
  `proxy/request` at its completion emit site only.
- The broker mirrors those totals off the event and reports them on
  `proxy:get-status` / `lmstudio-proxy:get-status` / `omlx-proxy:get-status`,
  still answered locally with no round-trip.
- Bridge and `nvpair-tui` **replace** totals from the event; both copies of the
  hand-rolled filter are deleted. `metrics:get-routing` keeps its existing
  contract, so the renderer is unchanged.

**Incidental bug fixed en route:** the old consumer filter counted unrouted
traffic. The 502 rejection emit sites (`ollama-proxy/proxy.go` ~1299,
`lmstudio-proxy/proxy.go` ~1088, `omlx-proxy/proxy.go` ~1097) post to inference
paths with `cache_affinity` omitted, which satisfied the old
`method === 'POST' && cache_affinity != null && path ∈ inference` guard. Failed
requests therefore inflated the dashboard's request counts and depressed its
affinity rate. Counting at the completion site excludes them structurally.

**Second bug caught by test, not review:** consumers that read absent totals as
`0` wiped real counts on every unrouted event. Both consumers now treat an absent
total as "preserve" and adopt only an explicit number — a genuine proxy restart
still reports `0` and resets correctly.

### 6.3 Still unclassified

`engines:state-changed` / `engines:progress*` producers were not located by
these greps (§4 tests must not rely on their classification). Resolve during
spec B.

## 7. Open questions

1. **`HEARTBEAT_INTERVAL` 15 s** — acceptable, or does a tray badge need tighter
   liveness?
2. **`revision` as content hash vs monotonic counter.** Content hash avoids a
   second persisted counter and survives broker restart honestly; a counter is
   cheaper to compute but lies after a restart. Spec assumes content hash.
3. **Gap policy granularity** — resolved: there is none, see §2.4.
4. **Where does routing-metrics aggregation live?** **RESOLVED — ship first.**
   Split out as **spec A0** (§6.2): proxies maintain `requests` /
   `cacheAffinityHits` per engine in Go and expose `metrics:get-routing` +
   `metrics:routing-update` as a real snapshot channel; the desktop-side
   accumulation is deleted. Sequenced ahead of spec A because it is a live
   AGENTS.md violation, is wire-compatible with both v1 and v2 (it adds a
   producer to channels that already exist in the desktop contract, and changes
   no envelope), and unblocks spec C's dashboard.

## 8. Blocked on

Notification payload classification is now **complete** (§6), except
`engines:*` (§6.3). Spec B must resolve §6.2 and §6.3.
