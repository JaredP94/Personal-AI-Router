// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// Test the shared command lifetime, not Windows syscalls, with a real child
// process and injected ownership operations. No engine installation is involved.
type commandJob struct {
	recordingJob
	cmd       *exec.Cmd
	activated chan struct{}
	once      sync.Once
}

func (j *commandJob) resume() error {
	j.once.Do(func() { close(j.activated) })
	return j.recordingJob.resume()
}
func (j *commandJob) terminate() error {
	err := j.recordingJob.terminate()
	if j.cmd.Process != nil {
		_ = j.cmd.Process.Kill()
	}
	return err
}

func TestJobCommandFixture(t *testing.T) {
	if os.Getenv("PAIR_JOB_FIXTURE") != "1" {
		return
	}
	if os.Getenv("PAIR_JOB_SLEEP") == "1" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func jobFixture(ctx context.Context, sleep bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobCommandFixture$")
	cmd.Env = append(os.Environ(), "PAIR_JOB_FIXTURE=1")
	if sleep {
		cmd.Env = append(cmd.Env, "PAIR_JOB_SLEEP=1")
	}
	return cmd
}

func TestJobCommandExitClosesJob(t *testing.T) {
	cmd := jobFixture(context.Background(), false)
	j := &commandJob{cmd: cmd, activated: make(chan struct{})}
	// Race-instrumented helper exit includes the runtime's one-second drain.
	if err := runJobCommand(cmd, newProcessJob(j), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := j.events[len(j.events)-1]; got != "close" {
		t.Fatal(j.events)
	}
}
func TestJobCommandStartFailureClosesJob(t *testing.T) {
	cmd := exec.Command("/nonexistent/pair-job-command")
	j := &commandJob{cmd: cmd, activated: make(chan struct{})}
	if runJobCommand(cmd, newProcessJob(j), time.Second) == nil {
		t.Fatal("expected start failure")
	}
	if len(j.events) != 1 || j.events[0] != "close" {
		t.Fatal(j.events)
	}
}
func TestJobCommandAssignmentFailureReapsAndCloses(t *testing.T) {
	cmd := jobFixture(context.Background(), true)
	j := &commandJob{recordingJob: recordingJob{fail: "assign"}, cmd: cmd, activated: make(chan struct{})}
	if runJobCommand(cmd, newProcessJob(j), time.Second) == nil {
		t.Fatal("expected assignment failure")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child not reaped")
	}
	if j.events[len(j.events)-1] != "close" {
		t.Fatal(j.events)
	}
}
func TestJobCommandShutdownTerminatesAndClosesJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := jobFixture(ctx, true)
	j := &commandJob{cmd: cmd, activated: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- runJobCommand(cmd, newProcessJob(j), time.Second) }()
	select {
	case <-j.activated:
	case <-time.After(5 * time.Second):
		t.Fatal("command not activated")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked")
	}
	if j.events[len(j.events)-2] != "terminate" || j.events[len(j.events)-1] != "close" {
		t.Fatal(j.events)
	}
}
func TestJobCommandDeadlineTerminatesJob(t *testing.T) {
	cmd := jobFixture(context.Background(), true)
	j := &commandJob{cmd: cmd, activated: make(chan struct{})}
	if runJobCommand(cmd, newProcessJob(j), 10*time.Millisecond) == nil {
		t.Fatal("expected deadline kill")
	}
	if j.events[len(j.events)-2] != "terminate" || j.events[len(j.events)-1] != "close" {
		t.Fatal(j.events)
	}
}
func TestControlCommandOwnershipIsExplicit(t *testing.T) {
	ctx := context.Background()
	if isControlCommand(ctx) {
		t.Fatal("ordinary commands must own descendants")
	}
	marked, cancel := context.WithTimeout(withControlCommand(ctx), time.Second)
	defer cancel()
	if !isControlCommand(marked) {
		t.Fatal("lost command-mode ownership across deadline")
	}
	if isControlCommand(ctx) {
		t.Fatal("control marker leaked to other helpers")
	}
}
