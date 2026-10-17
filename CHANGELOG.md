<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

All notable changes to the Personal AI Router (PAIR) project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- **Background Idle GPU Sampling**: Added `X-NVPAIR-Poll: background` header support (`PeekSnapshot()`) across `nvpair-node-info`, `nvpair-node-scanner`, `nvpair-manual-nodes`, and the Electron `node-info-poller` so background discovery and hidden-window polls read cached snapshots without resetting the 6-second client-activity window, allowing `nvpair-node-info` to drop GPU sampling from 2s to 10s when no interactive UI is open.
- **macOS Kernel Event Network Monitor**: Replaced `AF_ROUTE` routing-socket monitoring with a filtered `AF_SYSTEM` kernel-event socket managed by Go's `kqueue` netpoller. Shared network monitors, including those in discovery and the three proxies, no longer wake for routing-cache misses or a 200ms socket timeout.
- **Cluster Trust Certificate Caching**: Cache validated raw bytes for `cluster/trusted/*.json` in `clustertrust.Trust.Reload()` to avoid repeatedly decoding unchanged pins and parsing their X.509 certificates. Trust-directory polling remains in place.
- **Desktop File Logging**: Gate verbose/debug/silly file writes on the configured `modularLogLevel` being `debug`, and remove duplicate notification logging. Routine remote-engine polling uses verbose logging. This reduces idle log I/O; it does not suppress real warnings or guarantee zero disk writes.

### Fixed
- **Cluster Endorsement Broadcast Churn**: Report an endorsement merge as changed only when an endorsement was added successfully, avoiding redundant `cluster:trust-changed` broadcasts on unchanged roster gossip.
- **Manual & Cluster Node Alias Deduplication**: A manually added alias supersedes a cluster-seeded entry at the same address while retaining cluster identity; removing it restores cluster seeding. The broker deterministically selects one surviving alias per canonical node key on upserts and trust refreshes, avoiding name oscillation.
- **TLS-Aware Manual Node Probing & Log Deduplication**: Ordered `node-info` and `engine-manager` probes ahead of plaintext engine probes on clustered/TLS nodes in `nvpair-manual-nodes` and deduplicated non-loopback plaintext rejection warnings once per remote IP across `ollama-proxy`, `lmstudio-proxy`, and `omlx-proxy`.
- **Linux Idle GPU Activity Tracking**: Fixed `nvpair-node-info`'s internal 1-second Linux `decodeSnapshot` loop calling `Snapshot()` instead of `PeekSnapshot()`, which previously prevented Linux nodes from entering the 10-second idle GPU sampling cadence.
- **macOS Installer Safety**: Validate options and release asset names, require and verify `SHA256SUMS` for downloads before mounting, and validate the exact `PAIR.app` before stopping the installed application. Normal quit is bounded and scoped to the installed bundle; independent TUI/dev services are no longer killed by global names. Older downloads without a checksum manifest fail closed; local DMGs require independent verification.

### Documentation
- Backfilled the `0.7.0-rc.1` changelog and corrected fork artifact/signing claims, engine capabilities, worker inventory, relay v2 recovery, logging, and telemetry/privacy limitations across user and architecture guides and `AGENTS.md`.
- Regenerated dependency notices, retained companion Go attribution files, fixed embedded Markdown fencing, and replaced the stale services notice with a canonical-root pointer. README-only/missing upstream license text remains flagged for review.
- Added [next-release readiness follow-ups](docs/release-readiness.mdx), including unwired telemetry settings, collector exposure, private vulnerability reporting, attribution review, and final artifact validation. These are not resolved by documentation changes.

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
- **Service-Authoritative Routing Metrics (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`)**: Moved routing and prefix-cache affinity metric aggregation into the Go proxies (`metrics/get` and `metrics/routing-update`) via `nvpair-shared/routingmetrics` rather than reimplementing metric math in clients.

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
