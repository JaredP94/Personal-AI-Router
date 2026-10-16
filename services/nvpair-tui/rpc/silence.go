// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package rpc

import (
	"errors"
	"sync"
	"time"
)

// ErrHeartbeatTimeout distinguishes an open but unresponsive broker session
// from EOF, protocol mismatch, user quit and ordinary request timeouts.
var ErrHeartbeatTimeout = errors.New("broker connection lost: 45s inbound silence; restart PAIR if reconnection fails")

type silenceDeadline struct {
	mu         sync.Mutex
	duration   time.Duration
	expire     func()
	schedule   func(time.Duration, func()) func()
	cancel     func()
	generation uint64
	stopped    bool
}

func (d *silenceDeadline) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.generation++
	generation := d.generation
	if d.cancel != nil {
		d.cancel()
	}
	schedule := d.schedule
	if schedule == nil {
		schedule = func(duration time.Duration, callback func()) func() {
			timer := time.AfterFunc(duration, callback)
			return func() { timer.Stop() }
		}
	}
	d.cancel = schedule(d.duration, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stopped || generation != d.generation {
			return
		}
		d.stopped = true
		d.expire()
	})
}
func (d *silenceDeadline) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	d.generation++
	if d.cancel != nil {
		d.cancel()
	}
}
