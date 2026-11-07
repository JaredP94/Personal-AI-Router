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
	"testing"
	"time"
)

func TestBrokerLlamaCPPProxyRoutesOpenAI(t *testing.T) {
	const model = "fixture-model.gguf"
	const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"fixture\"}}]}\n\ndata: [DONE]\n\n"
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"fixture-model.gguf"}]}`)
		case "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != model {
				t.Errorf("wrong routed model: %q %v", body.Model, err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer engine.Close()
	stdin, msgs, stderr, cleanup := startBrokerWith(t, "--llamacpp-proxy-path", llamacppProxyBin)
	defer cleanup()
	go func() {
		for range stderr {
		}
	}()
	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	port := waitRoutingProxyReady(t, stdin, msgs, "llamacpp-proxy")
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":50001,"method":"workers:get-status"}`)
	inventory := waitForResponseID(t, msgs, 50001, 5*time.Second)
	var status struct {
		Workers []struct {
			Name          string `json:"name"`
			RestartOnHang bool   `json:"restartOnHang"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(inventory.Result, &status); err != nil || len(status.Workers) != 13 {
		t.Fatalf("worker inventory: %s %v", inventory.Result, err)
	}
	found := false
	for _, worker := range status.Workers {
		if worker.Name == "llamacpp-proxy" {
			found = true
			if worker.RestartOnHang {
				t.Fatal("proxy must require manual hang restart")
			}
		}
	}
	if !found {
		t.Fatal("new worker missing")
	}
	callBrokerRPC(t, stdin, msgs, 50002, "llamacpp-proxy:node/add-manual", map[string]any{"id": "fixture", "host": "127.0.0.1", "addresses": []string{"127.0.0.1"}, "port": portOfURL(t, engine.URL), "models": []string{model}})
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), "application/json", bytes.NewBufferString(`{"model":"fixture-model.gguf","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != stream {
		t.Fatalf("stream response: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
	response, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&models); err != nil || response.StatusCode != http.StatusOK || len(models.Data) != 1 || models.Data[0].ID != model {
		t.Fatalf("inventory: %+v %v", models, err)
	}
}
