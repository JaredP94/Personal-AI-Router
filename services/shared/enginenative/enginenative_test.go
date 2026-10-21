// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginenative

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseStatsDropsTheAPIKey(t *testing.T) {
	raw := []byte(`{"total_prompt_tokens":120,"total_completion_tokens":34,"total_cached_tokens":90,` +
		`"total_requests":2,"avg_prefill_tps":100,"avg_generation_tps":50,"cache_efficiency":75,` +
		`"api_key":"sk-super-secret","host":"127.0.0.1","port":8000,"cli_prefix":"omlx"}`)
	s, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-super-secret") {
		t.Fatalf("the api key leaked: %s", b)
	}
	if strings.Contains(string(b), "api_key") {
		t.Fatalf("the api key field leaked: %s", b)
	}
	if s.Totals.PromptTokens != 120 {
		t.Fatalf("totals: %+v", s.Totals)
	}
	if s.Rates.GenerationTokensPerSecond != 50 {
		t.Fatalf("rates: %+v", s.Rates)
	}
}

func TestParseStatsDropsDurationKeysTheEngineDoesNotReport(t *testing.T) {
	raw := []byte(`{"total_requests":2,"total_prompt_tokens":120,"total_completion_tokens":34,` +
		`"total_cached_tokens":90,"cache_efficiency":75,` +
		`"total_prefill_seconds":1.25,"total_generation_seconds":4.5,` +
		`"prefill_seconds":1.25,"generation_seconds":4.5,"uptime_seconds":900}`)
	s, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "econds") {
		t.Fatalf("a duration reached the view: %s", b)
	}
	var view struct {
		Totals map[string]json.RawMessage `json:"totals"`
	}
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	for key := range view.Totals {
		if strings.Contains(key, "econds") {
			t.Fatalf("a duration reached the totals: %s", b)
		}
	}
	if s.Totals.Requests != 2 {
		t.Fatalf("totals: %+v", s.Totals)
	}
	if s.Totals.CacheEfficiency != 75 {
		t.Fatalf("totals: %+v", s.Totals)
	}
}

func TestParseStatsReportsAbsentValuesAsZero(t *testing.T) {
	s, err := ParseStats([]byte(`{"total_requests":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.PromptTokens != 0 {
		t.Fatalf("totals: %+v", s.Totals)
	}
	if s.Rates.PrefillTokensPerSecond != 0 {
		t.Fatalf("rates: %+v", s.Rates)
	}
}

func TestParseStatsRejectsGarbage(t *testing.T) {
	if _, err := ParseStats([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseStatsReportsTheEngineAndSupport(t *testing.T) {
	s, err := ParseStats([]byte(`{"total_requests":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Engine != EngineOmlx {
		t.Fatalf("engine: %q", s.Engine)
	}
	if !s.Supported {
		t.Fatal("expected a supported view")
	}
}

func TestParseStatsMapsLoadedModels(t *testing.T) {
	raw := []byte(`{"active_models":["mlx-community/Llama-3.2-3B-Instruct-4bit",` +
		`{"model":"mlx-community/Qwen3-0.6B-4bit","loaded":true}]}`)
	s, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Models) != 2 {
		t.Fatalf("models: %+v", s.Models)
	}
	if s.Models[0].Model != "mlx-community/Llama-3.2-3B-Instruct-4bit" || !s.Models[0].Loaded {
		t.Fatalf("models: %+v", s.Models)
	}
	if s.Models[1].Model != "mlx-community/Qwen3-0.6B-4bit" || !s.Models[1].Loaded {
		t.Fatalf("models: %+v", s.Models)
	}
}

func TestParseStatsIgnoresAModelItCannotRead(t *testing.T) {
	raw := []byte(`{"active_models":["good-model",42,{"nope":1}]}`)
	s, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Models) != 1 || s.Models[0].Model != "good-model" {
		t.Fatalf("models: %+v", s.Models)
	}
}

// TestParseStatsReadsTheObjectKeyedModelList: the engine's payload shape is
// inferred rather than documented, so the model list is read as an object
// keyed by model name too, exactly as the activity rows are read.
func TestParseStatsReadsTheObjectKeyedModelList(t *testing.T) {
	raw := []byte(`{"active_models":{"mlx-community/Llama-3.2-3B-Instruct-4bit":{"loaded":true},` +
		`"mlx-community/Qwen3-0.6B-4bit":{"loaded":false}}}`)
	s, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Models) != 2 {
		t.Fatalf("models: %+v", s.Models)
	}
	byName := map[string]Model{}
	for _, model := range s.Models {
		byName[model.Model] = model
	}
	llama, ok := byName["mlx-community/Llama-3.2-3B-Instruct-4bit"]
	if !ok {
		t.Fatalf("models: %+v", s.Models)
	}
	if !llama.Loaded {
		t.Fatalf("models: %+v", s.Models)
	}
	qwen, ok := byName["mlx-community/Qwen3-0.6B-4bit"]
	if !ok {
		t.Fatalf("models: %+v", s.Models)
	}
	if qwen.Loaded {
		t.Fatalf("models: %+v", s.Models)
	}
}

// TestParseActivityReadsTheObjectKeyedModelList: the activity rows are read
// from the same shapes the model list is read from, so an object keyed by
// model name yields the same rows.
func TestParseActivityReadsTheObjectKeyedModelList(t *testing.T) {
	raw := []byte(`{"active_models":{"mlx-community/Llama-3.2-3B-Instruct-4bit":` +
		`{"generating":[{"request_id":"req-gen","generated_tokens":34}]}}}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].RequestID != "req-gen" {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].GeneratedTokens == nil || *a.Rows[0].GeneratedTokens != 34 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].Phase != PhaseGenerating {
		t.Fatalf("rows: %+v", a.Rows)
	}
}

func TestParseActivityMapsRows(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"mlx-community/Llama-3.2-3B-Instruct-4bit",` +
		`"waiting":[{"request_id":"req-wait","queue_position":2,"elapsed_seconds":1.5,"prompt_tokens":128}],` +
		`"prefilling":[{"request_id":"req-prefill","progress":0.42}],` +
		`"generating":[{"request_id":"req-gen","generated_tokens":34,"tokens_per_second":62.5,"elapsed_seconds":3.25}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Engine != EngineOmlx {
		t.Fatalf("engine: %q", a.Engine)
	}
	if !a.Supported {
		t.Fatal("expected a supported view")
	}
	if len(a.Rows) != 3 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	byPhase := map[string]ActivityRow{}
	for _, r := range a.Rows {
		byPhase[r.Phase] = r
	}
	waiting := byPhase[PhaseWaiting]
	if waiting.RequestID != "req-wait" {
		t.Fatalf("waiting: %+v", waiting)
	}
	if waiting.QueuePosition == nil || *waiting.QueuePosition != 2 {
		t.Fatalf("waiting: %+v", waiting)
	}
	if waiting.ElapsedSeconds == nil || *waiting.ElapsedSeconds != 1.5 {
		t.Fatalf("waiting: %+v", waiting)
	}
	if waiting.PromptTokens == nil || *waiting.PromptTokens != 128 {
		t.Fatalf("waiting: %+v", waiting)
	}
	prefilling := byPhase[PhasePrefilling]
	if prefilling.RequestID != "req-prefill" {
		t.Fatalf("prefilling: %+v", prefilling)
	}
	if prefilling.PrefillProgress == nil || *prefilling.PrefillProgress != 0.42 {
		t.Fatalf("prefilling: %+v", prefilling)
	}
	generating := byPhase[PhaseGenerating]
	if generating.RequestID != "req-gen" {
		t.Fatalf("generating: %+v", generating)
	}
	if generating.GeneratedTokens == nil || *generating.GeneratedTokens != 34 {
		t.Fatalf("generating: %+v", generating)
	}
	if generating.TokensPerSecond == nil || *generating.TokensPerSecond != 62.5 {
		t.Fatalf("generating: %+v", generating)
	}
	if generating.ElapsedSeconds == nil || *generating.ElapsedSeconds != 3.25 {
		t.Fatalf("generating: %+v", generating)
	}
}

func TestParseActivitySkipsARowItCannotRead(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":["not-an-object",` +
		`{"request_id":"req-gen","generated_tokens":7}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].RequestID != "req-gen" {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].GeneratedTokens == nil || *a.Rows[0].GeneratedTokens != 7 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].Phase != PhaseGenerating {
		t.Fatalf("rows: %+v", a.Rows)
	}
}

func TestParseActivityKeepsARowWithOneUnexpectedField(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":` +
		`[{"request_id":"req-gen","generated_tokens":"many","tokens_per_second":62.5}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].GeneratedTokens != nil {
		t.Fatalf("an unreadable reading was reported: %+v", a.Rows)
	}
	if a.Rows[0].TokensPerSecond == nil || *a.Rows[0].TokensPerSecond != 62.5 {
		t.Fatalf("rows: %+v", a.Rows)
	}
}

func TestParseActivityTellsAReportedZeroFromAnUnreportedReading(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":` +
		`[{"request_id":"req-zero","generated_tokens":0},` +
		`{"request_id":"req-absent"}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 2 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	byID := map[string]ActivityRow{}
	for _, row := range a.Rows {
		byID[row.RequestID] = row
	}
	zero := byID["req-zero"]
	if zero.GeneratedTokens == nil {
		t.Fatalf("a reported zero was reported as absent: %+v", zero)
	}
	if *zero.GeneratedTokens != 0 {
		t.Fatalf("a reported zero was not zero: %+v", zero)
	}
	absent := byID["req-absent"]
	if absent.GeneratedTokens != nil {
		t.Fatalf("an unreported reading was reported: %+v", absent)
	}
}

func TestParseActivityKeepsAReportedZeroInTheJSON(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":` +
		`[{"request_id":"req-zero","generated_tokens":0,"prefill_progress":0}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	b, err := json.Marshal(a.Rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	value, ok := view["generatedTokens"]
	if !ok {
		t.Fatalf("a reported zero is missing from the JSON: %s", b)
	}
	if string(value) != "0" {
		t.Fatalf("a reported zero is not zero in the JSON: %s", b)
	}
	if _, ok := view["prefillProgress"]; !ok {
		t.Fatalf("a reported zero is missing from the JSON: %s", b)
	}
}

func TestParseActivityRejectsAFractionalTokenCount(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":` +
		`[{"request_id":"req-gen","generated_tokens":2.9}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].GeneratedTokens != nil {
		t.Fatalf("a fractional count was reported: %+v", a.Rows)
	}
}

func TestParseActivityRejectsAnOutOfRangeTokenCount(t *testing.T) {
	raw := []byte(`{"active_models":[{"model":"m","generating":` +
		`[{"request_id":"req-gen","generated_tokens":1e30}]}]}`)
	a, err := ParseActivity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows: %+v", a.Rows)
	}
	if a.Rows[0].GeneratedTokens != nil {
		t.Fatalf("an out of range count was reported: %+v", a.Rows)
	}
}

func TestIntFieldRejectsAValueItCannotHold(t *testing.T) {
	fields := map[string]json.RawMessage{
		"fractional": json.RawMessage(`2.9`),
		"huge":       json.RawMessage(`1e30`),
		"negative":   json.RawMessage(`-1e30`),
		"whole":      json.RawMessage(`7`),
	}
	if _, ok := intField(fields, "fractional"); ok {
		t.Fatal("expected a fractional value to be rejected")
	}
	if _, ok := intField(fields, "huge"); ok {
		t.Fatal("expected an out of range value to be rejected")
	}
	if _, ok := intField(fields, "negative"); ok {
		t.Fatal("expected an out of range value to be rejected")
	}
	if _, ok := intField(fields, "missing"); ok {
		t.Fatal("expected an absent value to be rejected")
	}
	value, ok := intField(fields, "whole")
	if !ok || value != 7 {
		t.Fatalf("whole: %d %t", value, ok)
	}
}

func TestParseStatsDoesNotTruncateACounter(t *testing.T) {
	s, err := ParseStats([]byte(`{"total_requests":2.9,"total_prompt_tokens":1e30}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.Requests != 0 {
		t.Fatalf("a fractional counter was truncated: %+v", s.Totals)
	}
	if s.Totals.PromptTokens != 0 {
		t.Fatalf("an out of range counter was reported: %+v", s.Totals)
	}
}

func TestParseActivityRejectsGarbage(t *testing.T) {
	if _, err := ParseActivity([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseActivityAcceptsAnEmptyPayload(t *testing.T) {
	a, err := ParseActivity([]byte(`{"active_models":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 0 {
		t.Fatalf("rows: %+v", a.Rows)
	}
}

// TestSameNumbersIgnoresModelOrder pins the model list comparison: the engine
// lists its active models in no particular order, so the same models in a
// different order are the same models, and only a real difference counts.
func TestSameNumbersIgnoresModelOrder(t *testing.T) {
	a := &Stats{
		Engine:    EngineOmlx,
		Supported: true,
		Models: []Model{
			{Model: "mlx-community/Llama-3.2-3B-Instruct-4bit", Loaded: true},
			{Model: "mlx-community/Qwen3-4B-4bit", Loaded: false},
		},
	}
	b := &Stats{
		Engine:    EngineOmlx,
		Supported: true,
		Models: []Model{
			{Model: "mlx-community/Qwen3-4B-4bit", Loaded: false},
			{Model: "mlx-community/Llama-3.2-3B-Instruct-4bit", Loaded: true},
		},
	}
	if !SameNumbers(a, b) {
		t.Error("the same models in a different order read as a change")
	}

	// A different loaded flag is a real difference.
	c := *b
	c.Models = []Model{
		{Model: "mlx-community/Qwen3-4B-4bit", Loaded: true},
		{Model: "mlx-community/Llama-3.2-3B-Instruct-4bit", Loaded: true},
	}
	if SameNumbers(a, &c) {
		t.Error("a model that changed its loaded flag read as the same")
	}

	// A different model is a real difference.
	d := *b
	d.Models = []Model{{Model: "mlx-community/Qwen3-4B-4bit", Loaded: false}}
	if SameNumbers(a, &d) {
		t.Error("a different model list read as the same")
	}
}

// TestSameNumbersIgnoresTheObservationStamp pins the separation of the two
// questions: the numbers and the freshness of the reading are different
// things, and the numbers comparison answers only the first.
func TestSameNumbersIgnoresTheObservationStamp(t *testing.T) {
	a := &Stats{Engine: EngineOmlx, Supported: true, Totals: Totals{Requests: 2}, ObservedAt: 1000}
	b := &Stats{Engine: EngineOmlx, Supported: true, Totals: Totals{Requests: 2}, ObservedAt: 2000}
	if !SameNumbers(a, b) {
		t.Error("the observation stamp was taken for a change in the numbers")
	}
	if SameNumbers(nil, nil) != true {
		t.Error("two absent views are not the same")
	}
	if SameNumbers(a, nil) || SameNumbers(nil, a) {
		t.Error("an absent view and a present one are the same")
	}
}

// TestChangedReportsNumbersAndFreshness pins what is worth announcing: the
// engine's numbers moved, or the observation is newer. The stamp is the only
// thing that moves on a sweep of an idle engine, and it has to move, or a
// reader could not tell a live engine from one that died an hour ago.
func TestChangedReportsNumbersAndFreshness(t *testing.T) {
	view := &Stats{Engine: EngineOmlx, Supported: true, Totals: Totals{Requests: 2}, ObservedAt: 1000}

	// A newer observation of the same numbers is a change.
	newer := *view
	newer.ObservedAt = 2000
	if !Changed(view, &newer) {
		t.Error("a newer observation was not reported as a change")
	}

	// The same observation again is not.
	if Changed(view, view) {
		t.Error("the same reading was reported as a change")
	}
	if Changed(&newer, view) {
		t.Error("an older observation was reported as a change")
	}

	// Moved numbers are a change even at the same stamp.
	moved := *view
	moved.Totals.Requests = 3
	if !Changed(view, &moved) {
		t.Error("moved numbers were not reported as a change")
	}

	// A view appearing where there was none is a change; two absent views
	// are not.
	if !Changed(nil, view) {
		t.Error("a first view was not reported as a change")
	}
	if Changed(nil, nil) {
		t.Error("two absent views were reported as a change")
	}
}

// TestSameViewIsTheSymmetricReading pins the symmetric comparison the
// relay and the broker use: two views are the same reading only when they
// carry the same numbers AND were observed at the same moment, so a fresher
// reading is never the same as the one it replaces.
func TestSameViewIsTheSymmetricReading(t *testing.T) {
	view := &Stats{Engine: EngineOmlx, Supported: true, Totals: Totals{Requests: 2}, ObservedAt: 1000}

	if !SameView(view, view) {
		t.Error("a view is not the same as itself")
	}

	newer := *view
	newer.ObservedAt = 2000
	if SameView(view, &newer) {
		t.Error("a newer observation read as the same reading")
	}
	if SameView(&newer, view) {
		t.Error("the comparison is not symmetric")
	}

	moved := *view
	moved.Totals.Requests = 3
	if SameView(view, &moved) {
		t.Error("moved numbers read as the same reading")
	}

	if !SameView(nil, nil) {
		t.Error("two absent views are not the same reading")
	}
	if SameView(view, nil) || SameView(nil, view) {
		t.Error("an absent view and a present one are the same reading")
	}
}
