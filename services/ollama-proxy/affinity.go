// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
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

const maxCompletionLineBytes = 64 << 10

type completionFormat uint8

const (
	completionOnEOF completionFormat = iota
	completionSSE
	completionOllamaJSON
)

// A 200 status alone is insufficient: the engine body must reach a clean EOF.
// Streaming OpenAI-compatible responses must carry data: [DONE], while native
// Ollama streams must carry their final done:true JSON object. This also handles
// ReverseProxy returning after a failed copy in direct calls.
type affinityBodyReader struct {
	io.ReadCloser
	complete bool
	format   completionFormat
	done     bool
	invalid  bool
	line     []byte
}

func newAffinityBodyReader(body io.ReadCloser, contentType, path string) *affinityBodyReader {
	contentType = strings.ToLower(contentType)
	format := completionOnEOF
	if strings.HasPrefix(contentType, "text/event-stream") {
		format = completionSSE
	} else if (path == "/api/chat" || path == "/api/generate") &&
		(strings.HasPrefix(contentType, "application/x-ndjson") || strings.HasPrefix(contentType, "application/json")) {
		format = completionOllamaJSON
	}
	return &affinityBodyReader{ReadCloser: body, format: format}
}

func (r *affinityBodyReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if n > 0 && r.format != completionOnEOF {
		r.observe(b[:n])
	}
	if err == io.EOF {
		if r.format != completionOnEOF {
			r.inspectLine(r.line)
		}
		r.complete = r.format == completionOnEOF || (r.done && !r.invalid)
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
	switch r.format {
	case completionSSE:
		r.done = bytes.Equal(line, []byte("data: [DONE]"))
	case completionOllamaJSON:
		var event struct {
			Done bool `json:"done"`
		}
		if json.Unmarshal(line, &event) == nil && event.Done {
			r.done = true
		}
	}
}
