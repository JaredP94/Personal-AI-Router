<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-workload-manager

## Worker control health

An id-bearing `worker/health` request (no params) returns `{"ok":true}` with
that same id, inline on the stdin dispatch loop. Notifications receive no
response. This checks control-loop responsiveness, not engine, HTTP listener,
or domain readiness; it does not wait for engine operations.

## Purpose

Propagates inference-workload lifecycle events across the cluster, so every node
has the same view of what work is queued, running, or finished — and on which
node. That shared view is what makes the Jobs list meaningful on any member and
what supplies the pending-work side of `nvpair-job-scheduler`'s GPU-aware
ranking.

The manager is a **relay and deduplicator**, not the source of truth. The broker
and proxies decide a workload's state; this component passes `workloadInfo`
through opaquely and forwards it.

## Communication

Two channels:

- **Local** — bidirectional newline-delimited JSON-RPC 2.0 with the supervising
  `nvpair-ui-broker`. Stdio by default; `--ipc <path>` switches to a Unix domain
  socket or Windows named pipe.
- **Inter-node** — a cluster-mTLS server on port `14320`
  (`POST /v1/workloads/events`) that accepts the same JSON-RPC frames from pinned
  peers, plus an outbound broadcaster that pushes local events to those peers with
  bounded retry.

Peers are discovered through the broker's node records rather than by this
component browsing the network itself.

## CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port <n>` | `14320` | Inter-node port to listen on and advertise |
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt` / `node.key` + `trusted/`) supplying the identity and pins the inter-node mTLS channel requires. Without it the node has no cluster identity and exchanges no inter-node traffic |
| `--log-level <level>` | `info` | One of `error` / `warn` / `info` / `debug` |
| `--version` | | Print version and exit |

## Transport security

The inter-node interface is **always cluster mTLS**. There is no plaintext
personality on it. The listener presents this node's cluster certificate,
requires a client certificate, and refuses any caller that is not a pinned
cluster peer with `403`. Pins are re-read per request, so removing a member takes
effect immediately without a restart.

A node that is not a current cluster member has no identity to present, so it
neither serves nor broadcasts inter-node workload traffic at all. The port stays
bound: the certificate is resolved per handshake, so a node converges to serving
after it joins without a rebind or a restart.

Workload state is therefore only ever exchanged between paired members. See the
repository [`SECURITY.md`](../../SECURITY.md) for the surrounding trust
boundaries.

## Local control requests

| Method | Params | Result |
| --- | --- | --- |
| `worker/health` | — | `{"ok":true}` — control-loop responsiveness only |

See [Worker control health](#worker-control-health). Health is a local stdio
request, not an inter-node lifecycle event.

## Lifecycle events

Inbound lifecycle notifications, on either channel:

| Method | Resulting state |
| --- | --- |
| `workload:submitted` | `queued` |
| `workload:started` | `running` |
| `workload:completed` | `completed` |
| `workload:errored` | `failed` |

Each carries `params.workloadInfo`. Removal uses `workloads:remove` with
`params.workloadId` and the origin `params.originatedFrom`.

Alongside them the manager relays **progress frames** (`workload:progress`),
which are a live snapshot of a running request rather than a state
transition. See [Live progress frames](#live-progress-frames).

## Workload shape

Defined in [`workload.go`](workload.go):

| Field | Notes |
| --- | --- |
| `id` | Stable workload identifier |
| `model`, `engine` | What was requested and by which engine |
| `runId` | Optional grouping key |
| `state` | `initializing`, `queued`, `running`, `completed`, or `failed` |
| `originatedFrom` | Node the request entered the cluster on |
| `scheduledOn` | Node it was routed to; absent until a target is chosen |
| `createdAt`, `startedAt`, `completedAt` | Epoch milliseconds; the last two are nullable |
| `error` | Normalized failure text, nullable |
| `requesterId` | Optional client attribution, nullable |
| `prefillTokens`, `decodeTokens`, `cachedTokens` | Optional token counts the engine reported for this request, nullable |
| `prefillMs`, `decodeMs`, `ttftMs`, `totalMs` | Optional phase durations in milliseconds, nullable |

Optional and nullable fields use pointers so a peer's payload round-trips without
inventing zero values.

Prompts, messages, and response bodies are **not** part of this contract and must
never be added to it.

### The inference measurement

The token counts and phase durations above are the engine's own measurement of
the request, taken by the proxy that forwarded it. They are the only inference
performance data the workload contract carries, and they are metadata: counts
and durations, never content.

The manager treats them as opaque payload. It broadcasts every lifecycle frame
to the pinned peers unchanged, so a measurement taken on one node reaches every
other node in the cluster, where each broker attributes it to the node that
served the request. A member running an older workload-manager drops the
measurement from remote jobs, so the cluster-wide view of the numbers is only
as complete as its newest members.

A value the engine did not report stays absent, and stays absent through the
broadcast: the manager never fills in a zero and never rejects a lifecycle
event over a missing measurement.

## Live progress frames

A running request can be described while it runs. The proxies publish a
**progress frame** for an in-flight request under the method `workload:progress`:
what PAIR measured of the client's stream — the elapsed time, the content
chunks received, the response bytes received, the time to the first byte.

A frame is a **snapshot, not a state transition**. It never enters the
catalog, it is never tracked as an active workload, and it is never deduped —
not against the `workload:started` event that shares its job id, and not
against the frame before it. Deduping would freeze the live view at its first
reading, because each frame carries the newer numbers. Two frames for the
same job id are two readings, not one event seen twice.

The manager validates a frame (only the job id is required — every other field
is optional, because a frame reports what its publisher measured and stays
silent about the rest) and broadcasts it to the pinned peers unchanged.

The same job id can carry frames from two different publishers: the origin
proxy's frame, which holds what PAIR measured, and the serving node's frame,
which holds what the engine reported of its own request. Neither publisher can
see the other's facts, so a field a frame does not carry means "not measured
here", never "zero". Each broker merges the frames of one job field by field.

| Field | Notes |
| --- | --- |
| `jobId` | PAIR's own id for the request; the same id the lifecycle events carry |
| `originatedFrom` | the node the request entered the cluster on, stamped by the origin's broker |
| `nodeId` | the node that is serving the request |
| `elapsedMs` | the time since the request reached the proxy |
| `chunks` | the content chunks received so far — a chunk count is never a token count |
| `bytes` | the response body bytes received so far |
| `ttfbMs` | the time to the first response byte, absent until a byte has arrived |
| `engine` | the engine's own live reading, absent when the engine reported nothing |

## Output to the broker

Translated remote events are forwarded to the broker as:

| Notification | Params |
| --- | --- |
| `workloads:upsert` | `{ workloadInfo }` |
| `workloads:remove` | `{ workloadId, originatedFrom }` |
| `workloads:progress` | the progress frame, relayed from a peer unchanged — a snapshot of a running request, never a catalog event |
| `ready` | `{ version }` — startup handshake |

Duplicate events arriving from more than one peer are collapsed before they reach
the broker, so a workload observed over several paths is reported once. A
progress frame is never collapsed: it is a reading, and the later one carries
the newer numbers.

## Testing

```bash
go test ./...
```

## See also

- [`../nvpair-ui-broker/README.md`](../nvpair-ui-broker/README.md) — the
  supervisor and relay
- [`../nvpair-job-scheduler/README.md`](../nvpair-job-scheduler/README.md) — the
  primary consumer of workload counts
- [`../VERSIONING.md`](../VERSIONING.md) — SemVer bump rules
