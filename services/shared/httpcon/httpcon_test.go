// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpcon

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Closing a body unread keeps its connection out of the pool, so a poller
// opens one connection per poll; draining it first lets one connection serve
// every poll.
func TestDrainAndCloseReusesTheConnection(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		var conns atomic.Int32
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if chunked {
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, strings.Repeat("x", 4096))
		}))
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns.Add(1)
			}
		}
		srv.Start()
		client := srv.Client()
		for i := 0; i < 16; i++ {
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			DrainAndClose(resp.Body)
		}
		srv.Close()
		if got := conns.Load(); got != 1 {
			t.Errorf("chunked=%v: %d connections for 16 polls, want 1", chunked, got)
		}
	}
}
