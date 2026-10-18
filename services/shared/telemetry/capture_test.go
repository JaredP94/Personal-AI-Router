// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

func TestRequestAttributesAllowlist(t *testing.T) {
	h := http.Header{
		"Authorization":  {"Bearer test-only"},
		"Cookie":         {"session=test-only"},
		"X-Secret":       {"test-only"},
		"Content-Type":   {"application/json"},
		"Content-Length": {"12"},
	}
	want := []attribute.KeyValue{
		attribute.String("http.request.header.content-type", "application/json"),
		attribute.String("http.request.header.content-length", "12"),
	}
	if got := RequestAttributes(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %v, want %v", got, want)
	}
	if got := RequestAttributes(nil); len(got) != 0 {
		t.Fatalf("nil headers = %v", got)
	}
	h.Set("Content-Type", strings.Repeat("a", 255)+"é")
	got := RequestAttributes(h)
	if got[0].Value.AsString() != strings.Repeat("a", 255) || !utf8.ValidString(got[0].Value.AsString()) {
		t.Fatal("header was not UTF-8 truncated to 256 bytes")
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, tt := range []struct {
		input     string
		max       int
		want      string
		truncated bool
	}{
		{"héllo", 2, "h", true},
		{"héllo", 6, "héllo", false},
		{"héllo", 3, "hé", true},
		{"héllo", 0, "", true},
		{"héllo", -1, "", true},
		{"", 0, "", false},
		{"😀a", 3, "", true},
		{"😀a", 4, "😀", true},
		{strings.Repeat("a", 16<<10) + "é", MaxPayloadBytes, strings.Repeat("a", 16<<10), true},
	} {
		got, truncated := TruncateUTF8(tt.input, tt.max)
		if got != tt.want || truncated != tt.truncated || !utf8.ValidString(got) {
			t.Errorf("TruncateUTF8(%q, %d) = %q, %v; want %q, %v", tt.input, tt.max, got, truncated, tt.want, tt.truncated)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	for _, tt := range []struct {
		s     Settings
		valid bool
	}{
		{Settings{CollectorPort: 0}, false},
		{Settings{CollectorPort: 70000}, false},
		{Settings{CollectorPort: -1}, false},
		{Settings{CollectorPort: 4317, RecordPayloads: true}, false},
		{Settings{CollectorPort: 1}, true},
		{Settings{CollectorPort: 65535, Enabled: true, RecordPayloads: true}, true},
	} {
		if err := ValidateSettings(tt.s); (err == nil) != tt.valid {
			t.Errorf("ValidateSettings(%+v) = %v, valid = %v", tt.s, err, tt.valid)
		}
	}
}
