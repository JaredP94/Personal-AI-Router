<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PAIR desktop application

The Electron + React desktop client for NVIDIA Personal AI Router.

For what PAIR is, how to install it, and how to set it up, start at the
repository root [README](../README.md) and the
[Getting started guide](../docs/getting-started.mdx). This file covers only what
you need to work in this directory.

## Layout

```text
src/electron/     Main process: broker supervision, service bridge, IPC
src/preload/      Typed preload bridge (window.pairApi, window.windowApi)
src/ui/           React renderer
src/shared/       Types and utilities shared across the three above
src/declarations/ Ambient type declarations
scripts/          Build, license, and contract tooling
tests/            Vitest unit tests
docs/             Desktop-specific architecture and contract documentation
```

The renderer reaches services only through the preload bridge; it never talks to
a Go worker directly. Electron starts `nvpair-ui-broker` and the broker
supervises up to 13 workers. The service bundle contains 15 executables, including
`omlx-proxy` and the separate, unsupervised `nvpair-tui`. Desktop prevalidates
broker, scanner, Ollama proxy, node-info, and node-settings; standalone broker
requirements are narrower.

## Node identity and verification

Desktop node ids mirror Go's `DirectoryNode.RecordKey()`: local and verified
records use `hostUuid` (reserved `preview:` / `manual:` principals are invalid);
all other records need a namespaced `peerRecordId`. Invalid records are dropped.
The shared TypeScript helper and Go test consume the same record-key fixture.
A verified `peerRecordId` is a proof binding, not a host id.

Broker discovery owns identity and lifetime. Proxy events only enrich established
peer records; matching names, addresses, timestamps, or claimed UUIDs never merge
records. Membership, workload attribution, metrics, and engine actions use the
canonical record key. Priorities and routing selection remain service-owned;
the desktop exposes no scheduler-priority or proxy-selection action.

Previews show **Not in your cluster** and offer **Pair** by address through the
existing PIN exchange. Manual engines show **Engine only — added by address**,
without PAIR identity, peer telemetry, or peer controls. Local and verified peer
cards retain their existing controls. Older peers remain unverified until the
services can verify them; the payload does not identify why proof is unavailable,
so the UI does not guess that an update is needed.

## Engine launch settings

Expand a local or verified paired device's engine card, then **Settings**, to
edit the server port, proxy port, and launch arguments/environment assignments.
Apply validates with the owning device before saving; only changes requiring an
engine restart ask for confirmation. Progress stays in the Apply button, and a
failed operation reports one message. Revision conflicts reload saved settings
for review instead of overwriting another editor's changes.

For oMLX, only the proxy port is editable. Server settings remain managed in the
oMLX app, and no launch-text editor is shown. Preview records and manual bare
engines have no Settings panel. Unsupported devices/formats and disconnected
devices remain read-only. Launch text is displayed only in its editor.

When a reported effective server or proxy port differs from its saved port, a
quiet note beside that field shows the running port and explains that PAIR will
retry the saved port next start. This works for all four engines on local and
verified paired devices. Unknown or matching effective ports show no note;
runtime fallback ports never replace the form's saved values or enable Apply.

The renderer consumes the node-owned settings protocol described in
[`ENGINE_SETTINGS.md`](../services/nvpair-ui-broker/ENGINE_SETTINGS.md).

## llama.cpp models

On macOS arm64, expand **llama.cpp → Models → Add model** to browse the
Go-owned catalog grouped by publisher or search public single-file Q4_K_M GGUFs.
Search bounds and download failures are shown as service messages. Download,
Cancel, Canceling, Cancelled, Interrupted and Retry reuse the existing model
manager's operation tracking. llama.cpp does not promise partial-file resume.
Installed models can be refreshed while stopped; deletion requires a stopped
engine and a PAIR-owned file.

In **Settings → Startup model**, choose a downloaded catalog ID or **No model
(router mode)**. The selector writes a cache-relative file into the existing
arguments editor; Apply still previews, checks revisions and confirms restarts.
The build is not notarized, like PAIR. PAIR downloads the engine and models at
runtime; neither is bundled with the desktop app.

Verified peers show installed llama.cpp cache files from authenticated discovery
inventory, including while stopped. Owned rows offer Delete; stop the peer's
engine first. Unrecorded files are visible but not deletable, and previews receive
no inventory. Download/cancel/delete reuse the existing remote controls.
The startup picker remains local; peer Settings arguments still accept a known
installed catalog ID.

## Develop

```bash
npm install
npm start
```

`npm start` compiles the Go binaries from the sibling `../services` directory
into `cli-bin/`, builds the inference-dispatcher client into `tools/`, then
launches Electron. Prerequisites, with versions and download links, are listed under
[Prerequisites](../docs/building.mdx#prerequisites).

## Testing and checks

Five checks, all runnable from this directory:

```bash
npm run lint                     # ESLint, including Prettier formatting
npm run typecheck                # main, renderer, and test tsconfigs
npm test                         # unit tests
npm run service-contracts:check  # this app still agrees with ../services
npm run dead-code:check          # unused exported values/types
```

Run all five before opening a merge request. `make check` does not include
`dead-code:check`; run it separately. `npm run format` applies formatting
rather than just reporting it.

### While you are editing

```bash
npm run test:unit:watch                # re-runs on save
npm run test:unit tests/modular/x.test.ts   # one file
npm run test:unit:coverage             # coverage plus a summary
npm run test:clean                     # clears the Vitest cache and stray tmpdirs
```

Tests live in `tests/`, not beside the source. `tests/fixtures/isolation.ts` is
loaded for every test and enforces two things worth knowing: outbound HTTP to
anything other than loopback is blocked, and `PAIR_USER_DATA` points at a
per-worker temp directory, so a test can never write to your real application
data. A test that needs HTTP should stand up a local fake.

Coverage includes shared logic, renderer utilities/constants, and selected
Electron modules listed in `vitest.config.ts`; it is not whole-application
coverage. Unit regressions also exercise supervisor readiness and cleanup,
epoch/domain recovery, silence reconnect, node-info polling, engine/model state,
workload recovery, telemetry controls, and logging/redaction with isolated
fixtures. A coverage percentage is not evidence of live service interoperability.

`vitest.config.ts` also declares an `e2e` project, but it selects no files. There
is no end-to-end suite yet; nothing exercises the app together with its service
processes.

### When you change the service contract

`service-contracts:check` compares this app's bridge against the JSON-RPC surface
of `../services` and fails on drift. Run it whenever you touch a method, payload,
or push event. If the Go surface changed legitimately, regenerate the record:

```bash
npm run service-contracts        # report drift
npm run service-contracts:write  # regenerate docs/services-api.md
```

Changing the services themselves also means running their own tests — see
[services testing](../services/readme.md#testing).

## Build

```bash
npm run build              # renderer, main, preload, and CLI bundles
```

This produces the bundles the application runs from, for use on the machine that
built them; it does not stage the full runtime by itself. Platform `build:*`
scripts also stage services/tools and produce unsigned packages with
electron-builder. The current fork release, 0.9.0 for macOS arm64, is
unsigned and unnotarized, not an NVIDIA-certified release. Inspect actual assets
and signatures rather than assuming upstream release properties. Mac helper
packaging additionally needs the macOS SDK and Swift tools; see
[Building PAIR](../docs/building.mdx#build-the-desktop-application-alone).

## Documentation

Everything specific to the application is in [`docs/`](docs/):

- [Architecture](docs/architecture.md) — processes, IPC, and state flow
- [Frontend API](docs/frontend-api.md) — preload surface and push channels
- [Services backend integration](docs/services-backend.md) — service contract
  and update procedure
- [Services parity](docs/services-parity.md) — current capability status
- [Services API](docs/services-api.md) — generated JSON-RPC method surface

Contribution workflow, security policy, and support live at the repository root:
[CONTRIBUTING.md](../CONTRIBUTING.md), [SECURITY.md](../SECURITY.md), and
[SUPPORT.md](../SUPPORT.md).
