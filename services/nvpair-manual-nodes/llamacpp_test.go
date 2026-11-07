// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"reflect"
	"testing"
)

func TestProbeLlamaCPPReportsExactModelIDs(t *testing.T) {
	manager, _, transport := newTestManager(t)
	transport.set(http.MethodGet, "fixture.local:8082", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"model"},{"id":"model:latest"}]}`)
	})
	up, models := manager.probeLlamaCPP("fixture.local", llamacppPort)
	if !up || !reflect.DeepEqual(models, []string{"model", "model:latest"}) {
		t.Fatalf("inventory: %v %v", up, models)
	}
	if up, _ := manager.probeLlamaCPP("absent.local", llamacppPort); up {
		t.Fatal("absent engine reported up")
	}
}
