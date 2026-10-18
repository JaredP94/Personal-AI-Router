<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Security

This repository is the [JaredP94 fork](https://github.com/JaredP94/Personal-AI-Router)
of NVIDIA Personal AI Router (PAIR). Upstream NVIDIA security policies do not
imply NVIDIA support or security certification for fork-specific changes.

**Do not disclose vulnerabilities, exploits, credentials, or private data through
public issues or pull requests.**

## Report a Vulnerability

### This fork

GitHub private vulnerability reporting is enabled for this fork (verified
2026-10-18 via the repository API). To report a vulnerability affecting this fork,
use the repository's **Security → Report a vulnerability**.

### Upstream NVIDIA products

For vulnerabilities affecting upstream PAIR or another NVIDIA product:

- Submit the [NVIDIA Vulnerability Disclosure Program form](https://www.nvidia.com/en-us/security/report-vulnerability/).
- Email [psirt@nvidia.com](mailto:psirt@nvidia.com). For encrypted email, use
  the [NVIDIA public PGP key](https://www.nvidia.com/en-us/security/pgp-key).

NVIDIA PSIRT triages upstream NVIDIA product reports, not necessarily fork-only
changes. See [NVIDIA Product Security](https://www.nvidia.com/en-us/security/)
for its policies and disclosure guidance.

In a private report, include the affected version/commit, vulnerability class,
reproduction steps, expected impact, and a minimal synthetic proof of concept.
Remove unrelated personal information, prompts, model data, and secrets.

## Security Architecture Context

- Electron's renderer reaches main through a typed preload bridge. Electron
  starts only `nvpair-ui-broker`; the terminal client starts its own broker.
- The broker supervises **12 workers**: scanner, node-info, three engine proxies,
  engine manager, cluster manager, workload manager, errors, node settings,
  manual nodes, and job scheduler. Scanner startup is required; others are
  optional. Workers run under the user's operating-system account.
- Local control uses newline-delimited JSON-RPC 2.0. Relay protocol v2 stamps
  notifications with `epoch` and `seq` (`protocol: 2` is in broker epoch/resync
  payloads); heartbeat/recovery metadata
  is a liveness contract, not authentication or encryption.
- Discovery uses mDNS. Node metadata is available on `:14318`. Proxy defaults
  are Ollama `:11434`, LM Studio `:1234`, and oMLX `:1236`; configured ports may
  differ. Each proxy accepts loopback HTTP clients and pinned-peer mTLS traffic
  on the same port.
- Cluster manager `:14321` accepts pairing/bootstrap traffic and uses a six-digit
  EAP-NOOB PIN to establish certificate trust. Paired channels include errors
  `:14319`, workloads `:14320`, engine inventory `:14322`, remote engine control
  `:14323`, and promoted proxy ingress.
- Proxy traces export to a local loopback OTLP collector (`127.0.0.1:<collectorPort>`,
  default `4317`). Telemetry export is strictly loopback-only and is not protected by
  cluster mTLS. The managed Phoenix container publishes UI/OTLP HTTP `127.0.0.1:6006`
  and OTLP gRPC `127.0.0.1:<collectorPort>`.

The Go services own identity, cryptography, routing, and inter-node transport;
clients relay commands and render reported state.

## Boundaries and Risks

### Local APIs and Proxy Ingress

A proxy's substantive plaintext HTTP requests must originate on loopback;
non-loopback plaintext requests are rejected with `403`. CORS preflight can
return `204` before that check. The TLS personality requires a pinned peer
certificate and forwards directly to the local engine without routing onward.

Loopback is not user authentication. Other local processes can submit requests.
CORS defaults are permissive when no origin allowlist is configured; CORS is not
an authorization boundary. Review browser access before allowing web content to
reach these endpoints. Broker stdio and optional Unix-socket/named-pipe access
rely on parent-process and operating-system permissions, not per-message tokens.

For oMLX, the proxy and engine manager read `OMLX_API_KEY` or
`~/.omlx/settings.json` and inject the key for local engine calls when required.
These readers neither provision the key nor enforce permissions on that file.
Protect engine settings yourself; the engine manifest `omlx.json` is not a
credential store.

### LAN Metadata and Overlay Addresses

In the **broker-managed deployment**, `/v1/node-info` serves unauthenticated
plaintext hardware/utilization metadata even after pairing. Standalone node-info
with `--cluster-dir` can instead use membership-gated mTLS; do not assume that
mode is active in the desktop bundle.

PAIR can advertise/rank Tailscale CGNAT addresses (`100.64.0.0/10`) and use known
peer address fallbacks. This is not arbitrary tailnet peer enumeration, and
Tailscale reachability does not grant PAIR trust. Promoted cluster channels
still require pinned certificates. Pairing/bootstrap and discovery metadata
have different boundaries from authenticated inference/control traffic.

An untrusted LAN, shared Wi-Fi, hostile local process, or compromised router is
potentially adversarial. Firewall rules and network segmentation remain the
operator's responsibility. Do not forward PAIR ports through a public router or
put an unauthenticated public reverse proxy in front of them.

### Pairing Grants Substantial Peer Authority

The six-digit PIN is a low-entropy bootstrap convenience, not a durable credential
or proof of physical presence. Pair only when both devices and the network are
trusted; reject unexpected requests and remove unauthorized members.

Trust is transitive through Ed25519 endorsements. A compromised member can
endorse certificates that peers will pin. Any currently pinned peer can pass the
remote engine-control gate, including engine installation/start/stop and model
pull/load/unload/delete operations, without additional per-operation approval
in those handlers. Proxy mTLS ingress is not restricted to an inference-route
allowlist: it forwards engine API paths. Pairing is therefore not read-only or
inference-only access.

Identities use self-signed Ed25519 leaf certificates; generated private keys are
written with mode `0600`. TLS authentication uses exact certificate-DER pins,
not conventional CA, hostname, or certificate-expiry validation. The nominal
certificate lifetime does not automatically expire trust. Protect the PAIR data
directory and revoke/remove compromised peers.

### Cluster mTLS Does Not Cover Every Channel

mTLS protects participating channels only when the relevant worker is configured
with identity and trust material. It does not automatically protect loopback
HTTP, discovery, broker-managed node-info HTTP, OTLP export, Electron IPC,
third-party engine APIs, or traffic outside PAIR.

Engine installation executes downloaded vendor software; checksum verification
depends on the engine manifest and is not universally required. Catalogs,
updates, model downloads, Docker image pulls, engines, and applications can
contact external services. “Local-first” is not a guarantee that data never
leaves the machine or LAN.

### Prefix Affinity Is Not Prompt Storage

The proxies keep a bounded in-memory prefix-affinity table of SHA-256 digests and
node identity (10,000 entries, five-minute idle TTL). It is a routing hint, not
proof that an engine still holds a KV cache. Raw prompt/completion content is not
stored in that table. Low-entropy input digests remain susceptible to guessing;
do not log or gossip them.

## OpenTelemetry and Phoenix: Boundaries and Privacy Controls

Tracing and payload capture are governed by per-node settings with strict privacy boundaries:

- **Broker-Managed Configuration and Dynamic Reconciliation**: Proxies do not read
  `PAIR_TELEMETRY_*` or `OTEL_*` startup environment variables. Proxies start with
  tracing disabled and are configured dynamically by the broker via `telemetry/configure`.
  The broker reconciles desired settings into running proxies, preserves the last loaded
  configuration across settings-worker restarts, and reports applied state and export
  delivery health. Standalone proxies without a broker do not trace.
- **Explicit Informed Consent for Payload Capture**: Content capture is disabled by
  default and cannot be enabled without explicit consent recorded at `payload_consent_version: 1`.
  Migration from earlier settings resets tracing and capture to off and clears consent.
- **Local-Only Boundary and Trace Context Stripping**: Content capture is local-only.
  Serving nodes record nothing for peer-originated requests, and incoming W3C trace context
  headers (`traceparent`, `tracestate`) are stripped before forwarding to local or remote
  backends so no distributed trace context crosses node boundaries.
- **Strict Attribute and Payload Truncation**: When payload capture is disabled, only
  model/node metadata, timing, HTTP route/status, and allowlisted headers (`Content-Type`
  and `Content-Length`) are recorded. Other request headers are omitted entirely. When
  payload capture is explicitly consented and enabled, prompt and completion strings are
  truncated to 16 KB UTF-8 (`pair.gen_ai.prompt.truncated`).
- **Loopback-Only Exporter Transport**: The OTLP exporter dialer is pinned strictly to
  `127.0.0.1:<collectorPort>`, preventing spans from being directed to external or
  remote endpoints.
- **Managed, Pinned Phoenix Deployment**: Phoenix is managed by the broker from an
  embedded Compose template (`project nvpair-telemetry`, container `nvpair-phoenix`),
  pinned to release tag `version-20.23.0` and manifest digest
  `sha256:c542e7de1dc1fdd0f938de337d44ebefa7266e91391eed6e7978523d7f6e1f2f`. Both
  published ports (`6006` and `<collectorPort>`) bind exclusively to `127.0.0.1`.
- **Trace Storage and Scoped Purge**: Traces are stored in Docker volume
  `nvpair-phoenix-data`. Stopping the container retains trace data. The broker provides
  on-device storage inspection and `telemetry/purge-storage` (requiring
  `{"confirm": "delete-traces"}`) to safely remove volumes labeled
  `com.docker.compose.volume=nvpair-phoenix-data` without force-removing containers
  or pruning other Docker data.
- **Never Log Secrets**: Standard system log sinks (`slog`, `applog`, stderr, file logs)
  never contain prompts, completions, PINs, credentials, or key material.

## macOS Distribution and Privileged Helper

This fork's published `0.7.0-rc.1` candidate is unsigned and unnotarized, macOS
arm64 only. The privileged-helper flow requires NVIDIA's Developer ID policy
(Team ID `6KR3T733EC`); unsigned/ad-hoc fork builds do not satisfy it. Installing
through `scripts/install-mac.sh`, checking `SHA256SUMS`, or avoiding browser
quarantine does not provide code signing, notarization, helper registration, or
automatic firewall setup. Do not bypass helper trust checks or disable
system-wide macOS protections to make the candidate work.

## Security-Sensitive Changes

Review and test changes to preload/IPC exposure; JSON-RPC framing and process
ownership; discovery and identity; pairing, endorsements and revocation; proxy
listeners, CORS and routing; remote engine/model operations; trace configuration,
attributes and payload gating; logs and exports; downloads, installers, updates
and privileged helpers. Use synthetic fixtures, not real identities, keys,
private prompts, or proprietary datasets.
