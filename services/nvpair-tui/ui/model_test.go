// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests drive the epoch consumer the way the shipped program does: a
// broker frame is written to a net.Pipe, the real rpc.Client parses it, the
// callback registered the way supervisor.Spawn registers it publishes the epoch,
// waitForEpochChange turns it into an EpochChangeMsg, and Model.Update decides
// what to re-fetch. Nothing calls observeEpoch or setEpoch by hand, so a
// regression anywhere along that path shows up as a missing or extra call on the
// wire rather than as an assertion that cannot fail.

// snapshotReads is the whole whitelist: every method one epoch change may
// re-fetch, each exactly once.
var snapshotReads = []string{
	"engine:get-installed",
	"nodes:get-initial",
	"cluster:get-node-id",
	"errors:get-initial",
}

// openSubscriptions are the pushes the views hold open across a restart. The
// tests open them over the same client first, so "the burst must not
// re-subscribe" is a comparison against a session that genuinely holds them
// rather than against an idle wire.
var openSubscriptions = []string{
	"workloads:subscribe",
	"engine:subscribe",
	"discovery:subscribe",
}

// mustNeverFetch lists what the burst must not emit even once: the
// subscriptions above, and every mutation reachable from the TUI. Re-issuing a
// subscription duplicates server-side state; re-issuing a mutation takes a real
// action on a user's machine because a protocol banner changed. The names are
// the ones the views actually send, colon-style and slash-style alike.
var mustNeverFetch = []string{
	"workloads:subscribe",
	"engine:subscribe",
	"discovery:subscribe",
	"workloads:upsert",
	"workloads:remove",
	"nodes:remove",
	"nodes/list",
	"node/add",
	"node/remove",
	"errors:clear",
	"engine:install",
	"engine:uninstall",
	"engine:start",
	"engine:stop",
	"engine:restart",
	"engine:action",
	"cluster:invite-node",
	"cluster:respond-to-invite",
	"cluster:leave",
}

// fakeBroker is the server end of a net.Pipe. It answers every request with an
// empty JSON object and counts the methods it saw, so a test can state exactly
// what reached the wire. hold() stops it answering, which keeps a burst in
// flight while further epoch changes arrive.
type fakeBroker struct {
	clientConn net.Conn
	serverConn net.Conn
	codec      *rpc.Codec

	mu     sync.Mutex
	counts map[string]int
	gate   chan struct{}
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	cli, srv := net.Pipe()
	b := &fakeBroker{
		clientConn: cli,
		serverConn: srv,
		codec:      rpc.NewCodec(srv),
		counts:     map[string]int{},
	}
	go b.serve()
	t.Cleanup(func() {
		b.release()
		_ = cli.Close()
		_ = srv.Close()
	})
	return b
}

func (b *fakeBroker) serve() {
	for {
		msg, err := b.codec.Read()
		if err != nil {
			return
		}
		if msg.ID == nil {
			continue
		}
		b.record(msg.Method)
		if gate := b.currentGate(); gate != nil {
			<-gate
		}
		_ = b.codec.Respond(msg.ID, struct{}{})
	}
}

func (b *fakeBroker) record(method string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counts[method]++
}

func (b *fakeBroker) count(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[method]
}

func (b *fakeBroker) snapshotCounts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.counts))
	for method, n := range b.counts {
		out[method] = n
	}
	return out
}

func (b *fakeBroker) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.counts {
		n += c
	}
	return n
}

func (b *fakeBroker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counts = map[string]int{}
}

// hold stops the broker answering requests. The reads it already accepted stay
// outstanding, which is the state the coalescing guard has to survive.
func (b *fakeBroker) hold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gate = make(chan struct{})
}

func (b *fakeBroker) release() {
	b.mu.Lock()
	gate := b.gate
	b.gate = nil
	b.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (b *fakeBroker) currentGate() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gate
}

// writeNotif stamps one notification frame by hand. The canonical codec stamps
// only its own connection's epoch, so a test that has to speak as a restarted
// broker writes the envelope itself. Each call happens while the fake broker is
// parked in Read, never mid-response, so a raw write cannot interleave with one
// of the codec's own frames.
func writeNotif(t *testing.T, w io.Writer, method, epoch string, seq int64) {
	t.Helper()
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{},"epoch":%q,"seq":%d}`+"\n", method, epoch, seq)
	if _, err := io.WriteString(w, raw); err != nil {
		t.Fatalf("write %q frame: %v", method, err)
	}
}

// loop drives a Model the way the Bubble Tea program does: every command runs on
// its own goroutine, every result lands in the inbox, and the test feeds the
// inbox into Update one message at a time. That keeps exactly one waiter parked
// on the epoch channel, so a frame cannot be consumed twice.
//
// Batch expansion happens where bubbletea's own runtime does it: when a command
// is CALLED. tea.Cmd is a concrete func type, so batch membership is not a
// property of the value — tea.Batch returns one Cmd that, when called, returns a
// tea.BatchMsg of the commands to run. tea.Batch of a single command returns
// that command unwrapped, so nothing here can tell a one-command burst from a
// plain command by inspection; only the returned Msg says which it was.
type loop struct {
	m     Model
	inbox chan tea.Msg
}

func newLoop(m Model) *loop {
	return &loop{m: m, inbox: make(chan tea.Msg, 64)}
}

func (l *loop) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, inner := range batch {
				l.exec(inner)
			}
			return
		}
		if msg != nil {
			l.inbox <- msg
		}
	}()
}

// next waits for the next message the update loop produced.
func (l *loop) next(t *testing.T) tea.Msg {
	t.Helper()
	select {
	case msg := <-l.inbox:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("no message reached the update loop within 3s")
	}
	return nil
}

// nextEpoch waits for the next EpochChangeMsg and returns it. Anything else in
// the inbox belongs to a view — the decode result of one of the burst's own
// reads — and goes back through Update the way the Bubble Tea program routes it,
// so the model keeps its state while the test keeps looking for an epoch. Those
// handlers all return nil, so routing them costs no extra call on the wire and
// cannot inflate the counts the tests assert on.
func (l *loop) nextEpoch(t *testing.T) EpochChangeMsg {
	t.Helper()
	for {
		msg := l.next(t)
		if epoch, ok := msg.(EpochChangeMsg); ok {
			return epoch
		}
		l.turn(t, msg)
	}
}

// turn feeds msg into Update and fans out the command it returns. It reports
// nothing: the fired set is not observable through a tea.Cmd, so the burst is
// proven on the wire by the broker-side counts instead.
func (l *loop) turn(t *testing.T, msg tea.Msg) {
	t.Helper()
	next, cmd := l.m.Update(msg)
	m, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want ui.Model", next)
	}
	l.m = m
	l.exec(cmd)
}

// quietUntil polls cond so a test never depends on how long a goroutine takes.
func quietUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

// assertQuiet checks the update loop reports no epoch change for the whole of d.
// Non-epoch messages are routed on instead of being counted as silence, so a
// decode result left in the inbox by an earlier burst cannot satisfy this early.
// It is only meaningful once the caller has proved the frame that could have
// produced an epoch was processed.
func assertQuiet(t *testing.T, l *loop, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case msg := <-l.inbox:
			if epoch, ok := msg.(EpochChangeMsg); ok {
				t.Fatalf("update loop reported an epoch change for epoch %q, which was already current", epoch.Epoch)
			}
			l.turn(t, msg)
		case <-deadline:
			return
		}
	}
}

// awaitNotification consumes pushes until the named one arrives, proving the
// client's read loop processed that frame.
func awaitNotification(t *testing.T, client *rpc.Client, method string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-client.Notifications():
			if !ok {
				t.Fatalf("notification stream closed before %q", method)
			}
			if msg.Method == method {
				return
			}
		case <-deadline:
			t.Fatalf("notification %q did not arrive within 3s", method)
		}
	}
}

// startEpochSession wires a client to a fake broker and registers the epoch
// consumer exactly the way supervisor.Spawn does — before Run — then arms the
// single waiter the update loop keeps on the epoch channel and opens the push
// subscriptions the views hold across a restart.
func startEpochSession(t *testing.T) (*fakeBroker, *rpc.Client, *loop) {
	t.Helper()
	broker := newFakeBroker(t)
	client := rpc.NewClient(broker.clientConn, broker.clientConn)

	epochCh := make(chan string, 1)
	client.OnEpochChange(func(epoch string) {
		select {
		case epochCh <- epoch:
		default:
			select {
			case <-epochCh:
			default:
			}
			select {
			case epochCh <- epoch:
			default:
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go client.Run(ctx)

	l := newLoop(New(client, nil, epochCh, defaultViews(client)))
	l.exec(waitForEpochChange(epochCh))

	for _, method := range openSubscriptions {
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client.Call(cctx, method, nil)
		ccancel()
		if err != nil {
			t.Fatalf("open %s: %v", method, err)
		}
	}
	broker.reset()
	return broker, client, l
}

// assertBurstIsReadOnly checks that exactly the whitelisted reads reached the
// wire, once each per burst, and that nothing else did.
func assertBurstIsReadOnly(t *testing.T, counts map[string]int, bursts int) {
	t.Helper()
	for _, method := range snapshotReads {
		if got := counts[method]; got != bursts {
			t.Errorf("%s reached the wire %d time(s), want %d", method, got, bursts)
		}
	}
	for method, got := range counts {
		known := false
		for _, want := range snapshotReads {
			if want == method {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("%s reached the wire %d time(s) but is outside the read-only whitelist", method, got)
		}
	}
	for _, method := range mustNeverFetch {
		if got := counts[method]; got != 0 {
			t.Errorf("burst emitted %s %d time(s); it must never be re-fetched", method, got)
		}
	}
}

// TestEpochChangeRefetchesReadOnlySnapshotsOnce is the consumer the review found
// missing: one broker:epoch frame on the wire produces exactly one burst, and
// that burst is the four read snapshots and nothing else.
//
// Falsifier: delete the EpochChangeMsg case from Model.Update (so the message
// falls through to the default broadcast) and no read is issued, so the wait for
// the burst fails; dropping any view's epochSnapshotCmds fails the per-method
// count, and adding a subscribe or mutation to one fails the whitelist check.
func TestEpochChangeRefetchesReadOnlySnapshotsOnce(t *testing.T) {
	broker, _, l := startEpochSession(t)

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-A", 1)
	first := l.nextEpoch(t)
	if first.Epoch != "epoch-A" {
		t.Errorf("EpochChangeMsg carries %q, want epoch-A", first.Epoch)
	}

	l.turn(t, first)
	// The burst is proven on the wire, not by the returned command: quietUntil
	// waits for all four reads to arrive and assertBurstIsReadOnly checks that
	// exactly those four arrived, once each, and nothing outside the whitelist.
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	assertBurstIsReadOnly(t, broker.snapshotCounts(), 1)

	if l.m.epoch != "epoch-A" {
		t.Errorf("model recorded epoch %q, want epoch-A", l.m.epoch)
	}
	quietUntil(t, func() bool { return l.m.epochReloading.Load() == 0 })
}

// TestSeqGapWithUnchangedEpochDoesNotRefetch pins settled semantics: a frame
// under the current epoch with a gapped seq is delivered to the update loop and
// fetches nothing, while a genuinely new epoch still gets its burst.
//
// Falsifier: make Client.setEpoch report a change on an unchanged epoch (or make
// the supervisor publisher fire on every notification) and the gapped frame
// re-fetches, failing the zero-call check; make Model.Update fetch on
// NotificationMsg and it fails the same way.
func TestSeqGapWithUnchangedEpochDoesNotRefetch(t *testing.T) {
	broker, client, l := startEpochSession(t)

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-A", 1)
	first := l.nextEpoch(t)
	l.turn(t, first)
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	broker.reset()

	writeNotif(t, broker.serverConn, "errors:update", "epoch-A", 99)
	awaitNotification(t, client, "errors:update")
	assertQuiet(t, l, 300*time.Millisecond)
	if got := broker.total(); got != 0 {
		t.Errorf("a seq gap under an unchanged epoch fetched %d time(s), want 0", got)
	}

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-B", 100)
	second := l.nextEpoch(t)
	l.turn(t, second)
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	assertBurstIsReadOnly(t, broker.snapshotCounts(), 1)
}

// TestEpochChangesCoalesceWhileBurstInFlight holds the relay open so the second
// and third epoch change land mid-burst: they must not stack extra calls, the
// newest epoch must win, and the outstanding burst must still finish.
//
// Falsifier: remove the epochReloading guard in Model.Update and three epoch
// changes produce twelve calls instead of four, failing both the mid-burst count
// and the final per-method count.
func TestEpochChangesCoalesceWhileBurstInFlight(t *testing.T) {
	broker, _, l := startEpochSession(t)
	broker.hold()

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-A", 1)
	first := l.nextEpoch(t)
	l.turn(t, first)
	quietUntil(t, func() bool { return l.m.epochReloading.Load() == int64(len(snapshotReads)) })
	// Take the baseline only once the held broker has demonstrably accepted a
	// request of this burst. Reading the count immediately after Update would
	// race the serve loop: the fetch goroutines have been started, but the fake
	// broker reads one request at a time, so the first may not be recorded yet.
	// What matters is that nothing NEW lands on the wire while a burst is in
	// flight, so the assertion compares against the accepted count, not a guess.
	quietUntil(t, func() bool { return broker.total() >= 1 })
	accepted := broker.total()

	for _, epoch := range []string{"epoch-B", "epoch-C"} {
		writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, epoch, 2)
		l.turn(t, l.nextEpoch(t))
	}

	if got := broker.total(); got != accepted {
		t.Errorf("mid-burst epoch changes stacked the wire: %d call(s), want no growth over the %d already accepted", got, accepted)
	}
	if got := l.m.epochReloading.Load(); got != int64(len(snapshotReads)) {
		t.Errorf("guard counts %d outstanding fetch(es), want %d", got, len(snapshotReads))
	}
	if l.m.epoch != "epoch-C" {
		t.Errorf("epoch is %q, want the newest epoch, epoch-C", l.m.epoch)
	}

	broker.release()
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	quietUntil(t, func() bool { return l.m.epochReloading.Load() == 0 })
	assertBurstIsReadOnly(t, broker.snapshotCounts(), 1)
}

// TestDistinctEpochAfterBurstRefetchesAgain covers the other half of coalescing:
// once a burst has completed the same epoch stays silent, and the next distinct
// epoch fetches again through the re-armed waiter.
//
// Falsifier: never re-arm waitForEpochChange in Model.Update and the second
// distinct epoch never reaches the loop, failing the burst wait; or make the
// guard stick at a non-zero value and the second burst is suppressed.
func TestDistinctEpochAfterBurstRefetchesAgain(t *testing.T) {
	broker, _, l := startEpochSession(t)

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-A", 1)
	first := l.nextEpoch(t)
	l.turn(t, first)
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	assertBurstIsReadOnly(t, broker.snapshotCounts(), 1)
	broker.reset()

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-A", 5)
	assertQuiet(t, l, 300*time.Millisecond)
	if got := broker.total(); got != 0 {
		t.Errorf("an epoch already fetched caused %d call(s), want 0", got)
	}

	writeNotif(t, broker.serverConn, rpc.BrokerEpochMethod, "epoch-B", 6)
	second := l.nextEpoch(t)
	l.turn(t, second)
	quietUntil(t, func() bool { return broker.total() == len(snapshotReads) })
	assertBurstIsReadOnly(t, broker.snapshotCounts(), 1)
	if l.m.epoch != "epoch-B" {
		t.Errorf("model recorded epoch %q, want epoch-B", l.m.epoch)
	}
}
