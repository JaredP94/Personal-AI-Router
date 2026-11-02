// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/noderec"
)

// The local node's discovery entry follows its own node-info on the refresh
// tick, as every peer's does, and an unchanged entry emits nothing.
func TestRefreshSelfFollowsTheLocalInventory(t *testing.T) {
	var used atomic.Uint64
	used.Store(126877696)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v := used.Load()
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{GPUs: []GPUInfo{{Name: "GPU", VRAMUsedBytes: &v}}})
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	var out bytes.Buffer
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	d.codec = NewCodec(&out)
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: port})
	d.publishSelf()
	updates := func() int { return strings.Count(out.String(), noderec.NotifyNodeUpdated) }
	before := updates()

	d.refreshSelfOnce()
	if got := updates(); got != before {
		t.Fatalf("an unchanged self entry emitted %d update(s)", got-before)
	}

	used.Store(9782165504)
	d.refreshSelfOnce()
	n, _ := d.dir.get("self-uuid")
	if len(n.GPUs) != 1 || n.GPUs[0].VRAMUsedBytes == nil || *n.GPUs[0].VRAMUsedBytes != 9782165504 {
		t.Fatalf("self GPUs = %+v, want the refreshed VRAM reading", n.GPUs)
	}
	if got := updates(); got != before+1 {
		t.Fatalf("a changed self entry emitted %d update(s), want 1", got-before)
	}
}
