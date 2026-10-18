<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Personal AI Router — backend services

These are the Go services behind PAIR. They **discover other PAIR nodes** on the
local network: each node advertises itself over mDNS as one consolidated
`_nvpair-node._tcp` record, and peers learn from that record which services a
node offers and where to reach them.

What a discovered node can actually serve is a separate question, answered after
discovery. A node may run [Ollama](https://ollama.com/), LM Studio, oMLX, any
supported combination, or none. Model inventory is fetched from engine-manager
rather than crammed into mDNS TXT records. The bundled oMLX engine manifest
supports macOS arm64 only and has no model pull/delete actions.

Locally, each node exposes compatibility proxies — Ollama-compatible and
OpenAI-compatible — so an unmodified client on that machine can reach any capable
node in the cluster. Routing is automatic: the proxy picks a node that is online,
has a compatible engine running, and holds the requested model.

Standalone, the Ollama proxy defaults to `http://localhost:11435`; under the
broker it serves the Ollama-compatible `http://localhost:11434` when that port is
free. If an inherited `OLLAMA_HOST` names a different local HTTP port, the broker
can serve that loopback-only address from the same router when it is free —
`localhost` is claimed on IPv4 and IPv6 together, and remote or HTTPS targets are
never intercepted.

The oMLX proxy defaults to `1234` standalone. Under the broker, when LM Studio
occupies or reserves `1234`, oMLX is assigned a free fallback starting at `1236`,
excluding reserved ports. Use reported proxy status rather than assuming its
endpoint. Model inventory is served on `14322`; node-info on `14318` serves
hardware, utilization, and identity telemetry, not the model list.

PAIR ships a graphical UI alongside these services. The UI launches
**`nvpair-ui-broker`** from the same directory; the broker orchestrates the
workers and exposes a newline-delimited JSON-RPC 2.0 API over stdio (or a Unix
socket / Windows named pipe via `--ipc`). For supported platforms and engines,
see the [root README](../README.md#what-is-supported).

## Architecture

This tree builds 14 service executables: `nvpair-ui-broker`, 12 supervised
workers, and `nvpair-tui`. The broker attempts to start available workers at
startup. Standalone, only the scanner is required; missing default auxiliary
binaries and initial auxiliary spawn failures are non-fatal. Invalid explicit
worker paths are fatal. Failed initial spawns are not automatically retried;
crash supervision applies after a successful start. Desktop also prevalidates
broker, scanner, Ollama proxy, node-info, and node-settings binaries.

`nvpair-tui` launches and supervises its own broker instead of being supervised.
Processes use newline-delimited JSON-RPC 2.0 over stdio or optional Unix socket /
Windows named pipe, with relay protocol v2 notification envelopes. See
[Control Flow](../docs/architecture.mdx#control-flow).

| Binary | Role |
| --- | --- |
| `nvpair-ui-broker` | Parent service and JSON-RPC API surface used by the bundled UI and other clients. Supervises workers, relays consolidated discovery, and coordinates routing and scheduling. |
| `ollama-proxy` | Ollama-compatible HTTP reverse proxy. Routes only to advertised model owners, with owner failover and scheduler priorities. |
| `lmstudio-proxy` | LM Studio counterpart to `ollama-proxy`, forwarding OpenAI-compatible inference routes with equivalent owner-only routing and failover behavior. |
| `omlx-proxy` | OpenAI-compatible routing proxy for oMLX, with owner-only routing, failover, and cache-affinity hints. |
| `nvpair-node-info` | Local HTTP service on `:14318` exposing GPU, CPU, and memory inventory at `/v1/node-info`. |
| `nvpair-node-scanner` | Consolidated discovery daemon. Advertises and browses `_nvpair-node._tcp`, maintains the node directory, and enriches peers with hardware and model information over HTTP. |
| `nvpair-manual-nodes` | Manages user-added nodes that don't appear via mDNS; probes them every 10 s. |
| `nvpair-workload-manager` | Cluster-wide workload relay. |
| `nvpair-errors` | Service-errors datastore + cross-node sync. |
| `nvpair-engine-manager` | Config-driven control plane for local inference engines, model inventory, and trusted remote engine operations. |
| `nvpair-node-settings` | Typed key-value store for per-node preferences. |
| `nvpair-cluster-manager` | Node identity, PIN pairing, and the trusted-node store. |
| `nvpair-job-scheduler` | Responsive scheduler combining total node queue depth across engines with smoothed GPU pressure. |
| `nvpair-tui` | Terminal interface for headless and SSH operation; launches and supervises its own broker. |

Shared code lives in the local `shared/` Go module (imported as `nvpair-shared/…`, replaced via `replace nvpair-shared => ../shared`). It provides logging, wire types, JSON-RPC and IPC, discovery records, mDNS, network monitoring, stable node identity, application data paths, and cluster trust helpers.

The mDNS responder is our own rather than the host's, because Windows ships none. It sets `SO_REUSEADDR` so it shares UDP 5353 with sibling PAIR processes and with a system responder — `avahi-daemon` on Linux, Bonjour where present — needing no configuration on either platform.

The broker feeds every accepted local or peer workload transition plus compact
GPU telemetry to the scheduler. Queued and running work is counted by destination
node across Ollama, LM Studio, and oMLX together. Fresh maximum-GPU utilization is
smoothed into pressure 0–3; missing or stale telemetry is neutral. Rankings use
`pending + gpuPressure`, and each proxy adds local reservations before choosing,
so bursts spread without waiting for workload feedback.

## Repository layout

```
nvpair-ui-broker/        Parent service / JSON-RPC API surface
ollama-proxy/            Ollama-compatible routing proxy
lmstudio-proxy/          OpenAI-compatible routing proxy for LM Studio
omlx-proxy/              OpenAI-compatible routing proxy for oMLX
nvpair-node-info/        Local GPU-inventory HTTP service
nvpair-node-scanner/     Consolidated _nvpair-node._tcp discovery daemon
nvpair-manual-nodes/     Manual-node manager
nvpair-workload-manager/ Cluster workload relay
nvpair-errors/           Service-errors datastore + sync
nvpair-engine-manager/   Local and remote inference-engine control plane
nvpair-node-settings/    Per-node preferences store
nvpair-cluster-manager/  Node pairing / trust service
nvpair-job-scheduler/    Cluster job scheduler
nvpair-tui/              Terminal interface for headless / SSH operation
shared/                   Shared Go module (nvpair-shared/…)
eap-noob/                 EAP-NOOB implementation used by cluster pairing
tests/                    Cross-process integration tests (separate go.mod)
versions.json             Single source of truth for every component version
build.bat                 Builds all fourteen service executables (Windows)
build.sh                  Builds all fourteen service executables (Linux/macOS)
VERSIONING.md             SemVer rules and version-bump workflow
```

Every directory above is its own Go module and holds its own tests beside the
source it covers. See [Testing](#testing).

## Requirements

- [Go](https://go.dev/dl/) 1.25 or newer.
- [`jq`](https://jqlang.org/) on `PATH` — the build scripts use it to parse `versions.json`. Install via `winget install jqlang.jq` / `choco install jq` / `scoop install jq` on Windows, `sudo apt install jq` on Debian/Ubuntu, `sudo dnf install jq` on Fedora/RHEL, or `brew install jq` on macOS.
- No C toolchain, GUI, or webkit dependencies. Every module is pure Go (`cgo` is not used).

## Building

Always use the platform-appropriate build script, run from this `services/`
directory.

On Windows:

```powershell
.\build.bat
```

On Linux and macOS:

```bash
./build.sh
```

Both scripts read `versions.json`, build all fourteen service executables with
`-X main.Version=…` ldflags, and stage them together in `services/build/bin/`.

Do **not** build individual components by hand without also copying their binaries into `build/bin/`: the broker will silently keep using the older binary there.

The staged binaries in `build/bin/` are what you run locally. Installable builds
come from the
[releases page](https://github.com/NVIDIA/Personal-AI-Router/releases).

## Running

Normal installations launch the bundled UI, which starts the broker from the same
installation directory.

To drive a services-only build interactively, run `nvpair-tui` — it launches and
supervises its own broker, so nothing else needs to be running. See
[Using the PAIR terminal interface](../docs/terminal-interface.mdx).

For service development or direct API access, launch the broker with
`build/bin/` as its working directory. Default worker paths resolve from the
**current working directory**, not from the broker executable's location:

```powershell
Set-Location build\bin
.\nvpair-ui-broker.exe                         # Windows
```

```bash
cd build/bin
./nvpair-ui-broker                             # Linux/macOS
```

The bundled UI connects to the broker over its JSON-RPC API (stdio or `--ipc`).
Other clients can use the same contract. See `nvpair-ui-broker/README.md` for
the API surface.

## Debugging

Every binary respects two inputs for its starting log level, resolved before any other work happens. Precedence: `--log-level` flag > `NVPAIR_LOG_LEVEL` env var > default (`info`). The broker passes its own resolved level to every subprocess it spawns (`--log-level <level>`), so setting either before launch turns on debug output from the very first line of all processes.

From `services/build/bin/`, use either the environment variable or the flag:

```powershell
# Windows / PowerShell
$env:NVPAIR_LOG_LEVEL = "debug"
& ".\nvpair-ui-broker.exe"
# Alternatively:
& ".\nvpair-ui-broker.exe" --log-level debug
```

```bash
# Linux/macOS
NVPAIR_LOG_LEVEL=debug ./nvpair-ui-broker
# Alternatively:
./nvpair-ui-broker --log-level debug
```

Accepted values: `debug`, `info`, `warn`, `error`. The level can also be changed live over the broker's `log/set-level` JSON-RPC method (not persisted across restarts), so for launch-time issues use the env var or the flag.

Those are standalone defaults. Desktop persists `modularLogLevel`, defaults to
`warn`, and passes an explicit level to the broker. Current unreleased desktop
logging preserves structured service severity and writes verbose protocol/debug
entries to its log file only at `debug`; stderr alone does not mean warning.

### Inference tracing

Proxy OpenTelemetry providers read startup environment variables:
`PAIR_TELEMETRY_ENABLED`, `PAIR_TELEMETRY_ENDPOINT`, and
`PAIR_TELEMETRY_RECORD_PAYLOADS` (with the OTel alternatives documented in each
proxy's `telemetry.go`). Defaults are enabled, `localhost:4317`, and payloads
false. Saved UI telemetry settings currently do not configure live providers;
`otel` discovery advertisements are not consumed as exporter selection.
Export is plaintext OTLP, not cluster mTLS. Phoenix's Compose ports `6006` and
`4317` are not loopback-restricted and have no configured authentication/TLS;
stopping the container retains its named-volume data. Fixed-header redaction is
not general secret scrubbing. Local-only, explicitly opted-in evaluation capture
is contributor policy, not an enforced transport restriction. See
[Observability](../docs/architecture.mdx#observability-and-distributed-tracing)
and [SECURITY.md](../SECURITY.md) before enabling payload capture.

## Testing

There are two layers, and while you are editing a component you want the first
one.

### Per-component tests

Every component keeps its tests beside its source, and each is its own Go module,
so run them from that component's directory:

```bash
cd nvpair-engine-manager
go test ./...
```

These are fast and are what you should run on each edit. Substitute any component
directory — `shared/` and `eap-noob/` have their own tests too:

```bash
cd shared
go test ./...
```

**Every one of the fourteen service executables has tests**, as do `shared/` and
`eap-noob/`. Depth varies with how much behaviour a component carries:
`nvpair-engine-manager` and `nvpair-cluster-manager` have the largest suites,
while a component with one test file may still hold twenty test functions in it.
File count is a poor proxy for coverage, so read the suite rather than counting
files.

### Cross-process tests

`tests/` has its own `go.mod` and exercises real binaries talking to each other.
Its `TestMain` builds the workers it needs into a temp directory first, so you do
not have to build beforehand:

```bash
cd tests
go test ./...
```

Expect this to take a few minutes. Run it before opening a merge request, and
after any change to a JSON-RPC method, payload, or notification.

### Tests that skip themselves

Some tests need something the machine may not have, and skip rather than fail:

- Tests that bind a well-known port (`11434`, `14319`, `14321`) skip when it is
  already in use — so quit PAIR before running them, or expect gaps.
- Tests needing a real mDNS responder skip when one is unavailable.
- Live engine tests only run when you opt in:

From `nvpair-engine-manager/`, both the `live` build tag and environment opt-in
are required; the ordinary `go test ./...` excludes these files:

```bash
NVPAIR_LIVE_OLLAMA=1 go test -tags live -run '^TestLiveOllamaCleanRoom$' -v -timeout 900s
NVPAIR_LIVE_LMSTUDIO=1 go test -tags live -run '^TestLiveLMStudioCleanRoom$' -v -timeout 1200s
```

These perform real engine lifecycle operations with platform-specific guards;
read the selected test before running it. Separate `live_pound_test.go` coverage
uses an existing Ollama install. Run deliberately on systems you control.

From `omlx-proxy/`, the smoke test uses an existing loopback engine without
restarting it (no `live` build tag):

```bash
PAIR_OMLX_LIVE_URL=http://127.0.0.1:1235 PAIR_OMLX_LIVE_MODEL='your-model-id' \
  go test -run '^TestLiveOMLXCacheAffinityConversation$' -v -timeout 10m
```

Replace `your-model-id` with a real advertised model ID. This single-node test does not
prove cross-node routing, KV-cache hits, or comparative performance.

A skip is not a pass. If you are relying on a test, check it actually ran.

## Versioning

`services/versions.json` is the single source of truth for every component version and the umbrella `product` version. The build scripts read it and stamp each binary via `-ldflags "-X main.Version=..."`. Bump the components your change affects in the same pull request, and describe any user-facing change in the pull-request description so it reaches the release notes. [`VERSIONING.md`](VERSIONING.md) has the bump rules.

You can verify a built binary's stamped version at any time:

```powershell
.\build\bin\ollama-proxy.exe --version                                     # Windows
```

```bash
./build/bin/ollama-proxy --version                                         # Linux
```

## Further reading

- `VERSIONING.md` — SemVer bump rules and how a release is declared.
- `bom.md` — bill of materials for a given release.
- Per-component `README.md` files in each subdirectory.
- [Architecture](../docs/architecture.mdx) — how these services fit together at
  runtime, including node-to-node transport and discovery.
