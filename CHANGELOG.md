<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

All notable changes to the Personal AI Router (PAIR) project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
