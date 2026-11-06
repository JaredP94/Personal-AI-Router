<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Ordered, bounded JSON-RPC output

Each Codec has one ordered writer, started on demand and retired when idle.
Notification stamping, serialization and queue admission share one short critical
section; no pipe I/O or logging runs under that lock. Notifications, requests and
responses use the **same FIFO**. Notifications alone carry epoch/seq. An accepted
frame is immutable even if its caller later changes its buffers.

## Bounds and terminal failures

Admission allows **256 outstanding frames and 16 MiB**, including the active
frame. Each accepted frame has a **5 second deadline measured from admission**,
including time queued, not 5 seconds per partial Write. The writer handles short
writes without permitting interleaving. Notify and response methods wait at most
**10 ms** for the common immediate-completion case, then return acceptance, not
delivery. Encoding and enqueue-lock CPU time are additional; producers never
wait for pipe progress beyond that short completion window. Requests wait for
write completion, cancellation, or the same bounded stream deadline. A queued
request canceled before starting is skipped; an active partial frame is never
interrupted except by killing the stream.

Exhausting either queue limit, a write error, or a write deadline atomically
marks the stream dead: all later sends fail, Done closes, and the owned
transport is closed. Pending peer calls fail. There is **no drop-and-continue**
mode and no automatic reconnect here; existing process owners/supervisors own
recovery. One asynchronous terminal cleanup logs only a fixed failure category,
never the frame, method, error string, or payload. Neither a transport Close nor
a slow log sink can hold a producer's internal locks.

### Reproducible sizing fixture

Run from services/nvpair-ui-broker:

```sh
go test -run TestNotificationBurstBudget -bench '^BenchmarkNotificationBurst$' -benchtime=10x -count=1 -v .
```

The synthetic fixture uses the actual WorkersResult and DirectoryNode types:
24 full twelve-worker lifecycle snapshots (two transitions per worker), 128 node
deltas (16 model names each), then one full discovery directory. On macOS arm64,
Apple M5 Max, Go 1.27.3, it produced **153 frames / 198,511 bytes**; a healthy
net.Pipe reader drained the test burst in 2.871 ms, and ten benchmark iterations
averaged 1.474 ms/burst. These are local synthetic measurements, not a bound on
real cluster size or a performance guarantee. 256 frames leave 103 slots beyond
this fixture; 16 MiB also accommodates a large engine-manager frame (its inbound
limit is 8 MiB) plus other traffic. Five seconds leaves scheduling/large-frame
headroom without an indefinite lock or request-handler wait. Oversized or
sustained bursts are intentionally terminal, not unbounded memory growth.

## Lifecycle and transport requirements

- Flush(ctx) is a bounded barrier for already accepted output. A timeout or
  context cancellation makes the stream dead; it is not a recoverable flush
  error.
- Drain(ctx) first stops admission, then flushes without closing the read half.
  Use it before control-pipe EOF while a process owner continues draining child
  stdout through cleanup.
- Close stops admission and flushes within one total five-second window, then
  closes the stream. It does not multiply the timeout by queue length.
- BindWriteContext is **abortive** on cancellation. Call Drain/Close first for
  clean shutdown; canceling a context is not a delivery acknowledgement.
- Peer.Close is abortive and wakes pending calls. Clean owners drain before
  closing control; terminal read errors and process exits abort instead.
- Production transports must provide Close that promptly interrupts Write.
  A permanently blocked arbitrary io.Writer with no effective Close cannot be
  made interruptible by Go. Finite in-memory writers are supported for tests,
  but callers inspecting their buffers must Flush first.
- NewStdioTransport takes ownership of stdin/stdout. On Unix it transfers their
  descriptors into nonblocking, runtime-poller registered files; merely closing
  an inherited blocking os.Stdout can itself wait behind a blocked Write.
  Windows uses os.File's pipe-I/O cancellation. IPC connections and subprocess
  adapters also expose interruptible closure. Windows behavior is compile/vet
  checked locally, not runtime-certified by Unix tests.

## Codec-user audit

All twelve codec-based executable entry points defer Codec.Close ahead of their
transport close. Their synchronous Notify/Respond results now mean acceptance;
no state machine is allowed to depend on physical delivery. Pairing publication
under rosterMu, peer metadata under stdinMu, error report/clear/replay, and the
public port-assignment response need no production lock restructuring: FIFO
admission preserves their transaction ordering without pipe waits. Regression
tests drive those real callers with an unread net.Pipe.

The broker's seven worker-handle kinds drain before control EOF, clipped to the
existing teardown/phase budget, and keep stdout available during successful
cleanup. Their read/write adapters close both halves on terminal failure.
The node-info frame-filter adapter forwards Close. The broker drains the
shutdown acknowledgement inside its ingress budget before canceling its codec
context. Worker main defers flush queued final responses/notifications.

The TUI's codec closes control on failure but preserves the existing stdout-drain
through the broker's bounded process join; its session observes Codec.Done and
fails pending calls immediately. Its shutdown RPC waits for the response before
control EOF. The settings projection sender remains the ordered, coalescing
domain producer; it feeds this FIFO without relying on Notify completion.
Tests that previously stalled that sender or other state machines by blocking a
notification now synchronize the actual RPC acknowledgement or explicitly flush.

node-info's applog StdinRPC uses Codec only as a reader; its separate Notifier
and direct response writer are unchanged (not Codec write users). Shared
proxyport uses Peer; its cancellation and partial-frame tests remain exercised.
The isolated cross-process tests are consumers too, but are run only in CI.
