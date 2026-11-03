<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-scanner

**The consolidated discovery daemon.** Promoted from a plain node-info browser: it is now the single place this host advertises itself on and discovers the LAN through. Every other service registers its port with this daemon instead of advertising its own mDNS service, and consumers query/subscribe to it instead of browsing themselves.

Concretely, the daemon:

- **Advertises** this node's one `_nvpair-node._tcp` record, built from a registry of the local services its parent (the broker) registers — `ni`/`ol`/`lm`/`er`/`wl`/`cl`/`em`/`ec` ports plus the node identity (`uuid=`, `cluster-uuid=` when clustered, `ip=`). One record per node covers every service.
- **Browses** `_nvpair-node._tcp` for candidate endpoints. Verified/local records are keyed by authenticated host UUID; unverified previews have independent `preview:` record keys and an explicit `identityVerification: "unverified"` marker. A claimed UUID cannot overwrite or remove a verified record.
- **Authenticates before enrichment:** remote hardware and broker-owned service metadata come from `ni` over pinned cluster mTLS, with matching certificate/response/metadata identities and an exact-current-pin check after reading. Only authenticated metadata supplies service ports. Models, per-engine attribution and loaded state then come from the authenticated `em` port over pinned mTLS. Local inventory remains an owned loopback read. Public HTTP hardware is display-only and never supplies service models or scheduler telemetry.
  Node-info JSON decoding reads at most 1 MiB, and model inventory decoding reads
  at most 16 MiB. A JSON value that extends past its limit fails enrichment and
  leaves the last-good cache intact.
- **Carries the engine's own view** of every node: the engine-native counters and rates the node's `em` (engine-manager) port publishes at `GET /v1/engine-stats`, read over the same transport as the model list (cluster mTLS for a pinned peer, loopback in plaintext for this node's own engine-manager) and folded onto the node's directory entry as `engineNative`. It runs on its own, slower cadence (a minute) than the model sweep, because the engine's counters move slowly, and it reuses the same per-peer client pool, so it costs no extra connections to a peer. Every sweep is a new observation, so the view's `observedAt` stamp advances even when the engine's counters do not move, and a client can compute staleness as `now - observedAt`. A node with no engine-native view is a normal state, not a failure: a peer this node holds no pin for, an engine with no admin API, and an unreachable engine all leave the field absent. A node that has never produced a view has none; a node whose fetch fails keeps the view it had (the last-good rule the model list follows too), and a view that is already stored survives the mDNS browse path replacing the record.
- **Samples scheduling telemetry** from healthy nodes every two seconds with
  stable per-node jitter. Consecutive remote failures back off to 4, 8, 16, then
  30 seconds and reset on success; local loopback sampling stays at the healthy
  cadence. It emits only host UUID, maximum GPU utilization, validity, and
  source-reported sample age; it does not mutate the public directory for each
  telemetry sample.
- **Grants trust only after proof:** `trusted` requires a completed pinned mTLS inventory read. `peerRecordId` binds the proof to an active, non-retired local admission, local certificate and exact peer pin record. Retained pins after admission withdrawal cannot authorize fresh or cached inventory, even over an existing connection. Both local and peer certificates must be within their validity windows when accepting or rechecking proofs; expiry also disqualifies cached inventory and telemetry. A pin by itself never promotes a claim. Trust changes withdraw the verified record using its original token rather than clearing the token and turning it into a preview under the host key.
- **Advertises live membership:** the same reload re-derives this node's own `cluster-uuid=` from the cluster dir and re-advertises when it differs from what is being published, so joining or leaving a cluster takes effect without a restart.
- **Public-preview anti-flap:** a candidate has to be absent from every scan for the browser's full miss threshold (twelve scans at 5s, a full minute) before it is evicted, and from ~15s in it is TCP-probed on its advertised service ports on *every* scan — any answer resets the counter and recovers it outright. So neither a momentary multicast gap nor a sustained one drops a healthy node, and eviction rests on a run of ~10 consecutive failures rather than one dial on the deadline. The window is sized deliberately: a node saturated by its own inference load starves its control plane, so it stops answering mDNS *and* stops accepting probe connections for as long as the load runs, while still serving requests. Two signals can also vouch for a node without any probe — a node-info enrichment in the last 10s, and inference response bytes from it in the last 60s (`discovery:node-activity`, relayed by the broker from whichever proxy saw the bytes). The second is the only liveness evidence that gets stronger the busier a node is.
- **A scan that loses everything is treated as our own fault:** when more than one node is known and a scan returns *none* of them, no node is penalized for it — for up to six consecutive scans (~30s), after which they count normally. Independent machines do not leave the network in the same five-second window, but a process too starved to drain its multicast socket within `scanTimeout` hears silence from all of them at once; without this, one saturated machine dropped its entire cluster, idle peers included, in a single reconcile. The suppression needs two or more known nodes to engage: losing a lone peer looks the same whether it left or we stopped listening, so that case falls through to the ordinary threshold.
- **Converges without mDNS:** every 15s, pending candidates retry authentication and verified peers refresh their authenticated inventory before model refresh. Candidate removal affects only its preview. A verified peer expires after 45s without authenticated inventory unless recent inference activity proves it is busy; authorization changes withdraw it immediately. Older peers lacking authenticated service metadata remain untrusted previews until upgraded, with no plaintext downgrade.

## Discovery transport lifetime

Periodic browsing reuses IPv4/IPv6 receive sockets and source-bound UDP 5353
senders between scans. Interface metadata comes from `netmon` snapshots, refreshed
on interface-change notifications rather than re-enumerated on every five-second
scan. The responder reuses per-interface senders for announcements and replies;
removed or re-addressed interfaces retire their cached senders, including when
all eligible interfaces disappear. Shutdown sends the goodbye before closing them.

Every scan still performs real per-interface writes, preserving the consecutive
send-failure evidence that address selection consumes. Source binding preserves
Windows' multicast-send workaround, and source-bound sockets read unicast traffic
so replies/questions are not lost. Fresh scan windows, not a lifetime-wide DNS
cache, drive the existing discovery and eviction rules. This reduces socket churn;
it does not establish or reproduce the cause of reported macOS kernel panics.

## Worker control health

An id-bearing `worker/health` request (no params) returns `{"ok":true}` with
that same id, inline on the stdin dispatch loop. Notifications receive no
response. This checks control-loop responsiveness, not engine, HTTP listener,
or domain readiness; it does not wait for engine operations.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0. Stdio by default; `--ipc <path>` switches to a Unix domain socket or Windows named pipe.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`). Gates four things: a completed pinned-mTLS inventory proof grants `trusted`; this node advertises `cluster-uuid=` while it is actually a member; a peer's `em` model list is fetched over cluster mTLS; and the directory above it is the base this node's identity is resolved from |
| `--ca-bundle <path>` | _(none)_ | Optional CA bundle for display-only previews; never substitutes for pinned cluster authentication |
| `--client-cert <path>` | _(none)_ | PEM client certificate to present when fetching `/v1/node-info` from TLS-enabled nodes (mTLS; requires `--client-key`) |
| `--client-key <path>` | _(none)_ | PEM client private key matching `--client-cert` |
| `--log-level <level>` | `info` | `error` / `warn` / `info` / `debug` |
| `--version` | | Print version and exit |

## JSON-RPC surface

### Requests (caller → daemon)

| Method | Params | Result |
|---|---|---|
| `worker/health` | — | `{"ok":true}` — control-loop responsiveness only |
| `discovery:register` | `{service, port, txt?}` | `{ok:true}` — record a local service so it's folded into this node's `_nvpair-node` record |
| `discovery:unregister` | `{service}` | `{ok:true}` — drop a local service |
| `discovery:update-txt` | `{service, port, txt?}` | `{ok:true}` — same as register with a new TXT |
| `discovery:get-nodes` | `{service?}` | `{nodes:[DirectoryNode]}` — the directory, optionally filtered to nodes advertising one service |
| `discovery:reload-identity` | — | `{ok:true}` — re-resolve and re-advertise this node's identity |
| `discovery:reload-trust` | — | `{ok:true}` — withdraw proofs whose current authorization changed; pending candidates authenticate on the next sweep |
| `discovery:set-observed-addresses` | `{addresses}` | `{ok:true}` — the local addresses peers have actually reached this node on, so address selection can rank a peer-proven address above an inferred one |
| `shutdown` | — | `null` |

`log/set-level` is also accepted.

Two notifications are accepted rather than requests, because the daemon has
nothing to answer and the caller must not wait:

- `discovery:node-activity` `{hostUuid, msSince}` — a peer's engine returned
  inference response bytes `msSince` ms ago. Counts as proof of life and cancels
  that node's eviction. The broker sends this while inference is streaming, so an
  id-bearing call would cost it a pending entry and a blocked goroutine per
  report in exchange for an ack nobody reads.
- `telemetry/set-scheduling-demand` `{active}` — whether the broker has queued or
  running work anywhere, local or on a peer. It sets the `X-NVPAIR-Poll` intent of
  every outbound `/v1/node-info` GET: `scheduling` while true (the read renews the
  target's five-second scheduling lease), `background` while false (the read renews
  nothing, so the target keeps its ten-second idle cadence). The poll cadence stays
  two seconds either way. A freshly spawned daemon starts inactive, so an
  unrenewed lease — the safe direction — is the default until the broker pushes;
  on a cold start the next poll renews the lease, and until then the scheduler
  treats a sample of up to ten seconds as it treats any other: neutral pressure
  above the freshness budget, never a younger age than it has.
- `log/set-level` (above).

### Notifications (daemon → caller)

Emitted as the directory changes. Each carries the affected `DirectoryNode`, including `peerRecordId`, `identityVerification` (`local`, `verified`, or `unverified`) and `trusted`. Verified/local records use their authenticated host UUID as the key; previews use `peerRecordId` instead. Only verified/local service metadata is eligible for service consumers. Removed verified records retain their original proof token.

- `discovery:node-discovered` — a new node entered the directory
- `discovery:node-updated` — a known node's record or enrichment changed. A re-observation that changes nothing emits no event, so `lastSeen` is a last-*change* stamp rather than a liveness heartbeat
- `discovery:node-removed` — a node aged out (identity + last-known fields)
- `discovery:node-telemetry` — compact maximum-GPU utilization, validity, and
  sample age for broker-internal scheduling

A one-shot `ready` notification is sent on startup carrying the daemon version. The broker replays its service registrations on every scanner spawn, so a restarted daemon is repopulated without needing to compare an epoch.

### The engine-native field

The directory entry carries the engine's own report of itself as
`engineNative`, enriched from the node's `em` port
(`GET /v1/engine-stats`):

| Field | Notes |
| --- | --- |
| `engine` | the engine that published the view (`omlx`) |
| `supported` | whether the engine publishes an admin API at all |
| `totals` | the engine's cumulative counters: `requests`, `promptTokens`, `completionTokens`, `cachedTokens`, `cacheEfficiency` |
| `rates` | the engine's own average throughput: `prefillTokensPerSecond`, `generationTokensPerSecond` |
| `models` | the models the engine lists as active, each with `loaded` |
| `error` | a short sanitised reason, when a known engine could not be reached |
| `observedAt` | when the reading was taken, in milliseconds since the epoch |

The view is omitted entirely when the node never produced one: its engine
publishes no admin API, or it could not be reached. Every numeric field
inside it is `omitempty` — a value the engine did not report stays absent
rather than becoming a zero, and for a counter a reported zero and an absent
value mean the same thing to a reader.

`observedAt` advances on every sweep even when the counters do not move, so a
consumer computes staleness as `now - observedAt`: that is what tells a live
but idle engine apart from one that stopped answering. A node whose fetch
fails keeps the view it had (the last-good rule the model list follows too).

The counters are the engine's account of **all** the work it did, including
the traffic PAIR did not route, so a consumer presents them beside PAIR's own
numbers and never adds the two together. The view carries no phase
durations: the engine's stats endpoint reports none, and the durations on the
engine's usage endpoint are scoped to a date range rather than to the
engine's life.

## Shutdown

The daemon shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
