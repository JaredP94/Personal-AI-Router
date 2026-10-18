// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/workloadstore"
)

func TestWriteDemandFrame(t *testing.T) {
	for _, tc := range []struct {
		name           string
		localExecution bool
	}{
		{"active execution", true},
		{"idle execution", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newNodeInfoHandle(&buf).SetDemand(tc.localExecution); err != nil {
				t.Fatalf("write: %v", err)
			}

			line := buf.String()
			if !strings.HasSuffix(line, "\n") {
				t.Error("frame is not newline-terminated; node-info reads line-delimited frames")
			}
			if strings.Count(line, "\n") != 1 {
				t.Errorf("frame contains %d newlines, want exactly one", strings.Count(line, "\n"))
			}

			var frame struct {
				JSONRPC string                `json:"jsonrpc"`
				Method  string                `json:"method"`
				ID      json.RawMessage       `json:"id"`
				Params  noderec.DemandParams `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("decode frame %q: %v", line, err)
			}
			if frame.JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %q, want 2.0", frame.JSONRPC)
			}
			if frame.Method != "stats/set-demand" {
				t.Errorf("method = %q, want %q", frame.Method, "stats/set-demand")
			}
			if len(frame.ID) != 0 {
				t.Errorf("frame carries an id (%s); demand push must be a notification", frame.ID)
			}
			if frame.Params.LocalExecution != tc.localExecution {
				t.Errorf("localExecution = %v, want %v", frame.Params.LocalExecution, tc.localExecution)
			}
		})
	}
}

func TestBrokerDemandOnWorkloadTransitions(t *testing.T) {
	var buf lockedBuffer
	np := newNodeInfoHandle(&buf)
	b := &Broker{
		workloads: workloadstore.New(),
		nodeID:    "self",
	}
	b.setNodeInfo(np)

	readFrames := func() []noderec.DemandParams {
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		buf.Reset()
		var frames []noderec.DemandParams
		for _, l := range lines {
			if l == "" {
				continue
			}
			var f struct {
				Method string                `json:"method"`
				Params noderec.DemandParams `json:"params"`
			}
			if err := json.Unmarshal([]byte(l), &f); err == nil && f.Method == "stats/set-demand" {
				frames = append(frames, f.Params)
			}
		}
		return frames
	}

	// 1. Initial state: no workloads. Add a workload scheduled on peer; demand should remain unchanged (no frame).
	peerWL, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w1",
			"originatedFrom": "self",
			"state":          "running",
			"scheduledOn":    "peer-node",
			"createdAt":      100,
		},
	})
	b.emitWorkloadEvent("workloads:upsert", peerWL)
	if frames := readFrames(); len(frames) != 0 {
		t.Fatalf("unexpected demand frames on peer workload: %+v", frames)
	}

	// 2. Workload scheduled on self arrives -> transitions demand to true.
	selfWL1, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w2",
			"originatedFrom": "peer-node",
			"state":          "running",
			"scheduledOn":    "self",
			"createdAt":      200,
		},
	})
	b.emitWorkloadEvent("workloads:upsert", selfWL1)
	frames := readFrames()
	if len(frames) != 1 || !frames[0].LocalExecution {
		t.Fatalf("expected 1 frame with LocalExecution=true, got %+v", frames)
	}

	// 3. Second workload on self arrives -> still true, no duplicate frame sent.
	selfWL2, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w3",
			"originatedFrom": "self",
			"state":          "running",
			"scheduledOn":    "self",
			"createdAt":      300,
		},
	})
	b.emitWorkloadEvent("workloads:upsert", selfWL2)
	if frames := readFrames(); len(frames) != 0 {
		t.Fatalf("expected no frame on duplicate active state, got %+v", frames)
	}

	// 4. First self workload completes -> second still active, no frame sent.
	selfWL1Done, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w2",
			"originatedFrom": "peer-node",
			"state":          "completed",
			"scheduledOn":    "self",
			"createdAt":      200,
		},
	})
	b.emitWorkloadEvent("workloads:upsert", selfWL1Done)
	if frames := readFrames(); len(frames) != 0 {
		t.Fatalf("expected no frame while other workload still active, got %+v", frames)
	}

	// 5. Second self workload fails -> demand transitions back to false.
	selfWL2Done, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w3",
			"originatedFrom": "self",
			"state":          "failed",
			"scheduledOn":    "self",
			"createdAt":      300,
		},
	})
	b.emitWorkloadEvent("workloads:upsert", selfWL2Done)
	frames = readFrames()
	if len(frames) != 1 || frames[0].LocalExecution {
		t.Fatalf("expected 1 frame with LocalExecution=false, got %+v", frames)
	}
}

func TestBrokerDemandOnNodeInfoRestart(t *testing.T) {
	b := &Broker{
		workloads: workloadstore.New(),
		nodeID:    "self",
	}

	// Set active workload on self
	selfWL, _ := json.Marshal(map[string]any{
		"workloadInfo": map[string]any{
			"id":             "w1",
			"originatedFrom": "self",
			"state":          "running",
			"scheduledOn":    "self",
			"createdAt":      100,
		},
	})
	in, ok := workloadstore.ParseIncoming(json.RawMessage(`{
		"id": "w1",
		"originatedFrom": "self",
		"state": "running",
		"scheduledOn": "self",
		"createdAt": 100
	}`))
	if !ok {
		t.Fatal("parse failed")
	}
	b.workloads.Apply(in)

	// Simulate node-info restart
	var buf lockedBuffer
	np := newNodeInfoHandle(&buf)
	b.setNodeInfo(np)
	b.pushDemandToNodeInfoOnStart()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 frame on restart, got %d: %q", len(lines), buf.String())
	}
	var f struct {
		Method string                `json:"method"`
		Params noderec.DemandParams `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Method != "stats/set-demand" || !f.Params.LocalExecution {
		t.Fatalf("unexpected frame on restart: %+v", f)
	}
	_ = selfWL
}
