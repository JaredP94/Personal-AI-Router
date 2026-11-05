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
requests. `prepare-start` takes `{engine, requestId?}` and returns an opaque
reconciliation generation string. The optional request ID identifies a pending
managed port move and must still match the broker's captured configuration and
worker generation; ordinary lifecycle preparation omits it.
`authorize-start` takes `{engine, requestId}` and returns null
only for the current generation or that engine's active configure operation ID.
These use the existing `Request` envelope; no client request or result changes.
Authorization takes only the relay-state mutex, never the journal/apply locks,
and does no worker I/O. Preparation uses short journal critical sections and never
acquires the port-operation serialiser. If recovery is needed, it submits a
separate recovery operation before the worker takes its engine lock. That executor
uses nonblocking admission: a busy port operation rejects preparation immediately
instead of waiting for its own callback. Failed/applying settings remain
fail-closed until recovery succeeds; retrying start can retry recovery. Legacy
broker lifecycle relays hold neither journal nor apply lock across worker calls;
port assignments retain port-operation ownership through their post-call refresh. Replies stay with the captured
worker, never a replacement process. `engine:settings-cancel` cancels pending
requests. `engine:settings-projection` supplies full snapshots to the worker.

Managed Ollama moves and their rollback, and the LM Studio startup move, use broker-only
`internal:set-port {engine, port, operationId}`, returning the existing
`EngineStatus`. The worker registers each unique operation ID on its reader
before dispatching the move. On an RPC timeout the broker sends
`internal:cancel-set-port {operationId}` to that same worker; its null response
joins the canceled operation, including a queued engine-lock wait. The move
and its cancellation acknowledgement each have a five-second deadline; cancellation
uses a separate context so an expired caller cannot skip the join. Fallback and
rollback cannot proceed without that acknowledgement. An unconfirmed cancellation
quarantines and terminates the captured manager and leaves ownership gated. The worker rechecks
cancellation and its reconciliation grant after acquiring the engine lock and
before persistence, even for a stopped engine. Public `engine:set-port` is unchanged.
The broker holds no journal/apply lock during managed Ollama calls, including
rollback, proxy rebinds and conflict lookups. It snapshots state under the journal
lock, then revalidates after each call. Timed-out engine moves still require their
cancellation join before fallback or rollback.

### Port-operation ownership and lock order

`portoperation.go` defines **one per-engine port-operation serialiser**, stored in
`Broker.portOperations`. Apply (including compensation), port-only UI/TUI requests,
managed ready reconciliation, conflict recovery, startup planning, proxy spawning,
and settings restoration all retain it for the whole operation and worker I/O.
The lock order is **port-op → settingsApplyMu (when needed) → engineConfigMu**.
Never acquire port-op while holding `engineConfigMu` or `settingsApplyMu`.

Reverse worker callbacks (`prepare-start`, `authorize-start`, `rebind`, `rollback`)
never acquire port-op. Rebind and rollback are delegated writes under the active
parent Apply/restoration operation, validated against its operation ID. The parent
seals callback admission, cancels in-flight callbacks and joins them for at most
ten seconds before releasing ownership. Callbacks retain the captured proxy and
cannot publish startup intent after admission is sealed. The join excludes reply
notification delivery; that delivery owns no port-operation writes.
Ready/error notification readers only dispatch operations off the reader goroutine;
they never wait on port-op and can continue delivering worker replies.

The final step of every port operation compares the proxy's actual port with the
current accepted journal port (or the managed startup choice before explicit
settings exist) and corrects any mismatch, even on early/stale-result exits.
Ready observations never replace an explicit accepted proxy port in the journal.
Every proxy write carries a unique `operationId` on `set-port` and uses the
caller's context capped at five seconds. A timeout or cancellation sends
`internal:cancel-set-port {operationId}` to the same proxy, with a separate
five-second cancellation-join deadline. Each proxy registers writes on its reader
before dispatch, checks cancellation after listener-lock admission and before
binding, and acknowledges cancellation only after that operation has finished.
A late reply cannot update the broker's port. A proxy that cannot acknowledge is
quarantined and terminated; late ready notifications cannot revive it and settings
recovery stays blocked until a replacement is available. The parent callback join
has a ten-second cap (one write plus one cancellation join); an unjoined callback
also quarantines its captured proxy. Final convergence retains the same bounded,
fenced write path. Failed convergence withdraws availability rather than advertising
an unverified port. No client request or result shape changes.

The existing authenticated control channel serves POST
`/v1/engine-settings/{get,preview,apply}` and GET
`/v1/engine-settings/events` (full NDJSON snapshots). The target derives caller
identity from pinned client mTLS, forbids forwarding another hop, rechecks pins
while requests/streams are live, and gates every broker operation on current
trust. Losing the requesting UI or peer after acceptance cannot abandon the
node-owned transaction midway through a port swap.

Launch values are private configuration, not log data. Never mirror raw
settings frames into a log sink.
