// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A canceled write queued behind httpMu must not bind or persist after a newer
// operation owns the proxy. The cancellation check belongs after lock admission.
func TestCanceledPortWriteCannotApplyAfterListenerLock(t *testing.T) {
	redirectConfigDir(t)
	p := NewProxy(NewCodec(&bytes.Buffer{}), NewDiscovery(), 11435)
	p.httpMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.setPortContext(ctx, 11436) }()
	cancel()
	p.httpMu.Unlock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled write did not leave listener lock")
	}
	if p.port != 11435 {
		t.Fatalf("canceled write changed port to %d", p.port)
	}
	if port, ok := loadPersistedPort(); ok {
		t.Fatalf("canceled write persisted %d", port)
	}
}
