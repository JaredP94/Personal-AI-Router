//go:build !windows

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const macmonTestLine = `{"timestamp":"2026-11-02T06:29:04Z","temp":{"cpu_temp_avg":40.5}}`

// waitFor is also used by the darwin/arm64 sensor collector tests.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition not met within 2s")
		case <-tick.C:
		}
	}
}

func fakeMacmon(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "macmon")
	body := "#!/bin/sh\nDIR=${0%/*}\nprintf '%s\\n' \"$*\" >> \"$DIR/args\"\nLINE='" + macmonTestLine + "'\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func macmonTestConfig(paths ...string) macmonConfig {
	return macmonConfig{
		Paths: paths, Interval: func() time.Duration { return 250 * time.Millisecond },
		BackoffMin: 10 * time.Millisecond, BackoffMax: 80 * time.Millisecond,
		CheckEvery: 20 * time.Millisecond,
	}
}

func runMacmonSupervisor(t *testing.T, s *macmonSupervisor) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("supervisor did not stop within 2s")
		}
	})
	return cancel, done
}

func macmonArgs(path string) []string {
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "args"))
	return strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
}

func TestMacmonSupervisorMissingBinary(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := macmonTestConfig(filepath.Join(dir, "missing"), dir, notExecutable)
	var probes atomic.Int64
	cfg.Interval = func() time.Duration {
		probes.Add(1)
		return 250 * time.Millisecond
	}
	s := newMacmonSupervisor(cfg)
	runMacmonSupervisor(t, s)
	// Observe the missing-path retry timer, not just the constructor's state.
	waitFor(t, func() bool { return probes.Load() > 0 })
	waitFor(t, func() bool {
		status, sample, interval := s.Latest()
		return status == MacmonMissing && sample == nil && interval == 250*time.Millisecond
	})
}

func TestMacmonSupervisorPublishesSamples(t *testing.T) {
	path := fakeMacmon(t, `while :; do printf '%s\n' "$LINE"; sleep 0.05; done`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonAvailable && sample != nil && sample.CPUTempC != nil && *sample.CPUTempC == 40.5
	})
}

func TestMacmonSupervisorPassesIntervalMs(t *testing.T) {
	path := fakeMacmon(t, `exec sleep 30`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		args := macmonArgs(path)
		return len(args) == 1 && args[0] == "pipe -i 250"
	})
}

func TestMacmonSupervisorStartedChildIsNotMissing(t *testing.T) {
	path := fakeMacmon(t, `exec sleep 30`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { return len(macmonArgs(path)) == 1 })
	status, sample, _ := s.Latest()
	if status != MacmonFailed || sample != nil {
		t.Fatalf("started child status/sample = %q/%v, want failed/nil", status, sample)
	}
}

func TestMacmonSupervisorRestartsOnIntervalChange(t *testing.T) {
	// Only the original child publishes: the replacement must retain available
	// and the original sample even while it has not produced any stdout.
	path := fakeMacmon(t, `printf '%s\n' "$$" >> "$DIR/pids"
if [ "$3" = 250 ]; then printf '%s\n' "$LINE"; exec sleep 30; fi
exec /bin/cat "$DIR/lines"`)
	fifo := filepath.Join(filepath.Dir(path), "lines")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := os.OpenFile(fifo, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lines.Close() })
	var fast atomic.Bool
	cfg := macmonTestConfig(path)
	cfg.Interval = func() time.Duration {
		if fast.Load() {
			return 100 * time.Millisecond
		}
		return 250 * time.Millisecond
	}
	// A mistaken failure/backoff on policy changes will miss the 500ms bound.
	cfg.BackoffMin = time.Second
	cfg.BackoffMax = time.Second
	s := newMacmonSupervisor(cfg)
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		status, _, interval := s.Latest()
		return status == MacmonAvailable && interval == 250*time.Millisecond
	})
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "pids"))
	if err != nil {
		t.Fatal(err)
	}
	oldPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || oldPID <= 0 {
		t.Fatalf("invalid original child PID: %q, %v", data, err)
	}
	_, originalSample, _ := s.Latest()
	changedAt := time.Now()
	fast.Store(true)
	waitFor(t, func() bool {
		status, sample, interval := s.Latest()
		if status != MacmonAvailable {
			t.Fatalf("interval restart status = %q, want available", status)
		}
		if sample != originalSample {
			t.Fatal("interval restart changed the sample before new stdout")
		}
		args := macmonArgs(path)
		if len(args) != 2 || args[1] != "pipe -i 100" {
			return false
		}
		if interval != 250*time.Millisecond {
			t.Fatalf("retained sample interval = %v, want 250ms", interval)
		}
		state := sensorState{Macmon: status, Sample: sample, Interval: interval}
		if state.wire(sample.SampledAt.Add(400*time.Millisecond)).CPUTempC == nil {
			t.Fatal("fresh-enough idle reading blanked on interactive restart")
		}
		return true
	})
	if elapsed := time.Since(changedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("interval restart took %v, want <= 500ms", elapsed)
	}
	if err := syscall.Kill(oldPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("original child %d survived interval restart: %v", oldPID, err)
	}
	if _, err := lines.WriteString(macmonTestLine + "\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, sample, interval := s.Latest()
		return sample != originalSample && interval == 100*time.Millisecond
	})
}

func TestMacmonSupervisorLatestIntervalBeforeAndAfterStart(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$LINE"; exec sleep 30`)
	var fast atomic.Bool
	cfg := macmonTestConfig(path)
	cfg.CheckEvery = time.Second
	cfg.Interval = func() time.Duration {
		if fast.Load() {
			return 100 * time.Millisecond
		}
		return 250 * time.Millisecond
	}
	s := newMacmonSupervisor(cfg)
	if _, _, interval := s.Latest(); interval != 250*time.Millisecond {
		t.Fatalf("pre-start interval = %v, want 250ms", interval)
	}
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	fast.Store(true)
	if _, _, interval := s.Latest(); interval != 250*time.Millisecond {
		t.Fatalf("interval changed before child restart: %v", interval)
	}
}

func TestMacmonSupervisorCrashBecomesFailedThenRecovers(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$LINE"
if [ ! -e "$DIR/crashed" ]; then
    : > "$DIR/crashed"
    exit 1
fi
exec sleep 30`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonFailed && sample != nil && sample.CPUTempC != nil && *sample.CPUTempC == 40.5
	})
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonAvailable && sample != nil && len(macmonArgs(path)) == 2
	})
}

func TestMacmonSupervisorBinaryRemovedBecomesMissing(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$$" > "$DIR/pid"; printf '%s\n' "$LINE"; exec sleep 30`)
	cfg := macmonTestConfig(path)
	cfg.Interval = func() time.Duration { return 20 * time.Millisecond }
	s := newMacmonSupervisor(cfg)
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child PID: %q, %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonMissing && sample == nil
	})
}

func TestMacmonSupervisorBadLinesSetFailed(t *testing.T) {
	path := fakeMacmon(t, `while :; do printf '%s\n' garbage; sleep 0.05; done`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonFailed && sample == nil
	})
}

func TestMacmonSupervisorBadLinesKeepSampleAndRecover(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$LINE"; exec /bin/cat "$DIR/lines"`)
	fifo := filepath.Join(filepath.Dir(path), "lines")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := os.OpenFile(fifo, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lines.Close() })
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	if _, err := lines.WriteString("garbage\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		status, sample, _ := s.Latest()
		return status == MacmonFailed && sample != nil && sample.CPUTempC != nil && *sample.CPUTempC == 40.5
	})
	if _, err := lines.WriteString(macmonTestLine + "\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	if args := macmonArgs(path); len(args) != 1 {
		t.Fatalf("bad lines restarted the child: %v", args)
	}
}

func TestMacmonSupervisorAgesSamplesFromReceipt(t *testing.T) {
	for _, timestamp := range []string{"2000-01-01T00:00:00Z", "2100-01-01T00:00:00Z"} {
		t.Run(timestamp, func(t *testing.T) {
			line := `{"timestamp":"` + timestamp + `","temp":{"cpu_temp_avg":40.5}}`
			path := fakeMacmon(t, "printf '%s\\n' '"+line+"'; exec sleep 30")
			s := newMacmonSupervisor(macmonTestConfig(path))
			before := time.Now()
			runMacmonSupervisor(t, s)
			waitFor(t, func() bool { _, sample, _ := s.Latest(); return sample != nil })
			now := time.Now()
			status, sample, interval := s.Latest()
			if sample.SampledAt.Before(before) || sample.SampledAt.After(now) {
				t.Fatalf("sample timestamp = %v, want receipt between %v and %v", sample.SampledAt, before, now)
			}
			state := sensorState{Macmon: status, Sample: sample, Interval: interval}
			if got := state.wire(now); got.Macmon != MacmonAvailable || got.MSSince == nil || *got.MSSince < 0 {
				t.Fatalf("received sample not fresh: %+v", got)
			}
			if got := state.wire(sample.SampledAt.Add(3*interval + time.Millisecond)); got.CPUTempC != nil || got.Macmon != MacmonFailed {
				t.Fatalf("received sample did not expire: %+v", got)
			}
		})
	}
}

func TestMacmonSupervisorNoOutputWatchdog(t *testing.T) {
	for _, script := range []string{
		`exec sleep 30`,
		`printf '%s\n' "$LINE"; exec sleep 30`,
	} {
		t.Run(script, func(t *testing.T) {
			path := fakeMacmon(t, script)
			cfg := macmonTestConfig(path)
			cfg.Interval = func() time.Duration { return 40 * time.Millisecond }
			cfg.CheckEvery = 5 * time.Millisecond
			cfg.BackoffMin, cfg.BackoffMax = 100*time.Millisecond, 100*time.Millisecond
			s := newMacmonSupervisor(cfg)
			runMacmonSupervisor(t, s)
			waitFor(t, func() bool { return len(macmonArgs(path)) == 1 })
			if strings.Contains(script, "printf") {
				waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
			}
			waitFor(t, func() bool {
				status, _, _ := s.Latest()
				return status == MacmonFailed && len(macmonArgs(path)) == 1
			})
			waitFor(t, func() bool { return len(macmonArgs(path)) >= 2 })
		})
	}
}

func TestMacmonSupervisorKillsChildOnCancel(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$$" > "$DIR/pid"; exec sleep 30`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	cancel, done := runMacmonSupervisor(t, s)
	var pid int
	waitFor(t, func() bool {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	cancel()
	waitFor(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still exists after Run returned: %v", pid, err)
	}
}

func TestMacmonSupervisorReprobesMissingPaths(t *testing.T) {
	path := fakeMacmon(t, `printf '%s\n' "$LINE"; exec sleep 30`)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cfg := macmonTestConfig(filepath.Join(t.TempDir(), "absent"), path)
	var probes atomic.Int64
	cfg.Interval = func() time.Duration {
		probes.Add(1)
		return 20 * time.Millisecond
	}
	s := newMacmonSupervisor(cfg)
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { return probes.Load() > 0 })
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	if args := macmonArgs(path); len(args) != 1 {
		t.Fatalf("installed binary not started exactly once: %v", args)
	}
}

func TestMacmonSupervisorPrefersFirstExecutable(t *testing.T) {
	first := fakeMacmon(t, `printf '%s\n' "$LINE"; exec sleep 30`)
	second := fakeMacmon(t, `exit 1`)
	s := newMacmonSupervisor(macmonTestConfig(t.TempDir(), first, second))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { status, _, _ := s.Latest(); return status == MacmonAvailable })
	if len(macmonArgs(first)) != 1 || len(macmonArgs(second)) != 0 {
		t.Fatal("did not start only the first executable")
	}
}

func TestMacmonSupervisorBackoffDoublesCapsAndResets(t *testing.T) {
	path := fakeMacmon(t, `count=0
[ ! -f "$DIR/count" ] || read -r count < "$DIR/count"
count=$((count + 1))
printf '%s\n' "$count" > "$DIR/count"
[ "$count" -ne 4 ] || printf '%s\n' "$LINE"
[ "$count" -lt 5 ] || exec sleep 30
exit 1`)
	cfg := macmonTestConfig(path)
	cfg.BackoffMin = 50 * time.Millisecond
	cfg.BackoffMax = 150 * time.Millisecond
	s := newMacmonSupervisor(cfg)
	runMacmonSupervisor(t, s)
	var starts []time.Time
	waitFor(t, func() bool {
		count := len(macmonArgs(path))
		if count > len(starts) {
			if count != len(starts)+1 {
				t.Fatal("missed a child start while observing backoff")
			}
			starts = append(starts, time.Now())
		}
		return len(starts) == 5
	})
	for i, min := range []time.Duration{40 * time.Millisecond, 90 * time.Millisecond, 140 * time.Millisecond, 40 * time.Millisecond} {
		if gap := starts[i+1].Sub(starts[i]); gap < min {
			t.Errorf("restart gap %d = %v, want >= %v", i+1, gap, min)
		}
	}
	if gap := starts[3].Sub(starts[2]); gap >= 190*time.Millisecond {
		t.Errorf("backoff exceeded cap: %v", gap)
	}
	if gap := starts[4].Sub(starts[3]); gap >= 120*time.Millisecond {
		t.Errorf("backoff did not reset after good line: %v", gap)
	}
}

func TestMacmonSupervisorAcceptsLargeLines(t *testing.T) {
	line := `{"timestamp":"2026-11-02T06:29:04Z","temp":{"cpu_temp_avg":40.5},"ignored":"` + strings.Repeat("x", 128*1024) + `"}`
	path := fakeMacmon(t, "printf '%s\\n' '"+line+"'; exec sleep 30")
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { status, sample, _ := s.Latest(); return status == MacmonAvailable && sample != nil })
}

func TestMacmonSupervisorScannerFailureRestarts(t *testing.T) {
	path := fakeMacmon(t, "printf '%s\\n' '"+strings.Repeat("x", 1024*1024+1)+"'; exec sleep 30")
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool { return len(macmonArgs(path)) >= 2 })
	status, sample, _ := s.Latest()
	if status != MacmonFailed || sample != nil {
		t.Fatalf("oversized line status/sample = %q/%v", status, sample)
	}
}

type macmonLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *macmonLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *macmonLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestMacmonSupervisorStderrIsDebugOnly(t *testing.T) {
	var logs macmonLogBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	path := fakeMacmon(t, `printf '%s\n' stderr-marker >&2; printf '%s\n' "$LINE"; exec sleep 30`)
	s := newMacmonSupervisor(macmonTestConfig(path))
	runMacmonSupervisor(t, s)
	waitFor(t, func() bool {
		status, _, _ := s.Latest()
		return status == MacmonAvailable && strings.Contains(logs.String(), "stderr-marker")
	})
	output := logs.String()
	if !strings.Contains(output, "level=DEBUG") || strings.Contains(output, "level=INFO") || strings.Contains(output, "level=WARN") || strings.Contains(output, "cpu_temp_avg") {
		t.Fatalf("stderr severity or stdout logging violated: %s", output)
	}
}
