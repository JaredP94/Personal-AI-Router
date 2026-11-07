// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

type llamaHealthTransport struct {
	status int
	t      *testing.T
}

func (f *llamaHealthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Port() != "8082" || r.URL.Path != "/v1/models" {
		f.t.Errorf("health target: %s", r.URL)
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"model.gguf"}]}`)), Header: make(http.Header)}, nil
}

func TestLlamaCPPAdvertisesProxyAndLoopbackEngine(t *testing.T) {
	worker, codec := newTestRPCWorkerPipe(t)
	backends := make(chan proxyLocalBackend, 2)
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.Method == "node/set-local-backend" {
				var backend proxyLocalBackend
				_ = json.Unmarshal(msg.Params, &backend)
				backends <- backend
			}
			if msg.IsRequest() {
				_ = codec.Respond(msg.ID, nil)
			}
		}
	}()
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.setLlamaCPPProxy(&proxyProcess{peer: worker.peer, ready: true, port: 1237})
	transport := &llamaHealthTransport{status: http.StatusOK, t: t}
	b.reconcileAdvertiseLlamaCPP(&http.Client{Transport: transport})
	records := b.regCache.Snapshot()
	if len(records) != 1 || records[0].Service != noderec.ServiceLlamaCPP || records[0].Port != 1237 {
		t.Fatalf("advertisement: %+v", records)
	}
	select {
	case backend := <-backends:
		if backend.Engine != "llamacpp" || backend.Host != "127.0.0.1" || backend.Port != 8082 || !backend.Healthy {
			t.Fatalf("backend: %+v", backend)
		}
	case <-time.After(time.Second):
		t.Fatal("backend not delivered")
	}
	transport.status = http.StatusServiceUnavailable
	b.reconcileAdvertiseLlamaCPP(&http.Client{Transport: transport})
	if len(b.regCache.Snapshot()) != 0 {
		t.Fatal("unhealthy service remained advertised")
	}
	select {
	case backend := <-backends:
		if backend.Healthy {
			t.Fatal("unhealthy engine retained")
		}
	case <-time.After(time.Second):
		t.Fatal("clear not delivered")
	}
}

func TestLlamaCPPProxyPathResolution(t *testing.T) {
	if _, err := resolveLlamaCPPProxyPath("/nonexistent/pair-test/llamacpp-proxy"); err == nil {
		t.Fatal("invalid explicit path accepted")
	}
}
