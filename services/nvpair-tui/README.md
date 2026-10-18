<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-tui

A terminal UI for running and supervising the NVPAIR fleet on a **headless
machine over SSH**, where the bundled graphical UI cannot run. That is its
purpose: it is an operations tool for hosts without a desktop, not a replacement
for the graphical UI, and it does not cover every operation the desktop does.

It spawns and owns its own `nvpair-ui-broker` child over stdio; the broker in turn
supervises the worker subprocesses, so `nvpair-tui` drives one host on its own.

This file is the component reference. For task-oriented usage instructions, see
[Using the PAIR terminal interface](../../docs/terminal-interface.mdx).

## What it does

`nvpair-tui` is a JSON-RPC 2.0 client of `nvpair-ui-broker` (newline-delimited
JSON over the broker's stdin/stdout). It launches the broker, consumes its
notification stream, and renders a tabbed, keyboard-driven dashboard built
with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Tabs:

| Tab | Purpose |
| --- | --- |
| **Overview** | Broker version/uptime (`ping`) and all twelve workers from `workers:get-status` + full `workers:update` snapshots: state, attempts, last error. `R` restarts the selected worker after confirmation. |
| **Errors** | The service-error datastore (`errors:get-initial` + live `errors:update`); `c` clears the selected entry. |
| **Nodes** | mDNS-discovered Ollama nodes (`discovery:subscribe` / `discovery:nodes-changed`). |
| **Proxies** | Ollama, LM Studio and oMLX reverse proxies: status, cache-affinity selection rate, discovered upstreams, select a node (`enter`/`a`), set the listen port (`p`). |
| **Workloads** | Live cluster workloads (`workloads:subscribe` / `workloads:upsert` / `workloads:remove`). |
| **Engines** | Local inference engines: install (`i`), start (`s`), stop (`x`), restart (`r`), uninstall (`u`). |
| **Cluster** | Pairing + membership: invite by address (`i`, shows the six-digit PIN — the first invite auto-founds a cluster of one), accept (`a`) / decline (`d`) an inbound invite, remove a member (`r`), leave (`L`). |
| **Manual** | User-added nodes: add by address (`a`), remove (`r`). |
| **Settings** | The node-settings store (force-ports, cluster auto-sync, cluster id/name). |
| **Logs** | The broker's (and workers') stderr, with live log-level control (`d`/`i`/`w`/`e`). |

Worker snapshots are refreshed on broker epoch recovery, independently of the
Errors tab. The broker owns probe/retry policy; the TUI only renders its registry
and relays `workers:restart {name}`. States are `running`, `degraded`,
`restarting`, `exhausted`, and `missing`. Engine-manager and the three proxies
never auto-restart on a hang; use `R` if manual recovery is needed. Confirmation
warns that engines may remain running or stop and inference may be interrupted.
Restart initiation is not completed recovery; follow subsequent worker updates.
See [broker supervision](../nvpair-ui-broker/README.md#supervision--recovery).

## Keys

- `tab` / `shift+tab` (or `→` / `←`, `l` / `h`) — switch tabs
- `R` on Overview — confirm a worker restart; `y` sends it, `n`/`esc`/`enter`
  cancel. Global navigation/quit keys are captured while the prompt is open.
- `?` — toggle full help
- `q` / `ctrl+c` — quit (the broker is shut down cleanly on exit)
- Per-tab keys appear in the footer; while editing a field (port, PIN,
  address, setting) all keys go to the field until you press `enter` or
  `esc`.

## Running

`nvpair-tui` resolves `nvpair-ui-broker` next to its own executable (the
installed `bin/` layout). Override with `--broker-path`:

```sh
nvpair-tui                                   # broker is a sibling binary
nvpair-tui --broker-path /opt/nvpair/bin/nvpair-ui-broker
nvpair-tui --log-level debug                 # own logging (to stderr)
nvpair-tui --version
```

Logging goes to stderr (the broker's logs are shown inside the **Logs**
tab, not on the terminal), so it never corrupts the full-screen UI.

## Architecture

```
nvpair-tui (this process)
├── supervisor.go      spawn/own nvpair-ui-broker over stdio, graceful teardown
├── rpc/               JSON-RPC 2.0 codec + id-matching client
└── ui/                Bubble Tea root model + one file per tab
        │ stdio (newline-delimited JSON-RPC 2.0)
        ▼
   nvpair-ui-broker ──► nvpair-node-scanner, ollama-proxy, nvpair-errors, ... (workers)
```

The supervisor sends `shutdown` and closes the broker's stdin on exit; the
broker tears its own workers down using bounded escalation. This shutdown path
is not proof of orphan-free engine teardown: engine-manager is deliberately
ineligible for automatic hang restart. Cross-process hang tests are Unix-only;
Windows hang behavior remains unproven.

## Build & test

Built by the repo's top-level `build.bat` / `build.sh` (stamped via
`-X main.Version` from `versions.json`) and staged in `build/bin/` alongside the
other binaries. Standalone:

```sh
cd nvpair-tui
go build ./...
go test ./...
```

Cache-affinity diagnostics count completed inference POST requests observed while the
TUI is subscribed, including failed requests. The denominator excludes model lists,
embeddings and notifications without an explicit `cache_affinity` boolean. The
rate describes router affinity selections, not confirmed engine KV-cache hits.
