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

| Loop | Location | Cadence | Stated reason | Owner |
|---|---|---|---|---|
| Node-info HTTP poll | `desktop/src/electron/service-bridge/node-info-poller.ts` | 2 000 ms visible / 30 000 ms idle, **one connect per remote node per tick** | hardware telemetry; "the single sanctioned HTTP exception" | **spec B** |
| Remote engine status | `modular-supervisor.ts:1050` | 20 000 ms | "poll clustered peers' authoritative engine status" | **spec B** (needs an edge-triggered push first) |
| Pending-invite sweep | `modular-supervisor.ts:971` | **1 500 ms** | its own comment: cluster-manager already pushes `invite-canceled` / `invite-expired`; "this poll is the backstop that drops a resolved … invite **when a push is missed** (e.g. across a broker restart)" | **this spec** |
| Tray visibility retry | `desktop/src/electron/tray.ts:359` | 5 000 ms | **not a missed-push problem** — see correction | retained |

Three of these are compensation for the same defect: **a missed push is
undetectable.** `Codec.Notify` is fire-and-forget with no sequencing, so a
client that drops a frame — or reconnects to a restarted broker — cannot tell
"nothing happened" from "something happened and I did not hear it."

**Correction (found at planning).** Three, not four. The tray timer is a
*watchdog*, not a missed-push backstop: `setupVisibilityCheck` recreates the
status item after macOS has silently dropped it, and `NSStatusItem` offers no
notification for being removed — there is nothing to observe, so no protocol
primitive can replace it. The earlier claim that it could become "a one-shot
observer / KVO" was wrong. v2 deletes three timers and explicitly retains one.

## 2. Design

**Final acceptance reconciliation.** The envelope is notification-only; there is
no ready gate, sequence-gap recovery, replay, or payload restructuring. Existing
upstream deltas remain deltas. Snapshot-per-push below is the original rationale,
not a claim that this change converts every event family to full snapshots.
Every Go stdio receive path uses canonical codec validation and terminal owned
transport teardown. Desktop TS is an explicit exception: observe-only inbound
stamps, with adopted peer epoch, not strict rejection. Both shipped frontends
enforce a resettable 45s silence deadline and lifecycle-owned bounded reconnect;
fresh connections reinstall subscriptions and perform authoritative resync.

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

**Envelope vs payload.** §2.1.1 constrains `params` semantics, not the envelope.
`epoch`/`seq` are stamped on *every* notification in both directions (§2.2),
including the worker→broker deltas — stamping is mechanical and changes no
payload. What must not change upstream is the delta *shape* of `params`.

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

**Mechanism (decided at planning).** All 13 binaries construct their codec
through `jsonrpc.NewCodec` / `NewCodecMaxFrame` (18 sites, verified), and the
broker holds one `Codec` per client connection — `Broker` is documented "a new
Broker is constructed for every client connection" (`broker.go:150`). So:

- `epoch` is a **package-level** value in `shared/jsonrpc`, minted once by
  `sync.Once` per process.
- `seq` is a **per-`Codec`** `atomic.Uint64` — which, given the above, is
  already exactly per-connection.
- `Codec.Notify` stamps both.

This means **zero changes at the ~35 broker and ~160 worker `.Notify(` call
sites**: stamping is inherited by construction. A client only ever sees the
broker's epoch, because the broker re-stamps everything it forwards downstream.

### 2.3 Version enforcement (clean cut)

`encoding/json` ignores unknown members, so a v1 client silently parses a
protocol v2 frame. Enforcement therefore lives on the **receive** path, in
`Codec.Read()`:

- A frame with `Method != ""` and no `ID` (a notification) **must** carry both
  `epoch` and `seq`. Absent either → `*ProtocolMismatchError`, and the read loop treats
  it as **terminal**: close the connection, log `protocol mismatch: notification
  without epoch/seq`.
- A frame carrying `epoch`/`seq` on a *response* or *request* is likewise
  terminal — it means the peer misunderstands the envelope.
- The first frame from any peer must be `ready`, and `ready` must carry
  `"protocol": 2` in its params. `ReadyParams` gains that field.

  **Superseded during planning: there is no `ready` gate, and `ReadyParams` does
  not gain `protocol`.** The stamp itself is the discriminator, and it applies to
  *every* notification — a v1 peer is caught on its first unstamped notification,
  which is the same moment `ready` would have caught it. Gating on `ready` adds
  a new failure mode (a notification racing ahead of `ready` at startup, and a
  new "is this the first frame?" state machine on every connection) to buy back
  nothing. §2.3 reduces to one rule:

  > Every notification carries `epoch` and `seq`. Every reader treats their
  > absence as a terminal protocol mismatch.

  This makes v2 purely envelope stamping plus validation, with no handshake
  reordering anywhere.

This yields deterministic bidirectional failure with no negotiation branch:
a v1 broker never sends notification stamps, so a v2 Go worker refuses it; a
v1 caller's first unstamped notification is likewise terminal at the broker.
Unstamped requests/responses remain valid; no ready handshake is required. Per AGENTS.md — one
canonical path, no compatibility branch.

### 2.4 Client behaviour

Per connection the client owns observed/hydrated epoch generations and an inbound
silence deadline. Sequence values do not gate application or recovery.

| Observation | Action |
|---|---|
| Same-epoch notification, regardless of `seq` | **Apply.** Gaps and lower/repeated sequence values trigger no recovery. |
| `epoch != held epoch` | Broker restarted. **Resync all.** Discard cached facts. |
| Heartbeat silence > `HEARTBEAT_TIMEOUT` | Connection presumed dead; reconnect → new connection → new `seq` run → resync all. |

**There is no gap-recovery path, and that is the point.** A gap means
notifications were dropped, but under §2.1 the frame that reveals the gap
*is itself a full snapshot*, applied at send time under the sender's lock — so
state is correct the moment it is applied and a gap cannot cause drift. v2
therefore does not branch on `seq` at all for state purposes.

`seq` retains the sender's per-connection monotonic wire guarantee. Receivers
do not branch on gaps or regressions, including event channels. Silence is
measured by elapsed time since inbound traffic, not sequence comparisons.

### 2.5 Resync

New broker method, replacing the four timers:

```
broker:resync  →  { epoch: string, protocol: 2 }
```

**Simplified during planning: `channels: { name: revision }` and its content-hash
`revision` are dropped (§7.2 resolved).** After §2.1, resync fires *only* on an
epoch change, and §2.3's `ready` gate is gone — so the client's cache is empty or
wholly suspect at every resync point. A per-channel content hash can therefore
never skip anything it would not have had to re-fetch anyway: it is machinery
whose discriminating case cannot occur. The client re-invokes every snapshot getter
it consumes. That is a handful of local IPC round-trips, once, at connect.

Snapshot getters, all existing:

| Channel | Snapshot getter (exists today) |
|---|---|
| discovery / nodes | `discovery:get-nodes`, `nodes:get-initial` |
| engines | `engines:get-initial` |
| cluster | `cluster:get-initial` |
| errors | `errors:get-initial` |
| workloads | `workloads:get-initial` |
| routing metrics | `metrics:get-routing` — produced by the proxies since spec A0 |
| telemetry | `telemetry:get-status` |
| **node hardware telemetry** | **NONE — added by spec B** |
| **proxy request counters** | **NONE — added by spec B** |

`broker:resync` is the only new method. It is called on epoch change and connect,
with failed hydration retried on later same-epoch traffic — never on sequence
gaps or a snapshot-polling timer.

### 2.6 Heartbeat

The broker sends `heartbeat` on every live client connection every
`HEARTBEAT_INTERVAL` (**15 s**). It carries `epoch` and `seq` like any
notification, with an empty params object (no node-set snapshot). The
client **never** sends watchdog or resync RPCs on a timer.

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

The server talks and clients listen. Each connection owns a resettable one-shot
silence deadline, canceled on teardown and guarded against stale callbacks, not a
client ticker. Expiry wakes pending requests and marks disconnected once. TUI
main gracefully joins its old Supervisor before one fresh Spawn/UI lifetime per
invocation; desktop uses guarded owned stop/start, allowing one automatic retry
until a fresh lifetime becomes ready. New lifetimes reinstall subscriptions and
hydrate snapshots; no watchdog-generated healthy traffic is introduced.

### 2.7 Event channels

`proxy/request` and `schedule:priority` are intrinsically instantaneous. They
still carry `epoch`/`seq`. A gap on an event channel means **events were lost**,
and they cannot be recovered.

**Amended by spec A0:** `proxy/request` is no longer purely instantaneous. It now
carries cumulative `requests` / `cache_affinity_hits` (§6.2), so a gap on it is
*self-healing* — the next event restates the authoritative totals — and no counter
snapshot getter is needed for it. `ready` is a startup announcement, not a
handshake gate. No event channel triggers sequence-gap recovery.

## 3. Changes by component

| File / module | Change |
|---|---|
| `shared/jsonrpc/jsonrpc.go` | `Epoch`/`Seq` on `Message`; process-global `epoch`; per-`Codec` `seq`; stamp in `Notify`; terminal validation in `Read` |
| `shared/jsonrpc` heartbeat consts | `HeartbeatInterval` = 15 s, `HeartbeatTimeout` = 3× |
| `nvpair-ui-broker/broker.go` | `broker:resync`; heartbeat ticker per client connection |
| All 13 binaries | **No call-site changes** — stamping is inherited via `NewCodec` (§2.2) |
| `nvpair-tui/rpc/client.go` | track `epoch`/`lastSeq`; resync on epoch change |
| `desktop/src/electron/service-bridge/*` | track `epoch`; delete the 4 timers (**transitional** — spec C replaces this layer) |
| `desktop/scripts/verify-service-contracts.ts` | allowlist `broker:resync` + `heartbeat` or `service-contracts:check` fails |
| `services/versions.json` | **breaking**: bump all 13 components + `installer` + `product` |
| `docs/services-api.md` | regenerate via `npm run service-contracts:write` (never by hand) |

## 4. Testing

- **Codec** — notification without `epoch`/`seq` → terminal `ProtocolMismatchError`;
  `epoch`/`seq` on a response → terminal; monotonic stamping under concurrent
  writes.
- **Relay** — two subscribers on one connection get contiguous `seq`; a
  reconnect yields `seq` restarting at 1 and a fresh `epoch` only if the broker
  restarted.
- **Broker** — kill and restart the broker: client observes a new `epoch`,
  calls `broker:resync`, and converges with **no snapshot polling timer**. Assert
  `broker:resync` call count is 0 while the connection is healthy.
- **Cross-process** (`services/tests`, real binaries) — v1 worker against v2
  broker must be refused at `ready`, deterministically, not silently.
- **Regression guards for the four deleted timers** — a pending invite that is
  resolved with its push suppressed must still clear — via the next snapshot
  push or an epoch resync — without a 1 500 ms sweep.
- Cadence assertion: no `time.Ticker` remains in the broker notification path
  except `heartbeat`; no desktop bridge `setInterval` remains except the
  retained tray watchdog and the two timers owned by spec B.

## 5. Explicitly out of scope

- Replay buffers, durable sequence numbers, per-channel epochs, message
  retention — all unnecessary under §2.1, and all rejected deliberately.
- Telemetry collection moving into Go, GPU hysteresis, poll cadence → **spec B**.- The native app itself → **spec C**. `launchd`, Sparkle → **spec D**.
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
these greps. Resolve during spec B. **v2 does not depend on this classification:**
§2.3 stamps and validates every notification uniformly, so an unclassified channel
still gets a correct envelope. This is now the only open item in §8.

## 7. Open questions

1. **`HEARTBEAT_INTERVAL` 15 s** — acceptable, or does a tray badge need tighter
   liveness?
2. **`revision` as content hash vs monotonic counter.** **RESOLVED — neither.**
   Dropped entirely: resync only fires where the cache is already empty or wholly
   suspect, so per-channel revision cannot skip anything. See §2.5.
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
