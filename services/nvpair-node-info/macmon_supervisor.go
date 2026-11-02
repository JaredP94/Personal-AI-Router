// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type macmonConfig struct {
	Paths      []string
	Interval   func() time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	CheckEvery time.Duration
}

// macmonSupervisor owns one child at a time. Configuration durations must be
// positive, BackoffMax >= BackoffMin, and Interval must be safe for concurrent use.
// Published samples are immutable; readers may retain them across child restarts.
type macmonSupervisor struct {
	cfg macmonConfig

	mu             sync.Mutex
	status         MacmonStatus
	sample         *macmonSample
	interval       time.Duration
	sampleInterval time.Duration
	started        bool
	lastUsable     time.Time // receipt time for this child's watchdog
}

func newMacmonSupervisor(cfg macmonConfig) *macmonSupervisor {
	cfg.Paths = append([]string(nil), cfg.Paths...)
	return &macmonSupervisor{cfg: cfg, status: MacmonMissing}
}

func (s *macmonSupervisor) Latest() (MacmonStatus, *macmonSample, time.Duration) {
	s.mu.Lock()
	status, sample, interval, started := s.status, s.sample, s.interval, s.started
	if sample != nil {
		interval = s.sampleInterval
	}
	s.mu.Unlock()
	if !started {
		interval = s.cfg.Interval()
	}
	return status, sample, interval
}

// Run blocks until cancellation and does not return until its child has been
// killed, waited, and its output reader has stopped. Call Run only once.
func (s *macmonSupervisor) Run(ctx context.Context) {
	backoff := s.cfg.BackoffMin
	for ctx.Err() == nil {
		path := s.executable()
		if path == "" {
			s.mu.Lock()
			s.status, s.sample = MacmonMissing, nil
			s.mu.Unlock()
			if !waitMacmon(ctx, s.cfg.Interval()) {
				return
			}
			continue
		}

		changed, good := s.runChild(ctx, path, s.cfg.Interval())
		if ctx.Err() != nil {
			return
		}
		if good {
			backoff = s.cfg.BackoffMin
		}
		if changed {
			continue
		}
		s.mu.Lock()
		s.status = MacmonFailed
		s.mu.Unlock()
		if !waitMacmon(ctx, backoff) {
			return
		}
		if backoff >= s.cfg.BackoffMax-backoff {
			backoff = s.cfg.BackoffMax
		} else {
			backoff *= 2
		}
	}
}

func (s *macmonSupervisor) executable() string {
	for _, path := range s.cfg.Paths {
		// Never let exec.CommandContext search PATH, even with injected config.
		if !filepath.IsAbs(path) {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// runChild returns whether this was a policy restart and whether any usable
// sample arrived. Only the Run goroutine owns backoff; the scanner owns parsing.
func (s *macmonSupervisor) runChild(ctx context.Context, path string, interval time.Duration) (bool, bool) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, path, "pipe", "-i", strconv.FormatInt(interval.Milliseconds(), 10))
	// Bound exec's output-copy shutdown if a descendant inherits a pipe. The
	// supervised child itself is always killed and reaped before returning.
	cmd.WaitDelay = time.Second
	reader, writer := io.Pipe()
	defer reader.Close()
	cmd.Stdout = writer
	cmd.Stderr = macmonStderrWriter{}
	if err := cmd.Start(); err != nil {
		writer.Close()
		return false, false
	}
	s.mu.Lock()
	s.interval, s.started = interval, true
	s.lastUsable = time.Now()
	if s.status == MacmonMissing {
		s.status = MacmonFailed
	}
	s.mu.Unlock()

	scanned := make(chan bool, 1)
	go func() {
		good := s.readSamples(reader)
		// On a scanner error, unblock exec's writer and let the owner kill the
		// child. A child that keeps stdout open cannot strand the scanner.
		reader.Close()
		scanned <- good
	}()
	waited := make(chan struct{})
	go func() {
		// Stdout uses an io.Writer, not StdoutPipe: Wait drains exec's copy
		// goroutine before we close the scanner's input, preserving final lines.
		_ = cmd.Wait()
		writer.Close()
		close(waited)
	}()
	stop := func() bool {
		cancel()
		<-waited
		return <-scanned
	}
	tick := time.NewTicker(s.cfg.CheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, stop()
		case <-tick.C:
			s.mu.Lock()
			stalled := time.Since(s.lastUsable) > 3*interval
			if stalled {
				s.status = MacmonFailed
			}
			s.mu.Unlock()
			if stalled {
				return false, stop()
			}
			if s.cfg.Interval() != interval {
				return true, stop()
			}
		case <-waited:
			return false, <-scanned
		case good := <-scanned:
			// EOF normally follows Wait. Scanner errors can arrive first;
			// terminate the still-running child rather than abandon its pipe.
			cancel()
			<-waited
			return false, good
		}
	}
}

func (s *macmonSupervisor) readSamples(reader io.Reader) bool {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	good := false
	for scanner.Scan() {
		sample, ok := parseMacmonLine(scanner.Bytes())
		s.mu.Lock()
		if ok {
			sample.SampledAt = time.Now()
			s.lastUsable = sample.SampledAt
			s.status, s.sample = MacmonAvailable, &sample
			s.sampleInterval = s.interval
			good = true
		} else {
			s.status = MacmonFailed
		}
		s.mu.Unlock()
	}
	return good
}

func waitMacmon(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type macmonStderrWriter struct{}

func (macmonStderrWriter) Write(data []byte) (int, error) {
	slog.Debug("macmon stderr", "output", string(data))
	return len(data), nil
}
