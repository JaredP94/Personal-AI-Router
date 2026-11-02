// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A peer's snapshot is parsed only up to maxSyncBodyBytes; one that extends past
// it is refused rather than buffered.
func TestIngestRejectsAnOversizedSnapshot(t *testing.T) {
	m, _ := newTestManager(t)
	for _, tc := range []struct {
		name    string
		message string
		want    int
	}{
		{"within the limit", "disk full", http.StatusNoContent},
		{"past the limit", strings.Repeat("x", maxSyncBodyBytes), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"nodeId":"peer-a","errors":[{"id":"e1","message":"` + tc.message + `","timestamp":1}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/errors", strings.NewReader(body))
			rec := httptest.NewRecorder()
			handleIngest(rec, req, m)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
