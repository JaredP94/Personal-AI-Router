// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"nvpair-shared/prefixhash"
)

func TestRealServerTruncatedStreamEmitsFailedCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
	}))
	defer upstream.Close()

	rec := &prRec{}
	discovery := NewDiscovery()
	discovery.AddManual(nodeForModel(t, "node-a", upstream.URL, "model"))
	p := NewProxy(NewCodec(rec), discovery, 1235)
	defer p.closeIdleTransports()
	server := httptest.NewServer(http.HandlerFunc(p.handleHTTP))
	defer server.Close()

	body := []byte(`{"model":"model","messages":[{"role":"user","content":"private canary"}]}`)
	response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr == nil {
		t.Fatal("truncated upstream stream reached the client without an error")
	}
	if !rec.has(`"method":"proxy/request"`) || !rec.has(`"error":"upstream response stream aborted"`) {
		t.Fatal("truncated real-server stream omitted its failed completion event")
	}
	context := prefixhash.Extract(http.MethodPost, "/v1/chat/completions", body)
	if _, ok := p.prt.Lookup("model", context.Full); ok {
		t.Fatal("truncated real-server stream registered a cache-affinity hint")
	}
}
