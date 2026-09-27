// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bufpool provides an httputil.BufferPool implementation backed by sync.Pool
// to eliminate buffer reallocations during HTTP reverse proxy streaming.
package bufpool

import "sync"

const defaultBufferSize = 32 * 1024 // 32KB matches standard io.Copy / ReverseProxy buffer

// Pool implements httputil.BufferPool.
type Pool struct {
	pool sync.Pool
	size int
}

// New returns a BufferPool allocating buffers of defaultBufferSize (32KB).
func New() *Pool {
	return NewWithSize(defaultBufferSize)
}

// NewWithSize returns a BufferPool allocating buffers of the specified size.
func NewWithSize(size int) *Pool {
	if size <= 0 {
		size = defaultBufferSize
	}
	return &Pool{
		size: size,
		pool: sync.Pool{
			New: func() any {
				return make([]byte, size)
			},
		},
	}
}

// Get returns a byte slice from the pool.
func (p *Pool) Get() []byte {
	return p.pool.Get().([]byte)
}

// Put returns a byte slice to the pool. Slices with capacity smaller than
// the configured size are discarded.
func (p *Pool) Put(b []byte) {
	if cap(b) >= p.size {
		p.pool.Put(b[:p.size])
	}
}
