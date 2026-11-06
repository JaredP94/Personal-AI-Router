<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-manual-nodes

A Go service for probing explicitly configured PAIR machines or bare engines on networks where mDNS is unavailable. The broker owns persistence; this worker owns live probe state. See [spec.md](spec.md) for the normative trust, intent and replay contract.

Node-info JSON decoding reads at most 1 MiB, and model-list decoding at most
16 MiB. A JSON value that extends past its limit follows the existing probe or
model-parse failure path.

Cluster-derived entries follow cluster-manager's authenticated reached address
when a member roams. User-saved PAIR/engine entries retain their original
address, name and intent; the learned roster never overwrites them. Each new
endpoint still needs a pinned-mTLS inventory proof before it becomes routable.

## Worker control health

An id-bearing `worker/health` request (no params) returns `{"ok":true}` with
that same id, inline on the stdin dispatch loop. Notifications receive no
response. This checks control-loop responsiveness, not engine, HTTP listener,
or domain readiness; it does not wait for engine operations.

## Communication

Uses bidirectional newline-delimited JSON-RPC 2.0. By default, communication is over stdin/stdout. An alternative IPC transport (Unix domain socket or Windows named pipe) can be specified with `--ipc <path>`.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--client-cert <path>` | _(none)_ | PEM client certificate to present when probing TLS-enabled manual nodes (requires `--client-key`) |
| `--client-key <path>` | _(none)_ | PEM client private key matching `--client-cert` |
| `--ca-bundle <path>` | _(none)_ | PEM bundle of CAs to trust when verifying server certificates (additive to the system trust store) |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir for pinned mTLS peer inventory. A manual peer is discovered through a pinned certificate, then re-read against that exact principal before publishing its proof |
| `--log-level <level>` | `info` | `error` / `warn` / `info` / `debug`; falls back to `$NVPAIR_LOG_LEVEL`. Also changeable at runtime via the `log/set-level` method |
| `--version` | | Print version and exit |

## JSON-RPC Events (notifications, manager → caller)

### `ready`

Emitted once on startup.

```json
{"jsonrpc":"2.0","method":"ready","params":{"version":"0.1.0"}}
```

### `node/discovered`

Emitted when a manually added node has been probed and its initial status determined.

```json
{
  "jsonrpc":"2.0",
  "method":"node/discovered",
  "params":{
    "id":"manual:10.0.1.50",
    "address":"10.0.1.50",
    "ollama_up":true,
    "ollama_port":11434,
    "ollama_models":["llama3.2:latest","gemma3:4b"],
    "lmstudio_up":true,
    "lmstudio_port":1234,
    "lmstudio_models":["qwen2.5-7b-instruct"],
    "omlx_up":true,
    "omlx_port":1236,
    "omlx_models":["mlx-community/Qwen2.5-7B-Instruct-4bit"],
    "node_info_up":true,
    "node_info_port":14318,
    "gpus":[{"name":"NVIDIA GeForce RTX 3080","utilization_percent":37}],
    "telemetryValid":true,
    "msSince":120,
    "hostUuid":"stable-node-uuid",
    "engineNative":{
      "engine":"omlx",
      "supported":true,
      "totals":{"requests":2,"promptTokens":123,"completionTokens":599},
      "rates":{"prefillTokensPerSecond":161.1,"generationTokensPerSecond":69.1},
      "models":[{"model":"a-model","loaded":true}],
      "observedAt":1792663511584
    }
  }
}
```

PAIR peers must first prove identity through pinned cluster mTLS node-info on `:14318` (or the configured `tls_port`). Matching certificate, response and broker-owned `peerMetadata` identities are required. Only authenticated metadata supplies service ports; models and engine state are then read over pinned mTLS with no HTTP or operator-CA downgrade. Completed reads recheck the exact current pin and admission-bound `peerRecordId`.

Explicitly user-added bare engines retain independent manual routes: Ollama on `:11434`, LM Studio on `:1234`, and oMLX on `:1236`. They remain labelled `identityVerification: "manual"`, never acquire peer trust, and provide no hardware scheduling telemetry. Each entry stores `kind: "pair"` or `kind: "engine"`. PAIR entries that cannot authenticate are `unverified` previews keyed `preview:<sha>`, never engine bypasses, including after worker/broker restarts or when no PAIR endpoint answers. Only an explicit `engine` choice creates a `manual:<hash>` bypass; it reads no PAIR identity, hardware, inventory or telemetry claims. The broker atomically persists kind in `configs/manual-nodes.json` and replays entries into replacement workers. Historical entries without kind are explicitly migrated on disk to `pair`: a former bare engine must be removed and re-added as **Engine only** to resume direct routing. Unknown kinds fail closed.

`engineNative` is the remote engine's own report of itself — the counters, the rates and the active models the node's engine-manager publishes at `GET /v1/engine-stats`. It is read over the same transport as the model list beside it, because it is the same kind of data: a peer's engine counters are cluster data, so a clustered peer is read over cluster mTLS pinned to the principal the node advertises, and there is no weaker fallback for a peer. The field is absent when the node publishes no view — an engine with no admin API, an engine that could not be reached, or a node this one cannot read. A node whose view could not be re-read keeps the view it had (the last-good rule the node-scanner follows for a discovered node). It is read on a slower cadence than the probe (once a minute), because the engine's counters move slowly and every reading is a fresh observation.

### `node/updated`

Emitted when a periodic 10-second probe detects a topology change (service going up/down, models,
or GPU inventory changed). Topology updates keep engine inventory and topology in sync; scheduling telemetry
is delivered separately via `node/telemetry`. The engine's own view (`engineNative`) counts as a change
when its numbers move or when the reading is newer, so a node's view is re-announced as often as a
discovered node's is.

### `node/telemetry`

Emitted on a per-node cadence (~2 s ± 250 ms jitter) for reachable manual nodes to deliver compact scheduling telemetry to the supervising broker.

```json
{
  "jsonrpc":"2.0",
  "method":"node/telemetry",
  "params":{
    "id":"manual:10.0.1.50",
    "hostUuid":"stable-node-uuid",
    "gpuUtilizationPct":37,
    "telemetryValid":true,
    "msSince":120,
    "sampleSeq":42
  }
}
```

Unreachable nodes emit no telemetry notification. Unlike `node/updated`, this notification is internal to scheduler feeding and does not mutate discovery state or emit topology changes.

### `node/removed`

Emitted when a node is explicitly removed via `node/remove`.

### `errors:report` / `errors:clear`

Emitted so the supervising broker can forward them into the `nvpair-errors` pipeline. A node whose probes fail `probeFailThreshold` consecutive times (3 probes, so roughly 30 s of unreachability) reports under the id `manual-nodes:probe-failed:<node-id>`; a subsequent successful probe clears the same id.

## JSON-RPC Methods (caller → manager)

`worker/health` (no params) returns `{"ok":true}` for an id-bearing request;
see [Worker control health](#worker-control-health) for its limited meaning.

### `node/add`

Add a node by address. The manager immediately probes it and emits a `node/discovered` event.

A hostname is preferred over an IP literal: fresh dials re-resolve the name, allowing recovery after renumbering. Direct engine probes disable keep-alives; authenticated clients may reuse a live pinned connection. An IP-literal entry cannot follow renumbering. The address is stored as entered. Supply it without a scheme or port; malformed addresses and `host:port` receive `-32602` with an actionable message. IPv6 literals are accepted without brackets.

```json
{"jsonrpc":"2.0","id":1,"method":"node/add","params":{"address":"10.0.1.50","name":"my-server","kind":"pair"}}
```

| Param | Required | Description |
|---|---|---|
| `address` | Yes | IP address or hostname of the node, with no scheme or port; preserved as entered |
| `kind` | Choose explicitly | `pair` or `engine`; omission conservatively means `pair`, never an engine bypass |
| `name` | No | Friendly name (used as node ID; defaults to `manual:<address>`) |
| `tls_port` | No | Probe pinned cluster mTLS node-info on this port instead of the primary `14318`. Echoed back as `tls_enabled`; no plaintext downgrade |
| `mtls` | No | Stored and echoed back as `mtls_required`. The probe transport itself is chosen by `tls_port` and live cluster membership, so this field records intent rather than driving it |

Response: the initial node status object.

### `node/remove`

Remove a previously added manual node.

```json
{"jsonrpc":"2.0","id":2,"method":"node/remove","params":{"id":"my-server"}}
```

### `nodes/list`

Returns `{ nodes: ManualNodeStatus[] }` with each entry's `kind`, address, name, ID, `identityVerification` and probe status. Automatically tracked cluster addresses carry `from_cluster: true`; manual-entry management surfaces omit those rows. Removing a cluster peer via the broker also forgets its associated persisted PAIR addresses only after successful unpair.

```json
{"jsonrpc":"2.0","id":3,"method":"nodes/list"}
```

### `shutdown`

Gracefully shuts down the manager.

```json
{"jsonrpc":"2.0","id":4,"method":"shutdown"}
```

### `log/set-level`

Changes the log level at runtime. Accepted as either a request (answered with `{"level":"<resolved>"}`) or a fire-and-forget notification.

```json
{"jsonrpc":"2.0","id":5,"method":"log/set-level","params":{"level":"debug"}}
```

### `telemetry/set-scheduling-demand`

Broker → manager, as a notification: whether there is queued or running work
anywhere, local or on a peer. It sets the `X-NVPAIR-Poll` intent of every
outbound `/v1/node-info` probe — both the two-second telemetry poll and the
ten-second health probe — to `scheduling` while true, and `background` while
false, which renews no lease and lets the peer keep its ten-second idle cadence.
Probe cadence is unchanged either way. A freshly spawned manager starts inactive
until the broker pushes, so a respawn never pins a peer to a faster cadence on its
own.

```json
{"jsonrpc":"2.0","method":"telemetry/set-scheduling-demand","params":{"active":true}}
```

## Probing

Each manual node is probed every 10 seconds, with a 3-second timeout per leg, for:

- **Ollama** on port 11434: health check (`GET /`) and model list (`GET /api/tags`)
- **LM Studio** on port 1234: `GET /v1/models`, which doubles as the liveness check and the model list
- **Node Info** on port 14318, or `tls_port`: pinned cluster mTLS hardware inventory, matching identity and authenticated service metadata (`GET /v1/node-info`). Public responses cannot establish identity or scheduling measurements.

PAIR entries use only pinned cluster mTLS node-info and authenticated engine-manager inventory. Engine entries probe Ollama, LM Studio and oMLX (`:1236`) directly and never read node-info. A node can have any combination of eligible engines, or none if unreachable. Status changes trigger `node/updated` events. Because change detection compares CPU, memory, and GPU values, a node running node-info emits a `node/updated` on most probe cycles as utilization moves.

Bare-engine ports are fixed (`11434`, `1234`, `1236`). PAIR engine ports come only from authenticated broker-owned service metadata; `tls_port` can override its node-info port. Unverified peers cannot supply ports or model inventory.

## Shutdown

The manager shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
