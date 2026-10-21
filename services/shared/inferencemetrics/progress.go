// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package inferencemetrics

// ProgressFrame is the live view of one in-flight inference request.
//
// A frame is a snapshot of a running request, not a state transition: it
// never enters the workload catalog and it never changes a job's state.
//
// The frame is a PARTIAL view, and that is the whole point of it. Two
// different nodes publish under the same job id: the origin proxy publishes
// the facts PAIR measured of the client's stream (the elapsed time, the
// content chunks received, the response bytes received, the time to the
// first byte), and the serving node publishes what the engine reported of
// its own request. Neither publisher can see the other's facts, so each
// frame carries only what its publisher measured and a consumer merges the
// frames of one job field by field: a field a frame does not carry means
// "not measured here", never "zero".
//
// Every field but the job id is optional and omitted when it was not
// measured, so a measured zero stays distinguishable from an absent
// measurement.
//
// The job id and the engine together are what a frame joins a job by: the
// engine is not a measurement, so it is never absent on a frame that names a
// job — a frame without one joins no job at all.
type ProgressFrame struct {
	// JobID is PAIR's own id for the request. It is the same id the
	// workload lifecycle events carry, so a frame joins a job without a
	// lookup.
	JobID string `json:"jobId"`
	// NodeID is the node that is serving the request. For a request the
	// origin proxy forwarded to a peer it is the peer's id, not the
	// origin's: the frame describes where the work runs.
	NodeID string `json:"nodeId,omitempty"`
	// OriginatedFrom is the node the request entered the cluster on — the
	// origin of the job, and the half of the identity a job is keyed by.
	//
	// It is needed because PAIR's job id is a per-proxy counter: two
	// origins can both hold a job "1", and only the origin tells their
	// jobs apart. It is the same value the workload catalog carries as
	// `originatedFrom`, so a frame joins a job by the pair
	// (`originatedFrom`, `id`) — the same pair the catalog uses.
	//
	// It is absent on a frame a proxy publishes, because the proxy does
	// not know it is the origin of a job it forwarded onward; the origin's
	// own broker stamps it, exactly as it stamps `originatedFrom` onto a
	// workload lifecycle event.
	OriginatedFrom string `json:"originatedFrom,omitempty"`
	// Engine is the engine that serves the job: `omlx`, `ollama` or
	// `lmstudio`.
	//
	// It is part of the job's identity, not decoration: every proxy counts
	// its own requests from 1, so an oMLX job "1" and an Ollama job "1" on
	// the same node are two different jobs, and only the engine tells them
	// apart. It is the same value the workload catalog carries as
	// `engine`.
	Engine string `json:"engine,omitempty"`
	// RunID is the origin proxy's per-process run nonce — the same value
	// the workload carries as `runId`.
	//
	// It tells a recycled job id from the job it replaced: a proxy that
	// restarts counts its jobs from 1 again, and the run id says the job
	// "1" of this run is not the job "1" of the last one. It is the third
	// half of the identity the workload catalog keys a job by
	// (`originatedFrom`, `engine`, `runId`, `id`).
	RunID string `json:"runId,omitempty"`
	// ElapsedMs is the time since the request reached the proxy.
	ElapsedMs *int64 `json:"elapsedMs,omitempty"`
	// Chunks is the number of content chunks received so far.
	Chunks *int64 `json:"chunks,omitempty"`
	// Bytes is the number of response body bytes received so far.
	Bytes *int64 `json:"bytes,omitempty"`
	// TTFBMs is the time to the first response byte. It is absent until a
	// byte has arrived.
	TTFBMs *int64 `json:"ttfbMs,omitempty"`
	// EngineProgress is the engine's own live reading for this request. It
	// is absent when the engine reported nothing.
	//
	// It is the engine's own numbers and nothing else: no token count, no
	// rate and no phase timing that the engine did not report.
	EngineProgress *EngineProgress `json:"engineProgress,omitempty"`
}

// EngineProgress is the engine's own live reading of one request.
//
// Every number in it was measured by the engine, and only what the engine
// reported is present: a reading it did not report stays absent rather than
// becoming a zero.
//
// It is one reading of one engine row, so a consumer replaces it as a unit
// rather than merging it field by field: a later row supersedes the earlier
// one, it does not blend with it.
type EngineProgress struct {
	// RequestID is the engine's own request id — the id the engine's live
	// rows are keyed by.
	RequestID string `json:"requestId"`
	// Phase is the phase the engine reported the request in: waiting,
	// prefilling or generating.
	Phase string `json:"phase"`
	// GeneratedTokens is the number of tokens the engine has generated.
	GeneratedTokens *int `json:"generatedTokens,omitempty"`
	// TokensPerSecond is the engine's own generation rate.
	TokensPerSecond *float64 `json:"tokensPerSecond,omitempty"`
	// PrefillProgress is the engine's own prefill progress.
	PrefillProgress *float64 `json:"prefillProgress,omitempty"`
	// QueuePosition is the engine's own queue position.
	QueuePosition *int `json:"queuePosition,omitempty"`
}
