<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Relay Protocol v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put a monotonic `seq` and a process-lifetime `epoch` on every JSON-RPC notification so a missed push becomes detectable, then delete the three desktop polling loops that exist only to compensate for its absence.

**Architecture:** Stamping lives inside `shared/jsonrpc.Codec`, not at call sites. `epoch` is minted once per process (package-level, `sync.Once`); `seq` is a per-`Codec` counter, which is already per-connection because every binary constructs one `Codec` per connection and the broker constructs a new `Broker` per client connection. The broker adds `broker:resync` and a 15 s heartbeat; clients track `epoch` and re-fetch snapshots when it changes. Payload semantics are unchanged — the worker→broker half keeps sending deltas (§2.1.1 of the spec).

**Tech Stack:** Go 1.25 (`nvpair-shared`, 13 binaries), TypeScript/Node (Electron main + preload bridge), vitest, newline-delimited JSON-RPC 2.0 over stdio/IPC.

**Spec:** `docs/superpowers/specs/2026-10-04-relay-protocol-v2-design.md` — read it alongside this plan; the plan argues from it and does not restate it.

## Global Constraints

- Go 1.25 or newer; `jq` required for build scripts.
- No `as Type`, `as any`, `: any`, or `unknown` in TypeScript signatures. No casts at all — if one seems necessary, the types are wrong.
- Absolute `@/...` imports for anything crossing directories; `./sibling` within one directory.
- No `await import()`; static imports only.
- Renderer must not import from `src/electron/`; shared code goes in `src/shared/` or behind the preload bridge.
- No legacy fallbacks, no compatibility branches — one canonical path. Delete what you replace.
- Never log prompts, messages, response bodies, pairing PINs, or key material to `slog`, `applog`, stderr, or file logs.
- Every new file carries the two-line SPDX header (NVIDIA CORPORATION & AFFILIATES / Apache-2.0). Verify with `node scripts/spdx-headers.mjs`, fix with `--fix`.
- Say "engine", not "backend", in new code and user-facing copy. Wire names and Go symbols keep their existing spelling.
- Typecheck with `npm run typecheck` (split node/web/test targets), never bare `tsc`.
- `desktop/docs/services-api.md` is generated — regenerate with `npm run service-contracts:write`, never hand-edit.
- Commit with `git commit -s`; the DCO trailer must match the commit author.
- `HEARTBEAT_INTERVAL` = 15 s, timeout = 3× interval (45 s). Values verbatim from spec §2.6.
- `epoch` = UUID v4, minted once per broker process, never persisted, never reused.
- `seq` = monotonic per connection, starts at 1.

## Review Focus

The spec implies these but no single task's tests name them. Each line gets its test in the task that owns the code.

1. **A v1 binary against a v2 broker.** Expect: the connection closes at the peer's first notification with a message naming the mismatch — not a silent hang, not a half-initialised broker that supervises a worker it cannot hear.
2. **Stamping must never touch a response frame.** A client that tolerates extra notification members will not tolerate `epoch` on a response; stamping responses breaks every strict peer on the wire.
3. **Heartbeat fan-out on many connections.** Expect exactly one in-flight heartbeat per connection, written under the codec's write lock, never one per subscriber — a tray-resident app must not wake N times per interval.
4. **Broker restart while the UI is open.** Expect one bounded burst of snapshot re-fetches, not a stampede and not a permanently stale UI.
5. **`epoch` must be invisible per-worker.** A client tracks the broker's epoch only; if a supervised worker's epoch leaks downstream, every worker restart looks like a broker restart.

---

### Task 1: Codec envelope — stamp and validate

**Files:**
- Create: `services/shared/jsonrpc/envelope.go`
- Modify: `services/shared/jsonrpc/jsonrpc.go` (`Message` struct ~L27-35, `Notify` ~L147, `Codec` struct ~L71-77, `Read` ~L105-121)
- Test: `services/shared/jsonrpc/envelope_test.go`
- Modify: `services/shared/go.mod` (`github.com/google/uuid` moves from `// indirect` to a direct require)

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `const ProtocolVersion = 2`
  - `const HeartbeatInterval = 15 * time.Second`
  - `const HeartbeatTimeout = 3 * HeartbeatInterval`
  - `func Epoch() string` — the process UUID, stable across all calls in one process.
  - `type ProtocolMismatchError struct{ Msg string }` with `func (e *ProtocolMismatchError) Error() string`.
  - `(*Codec).Notify` stamps `epoch` + `seq` on every notification it writes.
  - `(*Codec).Read` returns `*ProtocolMismatchError` (not `*DecodeError`) for an envelope violation, and `Read` is the only validation point.

- [ ] **Step 1: Write the failing tests** in `services/shared/jsonrpc/envelope_test.go`. Reuse the existing `rw`, `writeCodec`, `readCodec` helpers already in `jsonrpc_test.go` (same package).

```go
func TestNotifyStampsEpochAndSeq(t *testing.T) {
	var buf bytes.Buffer
	c := writeCodec(&buf)
	if err := c.Notify("node/updated", map[string]string{"id": "n1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if err := c.Notify("node/updated", map[string]string{"id": "n2"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(lines))
	}
	// seq starts at 1 and increases by exactly 1 per notification.
	for i, wantSeq := range []uint64{1, 2} {
		var frame struct {
			Epoch *string `json:"epoch"`
			Seq   *uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &frame); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if frame.Seq == nil || *frame.Seq != wantSeq {
			t.Errorf("frame %d seq = %v, want %d", i, frame.Seq, wantSeq)
		}
		if frame.Epoch == nil || *frame.Epoch != Epoch() {
			t.Errorf("frame %d epoch = %v, want %s", i, frame.Epoch, Epoch())
		}
	}
}
```

```go
func TestSeqIsPerCodecNotGlobal(t *testing.T) {
	if got := Epoch(); got == "" {
		t.Fatal("Epoch() is empty")
	}
	var a, b bytes.Buffer
	ac, bc := writeCodec(&a), writeCodec(&b)
	for range 3 {
		if err := ac.Notify("m", nil); err != nil {
			t.Fatalf("ac.Notify: %v", err)
		}
	}
	if err := bc.Notify("m", nil); err != nil {
		t.Fatalf("bc.Notify: %v", err)
	}
	// Each codec numbers its own connection from 1, so the second codec's
	// single frame must carry seq 1 even though the first is at seq 3.
	if want := `"seq":1`; !strings.Contains(b.String(), want) {
		t.Errorf("second connection: want %s, got %s", want, b.String())
	}
}
```

```go
func TestStampingOmitsResponsesAndRequests(t *testing.T) {
	var buf bytes.Buffer
	c := writeCodec(&buf)
	id := json.RawMessage("7")
	if err := c.Respond(&id, map[string]bool{"ok": true}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `"epoch"`) || strings.Contains(out, `"seq"`) {
		t.Errorf("response frame must not carry epoch/seq: %s", out)
	}
}
```

```go
func TestReadRejectsUnstampedNotification(t *testing.T) {
	// A v1 peer: valid JSON-RPC, no envelope stamp.
	c := readCodec(strings.NewReader(`{"jsonrpc":"2.0","method":"ready","params":{}}` + "\n"))
	_, err := c.Read()
	var pm *ProtocolMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("want *ProtocolMismatchError, got %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Errorf("error must name the missing member: %v", err)
	}
}
```

```go
func TestReadRejectsStampOnResponse(t *testing.T) {
	c := readCodec(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{},"epoch":"e","seq":1}` + "\n"))
	_, err := c.Read()
	var pm *ProtocolMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("want *ProtocolMismatchError for stamped response, got %v (%T)", err, err)
	}
}
```

```go
func TestEpochIsStableWithinProcess(t *testing.T) {
	first, second := Epoch(), Epoch()
	if first != second {
		t.Fatalf("Epoch() unstable: %q then %q", first, second)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("Epoch() is not a UUID: %q (%v)", first, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail for the right reason**

Run: `cd services/shared && go test ./jsonrpc/ -run 'Stamp|Epoch|Seq|Unstamped|StampOnResponse' -v`
Expected: compile failure — `undefined: Epoch`, `undefined: ProtocolMismatchError`. Not a syntax error, not an assertion failure.

- [ ] **Step 3: Add the envelope fields and constants**

In `jsonrpc.go` (there is no `codec.go` — `Message`, `Codec`, `Notify` and `Read` all live in this one file), add to `Message` after `Error`:

```go
	// Protocol v2: present on every notification, absent on requests and
	// responses. Their presence IS the version discriminator (§2.2).
	Epoch *string `json:"epoch,omitempty"`
	Seq   *uint64 `json:"seq,omitempty"`
```

Add `seq atomic.Uint64` to the `Codec` struct in the same file. `jsonrpc.go` currently imports `sync`; add `sync/atomic`.

- [ ] **Step 4: Create `services/shared/jsonrpc/envelope.go`** with `ProtocolVersion`, `HeartbeatInterval`, `HeartbeatTimeout`, `Epoch()` (`sync.Once` + `uuid.NewString()`), `ProtocolMismatchError`, and:

`github.com/google/uuid` is currently an `// indirect` require in `services/shared/go.mod` and is imported by no Go file — `go mod tidy` in Step 7 promotes it to direct once `envelope.go` imports it.

```go
// stamp assigns this connection's next seq and the process epoch to a
// notification frame.
func (c *Codec) stamp(msg *Message)
```

and `validate(msg *Message) error` implementing exactly the two §2.3 rules: a notification missing `epoch` or `seq` → `*ProtocolMismatchError`; `epoch` or `seq` present on a non-notification → `*ProtocolMismatchError`. Anything else → `nil`.

- [ ] **Step 5: Wire stamp into `Notify` and validate into `Read`**

`Notify` calls `c.stamp(&Message{...})` before `c.write`. `Read` calls `validate(&msg)` after the existing version check and returns the error unchanged (do **not** wrap it in `*DecodeError` — `*DecodeError` is documented as recoverable, and a protocol mismatch must be terminal).

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd services/shared && go test ./jsonrpc/ -v`
Expected: all pass, including the pre-existing `TestClassifiers` / `TestNotifyRoundTrip`.

- [ ] **Step 7: Tidy the module and commit**

```bash
cd services/shared && go mod tidy && go vet ./... && go test ./...
git add services/shared/jsonrpc services/shared/go.mod services/shared/go.sum
git commit -s -m "feat(jsonrpc): stamp epoch and seq on every notification frame"
```

---

### Task 2: Read loops treat a protocol mismatch as terminal

**Files:**
- Modify: `services/shared/jsonrpc/peer.go` (`Serve` ~L191-215, the `p.codec.Read()` call inside its loop)
- Test: `services/shared/jsonrpc/peer_test.go`

**Interfaces:**
- Consumes: `*ProtocolMismatchError` from Task 1.
- Produces: `(*Peer).Serve` returns/closes on `*ProtocolMismatchError` instead of continuing, and `Peer.Done()` closes. No new exported names.

- [ ] **Step 1: Write the failing test** in `peer_test.go`: feed a `Peer`'s reader one unstamped notification frame followed by a valid one, call `Serve` with an `onNotify` that records calls, and assert zero notifications were delivered and `Done()` closed.

```go
func TestServeStopsOnUnstampedNotification(t *testing.T) {
	in := `{"jsonrpc":"2.0","method":"ready","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"node/updated","params":{}}` + "\n"
	// ...construct a Peer over that reader, Serve in a goroutine.
	// Assert: delivered == 0, and Done() closed within ~1s.
}
```

- [ ] **Step 2: Run it to verify it fails** — Expected: the peer delivers the second frame (the loop currently continues past recoverable errors) or the test fails to compile.

- [ ] **Step 3: Implement** — in `Serve`, `errors.As(err, &pm)` → log at `slog` level `Error` with the text `protocol mismatch: %v` (do **not** log `msg.Params`; global constraint) and return, closing `done`.

- [ ] **Step 4: Run `cd services/shared && go test ./...` and `go vet ./...`** — Expected: pass.

- [ ] **Step 5: Commit** `git commit -s -m "fix(jsonrpc): end the peer read loop on a protocol mismatch"`

---

### Task 3: `broker:resync` and the heartbeat ticker

**Files:**
- Create: `services/nvpair-ui-broker/resync.go`
- Modify: `services/nvpair-ui-broker/broker.go` (add `case "broker:resync":` alongside the existing `case "ping":` ~L2803 and `case "version":`; call `b.runHeartbeat(ctx)` from `Serve` ~L1669)
- Test: `services/nvpair-ui-broker/resync_test.go`

**Interfaces:**
- Consumes: `jsonrpc.Epoch()`, `jsonrpc.ProtocolVersion`, `jsonrpc.HeartbeatInterval`, `(*Codec).Notify` (already stamping, from Task 1).
- Produces:
  - `type ResyncResult struct { Epoch string `json:"epoch"`; Protocol int `json:"protocol"` }`
  - `func (b *Broker) handleResync(id *json.RawMessage)`
  - `func (b *Broker) runHeartbeat(ctx context.Context)` — one ticker per `Broker`, i.e. per client connection.
  - Wire method name `"broker:resync"`; notification method `"heartbeat"`.

- [ ] **Step 1: Write the failing tests** — `broker:resync` returns `epoch == jsonrpc.Epoch()` and `protocol == 2`; `runHeartbeat` with a short injected interval emits `heartbeat` and stops promptly on context cancel. Give `Broker` an injected `heartbeatInterval time.Duration` defaulting to `jsonrpc.HeartbeatInterval` so the test does not sleep 15 s. Assert `epoch == ""` never appears on the wire (Review Focus 5: the client-visible epoch is the broker's).

- [ ] **Step 2: Run to verify failure** — `cd services/nvpair-ui-broker && go test ./ -run 'Resync|Heartbeat' -v` → `undefined: ResyncResult` / `handleResync`.

- [ ] **Step 3: Implement** `resync.go`. The heartbeat notification's params may be the empty object — after spec §2.1 the client needs no payload from a heartbeat, only liveness and the envelope.

- [ ] **Step 4: Run to verify pass** — `cd services/nvpair-ui-broker && go test ./...` → pass, pre-existing suites included.

- [ ] **Step 5: Commit** `git commit -s -m "feat(broker): add broker:resync and a per-connection heartbeat"`

---

### Task 4: Delete the pending-invite sweep from the desktop bridge

**Files:**
- Modify: `desktop/src/electron/service-bridge/modular-supervisor.ts` (remove `pendingInviteSweepTimer`, `startPendingInviteSweep`, `sweepPendingInvites`, and `MODULAR_INVITE_STATUS_POLL_INTERVAL_MS` if it becomes unreferenced)
- Test: existing suite must stay green; add a regression guard in `desktop/tests/modular/`.

**Interfaces:**
- Consumes: cluster-manager's existing pushes (`cluster:invite-canceled`, `cluster:invite-expired`) — already handled in `handleClusterManagerNotification`.
- Produces: no new exports. `sweepPendingInvites` must be **gone**, not no-op'd.

**Scope note:** `remoteStatusTimer` (20 s) and `node-info-poller` (2 s) are **spec B's** deletions, not this one — they need an edge-triggered push first. Deleting them here would regress peer engine status. Do not touch them.

- [ ] **Step 1: Write the failing regression guard** — a pending invite resolved while its push is suppressed must still clear, driven by the epoch resync path rather than a 1 500 ms sweep. Assert no `setInterval` with a 1500 ms period remains reachable from the bridge.
- [ ] **Step 2: Run it to verify it fails** (`npm run test:unit`) — currently the sweep exists, so the guard fails.
- [ ] **Step 3: Delete** the sweep and its timer field; keep `handleClusterManagerNotification` as the sole path.
- [ ] **Step 4: Run `npm run test:unit && npm run typecheck && npm run lint && npm run dead-code:check`** — Expected: all pass; `dead-code:check` must report 0 unused exports (it will catch a now-unreferenced constant).
- [ ] **Step 5: Commit** `git commit -s -m "refactor(desktop): drop the pending-invite sweep timer"`

---

### Task 5: Client epoch tracking — `nvpair-tui`

**Files:**
- Modify: `services/nvpair-tui/rpc/client.go` (`NewCodec` at ~L45; add epoch tracking to `Client`)
- Test: `services/nvpair-tui/rpc/client_test.go`

**Interfaces:**
- Consumes: stamped frames (Task 1), `broker:resync` (Task 3).
- Produces: `(*Client).epoch() string` and `(*Client).setEpoch(string) bool` (returns true when the epoch changed); `Client.onEpochChange(func(epoch string))` registering the snapshot re-fetch. No `time.Ticker` anywhere in this client.

- [ ] **Step 1: Write the failing tests** — (a) a frame carrying a different `epoch` triggers exactly one re-fetch burst and updates the stored epoch; (b) a `seq` gap does **not** trigger a re-fetch (spec §2.4: snapshots self-heal); (c) a frame carrying a lower `seq` than held still applies.
- [ ] **Step 2: Run to verify failure** — `cd services/nvpair-tui && go test ./rpc/ -v` → undefined `epoch`/`setEpoch`.
- [ ] **Step 3: Implement** epoch tracking in the client's notification dispatch path.
- [ ] **Step 4: Run to verify pass** — `go test ./...` in `nvpair-tui`.
- [ ] **Step 5: Commit** `git commit -s -m "feat(tui): resync on broker epoch change"`

### Task 6: Client epoch tracking — desktop bridge

**Files:**
- Modify: `desktop/src/electron/service-bridge/json-rpc-subprocess.ts` (parse `epoch`/`seq` off notification frames)
- Modify: `desktop/src/electron/service-bridge/modular-supervisor.ts` (on epoch change: invoke `broker:resync`, then the snapshot getters from spec §2.5)
- Test: `desktop/tests/modular/epoch-resync.test.ts`

**Interfaces:**
- Consumes: `broker:resync` (Task 3) → `{ epoch: string; protocol: number }`; stamped frames (Task 1).
- Produces: `bridgeEpoch(): string` and `noteEpoch(epoch: string): boolean` exported from `modular-supervisor.ts` for the Task 4 regression guard to assert against.

- [ ] **Step 1: Write the failing test** — a notification whose `epoch` differs from the stored one causes exactly one `broker:resync` call and one call per consumed snapshot getter; two consecutive frames on the same epoch cause zero; a `seq` gap causes zero.
- [ ] **Step 2: Run to verify failure** — `npm run test:unit`.
- [ ] **Step 3: Implement** — no `setInterval`; the epoch change is the only trigger.
- [ ] **Step 4: Run to verify pass** — `npm run test:unit && npm run typecheck && npm run lint`.
- [ ] **Step 5: Commit** `git commit -s -m "feat(desktop): resync cached state on broker epoch change"`

### Task 7: Cross-process enforcement, contracts, versions

**Files:**
- Create: `services/tests/protocol_v2_test.go`
- Modify: `desktop/scripts/verify-service-contracts.ts` (allowlist `broker:resync` + `heartbeat`)
- Modify: `services/versions.json`
- Regenerate: `desktop/docs/services-api.md`

**Interfaces:**
- Consumes: real broker binary + real worker binaries; `jsonrpc.Epoch()`.
- Produces: no new production API.

- [ ] **Step 1: Write the failing cross-process test** — start the real broker, open a connection, write one **unstamped** notification as if from a v1 worker, and assert the broker closes that peer and logs a mismatch (Review Focus 1). Assert deterministically: no sleeps, wait on the closed connection.
- [ ] **Step 2: Run to verify failure** — `cd services/tests && go test -run ProtocolV2 -v`.
- [ ] **Step 3: Implement** — no production change should be needed if Tasks 1–3 are correct; if this test fails, the gap is in the earlier tasks. Fix there, not here.
- [ ] **Step 4: Contracts** — `cd desktop && npm run service-contracts:write && npm run service-contracts:check` → pass.
- [ ] **Step 5: Version bumps — MAJOR** (breaking wire change per `services/VERSIONING.md`): all 13 `components.*`, plus `product` and `installer` (which always equals `product`).
- [ ] **Step 6: Full gate, all must pass** — `make check`; `cd desktop && npm run dead-code:check`; `go test ./...` in every `services/` module; `node scripts/spdx-headers.mjs`.
- [ ] **Step 7: Commit** — state in the message which components were bumped and why, so it reaches the release notes.

---

## Out of scope for this plan

`remoteStatusTimer` and `node-info-poller` deletion, GPU hysteresis, `engines:*` classification (spec B); the native app (spec C); `launchd`/Sparkle (spec D); the retained tray watchdog (spec §1 correction).
