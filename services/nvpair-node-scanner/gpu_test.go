// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"

	"nvpair-shared/noderec"
)

func TestScannerEnrichmentCarriesGPUWireFields(t *testing.T) {
	gpus := `[{"name":"GPU","memory_kind":"dedicated","vram_bytes":8589934592,"vram_used_bytes":0,"shared_bytes":17179869184,"shared_used_bytes":0,"utilization_percent":0},{"name":"Unknown","memory_kind":"unavailable"}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"GPUs":` + gpus + `,"hostUuid":"node-a"}`))
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{http: server.Client()}
	node := noderec.DirectoryNode{HostUUID: "node-a", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: port}}}
	d.enrichInfoAt(&node, u.Hostname())
	data, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		GPUs []map[string]json.RawMessage `json:"gpus"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var want []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gpus), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.GPUs, want) {
		t.Fatalf("GPU directory transport = %s, want %s", data, gpus)
	}
}

func gpuReading[T ~uint32 | ~uint64](value T) *T {
	return &value
}

func TestTelemetryMixedMeasuredGPUReadings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gpus  string
		valid bool
		want  uint32
	}{
		{"mixed", `[{"name":"Unknown"},{"name":"Known","utilization_percent":77},{"name":"Lower","utilization_percent":12}]`, true, 77},
		{"idle", `[{"name":"Unknown"},{"name":"Idle","utilization_percent":0}]`, true, 0},
		{"unknown", `[{"name":"Unknown"}]`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(struct {
					GPUs     json.RawMessage `json:"GPUs"`
					HostUUID string          `json:"hostUuid"`
					Valid    bool            `json:"telemetryValid"`
				}{json.RawMessage(tc.gpus), "node-a", tc.valid})
			}))
			defer server.Close()
			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			d := &daemon{codec: NewCodec(&output), http: server.Client()}
			if !d.refreshNodeTelemetry(context.Background(), "node-a", u.Hostname(), port) {
				t.Fatal("no telemetry")
			}
			var msg Message
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &msg); err != nil {
				t.Fatal(err)
			}
			var got noderec.NodeTelemetry
			if err := json.Unmarshal(msg.Params, &got); err != nil {
				t.Fatal(err)
			}
			if got.GPUUtilizationPct != tc.want || got.TelemetryValid != tc.valid {
				t.Fatalf("telemetry = %+v", got)
			}
		})
	}
}
