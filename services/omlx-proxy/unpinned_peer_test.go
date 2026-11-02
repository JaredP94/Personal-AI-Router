// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	stderrors "errors"
	"net/http"
	"testing"
)

// A peer whose pin is gone is refused at dial time, never reached over a
// transport that does not pin its certificate.
func TestPeerTransportFailsClosedWithoutAPin(t *testing.T) {
	p := &Proxy{}
	req, err := http.NewRequest(http.MethodGet, "https://192.0.2.1:11434/api/tags", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.peerHTTPTransport("peer-without-pin").RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !stderrors.Is(err, errPeerUnpinned) {
		t.Fatalf("err = %v, want errPeerUnpinned", err)
	}
}
