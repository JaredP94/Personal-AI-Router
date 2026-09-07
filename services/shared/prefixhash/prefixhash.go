// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package prefixhash supplies passive, in-memory prompt affinity hints. A digest
// records request identity; it does not establish that an engine still has a KV cache.
package prefixhash

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Digest is a SHA-256 digest of canonical prompt data.
type Digest [32]byte

// Context contains only model metadata and digests, never message text.
type Context struct {
	Model    string
	Prefixes []Digest
	Full     Digest
}

// Extract reads supported inference request formats. Invalid or unsupported
// requests return an empty context and continue through ordinary routing.
// Chat prefixes include every message boundary before the latest message,
// longest first, so an earlier request still matches after an assistant reply.
func Extract(method, path string, body []byte) Context {
	path, _, _ = strings.Cut(path, "?")
	if method != http.MethodPost {
		return Context{}
	}
	switch path {
	case "/v1/chat/completions", "/api/chat", "/v1/completions", "/api/generate":
	default:
		return Context{}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return Context{}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return Context{}
	}
	options := make(map[string]json.RawMessage)
	for key, raw := range fields {
		switch key {
		case "model", "messages", "prompt", "context", "stream", "stream_options",
			"max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k",
			"min_p", "seed", "stop", "n", "frequency_penalty", "presence_penalty",
			"repetition_penalty", "logprobs", "top_logprobs", "logit_bias", "user",
			"metadata", "store", "keep_alive":
			continue
		}
		// Retain all other fields, including tools, templates, images and
		// engine-specific options. Unknown settings conservatively break affinity.
		canonical, ok := canonicalJSON(raw)
		if !ok {
			return Context{}
		}
		options[key] = canonical
	}
	envelope := promptEnvelope{Format: path, Options: options}
	result := Context{Model: model}
	if path == "/v1/chat/completions" || path == "/api/chat" {
		var messages []json.RawMessage
		if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
			return Context{}
		}
		for i, raw := range messages {
			var message struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(raw, &message) != nil || message.Role == "" {
				return Context{}
			}
			canonical, ok := canonicalJSON(raw)
			if !ok {
				return Context{}
			}
			messages[i] = canonical
		}
		// Hash each canonical message exactly once. Length framing prevents
		// ambiguous concatenation, and Sum snapshots the current message boundary
		// without replaying the preceding history.
		hash := sha256.New()
		optionsDigest := hashEnvelope(envelope)
		hash.Write(optionsDigest[:])
		var size [8]byte
		for i, message := range messages {
			binary.BigEndian.PutUint64(size[:], uint64(len(message)))
			hash.Write(size[:])
			hash.Write(message)
			var digest Digest
			hash.Sum(digest[:0])
			if i == len(messages)-1 {
				result.Full = digest
			} else {
				result.Prefixes = append(result.Prefixes, digest)
			}
		}
		for left, right := 0, len(result.Prefixes)-1; left < right; left, right = left+1, right-1 {
			result.Prefixes[left], result.Prefixes[right] = result.Prefixes[right], result.Prefixes[left]
		}
		return result
	}
	if path == "/api/generate" && len(fields["context"]) > 0 {
		var tokens []int64
		if json.Unmarshal(fields["context"], &tokens) != nil {
			return Context{}
		}
		for _, token := range tokens {
			if token < 0 {
				return Context{}
			}
		}
		if len(tokens) > 0 {
			// The response's context tokens are unavailable here. Record only
			// the supplied context bucket; do not predict an extended context.
			envelope.Tokens = tokens
			result.Full = hashEnvelope(envelope)
			result.Prefixes = []Digest{result.Full}
			return result
		}
	}
	var prompt string
	if json.Unmarshal(fields["prompt"], &prompt) != nil || len(prompt) < 1024 {
		return Context{}
	}
	// Bytes preserve an exact 1024-byte prefix even at a UTF-8 boundary.
	envelope.Prompt = []byte(prompt[:1024])
	result.Full = hashEnvelope(envelope)
	result.Prefixes = []Digest{result.Full}
	return result
}

type promptEnvelope struct {
	Format  string                     `json:"format"`
	Options map[string]json.RawMessage `json:"options"`
	Prompt  []byte                     `json:"prompt,omitempty"`
	Tokens  []int64                    `json:"tokens,omitempty"`
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	if decoder.Decode(new(interface{})) != io.EOF {
		return nil, false
	}
	canonical, err := json.Marshal(value)
	return canonical, err == nil
}

func hashEnvelope(envelope promptEnvelope) Digest {
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return Digest{}
	}
	return sha256.Sum256(canonical)
}
