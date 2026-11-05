// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

// The target withholds an Apply response until its broker finishes, which may
// take the whole settings budget. The requester's header wait must outlast the
// target's relay so it receives that definite answer instead of an error while
// the accepted change carries on. Budgets are scaled down here: the readiness
// pool stands in for the old 11-minute wait, the settings pool for RelayBudget.
func TestRemoteSettingsUseRelayHeaderBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	base := http.DefaultTransport.(*http.Transport)
	c := &remoteClient{
		http:         newRemoteHTTPClient(base, 20*time.Millisecond),
		readyHTTP:    newRemoteHTTPClient(base, 50*time.Millisecond),
		settingsHTTP: newRemoteHTTPClient(base, time.Second),
		base:         srv.URL,
	}
	for _, method := range []string{"get", "preview", "apply"} {
		if _, err := c.postJSON(context.Background(), settingsPath+method, "ollama", settings.Request{Engine: "ollama"}); err != nil {
			t.Fatalf("remote settings %s was cut off before the relay budget: %v", method, err)
		}
	}
}

// Every settings layer must wait longer than the one it calls: the target's
// broker answers within ApplyBudget and its relay within RelayBudget, so the
// requester's header wait may not be shorter than RelayBudget.
func TestRemoteSettingsHeaderBudgetCoversRelay(t *testing.T) {
	if remoteSettingsResponseHeaderTimeout < settings.RelayBudget {
		t.Fatalf("remote settings header budget %v < relay budget %v", remoteSettingsResponseHeaderTimeout, settings.RelayBudget)
	}
	if settings.RelayBudget <= settings.ApplyBudget {
		t.Fatalf("relay budget %v must exceed apply budget %v", settings.RelayBudget, settings.ApplyBudget)
	}
}
