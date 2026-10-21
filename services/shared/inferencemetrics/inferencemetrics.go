// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package inferencemetrics defines the per-request inference measurement that
// PAIR reports for a job and for the node that served it.
//
// Every field is a pointer so that "the engine did not report this" (nil) stays
// distinguishable from "the engine reported zero" (a pointer to 0). A field that
// was never reported is omitted from the JSON payload rather than serialised
// as 0, so a consumer can tell an absent measurement from a measured zero
// without consulting a separate flag.
package inferencemetrics

// Measurement carries the per-request prefill/decode measurements in
// workload-spec wire form. It is embedded by every struct that carries a
// measurement, so the JSON field names are a contract shared across services.
// Embed it anonymously: a named field would nest the measurement instead of
// inlining its fields into the enclosing payload.
type Measurement struct {
	PrefillTokens *int   `json:"prefillTokens,omitempty"`
	DecodeTokens  *int   `json:"decodeTokens,omitempty"`
	CachedTokens  *int   `json:"cachedTokens,omitempty"`
	PrefillMs     *int64 `json:"prefillMs,omitempty"`
	DecodeMs      *int64 `json:"decodeMs,omitempty"`
	TtftMs        *int64 `json:"ttftMs,omitempty"`
	TotalMs       *int64 `json:"totalMs,omitempty"`
}

// IntPtr returns a pointer to v. Go has no address-of-literal, so callers that
// need to record a measured value in a Measurement field use this helper.
func IntPtr(v int) *int {
	return &v
}

// Int64Ptr returns a pointer to v. Go has no address-of-literal, so callers
// that need to record a measured value in a Measurement field use this helper.
func Int64Ptr(v int64) *int64 {
	return &v
}
