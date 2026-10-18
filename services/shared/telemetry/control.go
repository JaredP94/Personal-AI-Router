// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"encoding/json"
	"errors"
	"time"
)

// ConfigureRequest is the broker-authorized tracing policy on the wire.
// All four fields are required, including explicit false values.
type ConfigureRequest struct {
	Revision       uint64 `json:"revision"`
	Enabled        *bool  `json:"enabled"`
	CollectorPort  *int   `json:"collectorPort"`
	RecordPayloads *bool  `json:"recordPayloads"`
}

// DecodeConfigureRequest validates a complete policy before it reaches a Manager.
func DecodeConfigureRequest(raw json.RawMessage) (uint64, Settings, error) {
	// Shadow the value field while decoding to distinguish a valid zero revision
	// from missing/null, just as the other required fields do with pointers.
	var params struct {
		ConfigureRequest
		Revision *uint64 `json:"revision"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || params.Revision == nil || params.Enabled == nil || params.CollectorPort == nil || params.RecordPayloads == nil {
		return 0, Settings{}, errors.New("expected revision, enabled, collectorPort and recordPayloads")
	}
	settings := Settings{Enabled: *params.Enabled, CollectorPort: *params.CollectorPort, RecordPayloads: *params.RecordPayloads}
	if err := ValidateSettings(settings); err != nil {
		return 0, Settings{}, err
	}
	return *params.Revision, settings, nil
}

// ConfigureResult reports the effective revision, including for stale requests.
type ConfigureResult struct {
	Revision uint64 `json:"revision"`
}

// StatusResult is the tracing delivery-health response. Missing health values
// are encoded as null rather than omitted or represented by empty strings.
type StatusResult struct {
	Revision          uint64  `json:"revision"`
	Enabled           bool    `json:"enabled"`
	LastExportOK      *string `json:"lastExportOk"`
	LastExportError   *string `json:"lastExportError"`
	LastExportErrorAt *string `json:"lastExportErrorAt"`
}

func NewStatusResult(s Status) StatusResult {
	var lastError *string
	if s.LastExportError != "" {
		lastError = &s.LastExportError
	}
	return StatusResult{
		Revision: s.Revision, Enabled: s.Enabled,
		LastExportOK: controlTimestamp(s.LastExportOK), LastExportError: lastError,
		LastExportErrorAt: controlTimestamp(s.LastExportErrorAt),
	}
}

func controlTimestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	value := t.UTC().Format(time.RFC3339)
	return &value
}
