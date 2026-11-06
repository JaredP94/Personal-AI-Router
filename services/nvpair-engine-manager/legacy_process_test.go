// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"
	"time"
)

// This injected kernel boundary models a pinned process object, not a PID
// lookup: changing its identity must never authorize terminating a replacement.
type fakePinnedProcess struct {
	pid, port  int
	image      string
	created    uint64
	queryErr   error
	terminated bool
	closes     int
}

func (p *fakePinnedProcess) imagePath() string {
	if p.closes != 0 {
		return ""
	}
	return p.image
}
func (p *fakePinnedProcess) creationTime() (uint64, error) {
	if p.closes != 0 {
		return 0, errors.New("closed handle")
	}
	return p.created, p.queryErr
}
func (p *fakePinnedProcess) ownsPort(port, pid int) bool { return port == p.port && pid == p.pid }
func (p *fakePinnedProcess) terminate(time.Duration)     { p.terminated = true }
func (p *fakePinnedProcess) close()                      { p.closes++ }

func TestLegacyListenerRejectsSameImagePIDReplacement(t *testing.T) {
	for _, replacement := range []struct {
		name    string
		port    int
		created uint64
	}{
		{"another port", 11436, 20},
		{"same port different creation time", 11435, 20},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			ops := &fakePinnedProcess{pid: 42, port: 11435, image: "/pair/engine", created: 10}
			p, ok := identifyPinnedProcess(42, 11435, ops)
			if !ok {
				t.Fatal("original listener not identified")
			}
			ops.port, ops.created = replacement.port, replacement.created
			p.terminate(0, "/pair/engine")
			p.close()
			if ops.terminated {
				t.Fatal("terminated a same-image PID replacement")
			}
			if ops.closes != 1 {
				t.Fatalf("handle closed %d times", ops.closes)
			}
		})
	}
}

func TestLegacyListenerRevalidatesPinnedIdentity(t *testing.T) {
	for _, failure := range []string{"none", "listener moved", "image changed", "creation query failed", "image query failed", "unexpected image"} {
		t.Run(failure, func(t *testing.T) {
			ops := &fakePinnedProcess{pid: 42, port: 11435, image: "/pair/engine", created: 10}
			p, ok := identifyPinnedProcess(42, 11435, ops)
			if !ok {
				t.Fatal("listener not identified")
			}
			expected := "/pair/engine"
			switch failure {
			case "listener moved":
				ops.port = 11436
			case "image changed":
				ops.image = "/other/engine"
			case "creation query failed":
				ops.queryErr = errors.New("exited")
			case "image query failed":
				ops.image = ""
			case "unexpected image":
				expected = "/other/engine"
			}
			p.terminate(0, expected)
			p.close()
			p.close()
			if ops.terminated != (failure == "none") {
				t.Fatalf("termination=%v", ops.terminated)
			}
			if ops.closes != 1 {
				t.Fatalf("handle closed %d times", ops.closes)
			}
		})
	}
}

func TestLegacyListenerIdentificationFailureClosesHandle(t *testing.T) {
	for _, failure := range []string{"creation query", "image query", "listener changed"} {
		t.Run(failure, func(t *testing.T) {
			ops := &fakePinnedProcess{pid: 42, port: 11435, image: "/pair/engine", created: 10}
			switch failure {
			case "creation query":
				ops.queryErr = errors.New("denied")
			case "image query":
				ops.image = ""
			case "listener changed":
				ops.port = 11436
			}
			if _, ok := identifyPinnedProcess(42, 11435, ops); ok {
				t.Fatal("unproven listener identified")
			}
			if ops.terminated || ops.closes != 1 {
				t.Fatalf("terminated=%v closes=%d", ops.terminated, ops.closes)
			}
		})
	}
}
