// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bufpool

import "testing"

func TestBufferPool(t *testing.T) {
	p := New()
	buf := p.Get()
	if len(buf) != defaultBufferSize {
		t.Fatalf("expected buffer size %d, got %d", defaultBufferSize, len(buf))
	}
	p.Put(buf)

	custom := NewWithSize(16 * 1024)
	buf2 := custom.Get()
	if len(buf2) != 16*1024 {
		t.Fatalf("expected buffer size 16384, got %d", len(buf2))
	}
	custom.Put(buf2)

	// Discard undersized slices
	p.Put(make([]byte, 10))

	// Invalid size defaults to defaultBufferSize
	invalid := NewWithSize(0)
	buf3 := invalid.Get()
	if len(buf3) != defaultBufferSize {
		t.Fatalf("expected buffer size %d, got %d", defaultBufferSize, len(buf3))
	}
	invalid.Put(buf3)
}
