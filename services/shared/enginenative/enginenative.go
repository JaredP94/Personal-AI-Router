// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package enginenative holds the engine-native view of a local inference
// engine: the counters the engine reports about itself, and the live rows of
// the requests it is currently working on.
//
// The engine is oMLX, and its admin API returns its own payload shape. PAIR
// does not publish that payload. It maps the fields it needs onto the types
// below, field by field, and nothing else crosses: oMLX's /admin/api/stats
// payload also carries the engine's api_key, along with host, port and
// cli_prefix, and PAIR's types have no field for any of them, so the key
// cannot leak by accident.
//
// Every numeric field is omitempty. A value the engine did not report is
// absent from the JSON rather than serialised as 0, so a consumer can tell
// "not reported" from "reported zero".
package enginenative

import (
	"encoding/json"
	"fmt"
	"sort"
)

// EngineOmlx is the name PAIR uses for the oMLX engine, the only engine that
// publishes an admin API. It is the value the parsers stamp on the views they
// build from an oMLX admin payload.
const EngineOmlx = "omlx"

// The phases a live row can be in, as reported by the engine's activity
// endpoint.
const (
	// PhaseWaiting is a request queued behind other requests.
	PhaseWaiting = "waiting"
	// PhasePrefilling is a request in the prefill phase.
	PhasePrefilling = "prefilling"
	// PhaseGenerating is a request in the generation phase.
	PhaseGenerating = "generating"
)

// Stats is the engine's own report of what it has done.
type Stats struct {
	Engine     string  `json:"engine"`
	Supported  bool    `json:"supported"`
	Totals     Totals  `json:"totals,omitempty"`
	Rates      Rates   `json:"rates,omitempty"`
	Models     []Model `json:"models,omitempty"`
	Error      string  `json:"error,omitempty"`
	ObservedAt int64   `json:"observedAt,omitempty"`
}

// Totals holds the engine's cumulative counters. The engine reports no phase
// durations on this endpoint, so none are carried here: the durations live on
// an endpoint whose totals are scoped to a date range rather than to the
// engine's life, and a day-scoped number would not belong beside these.
type Totals struct {
	Requests         int     `json:"requests,omitempty"`
	PromptTokens     int     `json:"promptTokens,omitempty"`
	CompletionTokens int     `json:"completionTokens,omitempty"`
	CachedTokens     int     `json:"cachedTokens,omitempty"`
	CacheEfficiency  float64 `json:"cacheEfficiency,omitempty"`
}

// Rates holds the engine's own average throughput numbers.
type Rates struct {
	PrefillTokensPerSecond    float64 `json:"prefillTokensPerSecond,omitempty"`
	GenerationTokensPerSecond float64 `json:"generationTokensPerSecond,omitempty"`
}

// Model is one model the engine reports as loaded.
type Model struct {
	Model  string `json:"model"`
	Loaded bool   `json:"loaded"`
}

// Activity is the engine's own report of the requests it is working on right
// now.
type Activity struct {
	Engine    string        `json:"engine"`
	Supported bool          `json:"supported"`
	Rows      []ActivityRow `json:"rows,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// ActivityRow is one in-flight request as the engine reports it. The request
// id is the engine's own, which is what the engine's activity rows are keyed
// by.
type ActivityRow struct {
	RequestID       string  `json:"requestId"`
	Phase           string  `json:"phase"`
	QueuePosition   int     `json:"queuePosition,omitempty"`
	GeneratedTokens int     `json:"generatedTokens,omitempty"`
	TokensPerSecond float64 `json:"tokensPerSecond,omitempty"`
	PrefillProgress float64 `json:"prefillProgress,omitempty"`
	ElapsedSeconds  float64 `json:"elapsedSeconds,omitempty"`
	PromptTokens    int     `json:"promptTokens,omitempty"`
}

// ParseStats maps oMLX's /admin/api/stats payload onto Stats.
//
// The payload's api_key, host, port and cli_prefix fields are dropped by
// construction: only the fields named below are read. A payload that is not a
// JSON object is an error. A field that is absent, or that holds a value of
// an unexpected shape, is reported as zero.
//
// ObservedAt is left at zero: the caller stamps it, because it owns the
// meaning of "observed" (when the fetch happened, not when the bytes were
// parsed).
func ParseStats(raw []byte) (Stats, error) {
	fields, err := decodeFields(raw)
	if err != nil {
		return Stats{}, err
	}
	s := Stats{Engine: EngineOmlx, Supported: true}
	s.Totals.Requests = intField(fields, "total_requests")
	s.Totals.PromptTokens = intField(fields, "total_prompt_tokens")
	s.Totals.CompletionTokens = intField(fields, "total_completion_tokens")
	s.Totals.CachedTokens = intField(fields, "total_cached_tokens")
	s.Totals.CacheEfficiency = numberField(fields, "cache_efficiency")
	s.Rates.PrefillTokensPerSecond = numberField(fields, "avg_prefill_tps")
	s.Rates.GenerationTokensPerSecond = numberField(fields, "avg_generation_tps")
	s.Models = parseModels(fields["active_models"])
	return s, nil
}

// ParseActivity maps oMLX's /admin/api/activity payload onto Activity.
//
// The engine's activity payload is not a documented schema, so the parser is
// deliberately forgiving: it reads the fields it knows, ignores the fields it
// does not, and never fails a whole payload because one row has an unexpected
// shape. A row that cannot be read at all is skipped and the rest of the
// payload still parses. A payload that is not a JSON object is an error.
func ParseActivity(raw []byte) (Activity, error) {
	fields, err := decodeFields(raw)
	if err != nil {
		return Activity{}, err
	}
	a := Activity{Engine: EngineOmlx, Supported: true}
	for _, model := range decodeModelEntries(fields["active_models"]) {
		a.Rows = append(a.Rows, parseRows(model, PhaseWaiting, "waiting")...)
		a.Rows = append(a.Rows, parseRows(model, PhasePrefilling, "prefilling")...)
		a.Rows = append(a.Rows, parseRows(model, PhaseGenerating, "generating")...)
	}
	return a, nil
}

// decodeFields splits a JSON object into its raw fields. A payload that is not
// a JSON object is an error.
func decodeFields(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("enginenative: the payload is not a JSON object: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("enginenative: the payload is not a JSON object")
	}
	return fields, nil
}

// numberField reads a numeric field. A field that is absent, or that does not
// hold a number, is reported as zero.
func numberField(fields map[string]json.RawMessage, key string) float64 {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0
	}
	value, err := number.Float64()
	if err != nil {
		return 0
	}
	return value
}

// intField reads a numeric field as an integer. A field that is absent, or that
// does not hold a number, is reported as zero.
func intField(fields map[string]json.RawMessage, key string) int {
	return int(numberField(fields, key))
}

// parseModels reads the engine's list of loaded models. The list holds either
// model names or objects carrying a name and a loaded flag; an entry that is
// neither is skipped.
func parseModels(raw json.RawMessage) []Model {
	entries, ok := decodeArray(raw)
	if !ok {
		return nil
	}
	var models []Model
	for _, entry := range entries {
		model, ok := parseModel(entry)
		if !ok {
			continue
		}
		models = append(models, model)
	}
	return models
}

// parseModel reads one entry of the engine's model list.
func parseModel(raw json.RawMessage) (Model, bool) {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		if name == "" {
			return Model{}, false
		}
		return Model{Model: name, Loaded: true}, true
	}
	fields, err := decodeFields(raw)
	if err != nil {
		return Model{}, false
	}
	for _, key := range []string{"model", "name", "id"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, &name); err != nil {
			continue
		}
		if name == "" {
			continue
		}
		// A model the engine lists as active is loaded, even when the entry
		// does not spell that out.
		loaded := true
		if raw, ok := fields["loaded"]; ok {
			var reported bool
			if err := json.Unmarshal(raw, &reported); err == nil {
				loaded = reported
			}
		}
		return Model{Model: name, Loaded: loaded}, true
	}
	return Model{}, false
}

// decodeModelEntries reads the engine's active model list. The list is an
// array of model objects; an object keyed by model name is accepted too, since
// the engine's payload shape is inferred rather than documented. An entry that
// is not an object is skipped.
func decodeModelEntries(raw json.RawMessage) []map[string]json.RawMessage {
	if entries, ok := decodeArray(raw); ok {
		models := make([]map[string]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			fields, err := decodeFields(entry)
			if err != nil {
				continue
			}
			models = append(models, fields)
		}
		return models
	}
	var byName map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byName); err != nil {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	models := make([]map[string]json.RawMessage, 0, len(names))
	for _, name := range names {
		models = append(models, byName[name])
	}
	return models
}

// parseRows reads the rows of one phase of one model. A row that cannot be
// read is skipped; the remaining rows of the phase are kept.
func parseRows(model map[string]json.RawMessage, phase string, key string) []ActivityRow {
	entries, ok := decodeArray(model[key])
	if !ok {
		return nil
	}
	var rows []ActivityRow
	for _, entry := range entries {
		row, ok := parseRow(entry, phase)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// parseRow reads one live row. A row that is not an object, or that carries
// none of the fields the parser knows, is skipped: a row PAIR cannot read at
// all is noise, while a row with one unexpected field still carries the rest
// of its numbers.
func parseRow(raw json.RawMessage, phase string) (ActivityRow, bool) {
	fields, err := decodeFields(raw)
	if err != nil {
		return ActivityRow{}, false
	}
	known := false
	for _, key := range []string{
		"request_id", "queue_position", "generated_tokens", "tokens_per_second",
		"progress", "prefill_progress", "elapsed_seconds", "prompt_tokens",
	} {
		if _, ok := fields[key]; ok {
			known = true
			break
		}
	}
	if !known {
		return ActivityRow{}, false
	}
	row := ActivityRow{Phase: phase}
	if value, ok := fields["request_id"]; ok {
		if err := json.Unmarshal(value, &row.RequestID); err != nil {
			row.RequestID = ""
		}
	}
	row.QueuePosition = intField(fields, "queue_position")
	row.GeneratedTokens = intField(fields, "generated_tokens")
	row.TokensPerSecond = numberField(fields, "tokens_per_second")
	row.PrefillProgress = numberField(fields, "progress")
	if row.PrefillProgress == 0 {
		row.PrefillProgress = numberField(fields, "prefill_progress")
	}
	row.ElapsedSeconds = numberField(fields, "elapsed_seconds")
	row.PromptTokens = intField(fields, "prompt_tokens")
	return row, true
}

// decodeArray reads a JSON array of raw entries. A value that is not an array
// is reported as not readable.
func decodeArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	return entries, true
}
