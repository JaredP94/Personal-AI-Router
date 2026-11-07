// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

type routingUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newRoutingUpstream(t *testing.T, status int) *routingUpstream {
	t.Helper()
	upstream := &routingUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"done":true,"choices":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// Match only the requested status response; startup notifications and earlier
// workers' replies must not supply another proxy's port.
func waitRoutingProxyReady(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, prefix string) int {
	t.Helper()
	id := map[string]int{"proxy": 10000, "lmstudio-proxy": 20000, "omlx-proxy": 30000, "llamacpp-proxy": 40000}[prefix]
	send := func() {
		writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, prefix+":get-status"))
	}
	send()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed waiting for proxy readiness")
			}
			if msg.ID == nil || string(*msg.ID) != fmt.Sprint(id) {
				continue
			}
			var status struct {
				Ready bool `json:"ready"`
				Port  int  `json:"port"`
			}
			if json.Unmarshal(msg.Result, &status) == nil && status.Ready {
				return status.Port
			}
		case <-tick.C:
			id++
			send()
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s readiness", prefix)
		}
	}
}

func TestStrictModelRoutingAcrossProcesses(t *testing.T) {
	if portBusy(11435) || portBusy(1234) {
		t.Skip("ollama-proxy (11435) or lmstudio-proxy (1234) default port already in use; skipping")
	}

	owner404 := newRoutingUpstream(t, http.StatusNotFound)
	ownerOK := newRoutingUpstream(t, http.StatusOK)
	ineligible := newRoutingUpstream(t, http.StatusOK)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin,
		"--lmstudio-proxy-path", lmstudioProxyBin,
		"--omlx-proxy-path", omlxProxyBin,
		"--llamacpp-proxy-path", llamacppProxyBin,
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	ollamaPort := waitRoutingProxyReady(t, stdin, msgs, "proxy")
	lmstudioPort := waitRoutingProxyReady(t, stdin, msgs, "lmstudio-proxy")
	omlxPort := waitRoutingProxyReady(t, stdin, msgs, "omlx-proxy")
	llamacppPort := waitRoutingProxyReady(t, stdin, msgs, "llamacpp-proxy")

	type proxyCase struct {
		name      string
		rpcPrefix string
		path      string
		port      int
	}
	cases := []proxyCase{
		{name: "ollama", rpcPrefix: "proxy", path: "/api/chat", port: ollamaPort},
		{name: "ollama-anthropic", rpcPrefix: "proxy", path: "/v1/messages", port: ollamaPort},
		{name: "lmstudio", rpcPrefix: "lmstudio-proxy", path: "/v1/chat/completions", port: lmstudioPort},
		{name: "lmstudio-anthropic", rpcPrefix: "lmstudio-proxy", path: "/v1/messages", port: lmstudioPort},
		{name: "omlx-anthropic", rpcPrefix: "omlx-proxy", path: "/v1/messages", port: omlxPort},
		{name: "llamacpp", rpcPrefix: "llamacpp-proxy", path: "/v1/chat/completions", port: llamacppPort},
		{name: "llamacpp-anthropic", rpcPrefix: "llamacpp-proxy", path: "/v1/messages", port: llamacppPort},
	}
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	snapshot := func() [3]int32 {
		return [3]int32{owner404.hits.Load(), ownerOK.hits.Load(), ineligible.hits.Load()}
	}

	for caseIndex, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targetModel := "strict-routing-" + tc.name
			owner404ID := tc.name + "-owner-404"
			ownerOKID := tc.name + "-owner-ok"
			missingID := tc.name + "-known-missing"
			unknownID := tc.name + "-unknown"
			nodes := []struct {
				id     string
				port   int
				models []string
			}{
				{id: owner404ID, port: portOfURL(t, owner404.server.URL), models: []string{targetModel}},
				{id: ownerOKID, port: portOfURL(t, ownerOK.server.URL), models: []string{targetModel}},
				{id: missingID, port: portOfURL(t, ineligible.server.URL), models: []string{"different-model"}},
				{id: unknownID, port: portOfURL(t, ineligible.server.URL)},
			}

			requestID := 1000 + caseIndex*10
			for _, node := range nodes {
				params := map[string]any{
					"id":        node.id,
					"host":      "127.0.0.1",
					"port":      node.port,
					"addresses": []string{"127.0.0.1"},
				}
				if node.models != nil {
					params["models"] = node.models
				}
				callBrokerRPC(t, stdin, msgs, requestID, tc.rpcPrefix+":node/add-manual", params)
				requestID++
			}
			callBrokerRPC(t, stdin, msgs, requestID, tc.rpcPrefix+":node/set-priority", map[string]any{
				"nodes": []string{missingID, unknownID, owner404ID, ownerOKID},
			})

			before := snapshot()
			endpoint := fmt.Sprintf("http://127.0.0.1:%d%s", tc.port, tc.path)
			resp, err := client.Post(endpoint, "application/json",
				bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, targetModel)))
			if err != nil {
				t.Fatalf("target-model request failed: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("target-model response = %d %s, want owner failover success", resp.StatusCode, body)
			}
			after := snapshot()
			if after[0] != before[0]+1 || after[1] != before[1]+1 || after[2] != before[2] {
				t.Fatalf("target-model hit deltas = %v -> %v, want owner 404/200 once and ineligible zero",
					before, after)
			}

			before = snapshot()
			resp, err = client.Post(endpoint, "application/json",
				bytes.NewBufferString(`{"model":"no-advertised-owner","messages":[]}`))
			if err != nil {
				t.Fatalf("ownerless request failed: %v", err)
			}
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway ||
				!strings.Contains(string(body), "no available node advertises the requested model") {
				t.Fatalf("ownerless response = %d %s, want actionable local 502", resp.StatusCode, body)
			}
			if after = snapshot(); after != before {
				t.Fatalf("ownerless request reached an upstream: hits %v -> %v", before, after)
			}
		})
	}
}
