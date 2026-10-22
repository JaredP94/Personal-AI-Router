// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
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
// The frame is engine-agnostic: an engine that reports live numbers of its
// own publishes them under the same job id, on a frame of the same shape.
// This engine reports no live numbers, so the frame never carries any.
//
// Every field but the job id is optional: a frame that carries only part of
// the view says so instead of claiming zeros.
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
	// 1, so an LM Studio job "1" and an oMLX job "1" on the same node are
	// two different jobs, and only the engine tells them apart.
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
}

// progressLimiter lets at most one frame per request leave every
// progressInterval. It is per request, not per proxy: one busy job must not
// silence another job's frames.
type progressLimiter struct {
	mu   sync.Mutex
	last time.Time
}

// newProgressLimiter returns the limiter of one job.
func newProgressLimiter() *progressLimiter {
	return &progressLimiter{}
}

// due reports whether a frame may be emitted now, and records the emission.
func (l *progressLimiter) due(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < progressInterval {
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
// ticker is involved.
func (p *Proxy) publishProgress(jobID, nodeID string, start time.Time, limiter *progressLimiter, prog LiveProgress) {
	now := time.Now()
	if !limiter.due(now) {
		return
	}
	frame := ProgressFrame{
		JobID:     jobID,
		NodeID:    nodeID,
		Engine:    workloadEngine,
		RunID:     p.runID,
		ElapsedMs: inferencemetrics.Int64Ptr(now.Sub(start).Milliseconds()),
		Chunks:    inferencemetrics.Int64Ptr(prog.Chunks),
		Bytes:     inferencemetrics.Int64Ptr(prog.Bytes),
	}
	if prog.HasByte {
		frame.TTFBMs = inferencemetrics.Int64Ptr(prog.TTFBMs)
	}
	p.emitProgress(frame)
}

// peerJob is the identity of a job another node forwarded, and the instant
// the request reached the serving proxy.
//
// The identity travelled with the request: the origin proxy's job id, the
// origin's node id and the origin's run nonce.
type peerJob struct {
	// jobID is the origin's id for the request.
	jobID string
	// originNodeID is the node the request entered the cluster on.
	originNodeID string
	// runID is the origin proxy's per-process run nonce.
	runID string
	// start is the instant the request reached this proxy, so the elapsed
	// time and the time to the first byte are the request's own.
	start time.Time
}

// peerProgress is what the serving side measured of a peer-served request's
// response: the bytes the engine sent and the instant the first of them
// arrived.
//
// It carries no chunk count: the serving side relays the engine's stream
// without parsing it, so it has no content chunks to count. The origin's own
// frame carries them, and the two partial views are merged field by field.
type peerProgress struct {
	// bytes is the number of response body bytes the engine sent.
	bytes int64
	// hasByte reports whether a response byte has arrived at all.
	hasByte bool
	// ttfbMs is the time to the first response byte, in milliseconds.
	ttfbMs int64
}

// peerProgressReader counts the bytes the engine sent to a peer-served
// request and notes the instant the first of them arrived.
//
// It runs on the goroutine that copies the response back to the peer, and it
// reports on every read, so the frame leaves with the bytes that triggered
// it.
type peerProgressReader struct {
	io.ReadCloser
	// start is the instant the request reached the proxy, so the elapsed
	// time and the time to the first byte are the request's own.
	start      time.Time
	onProgress func(peerProgress)

	mu          sync.Mutex
	bytes       int64
	firstByteAt time.Time
}

// newPeerProgressReader wraps a response body with the reader that counts
// the bytes the engine sent.
func newPeerProgressReader(body io.ReadCloser, start time.Time) *peerProgressReader {
	return &peerProgressReader{ReadCloser: body, start: start}
}

// Read reads the response body and counts what arrived.
func (r *peerProgressReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if n > 0 {
		r.mu.Lock()
		if r.firstByteAt.IsZero() {
			r.firstByteAt = time.Now()
		}
		r.bytes += int64(n)
		prog := peerProgress{
			bytes:   r.bytes,
			hasByte: true,
			ttfbMs:  r.firstByteAt.Sub(r.start).Milliseconds(),
		}
		r.mu.Unlock()
		if r.onProgress != nil {
			r.onProgress(prog)
		}
	}
	return n, err
}

// publishPeerProgress publishes the live facts the serving side measured for
// a job another node forwarded.
//
// The frame is attributed to the job's ORIGIN: the job id, the origin's node
// id and the origin's run nonce all travelled with the request, and a live
// view is keyed by (origin, engine, job id). A frame the serving side
// published without the origin would be stamped with the serving node's own
// id by the serving node's broker, and the job's terminal event — which
// names the origin — would never retire that view.
//
// The frame carries only what the serving side measured: the elapsed time,
// the bytes the engine sent and the time to the first byte. It carries no
// chunk count, because the serving side does not parse the engine's stream.
func (p *Proxy) publishPeerProgress(job peerJob, limiter *progressLimiter, prog peerProgress) {
	now := time.Now()
	if !limiter.due(now) {
		return
	}
	frame := ProgressFrame{
		JobID:          job.jobID,
		NodeID:         p.nodeID,
		OriginatedFrom: job.originNodeID,
		Engine:         workloadEngine,
		RunID:          job.runID,
		ElapsedMs:      inferencemetrics.Int64Ptr(now.Sub(job.start).Milliseconds()),
		Bytes:          inferencemetrics.Int64Ptr(prog.bytes),
	}
	if prog.hasByte {
		frame.TTFBMs = inferencemetrics.Int64Ptr(prog.ttfbMs)
	}
	p.emitProgress(frame)
}
