// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/applog"
)

type captureRW struct {
	frames chan []byte
}

func newCaptureRW() *captureRW {
	return &captureRW{frames: make(chan []byte, 64)}
}

func (rw *captureRW) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (rw *captureRW) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	rw.frames <- cp
	return len(p), nil
}

type fakeRoundTripper struct {
	mu       sync.RWMutex
	handlers map[string]func(*http.Request) (*http.Response, error)
}

func newFakeRoundTripper() *fakeRoundTripper {
	return &fakeRoundTripper{handlers: make(map[string]func(*http.Request) (*http.Response, error))}
}

func (rt *fakeRoundTripper) set(method, host, path string, fn func(*http.Request) (*http.Response, error)) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.handlers[method+" "+host+path] = fn
}

func (rt *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Host + req.URL.Path
	rt.mu.RLock()
	fn := rt.handlers[key]
	rt.mu.RUnlock()
	if fn == nil {
		return nil, errors.New("unexpected request: " + key)
	}
	return fn(req)
}

func httpJSON(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newTestManager() (*Manager, *captureRW, *fakeRoundTripper) {
	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil)
	if err != nil {
		panic(err)
	}
	rt := newFakeRoundTripper()
	m.client = &http.Client{Transport: rt, Timeout: time.Second}
	m.tlsClient = &http.Client{Transport: rt, Timeout: time.Second}
	m.telemetryClient = &http.Client{Transport: rt, Timeout: time.Second}
	m.telemetryTLSClient = &http.Client{Transport: rt, Timeout: time.Second}
	return m, rw, rt
}

func TestProbeNodeInfoKeepsSchedulingTelemetryActive(t *testing.T) {
	m, _, rt := newTestManager()
	var poll string
	rt.set(http.MethodGet, "node.local:14318", "/v1/node-info", func(req *http.Request) (*http.Response, error) {
		poll = req.Header.Get("X-NVPAIR-Poll")
		return httpJSON(http.StatusOK, `{"hostUuid":"peer-uuid"}`)
	})

	up, info := m.probeNodeInfo(m.client, "http", "node.local", 14318)
	if !up || info.HostUUID != "peer-uuid" {
		t.Fatalf("probeNodeInfo = %v, %+v; want a successful peer response", up, info)
	}
	if poll != "foreground" {
		t.Fatalf("scheduling telemetry poll = %q, want foreground to retain active sampling", poll)
	}
}

func configureHealthyNode(rt *fakeRoundTripper, addr string, models []string, info NodeInfoResponse) {
	host := net.JoinHostPort(addr, "11434")
	rt.set(http.MethodGet, host, "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, host, "/api/tags", func(*http.Request) (*http.Response, error) {
		var payload struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		for _, name := range models {
			payload.Models = append(payload.Models, struct {
				Name string `json:"name"`
			}{Name: name})
		}
		data, _ := json.Marshal(payload)
		return httpJSON(http.StatusOK, string(data))
	})

	infoHost := net.JoinHostPort(addr, "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})
}

// configureHealthyLMStudio registers a 200 GET /v1/models on addr:1234
// returning the given model ids in the OpenAI list shape, so probeLMStudio
// reports the node up with those models.
func configureHealthyLMStudio(rt *fakeRoundTripper, addr string, models []string) {
	host := net.JoinHostPort(addr, "1234")
	rt.set(http.MethodGet, host, "/v1/models", func(*http.Request) (*http.Response, error) {
		var payload struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		payload.Object = "list"
		for _, id := range models {
			payload.Data = append(payload.Data, struct {
				ID string `json:"id"`
			}{ID: id})
		}
		data, _ := json.Marshal(payload)
		return httpJSON(http.StatusOK, string(data))
	})
}

// TestProbeLMStudioReportsModels covers the new LM Studio probe: a reachable
// server reports up with its model ids parsed from /v1/models, and an absent
// one (no handler registered, so the request errors) reports down.
func TestProbeLMStudioReportsModels(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyLMStudio(rt, "node.local", []string{"qwen2.5-7b", "llama-3.1-8b"})

	up, models := m.probeLMStudio("node.local", lmStudioPort)
	if !up {
		t.Fatal("expected lmstudio up")
	}
	if len(models) != 2 || models[0] != "qwen2.5-7b" || models[1] != "llama-3.1-8b" {
		t.Fatalf("models = %#v", models)
	}

	downUp, downModels := m.probeLMStudio("absent.local", lmStudioPort)
	if downUp || downModels != nil {
		t.Fatalf("expected absent lmstudio down, got up=%v models=%#v", downUp, downModels)
	}
}

// configureHealthyOMLX registers a 200 GET /v1/models on addr:1236
// returning the given model ids in the OpenAI list shape.
func configureHealthyOMLX(rt *fakeRoundTripper, addr string, models []string) {
	host := net.JoinHostPort(addr, "1236")
	rt.set(http.MethodGet, host, "/v1/models", func(*http.Request) (*http.Response, error) {
		var payload struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		payload.Object = "list"
		for _, id := range models {
			payload.Data = append(payload.Data, struct {
				ID string `json:"id"`
			}{ID: id})
		}
		data, _ := json.Marshal(payload)
		return httpJSON(http.StatusOK, string(data))
	})
}

// TestProbeOMLXReportsModels covers the oMLX probe: a reachable
// server reports up with its model ids parsed from /v1/models, a loopback-only
// 403 proxy also reports up, and an absent one reports down.
func TestProbeOMLXReportsModels(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyOMLX(rt, "node.local", []string{"mlx-qwen", "mlx-llama"})

	up, models := m.probeOMLX("node.local", omlxPort)
	if !up {
		t.Fatal("expected omlx up")
	}
	if len(models) != 2 || models[0] != "mlx-qwen" || models[1] != "mlx-llama" {
		t.Fatalf("models = %#v", models)
	}

	// Loopback-only proxy 403 response
	loopbackHost := net.JoinHostPort("loopback.local", "1236")
	rt.set(http.MethodGet, loopbackHost, "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusForbidden, `{"code":"loopback-only","error":"loopback only"}`)
	})
	lbUp, _ := m.probeOMLX("loopback.local", omlxPort)
	if !lbUp {
		t.Fatal("expected loopback-only omlx-proxy to report up")
	}

	downUp, downModels := m.probeOMLX("absent.local", omlxPort)
	if downUp || downModels != nil {
		t.Fatalf("expected absent omlx down, got up=%v models=%#v", downUp, downModels)
	}
}

func requestMessage(id int, method string, params any) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: paramsRaw}
}

func requestMessageRaw(id int, method string, params json.RawMessage) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: params}
}

func notificationMessage(method string, params any) *Message {
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", Method: method, Params: paramsRaw}
}

func readCaptureFrame(t *testing.T, rw *captureRW) Message {
	t.Helper()
	select {
	case data := <-rw.frames:
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
		return Message{}
	}
}

func readCaptureUntil(t *testing.T, rw *captureRW, match func(Message) bool) Message {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-rw.frames:
			var msg Message
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode frame %q: %v", data, err)
			}
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("timed out waiting for matching frame")
			return Message{}
		}
	}
}

func assertNoCaptureMethod(t *testing.T, rw *captureRW, method string) {
	t.Helper()
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case data := <-rw.frames:
			var msg Message
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode frame %q: %v", data, err)
			}
			if msg.Method == method {
				t.Fatalf("unexpected %s notification: %+v", method, msg)
			}
		case <-timer.C:
			return
		}
	}
}

func decodeResult[T any](t *testing.T, msg Message) T {
	t.Helper()
	if msg.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", msg.Error)
	}
	var result T
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatalf("decode result %q: %v", msg.Result, err)
	}
	return result
}

func decodeParams[T any](t *testing.T, msg Message) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(msg.Params, &result); err != nil {
		t.Fatalf("decode params %q: %v", msg.Params, err)
	}
	return result
}

func responseWithID(id int) func(Message) bool {
	return func(msg Message) bool {
		if msg.ID == nil {
			return false
		}
		var got int
		return json.Unmarshal(*msg.ID, &got) == nil && got == id
	}
}

func methodIs(method string) func(Message) bool {
	return func(msg Message) bool { return msg.Method == method }
}

func sampleInfo() NodeInfoResponse {
	return NodeInfoResponse{
		GPUs: []GPUInfo{{
			Name:               "RTX 6000",
			VramBytes:          48 << 30,
			VramUsedBytes:      12 << 30,
			UtilizationPercent: 42,
		}},
		CPU:            &CPUInfo{Name: "Threadripper", Cores: 64, UtilizationPercent: 7},
		Memory:         &MemoryInfo{TotalBytes: 128 << 30, UsedBytes: 32 << 30},
		TelemetryValid: true,
		MSSince:        137,
		SampleSeq:      42,
	}
}

func TestNodeID(t *testing.T) {
	if got := nodeID(ManualEntry{Name: "workstation", Address: "10.0.0.5"}); got != "workstation" {
		t.Fatalf("named nodeID = %q", got)
	}
	if got := nodeID(ManualEntry{Address: "10.0.0.5"}); got != "manual:10.0.0.5" {
		t.Fatalf("unnamed nodeID = %q", got)
	}
}

func TestNodeAddRespondsWithInitialStatusThenDiscoversProbeResult(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyNode(rt, "node.local", []string{"llama3", "mistral"}, sampleInfo())

	m.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "node.local", Name: "lab"}))

	resp := readCaptureUntil(t, rw, responseWithID(1))
	initial := decodeResult[ManualNodeStatus](t, resp)
	if initial.ID != "lab" || initial.Address != "node.local" {
		t.Fatalf("initial status = %+v", initial)
	}
	if initial.OllamaPort != 11434 || initial.NodeInfoPort != 14318 {
		t.Fatalf("default ports not set: %+v", initial)
	}
	if initial.OllamaUp || initial.NodeInfoUp {
		t.Fatalf("initial status should be unprobed: %+v", initial)
	}

	discovered := readCaptureUntil(t, rw, methodIs("node/discovered"))
	status := decodeParams[ManualNodeStatus](t, discovered)
	if !status.OllamaUp || !status.NodeInfoUp {
		t.Fatalf("discovered status did not include healthy services: %+v", status)
	}
	if len(status.OllamaModels) != 2 || status.OllamaModels[0] != "llama3" || status.OllamaModels[1] != "mistral" {
		t.Fatalf("models = %#v", status.OllamaModels)
	}
	if len(status.GPUs) != 1 || status.GPUs[0].Name != "RTX 6000" {
		t.Fatalf("gpus = %#v", status.GPUs)
	}
	if status.CPU == nil || status.CPU.Cores != 64 {
		t.Fatalf("cpu = %#v", status.CPU)
	}
	if status.Memory == nil || status.Memory.UsedBytes != 32<<30 {
		t.Fatalf("memory = %#v", status.Memory)
	}
	if !status.TelemetryValid || status.MSSince < 137 {
		t.Fatalf("telemetry = valid:%v age:%d, want true/at least 137", status.TelemetryValid, status.MSSince)
	}
	if status.SampleSeq != 42 {
		t.Fatalf("sampleSeq = %d, want 42", status.SampleSeq)
	}
}

func TestNodeAddValidationErrors(t *testing.T) {
	m, rw, _ := newTestManager()

	m.handleMessage(requestMessageRaw(1, "node/add", json.RawMessage(`"bad"`)))
	resp := readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("malformed params error = %+v", resp.Error)
	}

	m.handleMessage(requestMessage(2, "node/add", ManualEntry{}))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("missing address error = %+v", resp.Error)
	}

	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("validation errors added nodes: %#v", got)
	}
}

func TestNodesListReturnsCurrentStatuses(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())

	m.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "node.local"}))
	_ = readCaptureUntil(t, rw, methodIs("node/discovered"))

	m.handleMessage(requestMessage(2, "nodes/list", nil))
	resp := readCaptureUntil(t, rw, responseWithID(2))
	var result struct {
		Nodes []ManualNodeStatus `json:"nodes"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode nodes/list: %v", err)
	}
	if len(result.Nodes) != 1 {
		t.Fatalf("nodes/list length = %d", len(result.Nodes))
	}
	if !result.Nodes[0].OllamaUp || !result.Nodes[0].NodeInfoUp {
		t.Fatalf("nodes/list status = %+v", result.Nodes[0])
	}
}

func TestNodeRemoveReturnsRemovedAndNotifies(t *testing.T) {
	m, rw, _ := newTestManager()
	m.nodes["lab"] = &trackedNode{
		entry:  ManualEntry{Name: "lab", Address: "node.local"},
		status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318},
	}

	m.handleMessage(requestMessage(1, "node/remove", map[string]string{"id": "lab"}))
	removed := readCaptureFrame(t, rw)
	if removed.Method != "node/removed" {
		t.Fatalf("first remove frame = %+v", removed)
	}
	status := decodeParams[ManualNodeStatus](t, removed)
	if status.ID != "lab" {
		t.Fatalf("removed params = %+v", status)
	}
	resp := readCaptureUntil(t, rw, responseWithID(1))
	result := decodeResult[map[string]bool](t, resp)
	if !result["removed"] {
		t.Fatalf("removed result = %#v", result)
	}
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("node still listed after removal: %#v", got)
	}

	m.handleMessage(requestMessage(2, "node/remove", map[string]string{"id": "lab"}))
	resp = readCaptureUntil(t, rw, responseWithID(2))
	result = decodeResult[map[string]bool](t, resp)
	if result["removed"] {
		t.Fatalf("second removal result = %#v", result)
	}
	assertNoCaptureMethod(t, rw, "node/removed")
}

func TestAddThenRemoveBeforeInitialProbeDoesNotRediscover(t *testing.T) {
	m, rw, rt := newTestManager()
	started := make(chan struct{})
	release := make(chan struct{})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[]}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"GPUs":[]}`)
	})

	status := m.addNode(ManualEntry{Address: "node.local", Name: "lab"})
	if status.ID != "lab" {
		t.Fatalf("add status = %+v", status)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("initial probe did not start")
	}

	if !m.removeNode("lab") {
		t.Fatal("removeNode returned false")
	}
	_ = readCaptureUntil(t, rw, methodIs("node/removed"))
	close(release)

	assertNoCaptureMethod(t, rw, "node/discovered")
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("node rediscovered in state: %#v", got)
	}
}

func TestProbeNodeEmitsUpdatedOnStateChange(t *testing.T) {
	m, rw, rt := newTestManager()
	m.nodes["lab"] = &trackedNode{entry: ManualEntry{Name: "lab", Address: "node.local"}, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if len(first.OllamaModels) != 1 || first.OllamaModels[0] != "llama3" {
		t.Fatalf("first update models = %#v", first.OllamaModels)
	}

	configureHealthyNode(rt, "node.local", []string{"mistral"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	second := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if len(second.OllamaModels) != 1 || second.OllamaModels[0] != "mistral" {
		t.Fatalf("second update models = %#v", second.OllamaModels)
	}
}

func TestProbeNodeNoUpdateWhenStable(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}
	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())

	m.probeNode(entry)
	_ = readCaptureUntil(t, rw, methodIs("node/updated"))
	m.probeNode(entry)

	assertNoCaptureMethod(t, rw, "node/updated")
}

func TestProbeFailuresClearAvailability(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaUp:     true,
			OllamaPort:   11434,
			OllamaModels: []string{"llama3"},
			NodeInfoUp:   true,
			NodeInfoPort: 14318,
			GPUs:         sampleInfo().GPUs,
			CPU:          sampleInfo().CPU,
			Memory:       sampleInfo().Memory,
		},
	}
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("ollama down")
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{not json`)
	})

	m.probeNode(entry)
	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if updated.OllamaUp || updated.NodeInfoUp {
		t.Fatalf("services should be down: %+v", updated)
	}
	if len(updated.OllamaModels) != 0 || len(updated.GPUs) != 0 || updated.CPU != nil || updated.Memory != nil {
		t.Fatalf("failed probe retained stale fields: %+v", updated)
	}
}

// TestProbeFailurePreservesHostUUID: a node-info blip
// (while Ollama stays reachable) must NOT blank the learned HostUUID, or the
// broker would rekey the live node UUID -> manual-id -> UUID.
func TestProbeFailurePreservesHostUUID(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	info := sampleInfo()
	info.HostUUID = "node-uuid"
	configureHealthyNode(rt, "node.local", []string{"llama3"}, info)

	// First probe learns the UUID.
	m.probeNode(entry)
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if first.HostUUID != "node-uuid" {
		t.Fatalf("first probe HostUUID = %q, want node-uuid", first.HostUUID)
	}

	// node-info goes down while Ollama stays up: the UUID must be preserved.
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("node-info down")
	})
	m.probeNode(entry)
	down := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if down.NodeInfoUp {
		t.Fatal("node-info should be down")
	}
	if down.HostUUID != "node-uuid" {
		t.Fatalf("HostUUID dropped on node-info failure: %q", down.HostUUID)
	}

	// node-info recovers: still the same UUID (no flap).
	configureHealthyNode(rt, "node.local", []string{"llama3"}, info)
	m.probeNode(entry)
	up := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if up.HostUUID != "node-uuid" {
		t.Fatalf("HostUUID after recovery = %q, want node-uuid", up.HostUUID)
	}
}

func TestManualTelemetryAgeIncludesProbeDuration(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaPort:   11434,
			NodeInfoPort: 14318,
		},
	}

	simulatedTime := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return simulatedTime
	}

	info := sampleInfo()
	info.TelemetryValid = true
	info.MSSince = 1000

	infoHost := net.JoinHostPort("node.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})

	ollamaHost := net.JoinHostPort("node.local", "11434")
	rt.set(http.MethodGet, ollamaHost, "/", func(*http.Request) (*http.Response, error) {
		mu.Lock()
		simulatedTime = simulatedTime.Add(2500 * time.Millisecond)
		mu.Unlock()
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, ollamaHost, "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3"}]}`)
	})

	m.probeNode(entry)

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if !updated.TelemetryValid {
		t.Fatalf("expected TelemetryValid to be true, got false")
	}
	if updated.MSSince < 3500 {
		t.Fatalf("expected published MSSince >= 3500, got %d", updated.MSSince)
	}
}

func TestManualTelemetryAgeInvalidTelemetryZero(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaPort:   11434,
			NodeInfoPort: 14318,
		},
	}

	simulatedTime := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return simulatedTime
	}

	info := sampleInfo()
	info.TelemetryValid = false
	info.MSSince = 1000

	infoHost := net.JoinHostPort("node.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})

	ollamaHost := net.JoinHostPort("node.local", "11434")
	rt.set(http.MethodGet, ollamaHost, "/", func(*http.Request) (*http.Response, error) {
		mu.Lock()
		simulatedTime = simulatedTime.Add(2500 * time.Millisecond)
		mu.Unlock()
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, ollamaHost, "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3"}]}`)
	})

	m.probeNode(entry)

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if updated.TelemetryValid {
		t.Fatalf("expected TelemetryValid false, got true")
	}
	if updated.MSSince != 0 {
		t.Fatalf("expected MSSince 0 for invalid telemetry, got %d", updated.MSSince)
	}
}

func TestManualTelemetryAgeSaturatesOnOverflow(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaPort:   11434,
			NodeInfoPort: 14318,
		},
	}

	simulatedTime := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return simulatedTime
	}

	info := sampleInfo()
	info.TelemetryValid = true
	info.MSSince = math.MaxInt64 - 500

	infoHost := net.JoinHostPort("node.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})

	ollamaHost := net.JoinHostPort("node.local", "11434")
	rt.set(http.MethodGet, ollamaHost, "/", func(*http.Request) (*http.Response, error) {
		mu.Lock()
		simulatedTime = simulatedTime.Add(2500 * time.Millisecond)
		mu.Unlock()
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, ollamaHost, "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3"}]}`)
	})

	m.probeNode(entry)

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if !updated.TelemetryValid {
		t.Fatalf("expected TelemetryValid true, got false")
	}
	if updated.MSSince != math.MaxInt64 {
		t.Fatalf("expected MSSince saturated to %d, got %d", int64(math.MaxInt64), updated.MSSince)
	}
}

func TestManualTelemetryAgeNeverDecreases(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaPort:   11434,
			NodeInfoPort: 14318,
		},
	}

	simulatedTime := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return simulatedTime
	}

	info := sampleInfo()
	info.TelemetryValid = true
	info.MSSince = 1000

	infoHost := net.JoinHostPort("node.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})

	ollamaHost := net.JoinHostPort("node.local", "11434")
	rt.set(http.MethodGet, ollamaHost, "/", func(*http.Request) (*http.Response, error) {
		mu.Lock()
		simulatedTime = simulatedTime.Add(-500 * time.Millisecond)
		mu.Unlock()
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, ollamaHost, "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3"}]}`)
	})

	m.probeNode(entry)

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if !updated.TelemetryValid {
		t.Fatalf("expected TelemetryValid true, got false")
	}
	if updated.MSSince < 1000 {
		t.Fatalf("expected MSSince >= 1000, got %d", updated.MSSince)
	}
}


func TestCPUAndMemoryNilAwareEquality(t *testing.T) {
	if !cpuEqual(nil, nil) || !memoryEqual(nil, nil) {
		t.Fatal("nil values should compare equal")
	}
	if cpuEqual(nil, &CPUInfo{}) || memoryEqual(nil, &MemoryInfo{}) {
		t.Fatal("nil and non-nil values should differ")
	}
	if !cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 8}) {
		t.Fatal("equal CPU values differed")
	}
	if cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 16}) {
		t.Fatal("different CPU values compared equal")
	}
	if !memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 5}) {
		t.Fatal("equal memory values differed")
	}
	if memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 6}) {
		t.Fatal("different memory values compared equal")
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, "bogus", nil))
	resp := readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("unknown method error = %+v", resp.Error)
	}
}

func TestLogSetLevelRequest(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, applog.SetLevelMethod, applog.SetLevelParams{Level: "debug"}))
	resp := readCaptureFrame(t, rw)
	result := decodeResult[map[string]string](t, resp)
	if result["level"] != "debug" {
		t.Fatalf("log/set-level result = %#v", result)
	}

	m.handleMessage(requestMessage(2, applog.SetLevelMethod, applog.SetLevelParams{Level: "not-a-level"}))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("invalid log/set-level error = %+v", resp.Error)
	}
}

func TestShutdownRequestCancelsRun(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	mgr, err := NewManager(NewCodec(server), tlsClientOptions{}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- mgr.Run(ctx)
	}()

	reader := bufio.NewReader(client)
	ready := readPipeFrame(t, client, reader)
	if ready.Method != "ready" {
		t.Fatalf("first frame = %+v", ready)
	}

	writePipeRequest(t, client, 7, "shutdown", nil)
	resp := readPipeFrame(t, client, reader)
	if !responseWithID(7)(resp) || resp.Error != nil {
		t.Fatalf("shutdown response = %+v", resp)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestNotificationIsIgnored(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(notificationMessage("node/add", ManualEntry{Address: "node.local"}))
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("notification mutated state: %#v", got)
	}
	assertNoCaptureMethod(t, rw, "")
}

func TestSyncClusterMembers(t *testing.T) {
	clusterDir := t.TempDir()
	selfUUID := "11111111-1111-1111-1111-111111111111"
	peerUUID := "22222222-2222-2222-2222-222222222222"

	// Write self identity
	idJSON := fmt.Sprintf(`{"node_uuid": %q}`, selfUUID)
	if err := os.WriteFile(filepath.Join(clusterDir, "identity.json"), []byte(idJSON), 0o600); err != nil {
		t.Fatalf("write identity.json: %v", err)
	}

	// Write members.json with self and remote peer
	members := []clusterMemberEntry{
		{
			ID:        "Self",
			NodeUUID:  selfUUID,
			Name:      "Self",
			IPAddress: "127.0.0.1",
			State:     "member",
		},
		{
			ID:        "Peer",
			NodeUUID:  peerUUID,
			Name:      "Peer",
			IPAddress: "100.107.205.77",
			State:     "member",
		},
	}
	data, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	membersPath := filepath.Join(clusterDir, "members.json")
	if err := os.WriteFile(membersPath, data, 0o600); err != nil {
		t.Fatalf("write members.json: %v", err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil, clusterDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	m.syncClusterMembers()

	nodes := m.listNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 synced node, got %d: %#v", len(nodes), nodes)
	}
	if nodes[0].HostUUID != peerUUID {
		t.Fatalf("expected HostUUID %s, got %s", peerUUID, nodes[0].HostUUID)
	}
	if nodes[0].ClusterUUID != peerUUID {
		t.Fatalf("expected ClusterUUID %s, got %s", peerUUID, nodes[0].ClusterUUID)
	}
	if nodes[0].Address != "100.107.205.77" {
		t.Fatalf("expected Address 100.107.205.77, got %s", nodes[0].Address)
	}

	// Now remove peer from members.json
	onlySelf := []clusterMemberEntry{members[0]}
	dataSelf, err := json.Marshal(onlySelf)
	if err != nil {
		t.Fatalf("marshal onlySelf: %v", err)
	}
	// Touch mtime slightly in future to ensure mod time changes
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(membersPath, dataSelf, 0o600); err != nil {
		t.Fatalf("write updated members.json: %v", err)
	}
	_ = os.Chtimes(membersPath, future, future)

	m.syncClusterMembers()

	nodesAfter := m.listNodes()
	if len(nodesAfter) != 0 {
		t.Fatalf("expected 0 nodes after removal, got %d: %#v", len(nodesAfter), nodesAfter)
	}
}

func TestAddNodeSupersedesClusterSeededEntryAtSameAddress(t *testing.T) {
	clusterDir := t.TempDir()
	selfUUID := "11111111-1111-1111-1111-111111111111"
	peerUUID := "22222222-2222-2222-2222-222222222222"

	idData := []byte(`{"node_uuid":"` + selfUUID + `"}`)
	if err := os.WriteFile(filepath.Join(clusterDir, "identity.json"), idData, 0o600); err != nil {
		t.Fatalf("write identity.json: %v", err)
	}

	members := []clusterMemberEntry{
		{
			ID:        "Self",
			NodeUUID:  selfUUID,
			Name:      "Self",
			IPAddress: "127.0.0.1",
			State:     "member",
		},
		{
			ID:        "desktop-peer.local",
			NodeUUID:  peerUUID,
			Name:      "desktop-peer.local",
			IPAddress: "192.0.2.2",
			State:     "member",
		},
	}
	data, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "members.json"), data, 0o600); err != nil {
		t.Fatalf("write members.json: %v", err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil, clusterDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.client = &http.Client{Transport: newFakeRoundTripper()}

	m.syncClusterMembers()
	if got := len(m.listNodes()); got != 1 {
		t.Fatalf("expected 1 cluster-seeded node, got %d", got)
	}

	// Replaying a user-added manual node for the same IP with a custom friendly
	// name must replace the cluster-seeded entry rather than keeping two entries
	// for the same address.
	st := m.addNode(ManualEntry{Address: "192.0.2.2", Name: "manual-peer"})
	if st.HostUUID != peerUUID {
		t.Fatalf("expected user-added node to inherit known HostUUID %q, got %q", peerUUID, st.HostUUID)
	}
	nodes := m.listNodes()
	if len(nodes) != 1 || nodes[0].ID != "manual-peer" {
		t.Fatalf("expected single superseded node 'manual-peer', got %+v", nodes)
	}

	// Removing the user-added alias restores the cluster member entry from members.json.
	if !m.removeNode("manual-peer") {
		t.Fatalf("expected removeNode('manual-peer') to succeed")
	}
	nodesAfter := m.listNodes()
	if len(nodesAfter) != 1 || nodesAfter[0].ID != "desktop-peer.local" {
		t.Fatalf("expected cluster member 'desktop-peer.local' restored after user alias removal, got %+v", nodesAfter)
	}
}

func TestProbeEngineManagerReportsModels(t *testing.T) {
	m, _, rt := newTestManager()

	infoHost := net.JoinHostPort("node.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"hostUuid":"uuid-node-1"}`)
	})

	emHost := net.JoinHostPort("node.local", "14322")
	rt.set(http.MethodGet, emHost, "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{
			"models": ["llama3:8b", "qwen2.5:7b", "mlx-qwen"],
			"modelsByEngine": {
				"ollama": ["llama3:8b"],
				"lmstudio": ["qwen2.5:7b"],
				"omlx": ["mlx-qwen"]
			},
			"loadedByEngine": {
				"ollama": ["llama3:8b"],
				"omlx": ["mlx-qwen"]
			}
		}`)
	})

	m.addNode(ManualEntry{Address: "node.local", Name: "test-node"})
	m.probeNode(ManualEntry{Address: "node.local", Name: "test-node"})

	nodes := m.listNodes()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if !n.OllamaUp {
		t.Errorf("expected OllamaUp = true")
	}
	if !n.LMStudioUp {
		t.Errorf("expected LMStudioUp = true")
	}
	if !n.OMLXUp {
		t.Errorf("expected OMLXUp = true")
	}
	if n.OMLXPort != 1236 {
		t.Errorf("expected OMLXPort = 1236, got %d", n.OMLXPort)
	}
	if len(n.Models) != 3 || n.Models[0] != "llama3:8b" || n.Models[1] != "qwen2.5:7b" || n.Models[2] != "mlx-qwen" {
		t.Errorf("unexpected Models: %#v", n.Models)
	}
	if len(n.ModelsByEngine["ollama"]) != 1 || n.ModelsByEngine["ollama"][0] != "llama3:8b" {
		t.Errorf("unexpected ModelsByEngine[ollama]: %#v", n.ModelsByEngine["ollama"])
	}
	if len(n.ModelsByEngine["lmstudio"]) != 1 || n.ModelsByEngine["lmstudio"][0] != "qwen2.5:7b" {
		t.Errorf("unexpected ModelsByEngine[lmstudio]: %#v", n.ModelsByEngine["lmstudio"])
	}
	if len(n.ModelsByEngine["omlx"]) != 1 || n.ModelsByEngine["omlx"][0] != "mlx-qwen" {
		t.Errorf("unexpected ModelsByEngine[omlx]: %#v", n.ModelsByEngine["omlx"])
	}
	if len(n.LoadedByEngine["ollama"]) != 1 || n.LoadedByEngine["ollama"][0] != "llama3:8b" {
		t.Errorf("unexpected LoadedByEngine[ollama]: %#v", n.LoadedByEngine["ollama"])
	}
	if len(n.LoadedByEngine["omlx"]) != 1 || n.LoadedByEngine["omlx"][0] != "mlx-qwen" {
		t.Errorf("unexpected LoadedByEngine[omlx]: %#v", n.LoadedByEngine["omlx"])
	}
}

func TestProbeNodeSkipsPlaintextEngineProbesForClusteredNode(t *testing.T) {
	m, _, rt := newTestManager()

	var plaintextProbes int
	for _, port := range []string{"11434", "1234", "1236"} {
		host := net.JoinHostPort("clustered.local", port)
		rt.set(http.MethodGet, host, "/", func(*http.Request) (*http.Response, error) {
			plaintextProbes++
			return httpJSON(http.StatusForbidden, `{"error":"cluster peers must use mTLS"}`)
		})
		rt.set(http.MethodGet, host, "/v1/models", func(*http.Request) (*http.Response, error) {
			plaintextProbes++
			return httpJSON(http.StatusForbidden, `{"error":"cluster peers must use mTLS"}`)
		})
	}

	infoHost := net.JoinHostPort("clustered.local", "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"hostUuid":"uuid-clustered-1","clusterUuid":"uuid-clustered-1"}`)
	})

	entry := ManualEntry{Address: "clustered.local", Name: "clustered-peer", MTLS: true}
	m.addNode(entry)
	m.probeNode(entry)

	if plaintextProbes != 0 {
		t.Fatalf("expected 0 plaintext engine probes for clustered/mTLS node, got %d", plaintextProbes)
	}
}

func readPipeFrame(t *testing.T, conn net.Conn, reader *bufio.Reader) Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pipe frame: %v", err)
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		t.Fatalf("decode pipe frame %q: %v", line, err)
	}
	return msg
}

func writePipeRequest(t *testing.T, conn net.Conn, id int, method string, params any) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
	}
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func TestTelemetryIntervalForNodeDeterministicAndBounded(t *testing.T) {
	if got := telemetryIntervalForNode(""); got != 2*time.Second {
		t.Fatalf("empty key interval = %v, want 2s", got)
	}
	first := telemetryIntervalForNode("manual:node-a")
	if again := telemetryIntervalForNode("manual:node-a"); again != first {
		t.Fatalf("node interval changed: %v vs %v", first, again)
	}
	minInterval := 2*time.Second - 250*time.Millisecond
	maxInterval := 2*time.Second + 250*time.Millisecond
	if first < minInterval || first > maxInterval {
		t.Fatalf("node interval %v out of bounds [%v, %v]", first, minInterval, maxInterval)
	}
	second := telemetryIntervalForNode("manual:node-b")
	if second == first {
		t.Fatalf("distinct keys produced identical interval: %v", first)
	}
}

func TestNodeTelemetryEmittedForReachableNodeAndNoneForUnreachable(t *testing.T) {
	m, rw, rt := newTestManager()
	m.telemetryInterval = func(_ string) time.Duration {
		return 10 * time.Millisecond
	}

	info := sampleInfo()
	info.HostUUID = "peer-uuid-1"
	info.TelemetryValid = true
	info.MSSince = 120
	info.SampleSeq = 42
	info.GPUs = []GPUInfo{
		{Name: "GPU 0", UtilizationPercent: 10},
		{Name: "GPU 1", UtilizationPercent: 77},
	}

	var pollHeader string
	rt.set(http.MethodGet, net.JoinHostPort("reachable.local", "14318"), "/v1/node-info", func(req *http.Request) (*http.Response, error) {
		pollHeader = req.Header.Get("X-NVPAIR-Poll")
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})
	rt.set(http.MethodGet, net.JoinHostPort("reachable.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("reachable.local", "11434"), "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[]}`)
	})

	m.addNode(ManualEntry{Address: "reachable.local", Name: "reachable-node"})

	msg := readCaptureUntil(t, rw, methodIs("node/telemetry"))
	var telem NodeTelemetryNotification
	if err := json.Unmarshal(msg.Params, &telem); err != nil {
		t.Fatalf("unmarshal node/telemetry params: %v", err)
	}
	if telem.ID != "reachable-node" {
		t.Errorf("expected ID = reachable-node, got %q", telem.ID)
	}
	if telem.HostUUID != "peer-uuid-1" {
		t.Errorf("expected HostUUID = peer-uuid-1, got %q", telem.HostUUID)
	}
	if telem.GPUUtilizationPct != 77 {
		t.Errorf("expected GPUUtilizationPct = 77, got %d", telem.GPUUtilizationPct)
	}
	if !telem.TelemetryValid {
		t.Errorf("expected TelemetryValid = true")
	}
	if telem.MSSince < 120 {
		t.Errorf("expected MSSince >= 120, got %d", telem.MSSince)
	}
	if telem.SampleSeq != 42 {
		t.Errorf("expected SampleSeq = 42, got %d", telem.SampleSeq)
	}
	if pollHeader != "scheduling" {
		t.Errorf("expected X-NVPAIR-Poll = scheduling, got %q", pollHeader)
	}

	// Remove reachable node and drain frames so its loop stops
	if !m.removeNode("reachable-node") {
		t.Fatal("failed to remove reachable-node")
	}
	_ = readCaptureUntil(t, rw, methodIs("node/removed"))

	// Now verify an unreachable node emits NO node/telemetry
	rt.set(http.MethodGet, net.JoinHostPort("unreachable.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	rt.set(http.MethodGet, net.JoinHostPort("unreachable.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	m.addNode(ManualEntry{Address: "unreachable.local", Name: "unreachable-node"})
	assertNoCaptureMethod(t, rw, "node/telemetry")
}

func TestNodeTelemetryLoopStopsOnRemove(t *testing.T) {
	m, rw, rt := newTestManager()
	m.telemetryInterval = func(_ string) time.Duration {
		return 10 * time.Millisecond
	}

	info := sampleInfo()
	info.HostUUID = "peer-uuid-1"
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[]}`)
	})

	m.addNode(ManualEntry{Address: "node.local", Name: "node-1"})
	_ = readCaptureUntil(t, rw, methodIs("node/telemetry"))

	if !m.removeNode("node-1") {
		t.Fatal("failed to remove node")
	}

	_ = readCaptureUntil(t, rw, methodIs("node/removed"))
	assertNoCaptureMethod(t, rw, "node/telemetry")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTelemetryLoopReusesKeepAliveConnections(t *testing.T) {
	var newConns atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/node-info" {
			w.Header().Set("Content-Type", "application/json")
			info := sampleInfo()
			info.HostUUID = "peer-uuid"
			info.TelemetryValid = true
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		http.NotFound(w, r)
	}))
	ts.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	ts.StartTLS()
	defer ts.Close()

	host, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Route 10s topology probes to a dummy roundtripper so only telemetry connects to ts
	dummy := newFakeRoundTripper()
	m.client = &http.Client{Transport: dummy}
	m.tlsClient = &http.Client{Transport: dummy}

	// Configure telemetryTLSClient with the test server's TLS config and keep-alives enabled
	tlsTransport := telemetryTransport(ts.Client().Transport.(*http.Transport).TLSClientConfig)
	m.telemetryTLSClient = &http.Client{
		Timeout:   telemetryProbeTimeout,
		Transport: tlsTransport,
	}

	m.telemetryInterval = func(_ string) time.Duration {
		return 10 * time.Millisecond
	}

	m.addNode(ManualEntry{Address: host, TLSPort: port, Name: "test-node"})

	// Collect several node/telemetry frames across ticks
	for i := 0; i < 4; i++ {
		_ = readCaptureUntil(t, rw, methodIs("node/telemetry"))
	}

	if !m.removeNode("test-node") {
		t.Fatal("failed to remove node")
	}

	// With keep-alive connection reuse, only 1 connection is dialed
	if got := newConns.Load(); got != 1 {
		t.Fatalf("expected 1 connection created across multiple telemetry ticks, got %d", got)
	}
}

func TestTelemetryTimeoutBelowInterval(t *testing.T) {
	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.telemetryClient.Timeout != 1500*time.Millisecond {
		t.Errorf("telemetryClient timeout = %v, want 1.5s", m.telemetryClient.Timeout)
	}
	if m.telemetryTLSClient.Timeout != 1500*time.Millisecond {
		t.Errorf("telemetryTLSClient timeout = %v, want 1.5s", m.telemetryTLSClient.Timeout)
	}
	minInterval := 2*time.Second - 250*time.Millisecond
	if m.telemetryClient.Timeout >= minInterval {
		t.Errorf("telemetryClient timeout %v is not strictly below min interval %v", m.telemetryClient.Timeout, minInterval)
	}
}

func TestTelemetryCancellationOnRemovalAbortsInFlightRequest(t *testing.T) {
	m, rw, _ := newTestManager()
	m.telemetryInterval = func(_ string) time.Duration {
		return 5 * time.Millisecond
	}

	inFlight := make(chan struct{})
	reqAborted := make(chan struct{})

	m.telemetryClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/v1/node-info") {
				select {
				case <-inFlight:
				default:
					close(inFlight)
				}
				<-req.Context().Done()
				select {
				case <-reqAborted:
				default:
					close(reqAborted)
				}
				return nil, req.Context().Err()
			}
			return httpJSON(http.StatusOK, `{}`)
		}),
	}

	m.addNode(ManualEntry{Address: "slow.local", Name: "slow-node"})

	// Wait until the request is in-flight
	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for telemetry request to enter in-flight")
	}

	// Removing the node must cancel its context and abort the in-flight request immediately
	if !m.removeNode("slow-node") {
		t.Fatal("failed to remove slow-node")
	}

	select {
	case <-reqAborted:
		// In-flight request was aborted immediately by context cancellation
	case <-time.After(500 * time.Millisecond):
		t.Fatal("in-flight telemetry request was not aborted on node removal")
	}

	_ = readCaptureUntil(t, rw, methodIs("node/removed"))
}


