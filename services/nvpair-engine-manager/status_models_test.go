// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"

	"testing"

	"github.com/stretchr/testify/require"
)

// Canceling inventory should release its caller promptly. A later status read
// must still be able to identify the listener after that cancellation.
func TestModelsResult_CanceledCallerReturnsBeforeProbeCompletes(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	ex := fixture.executor
	enableModelInventory(t, ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Receive cancellation while the fixture still withholds the engine reply.
	finished := make(chan error, 1)
	go func() {
		_, err := ex.modelsResult(ctx)
		finished <- err
	}()
	awaitProbeStart(t, fixture.probeStarted)
	cancel()
	require.ErrorIs(t, awaitResult(t, finished), context.Canceled)
	// The last caller leaving must stop the HTTP request before its reply is
	// released, rather than merely letting the caller stop waiting.
	awaitResult(t, fixture.probeCanceled)
	fixture.releaseProbe()
	status, err := ex.Status("fake")
	require.NoError(t, err)
	require.True(t, status.Running)
}

// enableModelInventory adds the result specification needed for modelsResult
// to include this engine in an inventory sweep.
func enableModelInventory(t *testing.T, ex *Executor) {
	t.Helper()
	m, ok := ex.reg.Get("fake")
	require.True(t, ok)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: http.MethodGet, Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
}
