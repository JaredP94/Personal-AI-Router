<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Personal AI Router (PAIR)

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Security Policy](https://img.shields.io/badge/security-policy-green.svg)](SECURITY.md)

NVIDIA Personal AI Router (PAIR) is a local inference router for a group of
compatible computers on the same network. It discovers participating nodes,
manages supported inference engines, and presents Ollama-compatible and
OpenAI-compatible proxy endpoints to applications and agents. Independent
requests can be routed to eligible nodes according to engine availability,
model availability, and current workload.

PAIR is useful for concurrent local workloads such as multi-agent applications.
Prompts and responses are intended to remain on the local network when every
configured client, model source, engine, and node is local.

> PAIR routes each independent request to one node. It does **not** pool GPU
> memory, combine GPUs into a larger logical GPU, shard one model across
> machines, or split an in-flight inference request between nodes.

![Two paired machines in PAIR's Overview. Requests arrive on one and are routed
across both, with each node reporting live GPU and memory use.](assets/pair-demo.gif)

*Two paired machines: requests arrive on one, run on whichever node suits each
one, and both report live GPU and memory use throughout.
[Watch the full clip](assets/pair-demo.mp4).*

## About This Fork & Upstream Alignment

This repository ([JaredP94/Personal-AI-Router](https://github.com/JaredP94/Personal-AI-Router)) is an actively maintained fork of [NVIDIA/Personal-AI-Router](https://github.com/NVIDIA/Personal-AI-Router). It is maintained in direct alignment with the upstream project, continually incorporating upstream changes, fixes, and architectural updates as new releases are published.

This project exists as a forward-looking extension that introduces practical feature enhancements, network topology flexibility, and engine integrations that are valuable to have today:

### Summary of Enhancements vs. Upstream

1. **oMLX Engine Integration (Apple Silicon Native MLX)**
   - First-class support for [oMLX](https://github.com/jundot/omlx) as a supervised local inference engine alongside Ollama and LM Studio.
   - Includes a dedicated `omlx-proxy` service, broker supervision, mDNS discovery (`om`), OpenAI-compatible ingress, dynamic fallback port negotiation, and full desktop UI integration.

2. **Prefix Cache-Aware Routing (KV-Cache Affinity)**
   - Dynamic prompt and conversation prefix hashing across Ollama, LM Studio, and oMLX proxies.
   - Prefers nodes with observed conversation/prefix affinity when they remain eligible. This is a routing heuristic, not proof of an engine cache hit or a guaranteed TTFT improvement.
   - Adds live telemetry and diagnostics in **Settings → Service → Routing Diagnostics**.

3. **Overlay Addresses & Multi-Network Clustering**
   - Advertises and ranks physical and overlay addresses, including Tailscale CGNAT (`100.64.0.0/10`), and retries known peer addresses as connectivity changes. This is not automatic enumeration of every tailnet device.
   - Cluster trust is still required for authenticated inference/control traffic over an overlay; reachability alone does not authorize a peer.

4. **Remote Node Engine & Model Discovery**
   - Directly probes `nvpair-engine-manager` over cluster mTLS on remote nodes.
   - Automatically discovers running engines, `modelsByEngine`, and `loadedByEngine` across the cluster, seamlessly bridging remote models into local Ollama, LM Studio, and oMLX proxies.

5. **Terminal-Based macOS Installation & Updates**
   - Provides an installer/updater script (`scripts/install-mac.sh`) for published macOS disk images. Downloading with `curl` normally avoids browser-added quarantine, but does not provide Developer ID signing, notarization, or privileged-helper approval.
   - Adapts the desktop in-app updates card on macOS to provide 1-click update command copying and direct GitHub release navigation.

6. **Arize Phoenix & OpenTelemetry Observability**
   - Built-in OpenTelemetry tracing with local Arize Phoenix Docker Compose lifecycle management (`http://127.0.0.1:6006`, OTLP gRPC `127.0.0.1:<collectorPort>`) from **Settings → Service**.
   - Broker-reconciled tracing configuration with explicit payload consent (`payload_consent_version: 1`), loopback-only export, header allowlisting (`Content-Type`, `Content-Length`), 16 KB payload truncation, on-device volume storage reporting, and confirmed trace purge (`telemetry/purge-storage`). Serving nodes record no spans for peer requests, and no trace context crosses nodes. See [Security](SECURITY.md) for details.

7. **Relay Protocol v2, Session Recovery & Live Menu Bar Activity**
   - Relay v2 JSON-RPC 2.0 notification envelopes (`epoch`, `seq`; `protocol: 2` in broker epoch/resync payloads) with automatic per-domain state resynchronization (`broker:epoch`, `broker:resync`), 15-second heartbeat liveness monitoring with 45-second frontend-owned silence reconnects, and service-authoritative routing metrics (`metrics/get`, `metrics/routing-update`).
   - Dynamic monochrome template menu bar / system tray icon displaying live active job counts and 30-second failure alerts (`!`).

8. **Idle Energy & Wakeup Optimizations (Unreleased)**
   - Explicit hidden-UI polls can read cached node-info snapshots. Scheduling consumers retain foreground GPU sampling to preserve the 10-second freshness contract; unattended sampler backoff is deferred pending coordinated collection/delivery timing. Filtered macOS network events, unchanged trust-pin caching, reduced notification churn, and debug-only verbose file logging reduce background work. These changes are not in the published `0.7.0-rc.1` candidate.

## What is supported

| | |
| --- | --- |
| **Operating systems** | Windows 11; Linux; macOS |
| **Architectures** | x64 and arm64 on all three. Windows on ARM is experimental. |
| **Packaging targets** | Windows `.exe`; Linux `.deb`; macOS `.dmg`. Published assets vary by release; this fork's current candidate ships **macOS arm64 only**. For other targets, [build from source](docs/building.mdx). |
| **Mixing nodes** | Windows, Linux, and macOS nodes can all be paired with each other |
| **Inference engines** | Ollama, LM Studio, and oMLX (Apple Silicon MLX) |

**PAIR running on a machine does not mean an engine will.** PAIR itself runs on
any supported Windows, Linux, or macOS machine. Each engine sets its own requirements
for the operating system, GPU, and drivers, and each model needs enough memory to
load. Whether a particular engine and model work on a particular machine is
between that engine and that machine, so check the engine's own documentation
before assuming a node can serve a model. A node only becomes a candidate for a
request once it is actually running a compatible engine, and PAIR prefers the
nodes it already knows hold the model.

## Quick start

Download a released build and use the desktop application. That is the path we
recommend and the one the rest of this guide assumes. Building from source and
the terminal interface both exist for good reasons — changing PAIR, and machines
with no desktop — but neither is the ordinary way in. Those are covered in
[Building and running PAIR from source](docs/building.mdx) and
[Terminal interface](docs/terminal-interface.mdx).

### Download a release

Download PAIR from this fork's
[GitHub releases page](https://github.com/JaredP94/Personal-AI-Router/releases).
The latest published build is **`0.7.0-rc.1` (2026-10-17), an unsigned,
unnotarized macOS Apple Silicon candidate**, not an NVIDIA-signed release.
It includes an arm64 DMG, application ZIP, service-binary ZIP, updater metadata,
and `SHA256SUMS`; it does not include Windows, Linux, or Intel Mac installers.
Its privileged helper and automatic helper-based firewall setup are unavailable.
Automatic updater installation is not certified for this unsigned candidate;
use the Terminal installer or a manual replacement. Read its
[release notes and limitations](https://github.com/JaredP94/Personal-AI-Router/releases/tag/v0.7.0-rc.1).

Upstream official releases are separate and available at
[NVIDIA/Personal-AI-Router](https://github.com/NVIDIA/Personal-AI-Router/releases).
Windows `.exe`, Linux `.deb`, and macOS `.dmg` are supported build targets, not a
promise that this fork publishes all of them for each release.

**Upgrade clients and services together:** `0.7.0-rc.1` introduces relay protocol
v2. Do not mix older desktop/TUI clients or individual workers with the new
bundle. Unreleased performance changes are listed separately in the changelog.

**On macOS,** review the installer script before running it. It replaces
`/Applications/PAIR.app` while retaining application data. A `curl` download
normally avoids browser-added quarantine; it does **not** make an unsigned app
trusted or repair privileged-helper signing:

```bash
curl -fsSL https://raw.githubusercontent.com/JaredP94/Personal-AI-Router/main/scripts/install-mac.sh | bash
```

To pin the candidate, pass `--version 0.7.0-rc.1` using `bash -s --` after the pipe.
The installer checks the downloaded DMG against that release's `SHA256SUMS` before
installation. This detects corruption but is not a signature or independent
publisher authentication. Local `--dmg` installs require your own verification.

Alternatively, open the downloaded `.dmg` and drag `PAIR.app` to **Applications**.
Do not disable system-wide macOS security protections to run this build.

**On Windows,** when an installer is available, double-click it and follow the prompts.

**On Linux,** when a Debian package is available, install it from the download directory:

```bash
sudo apt install ./NVPAIR-Setup-*.deb
```

If you have kept more than one PAIR package in that directory, install the one
you want by its full filename instead.

### Run it

- **Open PAIR** the way you would any application — the Start menu on Windows,
  Launchpad or the Applications folder on macOS, your applications list on Linux.
  On a machine with no desktop environment, drive it from the
  [terminal interface](docs/terminal-interface.mdx) instead, which starts the same
  background services and gives you a full-screen view in the terminal.

- **Let it finish starting.** **Overview** shows this machine once the services
  are up. If it stays on **Loading...**, open **Settings → Service** and read the
  status there.

- **Get an engine running.** On the node's card, open **Engine settings** and
  select **Install** next to Ollama, LM Studio, or oMLX (Apple Silicon only).
  PAIR downloads and sets the engine up for you, subject to its platform and
  installation prerequisites. If PAIR already found an engine you installed
  yourself, start that one instead.

  ![The Install engines dialog with Ollama downloading, reporting progress as it installs.](docs/assets/onboarding/engine-lifecycle/01-engine-installing.png)

- **Add a model.** For engines with download support, select **Add model** on
  the same card. The bundled oMLX manifest supports listing/loading/unloading
  existing models, not model pull/delete actions; prepare its model library
  through oMLX. `qwen4:12b` is used for the Ollama example below; replace it with
  a model available to your engine.

  ![A node card with its engine expanded, one model pulling and the Add model button beside the list.](docs/assets/onboarding/getting-started/07-add-model.png)

- **Send a request.** Two equally good options:

  - **Let PAIR generate the traffic.** Select **Test** on
    **Settings → Service** and PAIR sends a minute of inference through the same
    path, so you can watch the jobs appear without writing anything.

    ![Overview during a test run, with jobs in flight across both machines in the cluster.](docs/assets/onboarding/getting-started/12-demo-traffic.png)

  - **Send one yourself.** Use the `curl` call below. The job then appears under
    **Jobs**, naming the node that served it.

With Ollama on its default port, this runs as written:

```bash
curl http://127.0.0.1:11434/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen4:12b","messages":[{"role":"user","content":"In one sentence, what does a router do?"}]}'
```

The reply is ordinary OpenAI-shaped JSON, abbreviated here:

```json
{
  "object": "chat.completion",
  "model": "qwen4:12b",
  "choices": [
    {
      "message": {
        "role": "assistant",
        "content": "A router decides where each incoming message should go and forwards it there."
      },
      "finish_reason": "stop"
    }
  ]
}
```

If you changed a port, or you are using LM Studio or oMLX rather than Ollama, copy the
URL from **Endpoints → API endpoints** instead of assuming the one above.

That is a single machine working. To route across machines, pair a second one
from **Settings → Cluster** and repeat the engine and model steps there. The
inviting machine shows a six-digit PIN, and you enter that PIN on the machine you
invited.

![The six-digit pairing PIN on the inviting machine beside the Cluster invitation modal on the machine being invited.](docs/assets/onboarding/getting-started/04-pairing-pin.png)

The [Getting Started Guide](docs/getting-started.mdx) covers the same ground in
detail, plus pairing, ports, and connecting your own applications.

## Uninstalling

Removing PAIR and removing your data are separate steps, and the default is to
keep your data.

**Windows.** Uninstall from Apps & features, or the Start menu entry. The
uninstaller stops PAIR, removes its firewall rules, and asks whether to delete
your data. Decline and it stays; accept and it is removed.

**Linux.** `sudo apt remove nvpair` uninstalls the application and keeps your
data. Use `sudo apt purge nvpair` to remove the data as well. Run
`dpkg -l | grep -i pair` first if you need to confirm the installed package name.

**macOS.** Run the uninstaller that ships inside the app bundle. It stops PAIR,
removes its firewall rules, unregisters its privileged helper, and then removes
the application:

```bash
sudo "/Applications/PAIR.app/Contents/Resources/installer-tools/uninstall-macos.sh"
```

Add `--purge` to remove your data as well. Dragging PAIR to the Trash instead
leaves the privileged helper registered, so use the uninstaller.

Your data means settings, logs, cluster identity and certificates, and any engine
PAIR installed for you. **Model weights are not touched** — they live in the
engine's own storage, such as `~/.ollama`, so removing PAIR does not delete the
models you downloaded. Delete those through the engine, or by removing its
directory.

**To clear your data without uninstalling,** use **Settings → Service → Reset app
data**. It removes the same set — settings, logs, cluster identity and
certificates, and PAIR-installed engines — then restarts the application as if it
were newly installed. Model libraries are left alone here too. This is the quickest
way to start over after a broken cluster or a bad engine install.

If this machine belongs to a cluster, deal with membership too — otherwise the
other nodes keep listing it as a member. You have two options:

- **Leave from this machine** before uninstalling: **Settings → Cluster →
  Leave**, or press `L` on the terminal interface's **Cluster** tab.
- **Remove it from another node**, which any member can do from
  **Settings → Cluster** by removing that node from the list.

The second option works after the fact as well, so forgetting to leave first is
recoverable.

## Documentation

If you are new to PAIR, reading in this order will get you productive fastest.
Each entry assumes the ones before it.

1. **[Overview](docs/overview.mdx)** — what PAIR does and how its pieces fit
   together. Start here so the vocabulary in every other document makes sense.
2. **[Getting started](docs/getting-started.mdx)** — install it, pair two
   machines, prepare a model, and send a first request. This is the only document
   most users need.
3. **[Managing engines](docs/engine-lifecycle.mdx)** — install, start, stop,
   update, and uninstall engines; configure ports and models; what PAIR restores
   after you quit or relaunch.
4. **[Terminal interface](docs/terminal-interface.mdx)** — the same tasks from a
   terminal, for a machine with no desktop environment. Skip it if every machine
   you run has a desktop.
5. **[Troubleshooting](docs/troubleshooting.mdx)** — worth skimming once before
   you need it, so you know where the diagnostics live. Alongside it,
   **[Known issues](docs/known-issues.mdx)** lists the significant limitations we
   are already aware of, and
   **[Collecting and sanitizing logs](docs/log-collection.mdx)** covers preparing
   a log you can share.
6. **[Architecture](docs/architecture.mdx)** — the process model, how a request is
   routed, and where the trust boundaries are. Read this before changing
   anything, or if you want to know why PAIR behaves the way it does.
7. **[Prefix-aware routing specification](docs/prefix-aware-routing-spec.mdx)** —
   how passive conversation and prompt prefix affinity works across the Ollama,
   LM Studio, and oMLX proxies, along with its
   **[implementation validation](docs/cache-affinity-validation.mdx)** and the
   standalone **[Inference dispatcher](docs/inference-dispatcher.mdx)**.
8. **[Building and running](docs/building.mdx)** — prerequisites, building from
   source, running the services without the desktop application, and writing
   your own client against the JSON-RPC API.
9. **[Developer guide](docs/developing.mdx)** — read this before contributing:
   where the code lives, how a change travels through the layers, and the
   conventions the project enforces.

Component references, for when you already know what you are looking for:

- [Services](services/readme.md) — the background services, and from there each
  component's own reference
- [Desktop application](desktop/README.md) — working in the application, and from
  there its architecture, contracts, and CLI documentation

## Releases & Changelog

See [`CHANGELOG.md`](CHANGELOG.md) for published changes and the separate
**Unreleased** section, and [Next release readiness](docs/release-readiness.mdx)
for outstanding runtime, privacy, attribution, and artifact checks. Desktop release versions are independent of the service
product and per-component versions in `services/versions.json`.

Downloads and release notes are available on the
[GitHub releases page](https://github.com/JaredP94/Personal-AI-Router/releases)
(upstream official releases are hosted at [NVIDIA/Personal-AI-Router](https://github.com/NVIDIA/Personal-AI-Router/releases)).

## Roadmap

These are features we want to add to PAIR. This list is a direction for the
project, not a commitment to delivery or implementation order. Community
feedback and contributions will help shape priorities.

### Platform support

- [ ] Full support for Windows ARM64 systems.
- [ ] DGX Station support.

### Engines and integrations

- [ ] llama.cpp support.
- [ ] vLLM support.
- [ ] EXO support.
- [ ] ComfyUI integration.
- [ ] Unsloth support.
- [x] Overlay address preference/fallback, including Tailscale addresses.
- [x] oMLX support *(Implemented in this fork: Apple Silicon native MLX engine)*.
- [x] OpenTelemetry instrumentation and Phoenix container lifecycle management.
- [ ] Wire persisted telemetry switches and collector discovery into running proxy configuration; verify privacy controls end to end.

### Routing and clusters

- [x] Route OpenAI-compatible API requests across different inference engines *(Implemented in this fork)*.
- [ ] Inference request queuing.
- [ ] Increase number of scheduler variables for better QoS.
- [ ] A "cluster as a node" view.
- [x] KV cache-aware scheduling *(Implemented in this fork: prefix cache affinity)*.

### Usability and reliability

- [ ] Requester-only clients that send work without hosting an inference engine.
- [ ] Launch PAIR automatically on system startup.
- [ ] Configure engine launch options and environment variables from PAIR.
- [x] Live menu bar job activity indicator and Relay Protocol v2 session recovery *(Implemented in this fork)*.
- [ ] Stability and reliability improvements informed by real-world use.

Have a feature request or a workflow you want PAIR to support? Open an
[issue](https://github.com/JaredP94/Personal-AI-Router/issues) and tell us how you
would use it.

## Development Team

NVIDIA team members working on upstream PAIR:

| Name | GitHub | Role |
| --- | --- | --- |
| Noah Tervalon (Terve) | [@Noah-Tervalon-Nvidia](https://github.com/Noah-Tervalon-Nvidia) | PAIR Developer - Community Lead |
| Chris Kelsey | [@ckelseynv](https://github.com/ckelseynv) | PAIR Developer - UI/UX Lead |
| Sherief Farouk | [@sherief-nv](https://github.com/sherief-nv) | PAIR Developer - Scheduling, Team Lead |
| Preston Goode | [@nv-pgoode](https://github.com/nv-pgoode) | PAIR Developer - Engine Management Lead |
| Kaylee Lubick | [@kjlubick](https://github.com/kjlubick) | PAIR Developer - Security Lead |
| Lucas Brodzinski | [@LB-NV](https://github.com/lb-nv) | PAIR Technical Program Manager |
| Ambrish Dantrey | [@adantrey](https://github.com/adantrey) | PAIR Engineering Manager |
| Seth Schneider | [@NV-sschneider](https://github.com/NV-sschneider) | PAIR Product Manager |

## Contributing and governance

- [Contributing](CONTRIBUTING.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)
- [Governance](GOVERNANCE.md)

## Support

See [SUPPORT.md](SUPPORT.md) for public support channels and scope.

## Security

PAIR includes local HTTP endpoints, LAN discovery, a PIN-based trust bootstrap,
and cluster networking. Read [SECURITY.md](SECURITY.md) before deploying it on
an untrusted or shared network. Do not report vulnerabilities in a public issue.

## License

This project is licensed under the [Apache License 2.0](LICENSE). See
[Third-Party Software Notices](THIRD_PARTY_NOTICES.md) for bundled dependencies.
Inference engines, models, and other software used with PAIR may have separate terms.
