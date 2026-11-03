<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Manual nodes: normative service specification

Adapted from NVIDIA/Personal-AI-Router@b3b25ae (NVIDIA/Personal-AI-Router#128)
for the fork's pinned-mTLS inventory contract. This specification describes the
current fork, not the upstream transport or persistence architecture.

## Purpose and ownership

`nvpair-manual-nodes` is the broker-supervised, client-only prober for addresses
mDNS cannot reach. It opens no HTTP listener. It owns only live configuration,
probe results and per-node telemetry loops. The broker is the sole durable owner
of `configs/manual-nodes.json` under the PAIR user-data root. Electron and TUI
are clients, not persistence owners.

The broker already contained the persistence/replay work corresponding to
NVIDIA/Personal-AI-Router@aed93b6 (NVIDIA/Personal-AI-Router#60). Do not add a
second store or replay path.

## Entry contract

`node/add` accepts `{ address, kind?, name?, tls_port?, mtls? }`:

- `address`: a bare IP literal or hostname. Hostnames, including case and a final
  dot, are retained as entered and resolved when dialing. Schemes, paths,
  whitespace and `host:port` are rejected with `-32602`; unbracketed IPv6 is valid.
- `kind`: `pair` (PAIR machine) or `engine` (explicit bare-engine bypass). Missing
  historical intent means `pair`; unknown values are rejected. Endpoint responses
  never choose or change kind. Re-adding with a different kind is a user action.
- `name`: optional configuration alias; the worker ID is name when supplied,
  otherwise `manual:<address>`. This configuration ID is not a directory identity.
- `tls_port`: optional pinned-mTLS node-info port; zero uses `14318`.
- `mtls`: informational configuration metadata; never permits weaker peer reads.

`node/add` returns initial `ManualNodeStatus` immediately and probes asynchronously.
Replacement/removal and all node-status/telemetry publications share the worker's
state lock. A probe cannot publish after its tracked entry has been replaced or
removed. Replacement publishes an empty initial status immediately, withdrawing
the old route before the new network probe completes.

Status includes `kind`, `id`, `address`, optional name and configuration metadata,
`identityVerification`, availability, model inventory, and authenticated proof and
hardware fields when available. Initial PAIR status is `unverified`; initial engine
status is `manual`. `node/discovered` follows the first probe; `node/updated` follows
changed state. Removed/replaced configurations cannot publish late probe results.

`nodes/list` returns `{ nodes: ManualNodeStatus[] }`. The worker also follows
cluster member addresses; those statuses carry `from_cluster: true` and kind
`pair`. Manual-entry management surfaces exclude these automatically tracked rows.
`node/remove` accepts `{ id }`, returns `{ removed }`, cancels telemetry and emits
`node/removed` for an existing entry. Removing only an address does not unpair.

## Trust and routing invariants

### PAIR machine

1. Discover a currently pinned principal over cluster mTLS at the configured
   address, then re-read node-info pinned to that exact principal.
2. Certificate principal, node-info identity, and broker-owned `peerMetadata`
   identity must agree. Authenticated node-info must confirm its service port.
3. Only authenticated service metadata supplies engine-manager and proxy ports.
   Models, engine state and optional engine-native counters are read over pinned
   mTLS. Completed reads recheck the exact current pin and admission-bound
   `peerRecordId` before publication.
4. The broker independently checks that proof before admitting service inventory.
   A verified peer uses its proven UUID and the manual address in the relay
   directory. Proxies use that address with pinned mTLS, not plaintext engine
   probes. DNS names are dialed as names and may resolve again on subsequent dials.
5. Without proof, the broker projects only an unverified `preview:<sha>` record;
   no routing, scheduling inventory, identity or telemetry authority is granted.
   Failure, absence of node-info, an older peer, a worker/broker restart, or a new
   machine answering at the same address cannot turn this into an engine entry.
   There is no opt-in unclustered PAIR route. The operator must PIN-pair instead.

### Engine only

Only `kind: "engine"` enables the direct engine probes: Ollama `:11434`, LM Studio
`:1234`, oMLX `:1236`. The broker gives these `manual:<hash>` directory keys and
`identityVerification: "manual"`. They never read or trust PAIR node-info,
identity, hardware, telemetry, service metadata or engine-manager claims, even
if a PAIR endpoint exists at the same address. Their models are engine-local
inventory, not peer inventory. A PAIR machine's engine is reachable this way only
if its owner enabled LAN engine binding. Engine-only entries remain independent
of cluster membership/removal.

## Persistence, migration and removal

The canonical file is an array of `{ id, address, name?, kind, tls_port?, mtls?,
peer_uuid? }`. Writes use a private temporary file and rename. Successful broker
add/remove operations update this file; replacement workers receive `node/add`
replays. The worker itself writes no configuration file.

On reading a historical entry without kind, the broker explicitly rewrites kind
as `pair` before replay. There is no safe way to infer historical engine consent
from address, optional TLS flags or a network response. Thus former bare engines
stop direct routing until the user removes/re-adds them as **Engine only**. Unknown
stored kinds or failed migration writes prevent replay rather than guessing.

`peer_uuid` records a cleanup-only association from authenticated pairing
completion (or a later pinned inventory proof). Cluster-manager emits internal
`cluster:pairing-completed` with the original dial hostname and authenticated peer
UUID after committing the pin/member, before publishing paired state. The broker
associates matching manual PAIR aliases durably, even though the roster records
an observed numeric source address and no node-info proof exists yet. This field
is not an identity, inventory or transport proof; clients cannot supply it as an
authenticated association. After successful `nodes:remove`, the broker forgets
all associated manual PAIR aliases and their live projections. Failed unpair keeps
them. A cleanup failure is returned to the client, not silently swallowed. This
adapts the behavior of NVIDIA/Personal-AI-Router@692950c
(NVIDIA/Personal-AI-Router#172) at the shared broker boundary for both clients.

## Cadence, failure and process controls

Topology probes run every 10 seconds with 3-second per-request deadlines. Pinned
telemetry refresh runs at roughly 2 seconds with bounded per-node jitter and a
1.5-second deadline. Engine-native counters refresh at a slower 60-second cadence.
Clients disable keep-alives for direct probes to permit fresh DNS resolution.
Three consecutive unreachable topology probes emit one `errors:report`; recovery
or removal clears it. Probe failures never delete the durable configuration.

Node-info reads are bounded to 1 MiB, models to 16 MiB, and engine-native stats to
1 MiB. A malformed/oversized answer does not establish inventory. `worker/health`
is answered inline on the control loop and measures control responsiveness only.
`telemetry/set-scheduling-demand` controls poll intent, not probe cadence. `shutdown`,
stdin EOF or termination cancels work. `log/set-level` changes verbosity. Prompt,
completion, credential and key material must never enter logs.

See [README.md](README.md) for CLI flags and examples; the broker README defines
relay envelopes and supervision. Unit tests cover intent replay without a PAIR
endpoint, explicit engines ignoring PAIR claims, hostname mTLS resolution,
conservative migration and successful-only unpair cleanup. Cross-process tests
exercise real-worker replacement in CI; local unit evidence is not deployment or
packaged-artifact certification.
