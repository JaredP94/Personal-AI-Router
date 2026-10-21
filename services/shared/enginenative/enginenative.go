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
// The numeric fields of Stats, Totals and Rates are plain values with
// omitempty: a value the engine did not report is absent from the JSON
// rather than serialised as 0. For a counter a reported zero and an absent
// value mean the same thing to a reader.
//
// The numeric fields of an ActivityRow are pointers instead, because they are
// per-request readings rather than counters: there the difference between
// "the engine reported zero" and "the engine reported nothing" is
// meaningful, so a reported reading is always present in the JSON, even when
// it is zero, and an unreported one is absent.
package enginenative

import (
	"encoding/json"
	"fmt"
	"math"
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
//
// The numbers it holds are counters. A numeric field with omitempty is
// absent when the engine did not report it, and for a counter a reported zero
// and an absent value are equivalent to a reader.
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
//
// Every field is a counter with omitempty: a value the engine did not report
// is absent from the JSON, and a reported zero and an absent value are
// equivalent to a reader.
type Totals struct {
	Requests         int     `json:"requests,omitempty"`
	PromptTokens     int     `json:"promptTokens,omitempty"`
	CompletionTokens int     `json:"completionTokens,omitempty"`
	CachedTokens     int     `json:"cachedTokens,omitempty"`
	CacheEfficiency  float64 `json:"cacheEfficiency,omitempty"`
}

// Rates holds the engine's own average throughput numbers.
//
// Every field is a plain value with omitempty: a value the engine did not
// report is absent from the JSON, and a reported zero and an absent value are
// equivalent to a reader.
type Rates struct {
	PrefillTokensPerSecond    float64 `json:"prefillTokensPerSecond,omitempty"`
	GenerationTokensPerSecond float64 `json:"generationTokensPerSecond,omitempty"`
}

// Model is one model the engine lists as active.
type Model struct {
	Model string `json:"model"`
	// Loaded reports that the engine lists this model as active. It comes
	// from the engine's active-models list, so it means the engine offers
	// the model, not that the model is resident in memory.
	Loaded bool `json:"loaded"`
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
//
// The numeric fields are pointers, because they are per-request readings
// rather than counters. A present field means the engine reported that
// reading, and its value is in the JSON even when it is zero; an absent
// (nil) field means the engine did not report the reading, and the key is
// not in the JSON at all. A consumer can therefore tell "the engine reported
// zero" from "the engine reported nothing".
type ActivityRow struct {
	RequestID       string   `json:"requestId"`
	Phase           string   `json:"phase"`
	QueuePosition   *int     `json:"queuePosition,omitempty"`
	GeneratedTokens *int     `json:"generatedTokens,omitempty"`
	TokensPerSecond *float64 `json:"tokensPerSecond,omitempty"`
	PrefillProgress *float64 `json:"prefillProgress,omitempty"`
	ElapsedSeconds  *float64 `json:"elapsedSeconds,omitempty"`
	PromptTokens    *int     `json:"promptTokens,omitempty"`
}

// SameNumbers reports whether two views carry the same engine numbers.
//
// The observation stamp is deliberately not part of the comparison: it moves
// on every fetch, because every fetch is a new observation, and a view whose
// numbers did not move is not a new observation worth announcing. Two absent
// views are the same; an absent view and a present one are not.
func SameNumbers(a, b *Stats) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Engine != b.Engine || a.Supported != b.Supported || a.Error != b.Error {
		return false
	}
	if a.Totals != b.Totals || a.Rates != b.Rates {
		return false
	}
	if len(a.Models) != len(b.Models) {
		return false
	}
	for i := range a.Models {
		if a.Models[i] != b.Models[i] {
			return false
		}
	}
	return true
}

// ParseStats maps oMLX's /admin/api/stats payload onto Stats.
//
// The payload's api_key, host, port and cli_prefix fields are dropped by
// construction: only the fields named below are read. A payload that is not a
// JSON object is an error. A field that is absent, that holds a value of an
// unexpected shape, or that holds a number the parser cannot read as a
// counter, is reported as zero.
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
	s.Totals.Requests = counterField(fields, "total_requests")
	s.Totals.PromptTokens = counterField(fields, "total_prompt_tokens")
	s.Totals.CompletionTokens = counterField(fields, "total_completion_tokens")
	s.Totals.CachedTokens = counterField(fields, "total_cached_tokens")
	s.Totals.CacheEfficiency = rateField(fields, "cache_efficiency")
	s.Rates.PrefillTokensPerSecond = rateField(fields, "avg_prefill_tps")
	s.Rates.GenerationTokensPerSecond = rateField(fields, "avg_generation_tps")
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
		a.Rows = append(a.Rows, parseRows(model.fields, PhaseWaiting, "waiting")...)
		a.Rows = append(a.Rows, parseRows(model.fields, PhasePrefilling, "prefilling")...)
		a.Rows = append(a.Rows, parseRows(model.fields, PhaseGenerating, "generating")...)
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
// hold a number, is reported as not read.
func numberField(fields map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	value, err := number.Float64()
	if err != nil {
		return 0, false
	}
	return value, true
}

// intField reads a numeric field as an integer. A field that is absent, or that
// does not hold a number, that holds a fraction, or that holds a number
// outside the range of an int, is reported as not read: a count is never
// truncated to fit and never wrapped around.
func intField(fields map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	if whole, err := number.Int64(); err == nil {
		if int64(int(whole)) != whole {
			return 0, false
		}
		return int(whole), true
	}
	value, err := number.Float64()
	if err != nil {
		return 0, false
	}
	if value != math.Trunc(value) {
		return 0, false
	}
	if value < math.MinInt64 || value >= math.MaxInt64 {
		return 0, false
	}
	whole := int64(value)
	if int64(int(whole)) != whole {
		return 0, false
	}
	return int(whole), true
}

// counterField reads a counter the engine reports. A counter the engine did
// not report, or reported in a shape the parser cannot read, is zero: for a
// counter a reported zero and an absent value mean the same thing.
func counterField(fields map[string]json.RawMessage, key string) int {
	value, _ := intField(fields, key)
	return value
}

// rateField reads a rate the engine reports. A rate the engine did not
// report, or reported in a shape the parser cannot read, is zero.
func rateField(fields map[string]json.RawMessage, key string) float64 {
	value, _ := numberField(fields, key)
	return value
}

// optionalInt reads a per-request reading the engine reports. A reading the
// engine did not report, or reported in a shape the parser cannot read, is
// absent: it is not the same as a reported zero.
func optionalInt(fields map[string]json.RawMessage, key string) *int {
	value, ok := intField(fields, key)
	if !ok {
		return nil
	}
	return &value
}

// optionalNumber reads a per-request reading the engine reports. A reading
// the engine did not report, or reported in a shape the parser cannot read,
// is absent: it is not the same as a reported zero.
func optionalNumber(fields map[string]json.RawMessage, key string) *float64 {
	value, ok := numberField(fields, key)
	if !ok {
		return nil
	}
	return &value
}

// parseModels reads the engine's list of active models.
//
// It reads the same shapes the activity rows are read from, because the
// engine's exact payload shape is inferred rather than documented: the list
// holds either model names or objects carrying a name and a loaded flag, and
// it is accepted as an object keyed by model name too. An entry that carries
// no readable name is skipped.
func parseModels(raw json.RawMessage) []Model {
	var models []Model
	for _, entry := range decodeModelEntries(raw) {
		if entry.name == "" {
			continue
		}
		// A model the engine lists as active is loaded, even when the entry
		// does not spell that out.
		loaded := true
		if value, ok := entry.fields["loaded"]; ok {
			var reported bool
			if err := json.Unmarshal(value, &reported); err == nil {
				loaded = reported
			}
		}
		models = append(models, Model{Model: entry.name, Loaded: loaded})
	}
	return models
}

// modelNameOf reads the name an entry carries, under any of the keys the
// engine is known to use for it. An entry that carries no readable name is
// reported as not named.
func modelNameOf(fields map[string]json.RawMessage) string {
	for _, key := range []string{"model", "name", "id"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(value, &name); err != nil {
			continue
		}
		if name != "" {
			return name
		}
	}
	return ""
}

// modelEntry is one entry of the engine's active model list: the name the
// engine gave the model, when the list carries one, and the fields of the
// entry.
type modelEntry struct {
	name   string
	fields map[string]json.RawMessage
}

// decodeModelEntries reads the engine's active model list. The list is an
// array of model objects or of model names; an object keyed by model name is
// accepted too, since the engine's payload shape is inferred rather than
// documented. An entry that cannot be read at all is skipped.
func decodeModelEntries(raw json.RawMessage) []modelEntry {
	if entries, ok := decodeArray(raw); ok {
		models := make([]modelEntry, 0, len(entries))
		for _, entry := range entries {
			if name, ok := decodeModelName(entry); ok {
				models = append(models, modelEntry{name: name})
				continue
			}
			fields, err := decodeFields(entry)
			if err != nil {
				continue
			}
			models = append(models, modelEntry{name: modelNameOf(fields), fields: fields})
		}
		return models
	}
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byName); err != nil {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	models := make([]modelEntry, 0, len(names))
	for _, name := range names {
		fields, err := decodeFields(byName[name])
		if err != nil {
			continue
		}
		models = append(models, modelEntry{name: name, fields: fields})
	}
	return models
}

// decodeModelName reads an entry that is a bare model name. An entry that is
// not a name is reported as not a name.
func decodeModelName(raw json.RawMessage) (string, bool) {
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", false
	}
	if name == "" {
		return "", false
	}
	return name, true
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
	row.QueuePosition = optionalInt(fields, "queue_position")
	row.GeneratedTokens = optionalInt(fields, "generated_tokens")
	row.TokensPerSecond = optionalNumber(fields, "tokens_per_second")
	row.PrefillProgress = prefillProgress(fields)
	row.ElapsedSeconds = optionalNumber(fields, "elapsed_seconds")
	row.PromptTokens = optionalInt(fields, "prompt_tokens")
	return row, true
}

// prefillProgress reads the progress of a prefilling request. The engine
// spells this reading "progress" in one place and "prefill_progress" in
// another, so the first is read and the second is the fallback. A reading the
// engine did not report is absent; a reported zero stays a reported zero when
// the other reading is not there either.
func prefillProgress(fields map[string]json.RawMessage) *float64 {
	value, ok := numberField(fields, "progress")
	if ok && value != 0 {
		return &value
	}
	if alternate, alternateOK := numberField(fields, "prefill_progress"); alternateOK {
		return &alternate
	}
	if ok {
		return &value
	}
	return nil
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
