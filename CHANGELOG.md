<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

All notable changes to the Personal AI Router (PAIR) project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

**Upgrade notes:** **Breaking:** A PAIR peer counts for routing, identity, models,
telemetry and workloads only after proving itself over its pinned cluster
connection. A PAIR machine added by address that is not in your cluster shows
"Not in your cluster" with a **Pair** action and is not routed to. Upgrade every
machine: older peers appear unverified until upgraded. To use another machine's
engine without pairing, add the engine's own address as **Engine only**; on a
PAIR machine, its owner must also allow LAN engine binding with
`engine_allow_lan_bind`. Saved manual entries now record **PAIR machine** or
**Engine only** intent; entries saved before this release become **PAIR machine**,
never a routable bare engine without your explicit choice. JSON-RPC additions
for engine settings, model load profiles, download cancellation and LAN-bind
permission, plus peer-verification and progress fields, are additive; no method
was removed. Relay protocol stays v2.

### Security
- **Only proven peers supply cluster state (`nvpair-node-scanner`, `nvpair-manual-nodes`, `nvpair-ui-broker`)**: Discovery and public inventory are previews, not proof of a machine's identity; PAIR accepts a peer's services, models and telemetry only through its pinned cluster connection (adapted from NVIDIA/Personal-AI-Router#179). Removed members and expired certificates cannot keep cached routing or scheduling authority, and delayed replies from retired workers cannot restore it.
- **Pairing rejects out-of-order messages (`eap-noob`, `nvpair-cluster-manager`)**: A completion before the PIN was applied could reach Registered with an empty-key MAC, a key exchange before negotiation crashed either side, and the joiner accepted EAP-Success without a verified completion. A failed pairing message now also releases its pending session.
- **Trust-store paths are confined (`nvpair-cluster-manager`)**: A peer identifier such as `../identity` in an endorsed roster entry could write a pin outside `trusted/`. Unsafe identifiers are refused.
- **Bounded reads and fail-closed peers**: Node-info, model inventories, error-store ingest and remote progress frames are size-limited; a proxy whose peer pin has gone refuses the dial instead of falling back to an unpinned transport.

### Changed
- **Discovery reuses its sockets (`nvpair-node-scanner`, discovery responders)**: mDNS scans reuse sockets and refresh them on network changes rather than recreating them every scan. A three-minute synthetic test on one Mac measured 371 → 7 UDP socket opens, including startup and shutdown; this is not a long-duration reliability guarantee (NVIDIA/Personal-AI-Router#19, NVIDIA/Personal-AI-Router#59).

### Fixed
- **Stalled nodes no longer delay every request (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`)**: A node that stalls before headers moves behind eligible peers for one response-header timeout, then returns to normal on expiry or success. A dropped response can retry before its first body bytes without creating another visible workload; a committed stream is never retried (NVIDIA/Personal-AI-Router#6, NVIDIA/Personal-AI-Router#144).
- **Local port conflicts are reported (`services`)**: Another program listening on `127.0.0.1` is now detected when PAIR starts or changes a port. A separately running Ollama app on `127.0.0.1:11434`, for example, is reported as a conflict instead of silently receiving local traffic intended for PAIR.
- **Engine controls stay responsive (`nvpair-engine-manager`)**: Slow status probes no longer block start, stop or port changes, and an over-budget model inventory leaves peers with their last-good list rather than an empty one. Shutdown stops engines concurrently and cleans up detached daemons even when their start command fails or is cancelled (NVIDIA/Personal-AI-Router#84, NVIDIA/Personal-AI-Router#180, NVIDIA/Personal-AI-Router#13, NVIDIA/Personal-AI-Router#81).
- **Copied endpoints and download errors are clearer (`desktop`)**: LM Studio and oMLX copy OpenAI-compatible URLs ending in `/v1`; Ollama keeps its native API root. Recognized model-download failures keep the engine's error and add next steps, rather than guessing a cause for ambiguous errors (NVIDIA/Personal-AI-Router#152).
- **Departed machines do not leave jobs running (`nvpair-ui-broker`)**: In-flight workloads on a disappearing unverified machine are marked failed instead of remaining active indefinitely.
- **Uninstalling an engine keeps your models and settings (`nvpair-engine-manager`)**: LM Studio uninstall ran `rm -rf ~/.lmstudio` and oMLX `rm -rf ~/.omlx`. Each manifest now declares its model store, removal is confined and preserves it, and only the runtime is removed. PAIR uninstalls a vendor-installed engine only if it recorded installing it (engines installed before this release: use the vendor's uninstaller).
- **Stopping a leftover engine works on macOS (`nvpair-engine-manager`)**: The process-image lookup read `/proc`, so stopping an engine PAIR lost the handle to was always refused.
- **Manual nodes survive worker restarts (`nvpair-ui-broker`, `desktop`)**: The broker owns `configs/manual-nodes.json` and replays it into every new manual-nodes worker, so Tailscale and VPN peers are no longer lost when the worker restarts or the terminal interface drives the broker.
- **Jobs that share a number stay apart (`desktop`, `nvpair-tui`)**: Jobs are keyed by origin, engine, run and id, so an Ollama job and an oMLX job with the same number no longer share a card or a row.
- **Workload frames are broadcast in order (`nvpair-workload-manager`)**: A removal can no longer overtake its upsert and resurrect a job on a peer, and a failed emit's retry is no longer swallowed as a duplicate.
- **mDNS sends from UDP 5353**: Queries and responses use the RFC 6762 source port.
- **The local node's inventory refreshes (`nvpair-node-scanner`)**: VRAM, CPU and memory in the discovery snapshot no longer stay at their startup values.
- Also: IPv6 link-local pairing keeps a locally valid zone; LM Studio and oMLX chat actions and Ollama and oMLX model loads (locally and from a peer) wait up to 10 minutes for headers, and LM Studio loads run under a 4-minute limit; engine health probes reuse connections; every GPU shows on a node card; the renderer CSP is tighter; a closed stdout no longer crashes the app; model search finds Ollama models newer than the bundled catalog; macOS packages declare Local Network and Bonjour permission keys (not a discovery fix); broker shutdown joins workers concurrently within its deadline; install stages show without invented percentages.

### Added
- **Editable engine launch settings (`desktop`, `nvpair-ui-broker`, `nvpair-engine-manager`)**: Set ports, launch arguments and environment variables for this device and verified peers, including `OLLAMA_*` variables (NVIDIA/Personal-AI-Router#154). Bind-address overrides are rejected and a failed apply restores the previous settings, reporting any restoration failure; oMLX shows only its proxy port because its server settings live in the oMLX app.
- **Anthropic Messages through every proxy (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`)**: `/v1/messages` routes directly to engines with native support: Ollama 0.14.0+, LM Studio 0.4.1+ and oMLX 0.1.0+, including streaming. Adapted from NVIDIA/Personal-AI-Router#27 and NVIDIA/Personal-AI-Router@7b21d1f; PAIR does not translate protocols or promise Anthropic feature parity.
- **LM Studio load options per model and device (`desktop`, `nvpair-engine-manager`)**: Save an allowlisted set of tuning options, including GPU use, context length and parallelism, locally or on verified peers for the next load (NVIDIA/Personal-AI-Router#29). PAIR never unloads a model automatically: "Loaded with different options — reload to apply" means explicitly **Eject**, then **Load**, to apply the saved options.
- **Model download progress and cancellation (`desktop`, `nvpair-tui`, `nvpair-engine-manager`)**: Ollama and LM Studio show live progress locally and on verified peers, accept cancellation asynchronously and confirm when it stops; interrupted downloads can be retried. Adapted from NVIDIA/Personal-AI-Router#111 with the asynchronous cancellation of NVIDIA/Personal-AI-Router#115, but PAIR deliberately keeps partial data for resuming and never deletes cache files; oMLX has no PAIR download cancellation.
- **Saved nodes by IP address or hostname (`desktop`, `nvpair-tui`, `nvpair-manual-nodes`)**: Choose **PAIR machine** to save an address and pair, or **Engine only** to use an engine directly without trusting PAIR identity or telemetry claims (NVIDIA/Personal-AI-Router#10, NVIDIA/Personal-AI-Router#92). Failed pairing keeps the entry for retry or removal, and PAIR intent survives restarts without becoming an engine-only route. Removing a machine from the cluster also forgets its saved PAIR address; Engine only entries stay until you remove them.
- **Clear peer-verification labels (`desktop`, `nvpair-tui`)**: Unverified machines show "Not in your cluster" with **Pair**; direct engine entries show "Engine only — added by address" without peer controls or telemetry. Previews stay separate from verified machines even if they claim the same identity.
- **Optional first-body deadline (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`)**: `--first-body-timeout` or `NVPAIR_PROXY_FIRST_BODY_TIMEOUT` opts into retry and cooldown when an engine sends headers but no body. It is disabled by default so legitimate long prefill can wait; a short deadline can duplicate work on another node.
- **oMLX installs from its release disk image (`nvpair-engine-manager`, `desktop`)**: Installing oMLX from PAIR now follows oMLX's recommended path: the checksum-pinned `.dmg` from its GitHub releases (the macOS 26–27 or macOS 15 build, whichever fits), copied to `/Applications` and opened so the app writes its `~/.omlx/bin/omlx` CLI shim; the app updates itself from then on. The previous `install.sh` source no longer exists. An `oMLX.app` already in `/Applications` is opened, not replaced, and is never offered for uninstall; uninstalling one PAIR installed removes the app and its two shims and keeps the rest of `~/.omlx`. After oMLX's in-app updater replaces the bundle, PAIR no longer offers uninstall. The desktop's oMLX links now point at the oMLX repository and releases.
- **Configurable proxy header timeout**: `--response-header-timeout` or `NVPAIR_PROXY_RESPONSE_HEADER_TIMEOUT` (default unchanged at 120 s) for requests that queue or wait on slow model loads.

## [0.8.0] - 2026-11-02

Unsigned, unnotarized **macOS arm64 release** for this fork. Windows, Linux
desktop and Intel Mac packages are not published. Privileged-helper
registration and helper-based firewall setup remain unavailable in unsigned
builds; Settings → Service shows manual firewall guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.8.0)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.8.0` ships service product `2.6.0`. Relay
protocol stays v2. No JSON-RPC method was removed; `engine:install-tool`,
`engine:remote-install-tool` and the `/v1/node-info` `sensors` object are
additive. Installing macmon on a *remote* peer needs that peer on
`nvpair-engine-manager` 1.3.0 or newer; an older peer answers with a generic
error. macmon is a Homebrew dependency PAIR can install on demand; it is not
bundled.

### Added
- **Apple Silicon Node Sensors (`nvpair-node-info` 1.3.0, `nvpair-engine-manager` 1.3.0, `desktop`)**: Node cards on Apple Silicon gain a Sensors drawer (CPU/GPU temperature, fan speed, power draw, memory pressure and swap) and Throttling / Memory pressure badges. Temperature, fan, and power come from macmon, a Homebrew dependency PAIR can install locally or on a trusted cluster peer; it is not bundled. Thermal and memory pressure are read directly and work without macmon.

### Changed
- **Aligned Metrics Layout (`desktop`)**: Node cards, job cards and the performance legend show their metrics as aligned key/value groups. Each group names its window once ("Last 60 s", "Since broker start", "This request", "Engine total", "Engine average") instead of repeating it in every label, the per-model breakdown is a table, values use tabular figures, and an unreported value reads as a muted dash. Legend rows are buttons, so soloing a series works from the keyboard. No wire contract or service binary changes.

## [0.7.0] - 2026-10-22

Unsigned, unnotarized **macOS arm64 release** for this fork, and the first
non-prerelease build of the 0.7.0 line: it carries everything the
`0.7.0-rc.N` candidates carried, and the earlier candidates remain available
on the [releases page](https://github.com/JaredP94/Personal-AI-Router/releases).
Windows, Linux desktop and Intel Mac packages are not published.
Privileged-helper registration and helper-based firewall setup remain
unavailable in unsigned builds; Settings → Service shows manual firewall
guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.7.0` ships service product `2.5.0`. Relay
protocol stays v2. No JSON-RPC method was removed, so a mixed cluster keeps
working — but a member on an older workload-manager does not carry the
engine-native view from *remote* nodes, so that view is complete only once
every member is upgraded.

### Added
- **The engine's own view on the node card**: a node card now carries a
  separate "engine self-report" section with the engine's own counters — its
  requests, its prompt, completion and cached tokens, its cache efficiency,
  its average prefill and generation speeds, and the models it lists as
  loaded. The view is fetched from every peer over the transport PAIR already
  uses, so it reads the same cluster-wide. It is shown separately from PAIR's
  own numbers and never merged with them: the engine's counters include the
  traffic PAIR did not route, so adding the two together would count the
  same work twice. The engine's API key cannot leak through it — PAIR defines
  its own response type and reads the engine's payload field by field, so the
  key is never carried into a reply.
- **Live in-flight numbers on a running job**: a running job shows a live line
  with what PAIR has measured of it so far — the elapsed time, the time to the
  first byte, and how much has streamed (content chunks and response bytes) —
  and, for oMLX, the engine's own live reading: its token count, its decode
  rate, its prefill progress and its queue position. The frame is a snapshot,
  not a state transition, so it never enters the workload catalog; when the job
  ends the card switches to the final, exact numbers.
- **The no-estimates rule**: nothing on these surfaces is estimated,
  interpolated or derived from a different quantity. A chunk count is never
  presented as a token count, and a value nobody reported reads "not reported"
  rather than `0` — a measured zero stays a zero.

### Fixed
- **A manual peer's engine-native view was never fetched.** The engine-native
  view was read only by the node-scanner, and only for the nodes in its own
  mDNS directory. A peer registered as a **manual node** (reached over
  Tailscale or a static address) is never in that directory, so its record
  carried its model list but no engine counters, and the desktop could not
  show them. `nvpair-manual-nodes` now reads the engine's counters over the
  same pinned cluster-mTLS client it already used for the model list, and the
  broker projects them onto the manual node's record.
- **A job's live frame rarely carried the engine's numbers.** The job's two
  live publishers shared one per-job frame limiter, and the chunk-driven one —
  called on every read of the response — won nearly every window, so the
  engine's half of the frame was almost never published. Both publishers now
  put both halves on the frame they leave, so whichever one leaves a frame
  carries the facts PAIR measured and the engine's own reading. The bound is
  unchanged: a job still leaves at most one frame every two seconds.

## [0.7.0-rc.5] - 2026-10-22

Unsigned, unnotarized **macOS arm64 release candidate** for this fork. Windows,
Linux desktop and Intel Mac packages are not published. Privileged-helper
registration and helper-based firewall setup remain unavailable in unsigned builds;
Settings → Service shows manual firewall guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.5)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.7.0-rc.5` ships service product `2.4.1`. Relay
protocol stays v2. The new broker method (`workloads:progress`) and the new
node-record field (`engineNative`) are additive, so a mixed cluster keeps
working — but a member on an older workload-manager does not carry the
engine-native view from *remote* nodes, so that picture is complete only once
every member is upgraded.

### Fixed
- **A manual peer's engine-native view was never fetched.** The engine-native
  view was fetched only by the node-scanner, for the nodes in its own mDNS
  directory. A peer registered as a **manual node** (reached over Tailscale or
  a static address) is never in that directory, so its record carried models
  but no engine counters, and the desktop could not show them.
  `nvpair-manual-nodes` now reads the engine's own counters over the same
  pinned cluster-mTLS client it already used for the model list, and the broker
  projects them onto the manual node's record.

## [0.7.0-rc.4] - 2026-10-22

Unsigned, unnotarized **macOS arm64 release candidate** for this fork. Windows,
Linux desktop and Intel Mac packages are not published. Privileged-helper
registration and helper-based firewall setup remain unavailable in unsigned builds;
Settings → Service shows manual firewall guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.4)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.7.0-rc.4` ships service product `2.4.0`. Relay
protocol stays v2. The new broker method (`workloads:progress`) and the new
node-record field (`engineNative`) are additive, so a mixed cluster keeps
working — but a node running an older workload-manager drops the live frames
from *remote* jobs, and an older engine-manager or node-scanner drops the
engine-native view from *remote* nodes, so both pictures are complete only
once every member is upgraded.

### Added
- **The engine's own view on the node card**: a node card now carries a
  separate "engine self-report" section with the engine's own counters — its
  requests, its prompt, completion and cached tokens, its cache efficiency, its
  average prefill and generation speeds, and the models it lists as loaded. The
  view is fetched from every peer over the transport PAIR already uses, so it
  reads the same cluster-wide. It is shown separately from PAIR's own numbers
  and never merged with them: the engine's counters include the traffic PAIR
  did not route, so adding the two together would count the same work twice.
- **Live in-flight numbers on a running job**: a running job shows a live line
  with what PAIR has measured of it so far — the elapsed time, the time to the
  first byte, and how much has streamed (content chunks and response bytes) —
  and, for oMLX, the engine's own live reading: its token count, its decode
  rate, its prefill progress and its queue position. The frame is a snapshot,
  not a state transition, so it never enters the workload catalog; when the job
  ends the card switches to the final, exact numbers.
- **The no-estimates rule**: nothing on these surfaces is estimated,
  interpolated or derived from a different quantity. A chunk count is never
  presented as a token count, and a value nobody reported reads "not reported"
  rather than `0` — a measured zero stays a zero.

## [0.7.0-rc.3] - 2026-10-21

Unsigned, unnotarized **macOS arm64 release candidate** for this fork. Windows,
Linux desktop and Intel Mac packages are not published. Privileged-helper
registration and helper-based firewall setup remain unavailable in unsigned builds;
Settings → Service shows manual firewall guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.3)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.7.0-rc.3` ships service product `2.3.0`. Relay
protocol stays v2. The new broker methods and the new workload fields are
additive, so a mixed cluster keeps working — but a node running an older
workload-manager drops the measurement from remote jobs, so its per-node totals
are complete only once every member is upgraded.

### Added
- **Prefill and decode metrics per job and per node**: a job card now shows how
  many tokens were prefilled versus decoded, how long each phase took, the time
  to first token and the decode rate. A node card shows what that node has
  served — its phase rates over a rolling minute, its mean time to first token
  and the tokens it served — with a per-model breakdown behind a disclosure.
- The numbers are the engine's own: PAIR reads the prefill and decode
  durations that oMLX and Ollama report, and the time to first token and the
  cached-token count that only oMLX reports. Anything an engine does not
  report is shown as "not reported" rather than as a zero — LM Studio reports no
  phase split and Ollama reports no time to first token, so those fields are
  always absent. Per-node totals cover the period since the broker started.
- New broker methods `metrics:get-node-metrics`, `metrics:subscribe` and
  `metrics:unsubscribe`, plus a coalesced `metrics:node-metrics` push carrying
  the full per-node snapshot.
- The oMLX proxy now asks oMLX for the usage block on streaming requests, so
  oMLX's own prefill and decode numbers reach the job card instead of being
  missing.

## [0.7.0-rc.2] - 2026-10-20

Unsigned, unnotarized **macOS arm64 release candidate** for this fork. Windows,
Linux desktop and Intel Mac packages are not published. Privileged-helper
registration and helper-based firewall setup remain unavailable in unsigned builds;
Settings → Service shows manual firewall guidance instead. See the
[release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.2)
for validation scope and limitations.

**Upgrade notes:** Desktop `0.7.0-rc.2` ships service product `2.2.0`. Relay
protocol stays v2. Settings migrate to schema 2, which turns tracing and content
capture off until re-confirmed. In a mixed cluster, `0.7.0-rc.1` peers poll
node-info without `X-NVPAIR-Poll`, which keeps upgraded nodes sampling their GPU
every 2 seconds; idle backoff applies once every node is upgraded.

### Migration
- **Tracing and Capture Reset to Off**: Migration to settings schema 2 resets telemetry to disabled (`telemetry_enabled: false`), content capture to off (`telemetry_record_payloads: false`), consent version to 0, and collector port to 4317. Existing schema 1 settings with tracing or capture enabled are not carried forward as consent. Content capture requires explicit informed user consent conforming to payload policy version 1.
- **Proxy Configuration via Broker Only**: Proxies (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`) no longer read `PAIR_TELEMETRY_*` or `OTEL_*` startup environment variables. They start with tracing disabled and are configured solely by the supervising broker via serialized `telemetry/configure` requests. Standalone proxies without a broker do not trace.
- **Embedded Loopback-Only Phoenix & Compose Cleanups**: The broker now manages Phoenix from an embedded Compose template with both UI/HTTP (`127.0.0.1:6006`) and OTLP gRPC (`127.0.0.1:<collectorPort>`) strictly bound to loopback. The `--telemetry-compose-path` CLI flag, `NVPAIR_TELEMETRY_COMPOSE_PATH` environment variable, repository-root `docker-compose.telemetry.yml`, and `scripts/telemetry-phoenix.sh` have been removed.
- **In-App Phoenix Updates Removed**: The `telemetry/check-update` and `telemetry/update` JSON-RPC methods, runtime Docker Hub registry lookups, and the desktop update banner have been removed. Arize Phoenix is pinned by stable release tag and manifest digest (`arizephoenix/phoenix:version-20.23.0@sha256:c542e7de1dc1fdd0f938de337d44ebefa7266e91391eed6e7978523d7f6e1f2f`) and updates are delivered with PAIR releases.
- **Settings Method Consolidation**: The six legacy individual settings methods (`settings/get-telemetry-enabled`, `settings/set-telemetry-enabled`, `settings/get-telemetry-endpoint`, `settings/set-telemetry-endpoint`, `settings/get-telemetry-record-payloads`, `settings/set-telemetry-record-payloads`) have been removed; use atomic `settings/get-telemetry-config` and `settings/set-telemetry-config`.
- **Trace Volume Retention & Purge**: Trace data stored in Docker named volumes (`nvpair-phoenix-data`) persists across container stop and restarts until explicitly cleared. Users can purge trace storage via `telemetry/purge-storage` with confirmation (`{confirm: "delete-traces"}`).
- **Local-Only Tracing & Discovery Cleanup**: The `otel=4317` mDNS TXT record advertisement has been removed from `nvpair-node-scanner`. Telemetry is strictly local: the serving node records nothing for peer-originated requests, W3C trace context is not propagated across nodes, request headers are allowlisted to `Content-Type` and `Content-Length`, and captured payloads are truncated to 16 KB.

### Added
- **Worker Health and Startup Retry**: Every worker answers `worker/health`. The broker probes each worker every 30 s (5 s deadline), marks it degraded after 2 missed probes, and restarts eight child-free workers after 3; engine-manager and the three proxies are never auto-restarted on a hang. Optional workers whose first launch fails retry under the existing crash budget, and a missing binary is reported as `missing`. New `workers:get-status`, `workers:restart` and `workers:update`; Settings → Service shows a Workers card and the terminal interface Overview lists all 12 workers with restart.
- **GPU Memory Kinds**: Each GPU reports `memory_kind` (`dedicated`, `unified`, `shared`, `unavailable`) and only measured readings; the desktop labels memory by kind, shows unknown readings as "—" instead of 0, and draws chart gaps. Windows reports shared system memory separately; Intel integrated GPUs on Macs are reported as shared.
- **Engine Capabilities**: engine-manager reports each engine's effective capabilities (`list_models`, `pull_model`, `delete_model`, `delete_restarts_engine`, `set_port`, `install`, `uninstall`) in every `EngineStatus`, including peers' statuses. The desktop gates actions on them instead of a hardcoded table.
- **Terminal Interface Operations**: `nvpair-tui` lists, downloads and deletes models, changes a local engine's port, updates an engine (uninstall then install), starts, stops and installs engines on cluster peers, shows each workload's destination node, and confirms every destructive action.
- **`nvpair engine <engine> [args…]`**: Runs an installed engine's own command line through engine-manager's new one-shot `--list-engines` / `--resolve` modes, without starting a broker or changing `PATH`.
- **Scheduling Demand Notification**: The broker pushes `telemetry/set-scheduling-demand` to the scanner and manual nodes, which request the scheduling sampling lease only while a workload is queued or running.
- **Release Profiling Harness**: `scripts/perf/` profiles a baseline and candidate macOS app in sandboxed homes across the whole owned process tree, with verified clean shutdown and whole-bundle artifact identity.
- **Per-Node Tracing, Explicit Consent, and Storage Controls**: Persist revisioned telemetry configuration in `nvpair-node-settings` (schema 2) with atomic `settings/get-telemetry-config` and `settings/set-telemetry-config` methods, notifying changes via `settings/telemetry-changed`.
- **Dynamic Broker Telemetry Reconciliation**: The broker reconciles desired settings into proxies dynamically via `telemetry/configure` (with per-worker status tracking and delivery health reporting in `telemetry/get-status`), retaining the last loaded configuration if the settings worker restarts.
- **Broker-Managed Pinned Phoenix Observability**: Built-in, loopback-only Arize Phoenix Docker Compose deployment managed from an embedded template (`services/nvpair-ui-broker/telemetry/phoenix-compose.yml.tmpl`), pinned to `arizephoenix/phoenix:version-20.23.0@sha256:c542e7de1dc1fdd0f938de337d44ebefa7266e91391eed6e7978523d7f6e1f2f`, publishing only `127.0.0.1:6006` and `127.0.0.1:<collectorPort>`.
- **Trace Storage Inspection & Scoped Purge**: Added on-device trace storage measurement and `telemetry/purge-storage` RPC in `nvpair-ui-broker` to permanently remove Docker volumes labeled `com.docker.compose.volume=nvpair-phoenix-data` when confirmed with `{confirm: "delete-traces"}` without force-removing containers or pruning foreign volumes.
- **Desktop Observability Consent & Storage UI**: Redesigned `ObservabilityCard` in Service Settings with modal consent dialog for payload capture, status and delivery warning banners, numeric loopback port configuration, volume storage reporting, and double-confirmed trace purging.
- **macOS Firewall Setup Status in Service Settings**: Added a "Firewall setup: manual" status card to Service Settings on builds that cannot satisfy helper code-signing requirements, providing clear guidance on allowing incoming network connections when macOS firewall prompts appear.
- **Attribution Manifest & Release Gate**: Added `desktop/licenses/attribution-manifest.json`, reviewed upstream license texts in `desktop/licenses/texts/`, and `npm run licenses:check` (`make licenses-check`) to verify third-party license notices against manifest digests and fail on stale notices or unresolved entries without downloading Go modules.
- **Canonical Relay Envelope Conformance & Shared Test Corpus**: Both Go (`services/shared/jsonrpc`) and desktop TypeScript (`desktop/src/electron/service-bridge/relay-envelope.ts`) validate relay protocol v2 envelopes against a shared cross-process test corpus (`services/shared/jsonrpc/testdata/envelope-conformance.jsonl`). Framing rejects duplicate or case-variant reserved members (`jsonrpc`, `id`, `method`, `params`, `result`, `error`, `epoch`, `seq`) as recoverable decode errors.
- **Lease-Driven GPU Sampling & Restored Unattended Idle Backoff**: Implemented `X-NVPAIR-Poll` lease-driven sampling in `nvpair-node-info` (`foreground`, `scheduling`, `background`) and demand signaling from `nvpair-ui-broker` (`stats/set-demand`). On macOS and Linux, GPU collection runs every 2 s during interactive use or local workload execution, 5 s during scheduling demand, and relaxes to 10 s when fully idle (Windows retains its native 1 s PDH loop). Sampling latency is bounded by a 1 s timeout, and decision-point age at `nvpair-job-scheduler` is proven <= 8.5 s (strictly within the 10 s freshness budget).
- **GPU Sample Identity & Deduplication**: Node-info assigns a monotonic `sampleSeq` to each GPU hardware reading, carried across discovery records and broker feeds to `nvpair-job-scheduler` to prevent repeated or cached samples from artificially re-weighting the EWMA.
- **Decoupled Manual Node Telemetry Delivery**: `nvpair-manual-nodes` delivers compact scheduling telemetry via `node/telemetry` notifications every ~2 s (± 250 ms jitter) over persistent HTTP connections with honest sample age accounting (including probe and sample latency), completely separate from slower 10 s node discovery and model topology probes.

### Changed
- **Strict Privacy Boundaries for Inference Tracing**: Tracing is local-only; cluster serving nodes record no spans for peer-originated inference, W3C trace headers are stripped on dispatch and ingress, headers are allowlisted strictly to `Content-Type` and `Content-Length`, and captured prompt/completion payloads are truncated to 16 KB UTF-8.
- **Desktop Relay Envelope Validation & Protocol Mismatch Termination**: Desktop's JSON-RPC parser losslessly decodes sequence numbers as `bigint`, drops malformed frames without resetting the 45 s inbound-silence reconnect deadline, and on relay protocol mismatch cleanly fails pending calls, terminates the broker child, and reports a supervisor crash with `{ protocolMismatch: true }` without reconnecting.
- **macOS Kernel Event Network Monitor**: Replaced `AF_ROUTE` routing-socket monitoring with a filtered `AF_SYSTEM` kernel-event socket managed by Go's `kqueue` netpoller. Shared network monitors, including those in discovery and the three proxies, no longer wake for routing-cache misses or a 200ms socket timeout.
- **Cluster Trust Certificate Caching**: Cache validated raw bytes for `cluster/trusted/*.json` in `clustertrust.Trust.Reload()` to avoid repeatedly decoding unchanged pins and parsing their X.509 certificates. Trust-directory polling remains in place.
- **Desktop File Logging**: Gate verbose/debug/silly file writes on the configured `modularLogLevel` being `debug`, and remove duplicate notification logging. Routine remote-engine polling uses verbose logging. This reduces idle log I/O; it does not suppress real warnings or guarantee zero disk writes.

### Removed
- **Legacy Relay Epoch Fallback**: Removed the legacy `broker:epoch` params fallback for envelope epoch discovery in desktop `json-rpc-subprocess.ts`.
- **Proxy Ambient Environment Tracing**: Proxies no longer read `PAIR_TELEMETRY_*` or `OTEL_*` environment variables.
- **In-App Phoenix Updates**: Removed `telemetry/check-update`, `telemetry/update`, and the Docker Hub registry polling client.
- **Custom Telemetry Compose Paths**: Removed `--telemetry-compose-path`, `NVPAIR_TELEMETRY_COMPOSE_PATH`, root `docker-compose.telemetry.yml`, and `scripts/telemetry-phoenix.sh`.
- **Legacy Telemetry Settings Methods**: Removed six individual `settings/*-telemetry-*` RPC methods.
- **Collector Discovery Advertisement**: Dropped `otel=4317` TXT record advertisement from `nvpair-node-scanner`.

### Fixed
- **Idle GPU Sampling**: A tray-only desktop no longer requests foreground node-info readings, and discovery no longer renews the scheduling lease without demand, so idle nodes reach the 10 s sampling rate.
- **oMLX Command-Line Resolution**: oMLX counts as installed only when the CLI PAIR runs exists (`~/.omlx/bin/omlx`, `/opt/homebrew/bin/omlx` or `/usr/local/bin/omlx`); the app bundle alone no longer counts, so starts no longer fail with "no such file or directory". Uninstall is offered only when the declared uninstaller removes the resolved CLI (`uninstall.cli_roots`).
- **Engine Port Overrides**: Changing an engine's port keeps every other field of a user manifest override, including a port pinned for this host's platform, and restoring the default removes only the port.
- **Model Pull Failures**: An engine-reported pull error is returned as an error instead of a successful result.
- **Blocked Worker Writes**: A worker that stops reading its input no longer blocks the broker's request handling or the manual restart needed to recover it; calls honour their deadlines while a write is blocked.
- **Electron License Notices**: macOS packages ship `LICENSES.chromium.html` and `LICENSE.electron.txt`, and packaging fails on any platform if they are missing.
- **Unsigned macOS Helper Registration Prompts**: `nvpair-helper-ctl trust` checks the running host application and control tool against helper code-signing requirements before prompting; unsigned builds skip the helper explanation dialog and registration attempts entirely instead of repeatedly prompting.
- **Standalone Service Installer Packaging & Firewall Cleanup**: `services/installer_build.sh` stages every component in `services/versions.json` along with `LICENSE` and `THIRD_PARTY_NOTICES.md`, failing before archive creation if any file is missing or non-executable. The Windows NSIS installer (`nvpair-setup.nsi`) installs `omlx-proxy.exe` with firewall rules, bundles license notices, and removes all firewall rules on uninstall (fixing leaked LM Studio proxy firewall rules).
- **Kaizen CSS Vendoring Integrity**: `desktop/scripts/vendor-kaizen-ui-foundations-css.ts` validates pinned SHA-256 digests of downloaded CSS bytes before writing to disk, failing closed on digest mismatches.
- **Dependency Attribution Notices**: Audited 24 dependencies previously using README fallback or missing texts, resolving them with authoritative upstream license files and declared terms, and incorporating Lucide's and Kaizen's attributions directly into the canonical notice.
- **Cluster Endorsement Broadcast Churn**: Report an endorsement merge as changed only when an endorsement was added successfully, avoiding redundant `cluster:trust-changed` broadcasts on unchanged roster gossip.
- **Manual & Cluster Node Alias Deduplication**: A manually added alias supersedes a cluster-seeded entry at the same address while retaining cluster identity; removing it restores cluster seeding. The broker keeps a deterministic display alias per canonical node key, but prefers a reachable engine address over metadata-only or failed aliases for operational state, avoiding both name oscillation and suppression of healthy routes.
- **TLS-Aware Manual Node Probing & Log Deduplication**: Ordered `node-info` and `engine-manager` probes ahead of plaintext engine probes on clustered/TLS nodes in `nvpair-manual-nodes` and deduplicated non-loopback plaintext rejection warnings once per remote IP across `ollama-proxy`, `lmstudio-proxy`, and `omlx-proxy`.
- **Linux Idle GPU Activity Tracking**: Fixed `nvpair-node-info`'s internal 1-second Linux `decodeSnapshot` loop calling `Snapshot()` instead of `PeekSnapshot()`, which previously prevented Linux nodes from entering the 10-second idle GPU sampling cadence.
- **macOS Installer Safety**: Validate options and release asset names, require and verify `SHA256SUMS` for downloads before mounting, and validate the exact `PAIR.app` before stopping the installed application. Normal quit is bounded and scoped to the installed bundle; independent TUI/dev services are no longer killed by global names. Older downloads without a checksum manifest fail closed; local DMGs require independent verification.

### Documentation
- Recorded the [release-candidate profile](docs/performance-release-summary.mdx#release-candidate-profile-2026-10-20) of `0.7.0-rc.2` against `0.7.0-rc.1` (tray-only and Overview): CPU and memory match rc.1, idle GPU sampling reaches its 10 s rate, and package idle wakeups rose (cause not established). No percentage improvement is claimed.
- Backfilled the `0.7.0-rc.1` changelog and corrected fork artifact/signing claims, engine capabilities, worker inventory, relay v2 recovery, logging, and telemetry/privacy limitations across user and architecture guides and `AGENTS.md`.
- Regenerated dependency notices, retained companion Go attribution files, fixed embedded Markdown fencing, and replaced the stale services notice with a canonical-root pointer. README-only/missing upstream license text remains flagged for review.
- Added [next-release readiness follow-ups](docs/release-readiness.mdx), including unwired telemetry settings, collector exposure, private vulnerability reporting, attribution review, and final artifact validation. These are not resolved by documentation changes.
- Documented the unsigned macOS firewall status card in [Known issues](docs/known-issues.mdx) and added `npm run licenses:check` / `make licenses-check` as a release check in [Developing](docs/developing.mdx) and `AGENTS.md`.
- Updated [Next Release Readiness](docs/release-readiness.mdx) with completed installer packaging, Kaizen CSS integrity verification, audited attribution (resolving all remaining determinations on 2026-10-18 so `licenses:check` passes), and enabled private vulnerability reporting.
- Documented GitHub private vulnerability reporting channel for the fork in [Security](SECURITY.md) (enabled 2026-10-18).

## [0.7.0-rc.1] - 2026-10-17

Unsigned, unnotarized **macOS arm64 release candidate** for this fork, published
from `d18e6d2`. GitHub lists it as the regular/latest release for update discovery;
that channel designation does not make it a production-certified build. Windows,
Linux desktop, and Intel Mac packages were not published. Privileged-helper
registration and helper-based firewall setup are unavailable in this unsigned
candidate. See the [release notes](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.1)
for validation scope and remaining limitations.

**Upgrade compatibility:** Relay protocol v2 is a breaking stdio contract. Upgrade
desktop/TUI and bundled service binaries together; do not mix protocol versions.
Desktop `0.7.0-rc.1` and service product/installer `1.1.0` have independent version
cycles.

### Added
- **Relay Protocol v2 (`nvpair-shared/jsonrpc`, `nvpair-ui-broker`, `desktop`, `nvpair-tui`)**: Stamped `epoch` and monotonic `seq` on every stdio notification envelope while keeping requests and responses unstamped; report `protocol: 2` in broker epoch/resync payloads; added `broker:epoch`, `broker:resync`, and 15-second `heartbeat` liveness notifications. Go codecs now terminate incompatible stdio sessions with a protocol-mismatch diagnostic; the desktop parser extracts stamps but does not enforce every inbound envelope invariant.
- **Session Liveness & Automatic Reconnect Ownership (`desktop`, `nvpair-tui`)**: Added a 45-second inbound-silence detector that expires unresponsive broker sessions and triggers a frontend-owned reconnect with a bounded readiness deadline while preserving shutdown ownership across reconnects.
- **Service-Authoritative Routing Metrics (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`)**: Moved routing and prefix-cache affinity metric aggregation into the Go proxies (`metrics:get-routing` and `metrics:routing-update`) via `nvpair-shared/routingmetrics` rather than reimplementing metric math in clients.

### Changed
- **Epoch-Driven State Recovery (`desktop`, `nvpair-tui`)**: Desktop and TUI refresh subscribed state snapshots per domain when the broker announces a new epoch, without sequence-gap polling or repeated healthy-connection resync bursts.
- **Broker Node-Info Connection Reuse (`nvpair-ui-broker`)**: Reused a single JSON-RPC codec per `nvpair-node-info` connection instead of allocating a new codec on each control push.

### Fixed
- **TUI Snapshot Recovery Resilience (`nvpair-tui`)**: Applied successful snapshot domains even when optional workers are unavailable, adopted the broker's object-shaped workload snapshot response, and kept notification consumption active if initial connection setup partially fails.
- **Failed Desktop Startup Cleanup (`desktop`)**: Ensured a failed desktop startup reaps any subprocess instances it created without disturbing a newer concurrent start attempt, and made broker client writes cancellation-aware during teardown.

## [0.6.0] - 2026-10-04

### Changed
- **Menu Bar Job Activity**: The tray icon now reports live job activity instead of a static mark. The pentagon is drawn as a monochrome template with transparent cutouts (the full-colour app icon flattened to a solid white square in macOS template mode), its interior carries the count of queued, initializing, and running jobs as a numeral, and a `!` marks a failed job for 30 seconds. The hover tooltip spells out the counts. Windows and Linux render the alert in red; macOS relies on the glyph shape, since template images take the menu bar's colour.

## [0.5.0] - 2026-10-02

### Added
- **Phoenix Docker Image Update Checks**: Added in-app and background image freshness checks against Docker Hub Registry API (`arizephoenix/phoenix:latest`) comparing container and remote image digests via `telemetry/check-update`.
- **In-App Phoenix Container Updates**: Enabled triggering Phoenix Docker image pulls and non-blocking container rebuild/restart directly from PAIR UI via `telemetry/update`.
- **Observability UI Enhancements**: Added Phoenix image version badges (`v20.19.0`), an update alert banner with version target and dismiss support, a manual "Check for Updates" button, and an "Update Phoenix" action button to `ObservabilityCard` in Service Settings.
- **Dynamic Compose Project Detection**: Enhanced `scripts/telemetry-phoenix.sh` with `update` and `pull` subcommands and automatic `-p "$EXISTING_PROJECT"` detection, eliminating naming and container recreation collisions across packaged and dev environments.

## [0.4.1] - 2026-09-28

### Fixed
- **Packaged App Telemetry Compose Resolution**: Added `docker-compose.telemetry.yml` and `scripts/telemetry-phoenix.sh` to packaged application resources (`extraResources`) and passed `--telemetry-compose-path` to the supervisor broker.
- **Automatic Fallback Compose Provisioning**: Enabled `nvpair-ui-broker` to automatically self-provision the default Phoenix Docker Compose YAML into the user configuration directory if missing.
- **Asynchronous Docker Pulls & Condition Polling**: Handed off long Docker Compose operations (e.g. initial image pulls) to a background worker in `nvpair-ui-broker` with an extended 10-minute timeout and transitional `"starting"` container state, preventing JSON-RPC timeout errors.
- **Collector Endpoint Condition Polling**: Implemented active condition-based TCP polling of the OpenTelemetry collector port (`:4317`) before registering the service.
- **Observability UI Responsiveness**: Accelerated UI polling to 1.5s during container startup, added a "Starting…" status badge, and safely disabled dashboard links until the collector port responds.

## [0.4.0] - 2026-09-28

### Added
- **Arize Phoenix Observability Integration**: Built-in OpenTelemetry-compatible observability powered by Arize Phoenix running in a single Docker container (`http://localhost:6006`, OTLP gRPC `:4317`) on a designated cluster node.
- **W3C Distributed Tracing**: Added W3C `traceparent` context injection and extraction across all inference proxies (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`) for linked, end-to-end distributed tracing across multi-node cluster dispatches.
- **Proxy Metrics Instrumentation**: Root inference spans (`pair.proxy.inference`), router candidate scheduling spans (`pair.router.schedule`), dispatch client spans (`pair.engine.dispatch`), and cluster ingress spans (`pair.cluster.ingress`) with streaming Time to First Token (TTFT) and prompt/completion token metrics.
- **Zero-Config Cluster Discovery**: Added `otel=4317` mDNS TXT record advertisement in `nvpair-node-scanner` with automatic background reconcile in `nvpair-ui-broker`. Peer cluster nodes stream telemetry across the LAN with zero container requirements on peer nodes.
- **Desktop UI Controls (`ObservabilityCard`)**: Added an Observability & Evaluations card in PAIR Desktop Service Settings with interactive Start / Stop Phoenix action buttons, real-time connection status badges, Docker offline warning banner, and one-click dashboard access.
- **Dual-Mode Payload Control & AI Evaluations**: Default operational mode records strictly performance metadata; optional opt-in mode captures prompts and completions for Arize Phoenix evaluations (hallucination detection, toxicity, QA accuracy) with permanent credential redaction (`Authorization`, `Cookie`, `X-Pair-Pin`, `X-API-Key`, etc.).

## [0.3.10] - 2026-09-27

### Fixed
- **In-App Restart & Install**: Fixed an issue where the "Restart & install" button was unresponsive due to an uncaught `ENOENT` error writing the detached installer script to a non-existent Application Support `tmp` directory.
- **Temp Directory Initialization**: Ensured the Application Support `tmp` directory is created recursively when initializing Electron paths in `setPaths` and before writing the updater script.
- **Installer Reliability**: Added a 20-second watchdog to the macOS detached updater script to terminate unresponsive processes, cleared quarantine flags on the replaced application bundle (`xattr -cr`), and wrapped connector cleanup in shutdown fallbacks.
- **Update UI Feedback**: Added immediate "Restarting…" button feedback and surfaced error messages when update installation fails.

## [0.3.9] - 2026-09-27

### Added
- **ReverseProxy Buffer Pooling**: Introduced `bufpool.Pool` in `nvpair-shared` backed by `sync.Pool` (32 KB buffers). Wired into local ingress and outbound reverse proxies across Ollama, LM Studio, and oMLX proxies, eliminating buffer allocations during high-throughput streaming inference.
- **Adaptive GPU Sampling**: Scaled GPU telemetry collection in `nvpair-node-info` (`/usr/sbin/ioreg` on macOS, `nvidia-smi` on Linux) to dynamically transition from a 2-second active interval to a 10-second idle interval when no clients query `Snapshot()` for >6 seconds. Client requests wake the GPU worker immediately with zero latency.
- **Adaptive Node-Info Polling**: Added window state awareness to the Electron `node-info-poller`, reducing polling frequency from 2s to 30s when the application window is minimized or hidden, immediately resuming 2s polling on window restore or show.

### Fixed
- **Menu Bar Tray Auto-Resize**: Fixed a bug where the macOS menu bar tray popover was cropped to 100px and required scrolling by re-evaluating `ResizeObserver` and calculating frame dimensions upon popover visibility.
- **Root Component Re-rendering**: Replaced full store subscriptions in `MainApp` with atomic selectors for connection status and node counts, eliminating root DOM re-renders on 2s background telemetry cycles.
- **Compiler Warnings**: Cleaned up unreachable return statements in test helper loops in `services/tests/main_test.go` to satisfy `go vet`.

### Documentation
- Documented macOS application log directories in `docs/troubleshooting.mdx`.
- Synchronized repository `README.md` with upstream Roadmap, Development Team, and Engine Settings documentation.

## [0.3.8] - 2026-09-27

### Fixed
- **In-App Updates**: Resolved macOS update check errors in `ApplicationUpdatesCard` and enabled seamless in-app update checks against GitHub releases.

## [0.3.7] - 2026-09-27

### Changed
- **Energy Optimization**: Eliminated background rendering for closed menu bar tray popovers (`if (!isVisible) return null`).
- **Energy Optimization**: Disabled continuous CSS/SVG animation loops on GPU utilization bars when values are static or window is inactive.
- **Energy Optimization**: Throttled unneeded polling loops in background views.

## [0.3.6] - 2026-09-27

### Changed
- **Energy Optimization**: Optimized idle CPU power consumption across services.
- **Discovery**: Deduplicated mDNS discovery announcements and query responses across network interfaces.
- **UI**: Added rendering throttling for inactive desktop views.

## [0.3.5] - 2026-09-08

### Added
- **Automated macOS Installer**: Added `scripts/install-mac.sh` for one-line automated curl-based installation and upgrades without macOS Gatekeeper quarantine (`xattr -cr`).
- **UI**: Integrated 1-click update command copying and GitHub release navigation in the macOS update card.

### Fixed
- **Cluster Discovery**: Corrected oMLX proxy port resolution for manual nodes in discovery relay.

## [0.3.4] - 2026-09-08

### Added
- **Remote Engine & Model Discovery**: Enabled direct probing of `nvpair-engine-manager` over cluster mTLS on remote nodes to discover running engines, `modelsByEngine`, and `loadedByEngine`, bridging remote models into local client proxies.

## [0.3.3] - 2026-09-08

### Added
- **Tailscale Overlay Networking**: Added automatic discovery and synchronization of confirmed cluster members across Tailscale overlay networks (`100.64.0.0/10` CGNAT) directly into the broker relay directory.

## [0.3.2] - 2026-09-07

### Added
- **Network Interface Prioritization**: Prioritized physical low-latency Wi-Fi and Ethernet interfaces with seamless fallback to Tailscale overlay when roaming.
- **Dynamic Network Recovery**: Added 15-second dynamic recovery probes to automatically transition back to high-speed LAN interfaces upon reconnection.

## [0.3.1] - 2026-09-07

### Added
- **oMLX Model Management**: Added dynamic model loading and unloading support for oMLX.
- **Routing**: Added case-insensitive model matching across proxies.

### Fixed
- **Network Resilience**: Added multi-homed node polling recovery and IP address fallback.

## [0.3.0] - 2026-09-07

### Added
- **Prefix Cache Affinity (KV-Cache Routing)**: Added conversation and prompt prefix hashing across Ollama, LM Studio, and oMLX proxies to route multi-turn requests to nodes holding warm KV cache state, cutting TTFT and prefill computation.
- **Routing Diagnostics**: Added live prefix cache affinity telemetry under **Settings → Service → Routing Diagnostics**.

## [0.2.0] - 2026-09-06

### Added
- **oMLX Native Apple Silicon Engine Integration**: Added first-class support for [oMLX](https://github.com/jaredp94/omlx) as a supervised inference engine.
- **Proxy & Discovery**: Added `omlx-proxy` service, broker supervision, mDNS discovery (`om`), and OpenAI-compatible API ingress.
- **Port Management**: Added upfront port collision avoidance and dynamic port negotiation between LM Studio and oMLX.

## [0.1.1] - 2026-08-28

### Fixed
- Fixed bundled log collector CPU architecture mismatch on ARM systems.

## [0.1.0] - 2026-08-28

### Added
- Initial public release of NVIDIA Personal AI Router.
