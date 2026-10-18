// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"net/http"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

const MaxPayloadBytes = 16 << 10

// TruncateUTF8 limits a UTF-8 string by bytes without splitting its final rune.
func TruncateUTF8(s string, max int) (string, bool) {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s, false
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max], true
}

// RequestAttributes deliberately excludes all but these two request headers.
// A denylist cannot safely account for custom credential-bearing headers.
func RequestAttributes(h http.Header) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	for _, name := range []string{"Content-Type", "Content-Length"} {
		if value := h.Get(name); value != "" {
			value, _ = TruncateUTF8(value, 256)
			key := "http.request.header.content-type"
			if name == "Content-Length" {
				key = "http.request.header.content-length"
			}
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	return attrs
}
