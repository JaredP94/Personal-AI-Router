<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Node-owned engine settings protocol

The broker coordinates validation, private persistence, engine stop/restart,
proxy rebind and service advertisement as one revisioned operation. Desktop
clients relay commands and render reported state; they do not implement this
transaction. The fork retains its three separate proxy workers and each
worker's existing `set-port` RPC and `ready` port notification.

## Client requests

| Method | Request | Result |
| --- | --- | --- |
| `engine:get-settings` | `{nodeId?, engine}` | authoritative `Snapshot` |
| `engine:preview-settings` | `Request` | `Preview`, without saving or runtime mutation |
| `engine:apply-settings` | `Request`, with `requestId` | `{revision, phase}` receipt |

`engine` is `ollama`, `lmstudio` or `omlx`. Omitted/local `nodeId` addresses
this device; a remote stable host UUID routes through engine-manager's
verified peer directory and pinned cluster mTLS. Previews and bare manual
engines do not qualify, even if a claimed identity matches a saved pin.

`Request` contains `engine`, optional `nodeId`, `expectedRevision`,
`requestId` (required for apply), and `settings`:

```json
{
  "serverPort": 11435,
  "proxyPort": 11434,
  "launchText": "MY_SETTING=literal --future-option 3"
}
```

`launchText` follows
[`pair-arguments-v1`](../nvpair-engine-manager/LAUNCH_TEXT.md). Bind-address,
port and proxy-wiring assignments are rejected, not reconciled with numeric
fields. Unrelated vendor arguments/environment are opaque. `resolution` is
accepted as an optional upstream request field but cannot authorize managed
launch controls; `conflict` remains absent because those controls are rejected.

`Preview` contains normalized `settings`, `revision`, field-keyed string
`errors`, `restart`, `rebind`, and literal `launch_args`/`launch_env` arrays.
`restart` is true only when a running engine needs a different launch or server
port. Proxy-only changes do not restart the engine. Clients ask for confirmation
only when `restart` is true. Invalid applies return a single JSON-RPC error
(`-32000`) containing a validation message without submitted values. Malformed
requests/port-only requests can return `-32602`.

For oMLX, `serverPort` is read-only and `launchText` must be empty. Only the
PAIR-owned proxy port can change. Go rejects server-port, argument and
environment edits with a message directing the user to the oMLX app. A proxy
rebind never stops or restarts that app, including adopted running instances.

## State, progress and recovery

`Snapshot` contains node/engine identity, `revision`, `appliedRevision`,
`epoch`, `sequence`, desired `settings`, `effectiveServerPort`,
`effectiveProxyPort`, `running`, `adopted`, `editable`, `reason`, `format`,
`phase`, `error` and `requestId`. Phase is `idle`, `applying`, `succeeded` or
`failed`. Notifications `engine:settings-changed` carry snapshots; remote
stream loss emits `engine:settings-disconnected` with `{nodeId}`. Sequence and
epoch here belong to the settings domain, independently of relay envelopes.

Show Apply progress from `applying`; use the snapshot's single failure message
instead of opening a duplicate global lifecycle failure dialog. A failed apply
restores the previous persisted configuration, proxy port and running state under
the engine operation lock. Its receipt stays `failed`; snapshot `settings` returns
to the previous configuration and the existing `error` field says that previous
settings were restored. `revision` remains the failed attempt's revision and
`appliedRevision` remains the last successful revision. No wire fields are added.

If compensation fails, the snapshot error explicitly reports restoration failure
and the engine is not restarted. Observed ports/running state remain authoritative;
shutdown that cannot be confirmed is reported rather than claimed successful.
oMLX's app is never stopped or restarted by proxy recovery. Recovery uses a
separate bounded 20-second context and cannot be canceled by the failed apply.
Restarts retain the previous effective bind address (unknown adopted binds narrow
to loopback); proxy rebinds change only the port, not their listening address.
The journal retains previous choices after failure so startup never replays the
failed candidate. Startup reconciles failed component stores to those previous
choices before automatic engine restoration, without changing the failed receipt;
if reconciliation fails, that engine is excluded from automatic restoration and
its discovery advertisement and healthy proxy destination are withdrawn. Other
engines whose reconciliation succeeded still restore normally. Both interrupted
`applying` operations and failed compensation must reconcile successfully,
including saving the result, before restoration is allowed. A stopped engine is
not resumed during reconciliation; saved ON/OFF intent remains authoritative.
A value-free error is reported through the existing `errors:update` channel.
The next broker startup or engine-manager worker restart retries reconciliation.
Every engine launch or adoption passes engine-manager's `Executor.doStart`
authorization gate. Ordinary lifecycle operations retry broker-owned recovery
before taking the engine operation lock (including rereading a repaired unreadable
journal); failed recovery refuses both local and paired-device remote starts with
the same value-free error. Successful recovery issues an engine-scoped generation
grant, clears the recovery error and allows normal advertisement again. Accepting a
configure operation or failing recovery revokes ordinary grants, so a start queued
behind an apply cannot use stale reconciliation. No broker journal or proxy
reconciliation logic is duplicated in engine-manager.

The gate covers local start/restart, saved-ON startup restoration, remote start,
install autostart (including remote install), action-triggered restart, server-port
restart and persistence-failure compensation, and Apply's candidate and rollback
restart. Apply and rollback use the broker's active operation ID rather than
recursively retrying recovery while holding the engine lock. Both are authorized
at the same `doStart` chokepoint, even though the journal is still `applying`.
Missing broker authority or a missing, revoked or wrong-engine grant fails closed;
adding a direct `doStart` caller cannot bypass reconciliation.
An unreadable journal blocks all three settings-managed engines, since none of
their accepted settings can be established safely; it does not change saved intent.
Reusing a request ID with identical input returns its receipt without another
restart; reusing it for different input or applying a stale revision is refused.

The broker atomically records accepted intent in private
`engine-settings-operations.json` (mode `0600`) before runtime mutation, then
records its outcome. Interrupted accepted operations are replayed before enabled
engine restoration. Saved explicit choices suppress automatic facade moves.
Engine overrides and the three existing proxy port stores remain the runtime
component stores. Port validation reserves PAIR services, the inherited Ollama
proxy alias, every configured engine, and all three configured/live proxies.
A busy requested port fails; it is not silently replaced by a different choice.

Port-only `engine:set-port`, `proxy:set-port`, `lmstudio-proxy:set-port` and
`omlx-proxy:set-port` preserve their narrow request/result shapes while using
the same coordinator. Direct client `engine:configure-launch` is refused.

## Internal worker and authenticated peer surfaces

The broker-only `engine:restore-enabled` notification carries
`{excludedEngines?: string[]}`. Engine-manager skips these engines for this startup
attempt without changing persisted ON/OFF choices. The broker computes the list
from recovery results on initial startup and every manager respawn; desktop does
not send or interpret this internal notification.

Engine-manager exposes `engine:get-launch`, `engine:preview-launch`,
`engine:configured-ports`, `engine:configure-launch` and
`engine:remote-{get,preview,apply}-settings`. A configure callback requests the
selected proxy rebind using an operation ID recorded by the broker. The additive
internal `rollback` relay method restores only the broker-captured previous proxy
port, never a port nominated by the worker. Both callbacks require the same active
operation ID, engine and candidate port; an unknown operation cannot rebind a proxy.

One `engine:settings-request` / `engine:settings-reply` correlation path serves
both the live `settings/get-engine-allow-lan-bind` read and settings
`get`, `preview`, `apply`, `rebind`, `rollback`, `prepare-start`, `authorize-start`
requests. `prepare-start` takes `{engine}` and returns an opaque reconciliation
generation string; `authorize-start` takes `{engine, requestId}` and returns null
only for the current generation or that engine's active configure operation ID.
These use the existing `Request` envelope; no client request or result changes.
Authorization takes only the relay-state mutex, never the journal/apply locks,
and does no worker I/O. Legacy broker lifecycle relays also hold neither settings
lock across worker calls; their post-call snapshot refresh reacquires them in the
usual apply-then-journal order. Replies stay with the captured
worker, never a replacement process. `engine:settings-cancel` cancels pending
requests. `engine:settings-projection` supplies full snapshots to the worker.

The existing authenticated control channel serves POST
`/v1/engine-settings/{get,preview,apply}` and GET
`/v1/engine-settings/events` (full NDJSON snapshots). The target derives caller
identity from pinned client mTLS, forbids forwarding another hop, rechecks pins
while requests/streams are live, and gates every broker operation on current
trust. Losing the requesting UI or peer after acceptance cannot abandon the
node-owned transaction midway through a port swap.

Launch values are private configuration, not log data. Never mirror raw
settings frames into a log sink.
