<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-tui

A terminal UI for running and supervising the NVPAIR fleet on a **headless
machine over SSH**, where the bundled graphical UI cannot run. That is its
purpose: it is an operations tool for hosts without a desktop, not a replacement
for the graphical UI, and it does not cover every operation the desktop does.

In dashboard mode it spawns and owns its own `nvpair-ui-broker` child over stdio;
the broker in turn supervises the worker subprocesses, so `nvpair-tui` drives one
host on its own. The separate `engine` subcommand runs an engine CLI without
starting the dashboard, broker, or worker fleet.

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
| **Workloads** | Live cluster workloads (`workloads:subscribe` / `workloads:upsert` / `workloads:remove`), with separate origin and destination columns. |
| **Engines** | Local and paired-peer engines (`n` selects target): local install (`i`, then `s` to start), peer install and start (`i`), start (`s`), stop (`x`), models (`m`), pull (`p`), confirmed model delete (`d`). Local-only: restart (`r`), confirmed uninstall (`u`), update/reinstall (`U`), engine server port (`e`). Capability-gated keys appear only when supported. |
| **Cluster** | Pairing + membership: invite by address (`i`, shows the six-digit PIN — the first invite auto-founds a cluster of one), accept (`a`) / decline (`d`) an inbound invite, confirm member removal (`r`) or leave (`L`). |
| **Manual** | User-added nodes: add by address (`a`), confirm removal (`r`). |
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

## Engine operations and workload attribution

Engine-manager owns support policy. The TUI decodes all seven
[`EngineStatus.capabilities`](../nvpair-engine-manager/README.md#effective-capabilities)
flags from the target host; missing flags are false. Install, uninstall, pull,
delete and port editing require their reported capability; update requires both
install and uninstall. Start/stop are available locally and remotely; restart,
uninstall, update and persistent port editing are local-only. oMLX supports
listing but never pull/delete, and its server port is not editable.

`n` cycles This node, trusted discovered peers, and cluster members with a host
UUID. Peer status uses `engine:remote-get-installed {node:<hostUuid>}`, refreshed
on selection and after remote actions. Transport/trust errors remain visible.
Local `i` sends `engine:install {engine}` without `start`; press `s` after
installation to start it. Peer `i` installs and starts (`start:true`).
Only the existing `engine:remote-start`, `engine:remote-stop`,
`engine:remote-install` (`start:true`), `engine:remote-pull-model` and
`engine:remote-delete-model` methods perform peer mutations.

`m` toggles model inventory and moves table navigation to models. Local facts
come from `engine:models` and `engine:models-changed`; peer facts come from
cached discovery `modelsByEngine` / `loadedByEngine`, not a direct peer model
query. Loaded models are marked. An absent engine key shows
**Model list unavailable (engine stopped or unsupported)**; a present empty
list shows **No models**. Pull/delete replies trigger inventory refresh;
discovery may still lag the peer.

`d` confirms the frozen node/engine/model, adding an inference-interruption
warning when `delete_restarts_engine` is true. Local delete uses `engine:action`
with `delete_model`; peer delete uses `engine:remote-delete-model`. A deletion
followed by a failed engine restart is shown as **deleted, restart failed**.

`e` accepts an integer server port from 1–65535, then confirms a possible running
engine restart before `engine:set-port`. It does not change the proxy port.
`U` confirms destructive reinstall: `engine:uninstall` must succeed before
`engine:install {engine,start:true}` is sent. Failures identify the phase. Files
removed by the uninstaller are deleted; update has no rollback guarantee.

All destructive confirmations (including `u`, cluster remove/leave and manual
remove) use the shared default-cancel prompt: only `y` dispatches; `n`, Esc and
Enter cancel. Navigation/quit stay captured and target details are frozen. A
cluster identity change cancels a pending leave rather than leaving the new
cluster.

RPC call budgets are 31 minutes for install, pull and the complete update
sequence; 11 minutes for start, stop, restart, set-port, uninstall (including
update's uninstall phase), and model delete; 35 seconds otherwise. Update's
install phase gets the remainder of its overall 31-minute budget. Remote
operations wait for the RPC reply, not progress events, to complete. A client
deadline is not proof the engine operation was cancelled on the serving node.

Workloads decode `originatedFrom` and `scheduledOn`; **DESTINATION** follows
**ORIGIN**. Labels resolve friendly names from identity/discovery/membership,
fall back to an eight-character UUID prefix for unknown or departed nodes, and
show `—` for absent identifiers. Later upserts replace the destination (including
failover); assignment alone does not prove completed inference.

The TUI cannot submit inference, uninstall/update/change engine ports remotely,
or pull/delete oMLX models. Use an inference client against a proxy and perform
local-only engine operations on the owning host.

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

## Managed engine CLIs

The installed `nvpair` wrapper forwards to this executable. Use `nvpair-tui`
directly instead if the wrapper is not installed:

```sh
nvpair engine                         # ENGINE, NAME, INSTALLED, SUPPORTED table
nvpair engine ollama --version
nvpair engine ollama list
nvpair engine lmstudio --help
nvpair-tui engine omlx --help
```

`nvpair engine` also prints `nvpair engine <engine> [args...]`. Engine names,
installation status, and platform support come solely from the sibling
`nvpair-engine-manager --list-engines`. With an engine name, this command uses
that sibling's `--resolve <engine>` JSON result, then runs the resolved executable.
It never searches PATH or guesses vendor locations. Sibling discovery uses the
running executable's directory, not the wrapper's directory or the current
working directory, matching broker discovery.

Every argument after the engine name is passed untouched, including `--version`,
`-h`, `--`, and quoted spaces; these are not TUI flags. The engine inherits stdin,
stdout, stderr, the working directory, and the full current environment overlaid
with the resolver's additions. For example, engine-manager supplies the effective
engine endpoint in `OLLAMA_HOST` and required library paths, rather than targeting
the PAIR proxy. This does not change the calling shell's environment or PATH.

On Unix the engine replaces this process, owning its signals and exit status.
On Windows it runs in the same console with inherited stdio; Ctrl-C reaches the
engine directly, the waiting parent stays alive, and the engine's exit code is
returned. Resolver or launch failures print one `nvpair: <message>` stderr line
and exit 2 (including unavailable engines or external services without a local
executable).

Unlike dashboard mode, this dispatch can run alongside the desktop app: it only
invokes engine-manager's read-only one-shot resolver and the chosen CLI, without
starting a broker, workers, or the TUI or writing PAIR settings. The engine CLI
itself can still perform whatever operations you request, including mutations;
this is not a sandbox or a lifecycle operation through the broker.

## Architecture

```
nvpair-tui (this process)
├── enginecmd*.go      early engine CLI dispatch (no dashboard or broker)
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
