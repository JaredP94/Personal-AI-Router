// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "sync"

// processJob owns one gated launch. The lock protects only handle operations,
// never a process wait. Implementations must not wait in these methods.
type jobOperations interface {
	assign() error
	resume() error
	terminate() error
	close() error
}

type processJob struct {
	mu     sync.Mutex
	ops    jobOperations
	closed bool
}

func newProcessJob(ops jobOperations) *processJob { return &processJob{ops: ops} }
func (p *processJob) activate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ops.assign(); err != nil {
		p.abort()
		return err
	}
	if err := p.ops.resume(); err != nil {
		p.abort()
		return err
	}
	return nil
}
func (p *processJob) abort() {
	_ = p.ops.terminate()
	_ = p.ops.close()
	p.closed = true
}
func (p *processJob) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	return p.ops.terminate()
}
func (p *processJob) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		_ = p.ops.close()
		p.closed = true
	}
}
