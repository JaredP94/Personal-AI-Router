<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-manual-nodes

A Go service for managing manually configured nodes on networks where mDNS discovery is unavailable. Accepts node addresses via JSON-RPC, probes each for Ollama, LM Studio, and node-info, and emits status events.

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
| `--cluster-dir <path>` | _(none)_ | Cluster config dir; when set, a TLS manual node's node-info is probed over cluster mTLS, presenting this node's leaf and accepting any currently-pinned server cert |
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
    "hostUuid":"stable-node-uuid"
  }
}
```

Each node is probed for inference engines: Ollama on its default `:11434` (`GET /` + `/api/tags`), LM Studio on its default `:1234` (`GET /v1/models`), and oMLX on its default `:1236` (OpenAI-compatible `/v1/models` or loopback-only 403 proxy identification). Nodes running `nvpair-engine-manager` on `:14322` are also probed for authoritative multi-engine inventory. `lmstudio_*` and `omlx_*` mirror the `ollama_*` fields and let a supervising broker bridge the node into `lmstudio-proxy` and `omlx-proxy` the same way it bridges Ollama into `ollama-proxy`. A node can run any engine combination.

### `node/updated`

Emitted when a periodic 10-second probe detects a topology change (service going up/down, models,
or GPU inventory changed). Topology updates keep engine inventory and topology in sync; scheduling telemetry
is delivered separately via `node/telemetry`.

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

A hostname is preferred over an IP literal: probe clients disable keep-alives specifically so every probe re-resolves the name, which lets a node that gets a new address recover on its own. An IP-literal entry is dead once the device is renumbered. Supply the address on its own — a `host:port` string is not parsed, because ports are appended to it, so such an entry reads permanently down.

```json
{"jsonrpc":"2.0","id":1,"method":"node/add","params":{"address":"10.0.1.50","name":"my-server"}}
```

| Param | Required | Description |
|---|---|---|
| `address` | Yes | IP address or hostname of the node, with no port |
| `name` | No | Friendly name (used as node ID; defaults to `manual:<address>`) |
| `tls_port` | No | Probe node-info over HTTPS on this port instead of plain HTTP on `14318`. Echoed back as `tls_enabled` |
| `mtls` | No | Stored and echoed back as `mtls_required`. The probe transport itself is chosen by `tls_port` and live cluster membership, so this field records intent rather than driving it |

Response: the initial node status object.

### `node/remove`

Remove a previously added manual node.

```json
{"jsonrpc":"2.0","id":2,"method":"node/remove","params":{"id":"my-server"}}
```

### `nodes/list`

Returns all currently tracked manual nodes with their latest probe status.

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
- **Node Info** on port 14318, or `tls_port` over HTTPS: hardware inventory and identity (`GET /v1/node-info`)

A node can have any combination of these, or none if the target is unreachable. Status changes trigger `node/updated` events. Because change detection compares CPU, memory, and GPU values, a node running node-info emits a `node/updated` on most probe cycles as utilization moves.

The three engine ports are compiled in: only the node-info leg's port can be moved, via `tls_port`. A remote engine on a non-default port is not discovered.

## Shutdown

The manager shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
