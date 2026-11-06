// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "time"

// legacyListener owns the identity used for an adopted-process stop. Windows
// implementations retain a query/terminate handle until close; Unix retains
// its existing best-effort PID/group cleanup semantics.
type legacyListener interface {
	processID() int
	imagePath() string
	terminate(time.Duration, string)
	close()
}

// pinnedProcessOperations always addresses one open kernel process object.
// ownsPort re-reads the listener table while that handle prevents PID reuse.
type pinnedProcessOperations interface {
	imagePath() string
	creationTime() (uint64, error)
	ownsPort(port, pid int) bool
	terminate(time.Duration)
	close()
}

type pinnedProcess struct {
	pid, port int
	image     string
	created   uint64
	ops       pinnedProcessOperations
}

// identifyPinnedProcess takes ownership of ops, including on failure. A zero
// port identifies a non-listening helper; its caller must prove ownership by
// other means before asking to terminate it.
func identifyPinnedProcess(pid, port int, ops pinnedProcessOperations) (*pinnedProcess, bool) {
	created, err := ops.creationTime()
	image := ops.imagePath()
	if err != nil || image == "" || (port != 0 && !ops.ownsPort(port, pid)) {
		ops.close()
		return nil, false
	}
	return &pinnedProcess{pid: pid, port: port, image: image, created: created, ops: ops}, true
}

func (p *pinnedProcess) processID() int    { return p.pid }
func (p *pinnedProcess) imagePath() string { return p.image }
func (p *pinnedProcess) close() {
	if p.ops != nil {
		p.ops.close()
		p.ops = nil
	}
}

func (p *pinnedProcess) terminate(grace time.Duration, expectedImage string) {
	if p.ops == nil {
		return
	}
	created, err := p.ops.creationTime()
	if err != nil || created != p.created ||
		!isOurEngineImage(p.ops.imagePath(), p.image) ||
		!isOurEngineImage(p.image, expectedImage) ||
		(p.port != 0 && !p.ops.ownsPort(p.port, p.pid)) {
		return
	}
	p.ops.terminate(grace)
}
