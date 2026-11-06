// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"reflect"
	"testing"
)

type recordingJob struct {
	events []string
	fail   string
}

func (j *recordingJob) step(s string) error {
	j.events = append(j.events, s)
	if j.fail == s {
		return errors.New(s)
	}
	return nil
}
func (j *recordingJob) assign() error    { return j.step("assign") }
func (j *recordingJob) resume() error    { return j.step("resume") }
func (j *recordingJob) terminate() error { return j.step("terminate") }
func (j *recordingJob) close() error     { return j.step("close") }

func TestProcessJobCancellationBeforeActivationDoesNotResume(t *testing.T) {
	j := &recordingJob{}
	p := newProcessJob(j)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if p.activate() == nil {
		t.Fatal("canceled launch was resumed")
	}
	p.release()
	if !reflect.DeepEqual(j.events, []string{"terminate", "close"}) {
		t.Fatal(j.events)
	}
}

func TestProcessJobAssignmentBeforeResume(t *testing.T) {
	j := &recordingJob{}
	p := newProcessJob(j)
	if err := p.activate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(j.events, []string{"assign", "resume"}) {
		t.Fatal(j.events)
	}
}
func TestProcessJobActivationFailureClosesWithoutResume(t *testing.T) {
	for _, fail := range []string{"assign", "resume"} {
		t.Run(fail, func(t *testing.T) {
			j := &recordingJob{fail: fail}
			p := newProcessJob(j)
			if p.activate() == nil {
				t.Fatal("expected activation error")
			}
			want := []string{"assign", "terminate", "close"}
			if fail == "resume" {
				want = []string{"assign", "resume", "terminate", "close"}
			}
			if !reflect.DeepEqual(j.events, want) {
				t.Fatal(j.events)
			}
		})
	}
}
func TestProcessJobStopAndCloseAreIdempotent(t *testing.T) {
	j := &recordingJob{}
	p := newProcessJob(j)
	if err := p.activate(); err != nil {
		t.Fatal(err)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	p.release()
	p.release()
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(j.events, []string{"assign", "resume", "terminate", "close"}) {
		t.Fatal(j.events)
	}
}
func TestProcessJobExitAndShutdownCloseHandle(t *testing.T) {
	for _, reason := range []string{"exit", "shutdown", "start failure"} {
		t.Run(reason, func(t *testing.T) {
			j := &recordingJob{}
			p := newProcessJob(j)
			p.release()
			p.release()
			if !reflect.DeepEqual(j.events, []string{"close"}) {
				t.Fatal(j.events)
			}
		})
	}
}
func TestProcessJobTerminationErrorStillCloses(t *testing.T) {
	j := &recordingJob{fail: "terminate"}
	p := newProcessJob(j)
	if p.stop() == nil {
		t.Fatal("expected termination error")
	}
	if !reflect.DeepEqual(j.events, []string{"terminate", "close"}) {
		t.Fatal("termination error must close the job immediately", j.events)
	}
	p.release()
}
