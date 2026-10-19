<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-engine-manager

A config-driven control plane for local inference engines (Ollama today;
Intel/others via a dropped-in manifest). It manages everything about an
engine **except serving inference**: detect, user-mode install,
start/stop/restart, health, and config-declared actions. Adding an engine
is a JSON manifest, not code.

The bundled manifests under `manifests/` are the working reference for manifest
authoring.

## Worker control health

An id-bearing `worker/health` request (no params) returns `{"ok":true}` with
that same id, inline on the stdin dispatch loop. Notifications receive no
response. This checks control-loop responsiveness, not engine, HTTP listener,
or domain readiness; it does not wait for engine operations.

## Communication

Bidirectional newline-delimited JSON-RPC 2.0 — the same conventions as
every other NVPAIR subprocess. Stdio by default; `--ipc <path>` dials a Unix
domain socket or Windows named pipe instead. The service is
**parent-agnostic**: it speaks to whatever owns its pipe (in practice
`nvpair-ui-broker`) and carries no front-end dependency, so the
same binary runs under any supervisor with zero code change. The `engine:*`
namespace is the surface the orchestrator/broker forwards from the UI.

## One-shot executable resolution

For normal CLI use, prefer [`nvpair engine <engine> [args...]`](../../docs/getting-started.mdx#using-an-engines-own-command-line),
which consumes these modes and launches the resolved executable. If the `nvpair`
wrapper is unavailable, invoke the bundled `nvpair-tui` by its full path with
`engine`; keep this binary beside it. No engine directory is added to PATH.

These read-only modes may run alongside the desktop application's engine-manager:

```sh
nvpair-engine-manager --list-engines
nvpair-engine-manager --resolve ollama
```

`--list-engines` prints a JSON array and exits 0. Each entry has exactly
`engine` (string), `display_name` (string), `installed` (boolean), and
`supported` (boolean), including engines unsupported on this OS/architecture.
Entries are sorted by engine name. Installed means the service's filesystem
detection succeeded or its service-only adoption criterion succeeded; it does
not guarantee a command-mode engine's declared CLI still exists.

`--resolve <engine>` prints one JSON object and exits 0:

```json
{"engine":"ollama","path":"/effective/manifest/path/to/ollama","env":{"OLLAMA_HOST":"127.0.0.1:11435"}}
```

The path must exist and not be a directory. For process-mode engines it is the
same first matching detect path used by the service, not a PATH lookup or a
fallback to `runtime.bin`. For command-mode engines it is the expanded
`runtime.cli`, not an alternate detect path. `env` contains only the platform's
declared runtime environment (an empty object if none); callers should overlay
it onto the child process's inherited environment. Placeholders use the same
resolver as service operations, with `{host}` always `127.0.0.1`, `{port}` the
effective configured engine port (persisted override or manifest default), and
`{install_dir}` the engine's managed installation directory. Thus Ollama's CLI
targets the engine port rather than the proxy; Linux library paths are retained.

Both modes load the same embedded manifests and deep-merged per-user overrides
under the service's normal config root. They accept **only `--log-level`** in
addition to the selected mode. They never connect to stdio/IPC, bind listeners,
open cluster identity, start watchers, execute engine commands, change PATH, or
write settings. Like the service, they skip malformed user overrides and keep
the remaining effective manifests. Diagnostics are suppressed unless
`--log-level=debug` is explicitly supplied (`NVPAIR_LOG_LEVEL=debug` alone does
not enable them); debug diagnostics go to stderr. Stdout contains only the JSON
result. Failures exit 2 with a one-line reason on stderr and no JSON: unknown
engine, unsupported platform, or engine not installed. Explicit debug logging
may add diagnostic lines before that reason.

When a process-mode engine has no detected executable, a single read-only
readiness probe checks for an externally adopted service on its configured fixed
port. It reuses the service's readiness check, is loopback-only, ignores HTTP
proxies, refuses redirects, and has a 500 ms total deadline rather than the
manifest's startup timeout. No port or readiness definition means no probe;
command-mode engines still require a filesystem installation. A successful probe
without an executable returns
`<engine> is running as an external service; PAIR has no executable for it`.
An unresponsive, unhealthy, non-loopback, or otherwise unidentified service is
reported as not installed, not guessed to be an installation. Listing applies
the same criterion and may spend up to 500 ms sequentially per missing supported
process-mode engine.

## JSON-RPC surface

Requests (caller → service):

| Method | Params | Result |
|---|---|---|
| `worker/health` | — | `{"ok":true}` — control-loop responsiveness only |
| `engine:get-installed` | — | `{ engines: [EngineStatus] }` |
| `engine:describe` | `{ engine }` | the engine's manifest |
| `engine:status` | `{ engine }` | `EngineStatus` |
| `engine:install` | `{ engine, start?, port?, bind? }` | `EngineStatus` (after install; also starts it if `start:true`) |
| `engine:uninstall` | `{ engine }` | `EngineStatus` (after removal) |
| `engine:start` | `{ engine, port?, bind? }` | `EngineStatus` (after readiness) |
| `engine:stop` | `{ engine }` | `EngineStatus` |
| `engine:restart` | `{ engine }` | `EngineStatus` |
| `engine:set-port` | `{ engine, port }` | `EngineStatus` (after rebind) |
| `engine:action` | `{ engine, action, params }` | the engine's raw response. `action:"pull_model"` is streamed: it emits live `engine:pull-progress` notifications and returns the pull's terminal result (see below). An action whose manifest declares `restart_after` (LM Studio's `delete_model`) restarts a running engine before replying, so the response also means the engine is back and healthy |
| `engine:logs` | `{ engine }` | `{ lines: [LogLine] }` |
| `engine:errors` | — | `{ errors: [ServiceError] }` |
| `engine:models` | — | `{ models: [string], modelsByEngine: { <engine>: [string] }, loadedByEngine: { <engine>: [string] } }` — the flat de-duplicated union of every running engine's models, the per-engine breakdown keyed by engine name, and the per-engine set of models currently **loaded in memory** (all normalized from each engine's `list_models` / `loaded_models` action `result` spec). `modelsByEngine` carries a key for every running engine whose inventory was successfully queried, including an empty list = "running, no models available"; a missing key means not running / not queryable / invalid response. `loadedByEngine` uses the same known-empty distinction for residency and also omits engines with no loaded endpoint. The `/v1/models` HTTP surface returns the same shape. |
| `engine:remote-get-installed` | `{ node }` | `{ engines: [EngineStatus] }` fetched from the remote node over `ec` mTLS |
| `engine:remote-install` | `{ node, engine, start? }` | `{ opId, status: EngineStatus }` after the remote install (live progress via `engine:remote-progress`) |
| `engine:remote-pull-model` | `{ node, engine, model?, params? }` | `{ opId, result }` after a successful remote pull (live progress via `engine:remote-progress`); engine-level failures, including HTTP 200 streams containing `error`, return a JSON-RPC error |
| `engine:remote-start` | `{ node, engine, port? }` | `EngineStatus` from the remote node (always the manifest's `runtime.bind`; no per-call bind override on the remote path) |
| `engine:remote-stop` | `{ node, engine }` | `EngineStatus` from the remote node |
| `shutdown` | — | `null` |
| `log/set-level` | `{ level }` | `{ level }` |

### Effective capabilities

`EngineStatus` = `{ engine, display_name, installed, running, healthy, port?, capabilities }`.
Every status snapshot, lifecycle result, and `engine:state-changed` notification
includes all seven boolean capability flags, derived by Go from the effective
manifest (including user overrides), the host OS/architecture, and adoption state:

| `capabilities` field | True when |
|---|---|
| `list_models`, `pull_model`, `delete_model` | The effective manifest declares that action and has a platform block for this host. |
| `delete_restarts_engine` | `delete_model` is declared with `restart_after: true` on a supported platform. |
| `set_port` | The platform's start argv, CLI args, or environment reference `{port}` — a probe or stop template alone moves nothing — and `engine:set-port` would not refuse the engine as externally adopted, which is the same adoption check it makes (a command-mode engine with an official stop command can be moved). |
| `install` | The effective manifest has a platform block for this host OS/architecture. |
| `uninstall` | `install` is true, an uninstall is declared, and the install is not externally adopted. |

Unsupported platforms report all flags false. Bundled oMLX supports model listing
but never pull/delete; its start command has no `{port}` input, so `set_port` is
false. LM Studio deletion reports `delete_restarts_engine: true`.
Capabilities describe support, not readiness or guaranteed success: normal action
validation, port conflicts, ownership checks, and operation errors still apply.
`engine:remote-get-installed` and other remote status results carry the **peer's**
capabilities unchanged, not capabilities inferred from the caller's manifests.
Clients must treat an absent `capabilities` object from an older peer as all-false.

Notifications (service → caller): `engine:ready{version}`,
`engine:state-changed{EngineStatus}`,
`engine:models-changed{engine, models}` — pushed when an engine's set of
loaded (in-memory) models changes (explicit load/unload, JIT auto-load, or
TTL/idle eviction); `models` is the full `engine:models` shape (incl.
`loadedByEngine`) so a consumer swaps its whole snapshot,
`engine:install-progress{engine, stage, percent}`,
`engine:pull-progress{engine, op, stage, percent, message}` (live progress for a
local model pull driven via `engine:action{action:"pull_model"}` — the local
counterpart of `engine:remote-progress`; frames are coalesced to changes in
stage/percent, the engine's terminal success surfaces as `stage:"success"`, and
a failed pull emits a terminal `stage:"error", percent:-1, message` frame so a
UI converges even if its synchronous call already timed out),
`engine:remote-progress{opId, node, engine, op, stage, percent, message}`
(relayed live progress for a remote install/pull), and — for the error
pipeline — `errors:report` / `errors:clear` (consumed by `nvpair-errors`
via the broker; see below).

For `engine:action` with `action: "pull_model"`, a streaming engine-level
failure (including an HTTP 200 NDJSON `{"error":"…"}` frame) returns a
JSON-RPC error, not a successful result containing an error body. The peer's
pull endpoint emits a terminal error frame for the same failure, which the
initiating `engine:remote-pull-model` call propagates as a JSON-RPC error.

The `engine:remote-*` methods are the client half of remote engine
management: engine-manager resolves the target `node` in an `ec` peer
directory (fed by its own `discovery:subscribe{services:[ec]}` to the broker
relay), dials that peer's `ec` surface over pin-based cluster mTLS, and — for
`remote-install` / `remote-pull-model` — mints an `opId`, relays each streamed
progress frame up as `engine:remote-progress`, and settles the request with the
terminal result. They fail if this node isn't clustered or the target isn't a
pinned cluster peer. See "Remote engine management" below.

`engine:install`, `engine:start`, `engine:stop`, `engine:restart`,
`engine:set-port`, and `engine:action` run in their own goroutine on the
service side, so the read loop never blocks and their responses arrive when
the op finishes.

`engine:set-port` is the **persistent** port setter (distinct from the
one-shot `engine:start {port}` override, which reverts on the next restart).
It validates `1-65535` and atomically merges `runtime.port` into the existing
manifest override in the per-user `engines/` dir, preserving custom detection
paths, environment, and every unrelated field. With no existing override it
writes only `{ engine, runtime: { port } }`. This deep-merges onto the bundled
manifest, making `runtime.port` the single source of truth across restarts,
including one-shot CLI resolution. It applies the choice immediately, bouncing
the engine onto the new port if it was running. Setting the port back to the
bundled default removes only the `runtime.port` pin; the override file is deleted
only if no other customizations remain. Because the chosen port lives in the
effective manifest, restore is automatic: a normal `engine:start` (no explicit
port) and the adopt-on-fixed-port path both come up on the retained port.
Moving a **running, adopted process-mode** engine, or an adopted command-mode
engine without an official stop command, is **refused** with an error (nothing
is persisted). An identified command-mode engine with an official stop command
can be moved; see Adoption below.

## Lifecycle

```
NotInstalled --engine:install--> (HTTPS download + verify-if-pinned + user-mode run) --> Stopped
Stopped      --engine:start----> (adopt if already serving the port, else spawn) --> Running --health--> Running
Running      --engine:stop-----> (stop signal, wait for exit; no timeout) --> Stopped
```

Detect uses the manifest's `detect` paths. Install is one-shot and
user-mode — an HTTPS download, verified against the manifest's `sha256`
when one is pinned (an unpinned fetch runs with a loud warning). Start
waits for the readiness probe, then runs a periodic health probe; an
unexpected exit is reported. The bundled Ollama manifest allows up to ten
minutes for startup because GPU discovery can exceed the previous 30-second
allowance on supported Windows systems. The deadline remains finite: if Ollama
never serves its readiness endpoint, engine-manager stops the owned process and
reports the failed start. Stop sends one stop signal and waits for the engine
to exit, with no timeout: SIGTERM to the process group on Unix (graceful, no
SIGKILL escalation), and `taskkill /T /F` on Windows — where the windowless
engines we spawn can't receive a graceful (non-`/F`) close, so a forced
terminate is the only signal that actually stops them.

### Adoption — start may attach to an engine it didn't launch

Before spawning, `engine:start` **probes the chosen port's readiness
endpoint**. If something already answers there — the engine's own desktop
app (e.g. the Ollama tray app on `11434`), or an instance left running from a
previous session — the service **adopts** that instance: it marks the engine
`running` without launching its own, rather than spawning a duplicate that
would only collide on the port. Consequences worth knowing:

- **An adopted engine has no child process the service owns**, so `engine:stop`
  (and `engine:restart`'s stop phase, and shutdown's cleanup) resolves the PID
  bound to the engine's port and terminates it **only when that process is
  running the very binary NVPAIR manages for the engine** — reclaiming an orphan
  a prior run left on our own managed port (e.g. an `ollama serve` on `11435`
  whose handle was lost after a crash). This is precise to the port, so a
  genuine third-party listener on a *different* port (Ollama's own desktop app
  on `11434` while NVPAIR manages `11435`) is never touched. A listener whose
  image is **not** our managed binary is declined with an actionable error
  naming the offending PID and image path — the user / desktop app owns that
  process, and NVPAIR won't terminate it out from under them.
- **`engine:stop` may return an error while still saving OFF.** When stop
  declines a foreign listener it returns an actionable error, but the user's
  OFF choice is persisted anyway — UI layers should treat the saved desired
  state as authoritative (the engine will not restore on restart) and surface
  the error as guidance, not as proof the OFF intent was lost. The same applies
  to cluster `POST /v1/engines/stop`, which may answer HTTP 500 even though OFF
  was recorded.
- **`engine:set-port` on a running adopted process-mode engine is refused**
  (returns an error, persists nothing); an adopted command-mode engine may move
  only with an official stop command. Moving a process-mode engine would mean
  killing the old listener and spawning a new one; since NVPAIR can't kill what
  it didn't start, it errors rather than leaving a duplicate serving the new port
  while the original keeps serving the old one. Stop the engine in its own app
  first, then set the port.
- **`engine:install` short-circuits the same way.** `detect` honors existing
  system installs (Ollama's detect paths include `/Applications/Ollama.app`,
  `%LOCALAPPDATA%\Programs\Ollama`, etc.), so "installing" an engine that's
  already present downloads nothing and reports `installed: true` — and a
  following `start:true` then adopts the running instance.
- **Liveness reconciliation uses the same probe.** A fixed-port engine found
  already serving is reported `running: true` even though NVPAIR never started it.

To get a **NVPAIR-owned, stoppable** instance, start it on a port nothing is
already serving — the probe misses, so the service spawns and owns the child
(tracked in `st.proc`), and a later `engine:stop` / `engine:restart` actually
terminates it. `engine:set-port` to a free port does exactly this; quitting the
external app first and re-starting on its usual port works too. Auto-assigned
ports (manifest `runtime.port: 0`) never adopt — there's no fixed address to
probe — so they always spawn an owned process.

## Remote engine management

When the parent passes `--control-port`, engine-manager serves the **`ec`
surface** — a cluster-scoped remote-control endpoint over **pin-based mutual
TLS** (`nvpair-shared/clustertrust`, the same identity + per-peer pins
`nvpair-cluster-manager` mints). It presents this node's cluster leaf, requires a
client cert, and rejects any caller that isn't a byte-for-byte pinned cluster
peer with a `403`. It differs from `em` (`--http-port`) only in what it permits:
`ec` performs privileged operations, while `em` is a read-only inventory. Both are
locked to pinned cluster peers on the LAN — a node's model list tells a caller
which models that machine holds, which is cluster data like any other. `em` also
keeps a plaintext personality on loopback, because this node's own scanner reads
its own inventory that way and must be able to while unclustered.

Membership is evaluated **live**, per handshake and per request, from
`--cluster-dir`. While this node is not a cluster member it presents no leaf, so
every handshake is refused and nothing privileged is reachable; the moment it
becomes a member the same listener serves pinned peers. The listener is therefore
bound for the life of the process rather than only when clustered at startup — a
node joins and leaves a cluster while engine-manager runs, and a surface chosen
at bind time would stay dark until the process was restarted.

Endpoints (all under `/v1`):

| Route | Shape | Purpose |
|---|---|---|
| `GET /v1/engines` | JSON | remote `engine:get-installed` |
| `POST /v1/engines/install` | NDJSON stream | remote install (+ optional start) with live progress |
| `POST /v1/models/pull` | NDJSON stream | remote model pull with live progress |
| `POST /v1/engines/start` | JSON | remote start → `EngineStatus` |
| `POST /v1/engines/stop` | JSON | remote stop → `EngineStatus` |

The streaming routes emit zero or more `{"type":"progress",...}` frames followed
by exactly one terminal `{"type":"result",...}` or `{"type":"error",...}` frame.
The initiating node's engine-manager consumes that stream, relays each progress
frame up as `engine:remote-progress`, and settles the originating
`engine:remote-*` request on the terminal frame — so a UI gets one synchronous
response plus a live progress feed keyed by `opId`.

The broker wires both directions: it passes `--control-port`/`--cluster-dir`,
registers `ec` with the discovery daemon whenever a cluster dir is configured, and
relays engine-manager's `discovery:subscribe{services:[ec]}` into the relay
directory so the peer directory stays current. It does **not** restart
engine-manager on `cluster:identity-changed` — the surface follows membership on
its own.

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--http-port <port>` | `0` (off) | Serve the plain LAN model-list surface (`GET /v1/models`, the `em` service) on this port; the broker passes `:14322` |
| `--control-port <port>` | `0` (off) | Serve the cluster-scoped mTLS remote-control surface (the `ec` service) on this port; callers are admitted only while this node is a cluster member. The broker passes `:14323` |
| `--reserved-port <port>` | `0` (off) | Refuse local or remote engine starts and persisted port changes on a parent-owned proxy alias; the broker configures this from `OLLAMA_HOST` |
| `--cluster-dir <dir>` | _(none)_ | Cluster identity/pin directory; gates the `ec` surface on and supplies the leaf/pins used to serve it and to dial peers |
| `--loaded-poll-interval <sec>` | `5` | Seconds between loaded-model polls that drive `engine:models-changed`; `0` disables the watcher |
| `--log-level <level>` | _(env `NVPAIR_LOG_LEVEL` or `info`)_ | `debug` \| `info` \| `warn` \| `error` |
| `--version` | | Print version and exit |
| `--list-engines` | | Print engine availability as JSON and exit; only `--log-level` may accompany it |
| `--resolve <engine>` | | Print executable path and runtime environment as JSON and exit; only `--log-level` may accompany it |

Logs go to **stderr** via the shared `nvpair-shared/applog` format; stdout is
reserved for JSON-RPC frames in stdio mode (or the JSON result in one-shot mode).

## Logging & errors

Each managed engine's stdout/stderr is captured into a bounded ring
(queryable via `engine:logs`). Operational failures (install/start/health)
are recorded and surfaced as `errors:report` / `errors:clear`
notifications using the `nvpair-shared/errors` wire shape. The broker
(`nvpair-ui-broker`) forwards them to the `nvpair-errors` registry.
Ids follow `engine-manager:<class>:<engine>`.

## Security posture

Runs **user mode only** — no admin/sudo at runtime (privilege escalation
is reserved for NVPAIR's own install time). It has two optional LAN listeners, and
**both are pin-based cluster mutual TLS with a live membership check** — every
caller is authorized against a per-peer pin, a non-member is refused with a `403`,
and while this node belongs to no cluster it presents no leaf so no handshake
completes:

- **`--http-port`** — the model-list surface (`em`, `GET /v1/models`; the broker
  passes `:14322`). A node's model inventory is cluster data, so LAN callers must
  be pinned peers. This port additionally serves **plaintext on loopback only**,
  which is how this node's own scanner enriches its own card — including when the
  node belongs to no cluster, so a standalone machine still shows its own models.
- **`--control-port`** — the remote-control surface (`ec`, the `engine:remote-*`
  targets; the broker passes `:14323`). mTLS only, no plaintext personality, since
  every route performs a privileged operation.

Unlike the rest of NVPAIR, engine-manager therefore **does terminate inter-node
mTLS itself** (and dials peers' `ec` surfaces with the same pinned identity),
matching how `nvpair-errors` and `nvpair-workload-manager` handle their own
cluster traffic. Managed engines bind **loopback by default**, but a manifest's
`runtime.bind` may open an inference engine to the LAN — Ollama ships
`0.0.0.0` to serve the cluster — overridable per call via
`engine:start {bind}`; readiness/health probes always target loopback.
Downloads are **HTTPS-only** (plain `http` only from loopback) and verified
against the manifest's `sha256` when one is pinned; an unpinned fetch runs
with a loud warning.

## Cross-platform

One binary compiles and runs on Windows, Linux, and macOS × amd64/arm64.
Per-OS variance lives in the manifest first; OS primitives (process
termination, console hiding) are the only build-tagged Go
(`proc_windows.go` / `proc_unix.go`).

## Shutdown

Shuts down on stdin EOF (parent closed the pipe), `SIGINT`/`SIGTERM`, or a
`shutdown` JSON-RPC request — stopping any running engines first so none
are orphaned.
