// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// supervisedHandle is the minimal surface the supervisor needs from a
// worker-process handle. Every broker worker handle (scannerProcess,
// proxyProcess, ...) satisfies it: a channel that closes once cmd.Wait
// returns, and a Stop that tears the process down. The supervisor never
// needs the concrete type — the per-worker spawn closure is responsible
// for wiring readers and storing the concrete handle on the Broker; the
// supervisor only watches Done and calls Stop.
type supervisedHandle interface {
	// Done returns a channel that is closed when the underlying process
	// has exited (after cmd.Wait returns). The supervisor selects on it
	// to learn that a worker died.
	Done() <-chan struct{}
	// Stop tears the worker down (closes its stdin, which the worker observes
	// as EOF and treats as its shutdown signal) and blocks until it has
	// exited. The wait is bounded and escalates to a signal and then a kill
	// (see waitForStdinClose). Safe to call multiple times.
	Stop()
}

// WorkerState is the broker-reported lifecycle state of a worker.
type WorkerState string

const (
	WorkerRunning    WorkerState = "running"
	WorkerDegraded   WorkerState = "degraded"
	WorkerRestarting WorkerState = "restarting"
	WorkerExhausted  WorkerState = "exhausted"
	// WorkerMissing is assigned by the registry when a binary cannot be resolved.
	WorkerMissing WorkerState = "missing"
)

// WorkerStatus is a snapshot of one worker's supervision state.
type WorkerStatus struct {
	Name          string      `json:"name"`
	State         WorkerState `json:"state"`
	Attempts      int         `json:"attempts"`
	PID           int         `json:"pid,omitempty"`
	LastError     string      `json:"lastError,omitempty"`
	RestartOnHang bool        `json:"restartOnHang"`
	Since         time.Time   `json:"since"`
}

// hangPolicy probes the live worker's control loop, not its domain operations.
// The probe should honour ctx; its result is also bounded by deadline in watch.
// A nil probe disables probing. restartAfter is ignored when restart is false.
type hangPolicy struct {
	probe                      func(ctx context.Context) error
	interval, deadline         time.Duration
	degradeAfter, restartAfter int
	restart                    bool
}

func (p hangPolicy) withDefaults() hangPolicy {
	if p.interval <= 0 {
		p.interval = 30 * time.Second
	}
	if p.deadline <= 0 {
		p.deadline = 5 * time.Second
	}
	if p.degradeAfter <= 0 {
		p.degradeAfter = 2
	}
	if p.restartAfter <= 0 {
		p.restartAfter = 3
	}
	return p
}

// restartPolicy configures the hybrid auto-restart behaviour the broker
// applies to a crashed worker. The defaults (see defaultRestartPolicy)
// implement: exponential backoff between attempts, a bounded budget so a
// hard-broken binary doesn't spin forever, and a "healthy reset" window —
// a worker that stays up that long and answers at least one probe is
// considered recovered, clearing its crash error and resetting the counter
// so a much-later crash gets a fresh budget rather than tripping exhaustion.
type restartPolicy struct {
	// baseDelay is the backoff before the first restart attempt.
	baseDelay time.Duration
	// maxDelay caps the exponential backoff so it never grows unbounded.
	maxDelay time.Duration
	// maxAttempts is the restart budget within a single unhealthy streak.
	// Once exceeded the supervisor gives up and leaves the worker down
	// (its crash error stays sticky). A value <= 0 disables auto-restart
	// entirely — the crash is surfaced but the worker is not respawned.
	maxAttempts int
	// healthyReset is how long a freshly (re)started worker must stay up,
	// with at least one successful probe, before the attempt counter resets to 0
	// and onRecovered fires. A value <= 0 disables the healthy-reset /
	// auto-clear behaviour (every crash counts against the budget forever).
	healthyReset time.Duration
}

// defaultRestartPolicy is the hybrid policy the broker uses for every
// auto-restarted worker: ~1s -> 16s exponential backoff, a budget of 5
// attempts per unhealthy streak, and a 60s healthy-reset window. Tuned to
// recover quickly from a transient crash while not hammering a binary
// that's deterministically broken.
func defaultRestartPolicy() restartPolicy {
	return restartPolicy{
		baseDelay:    1 * time.Second,
		maxDelay:     16 * time.Second,
		maxAttempts:  5,
		healthyReset: 60 * time.Second,
	}
}

// noRestartPolicy disables auto-restart: a crashed worker is surfaced (via
// onCrash) but never respawned. It preserves the broker's historical
// "log and carry on" behaviour while still routing the worker through the
// supervision framework, and is the policy used until auto-restart is
// switched on for a worker.
func noRestartPolicy() restartPolicy {
	return restartPolicy{}
}

// backoff returns the delay before the given 1-indexed restart attempt:
// baseDelay * 2^(attempt-1), capped at maxDelay. The doubling is done in a
// loop with an early return at the cap so a large attempt count can never
// overflow the duration.
func (p restartPolicy) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := p.baseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.maxDelay {
			return p.maxDelay
		}
	}
	if d > p.maxDelay {
		return p.maxDelay
	}
	return d
}

// watchResult is the outcome of watching a single running worker handle.
type watchResult int

const (
	// watchStopped means the supervisor was asked to stop (Stop closed
	// stopCh) while the worker was still running.
	watchStopped watchResult = iota
	// watchExited means the worker process exited on its own (an
	// unexpected death the supervisor should react to).
	watchExited
	// watchRestart means Restart was called: stop the current (healthy)
	// worker and respawn it promptly, without counting it as a crash.
	watchRestart
	// watchUnresponsive means the hang policy requires a bounded teardown
	// followed by crash recovery, consuming the same restart budget as exits.
	watchUnresponsive
)

// supervisor owns the lifecycle of a single broker worker: it spawns the
// worker, watches it for unexpected exit, and restarts it per the
// restartPolicy. One supervisor instance manages one logical worker
// (re-spawning produces a fresh OS process each time).
//
// The supervisor is deliberately decoupled from the concrete worker type:
// spawn returns a supervisedHandle and is expected to have already wired
// the worker's reader goroutines and stored the concrete handle on the
// Broker (under whatever lock that field needs) as a side effect. That
// keeps the broker's request handlers reading b.proxy / b.scanner / ...
// directly while the supervisor transparently swaps the process underneath
// on a restart.
type supervisor struct {
	name   string
	policy restartPolicy
	hang   hangPolicy
	// onState publishes lifecycle changes and attempt-budget updates. Like the
	// other callbacks it runs on the monitor goroutine and must not block.
	onState func(WorkerStatus)
	// spawn starts a fresh worker process, wires its readers, stores the
	// concrete handle on the Broker, and returns the handle for the
	// supervisor to watch. It is called once by Start and again on every
	// restart.
	spawn func() (supervisedHandle, error)

	// onCrash, if set, is invoked on every unexpected exit with the
	// 1-indexed attempt number. The broker uses it to emit a
	// supervisor:subprocess-crashed:<name> error into the nvpair-errors
	// pipeline. Hangs also invoke it, after onState publishes LastError
	// "unresponsive". Invoked on the monitor goroutine.
	onCrash func(attempt int)
	// onRecovered, if set, is invoked when a restarted worker has stayed
	// up for healthyReset and answered at least one probe since that spawn.
	// The broker uses it to clear the crash error.
	// Invoked on the monitor goroutine.
	onRecovered func()
	// onExhausted, if set, is invoked once when the worker has no remaining
	// restart path and is terminally down. Invoked on the monitor goroutine
	// before it waits for Stop or an explicit Retry.
	onExhausted func(attempt int)

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}

	// restartCh requests a graceful restart of the current worker process so
	// it re-reads external state it only loads at startup — e.g. cluster
	// identity/pins that appear under --cluster-dir when this node joins a
	// cluster, flipping the worker from plain HTTP to pin-gated mTLS (a
	// bind-time decision). Buffered + coalesced via Restart.
	restartCh chan struct{}
}

// newSupervisor constructs a supervisor for one worker. onCrash and
// onRecovered are set by the caller as needed before Start.
func newSupervisor(name string, policy restartPolicy, spawn func() (supervisedHandle, error)) *supervisor {
	return &supervisor{
		name:      name,
		policy:    policy,
		spawn:     spawn,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		restartCh: make(chan struct{}, 1),
	}
}

// Start spawns the worker's first instance synchronously and, on success,
// launches the monitor goroutine that watches for exits and restarts. The
// synchronous first spawn lets a caller treat a hard spawn failure as
// fatal (e.g. the scanner) by inspecting the returned error. Stop must
// only be called after Start has returned nil.
func (s *supervisor) Start() error {
	h, err := s.spawn()
	if err != nil {
		return err
	}
	go s.monitor(h, nil)
	return nil
}

// StartRetrying attempts an optional worker's first spawn, then monitors even
// if it failed. Initial failures and later respawn failures share one budget.
// Unlike Start, it always establishes a monitor and is always safe to Stop.
func (s *supervisor) StartRetrying() {
	h, err := s.spawn()
	go s.monitor(h, err)
}

// workerStopReportAfter is how long a worker may take to tear down before this
// starts naming it. The whole tree normally stops in well under a second, so a
// worker past this is either doing real work or wedged.
const workerStopReportAfter = 2 * time.Second

// Stop signals the supervisor to shut down and blocks until the monitor
// goroutine has torn the current worker down and exited. Idempotent.
//
// Slow teardown is reported while the handle's bounded EOF / signal / kill
// escalation (see waitForStdinClose) completes. Probes are cancelled before
// stopping the handle and cannot hold this join beyond their deadline.
func (s *supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })

	started := time.Now()
	select {
	case <-s.doneCh:
		return
	case <-time.After(workerStopReportAfter):
	}

	slog.Warn("supervisor: worker still stopping",
		"worker", s.name, "waitedMs", time.Since(started).Milliseconds())
	<-s.doneCh
	slog.Info("supervisor: worker stopped",
		"worker", s.name, "waitedMs", time.Since(started).Milliseconds())
}

// Restart asks the supervisor to stop the current worker process and spawn a
// fresh one, without counting it as a crash. Used to re-point a worker at
// external state it only reads at startup — e.g. cluster identity/pins that
// appear under --cluster-dir when this node joins a cluster, which flip the
// worker from plain HTTP to pin-gated mTLS (a bind-time decision). Non-blocking
// and coalescing: a burst collapses to one restart. A no-op once Stopped.
func (s *supervisor) Restart() {
	if s.stopping() {
		return
	}
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
}

// Retry explicitly restarts a live worker, or resets an exhausted worker's
// budget and immediately tries spawning it again. This also applies to scanner.
func (s *supervisor) Retry() { s.Restart() }

func (s *supervisor) stopping() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// monitor is the supervisor's control loop. It watches the current handle;
// on an unexpected exit it surfaces the crash, then backs off and respawns
// per the policy, until either the worker recovers (resetting the budget),
// the restart budget is exhausted (leaving the worker down with a sticky
// crash error), or Stop is called.
func (s *supervisor) monitor(h supervisedHandle, spawnErr error) {
	defer close(s.doneCh)

	attempts := 0
	lastError := ""
	status := WorkerStatus{Name: s.name, RestartOnHang: s.hang.probe != nil && s.hang.restart}
	publish := func(state WorkerState, live supervisedHandle, reason string) {
		if status.State != state {
			status.Since = time.Now()
		}
		status.State, status.Attempts, status.LastError = state, attempts, reason
		status.PID = 0
		// Concrete handles may expose their process ID without widening the
		// minimal lifecycle interface (including existing test doubles).
		if process, ok := live.(interface{ PID() int }); ok {
			status.PID = process.PID()
		}
		if s.onState != nil {
			s.onState(status)
		}
	}

	crashed := false
	// Explicit retries may reset attempts before health is proven. Remember
	// any reported crash independently so a later healthy window still clears
	// its sticky error through onRecovered.
	recoveryPending := false
	for {
		if s.stopping() {
			if h != nil {
				h.Stop()
			}
			return
		}
		if h != nil {
			publish(WorkerRunning, h, lastError)
			res := s.watch(h, func() {
				if attempts != 0 || recoveryPending {
					attempts, lastError, recoveryPending = 0, "", false
					reason := status.LastError
					if status.State == WorkerRunning {
						reason = ""
					}
					publish(status.State, h, reason)
					slog.Info("supervisor: worker recovered (stable)", "worker", s.name)
					if s.onRecovered != nil {
						s.onRecovered()
					}
				}
			}, func(err error) {
				if err != nil {
					publish(WorkerDegraded, h, err.Error())
				} else {
					publish(WorkerRunning, h, lastError)
				}
			})
			if res == watchStopped {
				h.Stop()
				return
			}
			if res == watchRestart || res == watchUnresponsive {
				// Both paths use the handle's existing bounded Stop escalation.
				// Only a hang is charged as a crash; requested restarts are free.
				h.Stop()
			}
			h = nil
			if s.stopping() {
				return
			}
			if res == watchRestart {
				publish(WorkerRestarting, nil, "")
				h, spawnErr = s.spawn()
				if spawnErr == nil {
					attempts, lastError = 0, ""
					continue
				}
			} else if res == watchUnresponsive {
				spawnErr = errors.New("unresponsive")
			} else {
				spawnErr = errors.New("exited unexpectedly")
			}
			crashed = true
		}

		// Every failed spawn, exit, and hang consumes the same unhealthy
		// streak. Initial instance + maxAttempts replacements are permitted.
		attempts++
		lastError = spawnErr.Error()
		exhausted := s.exhausted(attempts)
		state := WorkerRestarting
		if exhausted {
			state = WorkerExhausted
		}
		publish(state, nil, lastError)
		slog.Warn("supervisor: worker unavailable", "worker", s.name, "attempt", attempts, "reason", lastError)
		if crashed {
			recoveryPending = true
			if s.onCrash != nil {
				s.onCrash(attempts)
			}
		}
		if exhausted {
			s.notifyExhausted(attempts)
			select {
			case <-s.stopCh:
				return
			case <-s.restartCh:
				attempts, lastError = 0, ""
			}
		} else if !s.sleep(s.policy.backoff(attempts)) {
			return
		}
		if s.stopping() {
			return
		}
		h, spawnErr = s.spawn()
		// Failed respawns advance the budget and publish their error, but do
		// not repeat the legacy onCrash callback for the already-dead handle.
		crashed = false
	}
}

// exhausted reports whether the supervisor should stop restarting the
// worker — either because auto-restart is disabled (maxAttempts <= 0) or
// because the attempt budget for the current unhealthy streak is spent —
// and logs the reason. When it returns true the caller leaves the worker
// down (its crash error stays sticky) until Stop or an explicit Retry.
func (s *supervisor) exhausted(attempts int) bool {
	if s.policy.maxAttempts <= 0 {
		slog.Warn("supervisor: auto-restart disabled; leaving worker down", "worker", s.name)
		return true
	}
	if attempts > s.policy.maxAttempts {
		slog.Warn("supervisor: restart budget exhausted; leaving worker down", "worker", s.name, "attempts", attempts)
		return true
	}
	return false
}

func (s *supervisor) notifyExhausted(attempts int) {
	if s.onExhausted != nil {
		s.onExhausted(attempts)
	}
}

// watch owns timers and probe results for exactly one live handle. Cancelling
// its in-flight probe on every exit fences late results from later generations.
// Healthy reset requires both the uptime window and a successful probe, in
// either order. Probes run independently so Stop never waits on a probe call.
func (s *supervisor) watch(h supervisedHandle, onStable func(), onHealth func(error)) watchResult {
	var healthyC <-chan time.Time
	if s.policy.healthyReset > 0 {
		t := time.NewTimer(s.policy.healthyReset)
		defer t.Stop()
		healthyC = t.C
	}
	policy := s.hang.withDefaults()
	var tickC <-chan time.Time
	if policy.probe != nil {
		ticker := time.NewTicker(policy.interval)
		defer ticker.Stop()
		tickC = ticker.C
	}
	var probeC <-chan error
	var deadlineC <-chan struct{}
	var probeCtx context.Context
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	failures := 0
	degraded, uptimeMet, probeSucceeded, stable := false, false, false, false
	markStable := func() {
		if !stable && uptimeMet && probeSucceeded {
			stable = true
			if onStable != nil {
				onStable()
			}
		}
	}
	probeFinished := func(err error) bool {
		cancel()
		probeC, deadlineC = nil, nil
		if err == nil {
			failures, probeSucceeded = 0, true
			if degraded {
				degraded = false
				onHealth(nil)
			}
			markStable()
			return false
		}
		failures++
		if !degraded && failures >= policy.degradeAfter {
			degraded = true
			onHealth(err)
		}
		return policy.restart && failures >= policy.restartAfter
	}
	for {
		// Prefer an already-pending Stop over launching another probe.
		if s.stopping() {
			return watchStopped
		}
		select {
		case <-s.stopCh:
			return watchStopped
		case <-s.restartCh:
			return watchRestart
		case <-healthyC:
			healthyC = nil
			uptimeMet = true
			markStable()
		case <-tickC:
			if probeC != nil {
				continue // Never overlap probes for the same live handle.
			}
			probeCtx, cancel = context.WithTimeout(context.Background(), policy.deadline)
			result := make(chan error, 1)
			probeC, deadlineC = result, probeCtx.Done()
			go func(ctx context.Context) { result <- policy.probe(ctx) }(probeCtx)
		case err := <-probeC:
			if probeCtx.Err() != nil {
				err = probeCtx.Err() // A late success cannot beat the deadline.
			}
			if probeFinished(err) {
				return watchUnresponsive
			}
		case <-deadlineC:
			if probeFinished(probeCtx.Err()) {
				return watchUnresponsive
			}
		case <-h.Done():
			return watchExited
		}
	}
}

// sleep waits for d, returning true if the full delay elapsed and false if
// Stop was called first. A non-positive delay returns immediately (true
// unless already stopped) so a zero backoff still honours a pending stop.
func (s *supervisor) sleep(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-s.stopCh:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-t.C:
		return true
	}
}
