// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"strings"

	"nvpair-shared/prefixhash"
)

// affinityNode matches the longest previously served request prefix. Only
// digests survive the request; generated responses are never retained.
func (p *Proxy) affinityNode(context prefixhash.Context) string {
	for _, digest := range context.Prefixes {
		if record, ok := p.prt.Lookup(context.Model, digest); ok && record.Engine == workloadEngine {
			return record.NodeID
		}
	}
	return ""
}

const (
	sseDoneMarker          = "data: [DONE]"
	maxCompletionLineBytes = 64 << 10
)

// A 200 status alone is insufficient: the engine body must reach a clean EOF.
// Streaming OpenAI-compatible responses must also carry their terminal SSE
// marker. This also handles ReverseProxy returning after a failed copy in
// direct calls.
type affinityBodyReader struct {
	io.ReadCloser
	complete       bool
	requireSSEDone bool
	done           bool
	invalid        bool
	line           []byte
}

func newAffinityBodyReader(body io.ReadCloser, contentType string) *affinityBodyReader {
	return &affinityBodyReader{
		ReadCloser:     body,
		requireSSEDone: strings.HasPrefix(strings.ToLower(contentType), "text/event-stream"),
	}
}

func (r *affinityBodyReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if n > 0 && r.requireSSEDone {
		r.observe(b[:n])
	}
	if err == io.EOF {
		if r.requireSSEDone {
			r.inspectLine(r.line)
		}
		r.complete = !r.requireSSEDone || (r.done && !r.invalid)
	}
	return n, err
}

func (r *affinityBodyReader) observe(chunk []byte) {
	r.line = append(r.line, chunk...)
	for {
		end := bytes.IndexByte(r.line, '\n')
		if end < 0 {
			if len(r.line) > maxCompletionLineBytes {
				if r.done {
					r.invalid = true
				}
				r.line = nil
			}
			return
		}
		r.inspectLine(r.line[:end])
		r.line = r.line[end+1:]
	}
}

func (r *affinityBodyReader) inspectLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if r.done {
		r.invalid = true
		return
	}
	if bytes.Equal(line, []byte(sseDoneMarker)) {
		r.done = true
	}
}
