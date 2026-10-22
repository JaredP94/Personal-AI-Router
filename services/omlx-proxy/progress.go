// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync"
	"time"

	"nvpair-shared/inferencemetrics"
)

// progressMethod is the notification method a live progress frame travels
// on. The frame is a snapshot of a running request, not a state transition:
// it never enters the workload catalog and is never deduped against the
// lifecycle event that shares its job id.
const progressMethod = "workload:progress"

// progressInterval is the smallest gap between two progress frames for one
// request. The limit is at the source, so a cluster with twenty running
// jobs carries at most ten frames per second whatever the engines' chunk
// rates are.
//
// It is a var (not a const) only so a test can shorten it; production never
// reassigns it.
var progressInterval = 2 * time.Second

// ProgressFrame is the live view of one in-flight inference request.
//
// It carries only what PAIR measured exactly: the elapsed time, the number
// of content chunks received, the number of response bytes received, and the
// time to the first byte. There is no token count and no rate — PAIR does
// not know the engine's token count until the response ends, and a chunk is
// not a token.
//
// The engine's own live numbers (oMLX only) ride in EngineProgress, keyed by
// the same job id, on frames the engine's poller publishes.
//
// Every field but the job id is optional: a frame that carries only part of
// the view says so instead of claiming zeros. A consumer merges the frames
// of one job field by field — the engine object is replaced as a unit,
// because it is one reading of one row.
type ProgressFrame struct {
	// JobID is PAIR's own id for the request. It is the same id the
	// workload lifecycle events carry, so a frame joins a job card without
	// a lookup.
	JobID string `json:"jobId"`
	// NodeID is the node that is serving the request. For a request this
	// proxy forwarded to a peer it is the peer's id, not this node's: the
	// frame describes where the work runs.
	NodeID string `json:"nodeId,omitempty"`
	// OriginatedFrom is the node the request entered the cluster on. It is
	// the origin of the job, and the half of the identity a job is keyed
	// by: PAIR's job id is a per-proxy counter, so two origins can both
	// hold a job "1".
	//
	// It is set only for a request this proxy serves on behalf of a peer:
	// the origin's id travelled with the request. A request this proxy
	// received directly leaves it empty, and the origin's own broker stamps
	// this node's id onto the frame.
	OriginatedFrom string `json:"originatedFrom,omitempty"`
	// Engine is the engine that serves the job. It is part of the job's
	// identity, not decoration: every proxy counts its own requests from
	// 1, so an oMLX job "1" and an Ollama job "1" on the same node are two
	// different jobs, and only the engine tells them apart.
	Engine string `json:"engine"`
	// RunID is this proxy's per-process run nonce — the same value the
	// workload carries as `runId`. It tells a recycled job id from the job
	// it replaced: a proxy that restarts counts its jobs from 1 again.
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
	EngineProgress *EngineProgress `json:"engineProgress,omitempty"`
}

// EngineProgress is the engine's own live reading for one request.
//
// Every number in it was measured by the engine, and only what the engine
// reported is present: a reading it did not report stays absent rather than
// becoming a zero.
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

// progressLimiter lets at most one frame per job leave every
// progressInterval. It is per job, not per proxy: one busy job must not
// silence another job's frames.
//
// It is SHARED by the two publishers of one job — the PAIR-derived facts and
// the engine's own reading — so a job leaves at most one frame per window
// whichever publisher has something to say. Both publishers put both halves
// of the view on the frame they leave, so a frame the limiter held back
// loses nothing: the other publisher's numbers are still in the merged view,
// and the next window brings the newer ones.
type progressLimiter struct {
	mu   sync.Mutex
	last time.Time
	// interval is the job's own window. It is read from the package
	// setting once, when the limiter is created, so the window a job
	// leaves frames at does not change under it — and so the setting is
	// read on the request's own goroutine, never from the poller's.
	interval time.Duration
}

// newProgressLimiter returns the limiter of one job.
func newProgressLimiter() *progressLimiter {
	return &progressLimiter{interval: progressInterval}
}

// due reports whether a frame may be emitted now, and records the emission.
//
// The time is the frame's effective time — the moment the frame was
// produced, not the moment it was written to the wire.
func (l *progressLimiter) due(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < l.interval {
		return false
	}
	l.last = now
	return true
}

// emitProgress publishes a live progress frame. A failed write is logged and
// dropped: a frame is a snapshot, and the next one carries the newer
// numbers.
func (p *Proxy) emitProgress(frame ProgressFrame) {
	if err := p.codec.Notify(progressMethod, frame); err != nil {
		slog.Debug("failed to emit progress frame", "job_id", frame.JobID, "err", err)
	}
}

// publishProgress publishes the PAIR-derived live facts of an in-flight
// request, rate-limited to one frame every progressInterval.
//
// It runs on the request's own goroutine — the one copying the response
// body — so a frame leaves with the chunk that triggered it and no separate
// ticker is involved. The frame carries only what PAIR measured: the
// engine's own live numbers travel on their own frames, under the same job
// id.
//
// The limiter is the job's own, shared with the engine's own publisher, so
// the two never both leave a frame in one window. The engine's own numbers
// ride on this frame as well: the two publishers share one window, and the
// request's own publisher — driven by the response's chunks — wins nearly
// every one of them, so a frame that left only the PAIR-derived facts
// would leave the engine's numbers to a publisher that never wins.
//
// The facts are recorded on the job as they are measured, so the engine's
// publisher can carry them on the frames it leaves.
func (p *Proxy) publishProgress(job *engineJob, prog LiveProgress) {
	now := time.Now()
	if !job.limiter.due(now) {
		return
	}
	frame := ProgressFrame{
		JobID:     job.jobID,
		NodeID:    job.nodeID,
		Engine:    workloadEngine,
		RunID:     job.runID,
		ElapsedMs: inferencemetrics.Int64Ptr(now.Sub(job.start).Milliseconds()),
		Chunks:    inferencemetrics.Int64Ptr(prog.Chunks),
		Bytes:     inferencemetrics.Int64Ptr(prog.Bytes),
	}
	if prog.HasByte {
		frame.TTFBMs = inferencemetrics.Int64Ptr(prog.TTFBMs)
	}
	if reading := job.latestReading(); reading != nil {
		frame.EngineProgress = reading
	}
	p.emitProgress(frame)
}
