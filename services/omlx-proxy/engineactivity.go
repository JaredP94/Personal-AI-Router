// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"nvpair-shared/enginenative"
	"nvpair-shared/inferencemetrics"
)

// The engine's own live rows are published on the engine-manager's em
// surface, which the broker gives a fixed port so peers can reach it (the
// same fixed-port pattern the node-info and workload surfaces use). The
// proxy reads it over loopback: the surface answers with PAIR's own type,
// needs no API key, and its loopback personality makes no trust decision.
//
// It is a var (not a const) only so a test can point it at a fake em
// surface; production never reassigns it.
var engineManagerActivityURL = "http://127.0.0.1:14322/v1/engine-activity"

const (
	// engineAdminActivityPath is the engine's own admin path for the same
	// rows. It is the fallback for a node whose em surface cannot be
	// reached; the proxy already holds the engine's API key for the
	// inference API, and the key never leaves this node.
	engineAdminActivityPath = "/admin/api/activity"
)

// engineActivityPollInterval is how often the engine's live rows are
// refreshed while this proxy is serving at least one request.
//
// It is a var (not a const) only so a test can shorten it; production never
// reassigns it.
var engineActivityPollInterval = 2 * time.Second

// engineActivityTimeout bounds one fetch of the engine's live rows, so a
// slow engine cannot hold the poller open.
const engineActivityTimeout = 2 * time.Second

// engineActivityMaxBody caps how much of the engine's answer PAIR reads.
const engineActivityMaxBody = 1 << 20

// engineJob is one in-flight request this proxy is serving, paired with the
// engine's own request id.
//
// The pairing is the whole point: the engine's live rows are keyed by the
// engine's request id, and PAIR's jobs are keyed by PAIR's own. Holding the
// pair locally lets the engine's rows be published already keyed by PAIR's
// job id, so no client ever joins anything.
type engineJob struct {
	// jobID is PAIR's id for the request. For a request this proxy serves
	// on behalf of a peer it is the origin's job id, carried in the
	// forwarded request.
	jobID string
	// engineReqID is the engine's own request id, read from the engine's
	// response headers.
	engineReqID string
	// originNodeID is the node the request entered the cluster on. It is
	// empty for a request this proxy received directly (the broker stamps
	// this node's own id onto such a frame), and the origin's id for a
	// request a peer forwarded — PAIR's job id is a per-proxy counter, so
	// the origin is what tells two peers' job "1" apart.
	originNodeID string
	// runID is the run the job belongs to: this proxy's own run nonce for
	// a request it received directly, and the origin's run nonce for a
	// request a peer forwarded (it travelled with the request). It tells a
	// recycled job id from the job it replaced.
	runID string
	// limiter is the job's own progress limiter. It is shared with the
	// publisher of the PAIR-derived facts, so the job leaves at most one
	// frame per window whichever publisher has something to say.
	limiter *progressLimiter
	// nodeID is the node that serves the request: this node's own id for a
	// request this proxy serves, and this node's id for a request a peer
	// forwarded (the peer is the origin, this node does the work).
	nodeID string
	// start is when the request reached this proxy.
	start time.Time

	// mu guards the two readings the job's two publishers share.
	mu sync.Mutex
	// latest is the engine's most recent reading of this job, as the
	// engine's publisher last saw it.
	//
	// The request's own publisher attaches it to its frames. The job's two
	// publishers share one window, so the one that leaves the frame may be
	// either of them — and a frame that carried only one half of the view
	// would leave the other half to a publisher that may never win a
	// window. The engine's publisher polls on a timer while the request's
	// publisher is driven by the response's own chunks, so the request's
	// publisher wins nearly every window: without this the engine's
	// numbers would never reach a client.
	latest *EngineProgress
	// facts is what PAIR measured of the response so far, and hasFacts
	// reports whether the response has produced anything yet.
	//
	// The engine's publisher attaches them to its frames, for the same
	// reason: a frame that leaves in a window the request's publisher did
	// not win must still carry what PAIR measured.
	facts    LiveProgress
	hasFacts bool
}

// recordLatest stores the engine's most recent reading of this job.
func (j *engineJob) recordLatest(reading *EngineProgress) {
	j.mu.Lock()
	j.latest = reading
	j.mu.Unlock()
}

// latestReading returns the engine's most recent reading of this job, or nil
// when the engine has reported nothing for it yet.
func (j *engineJob) latestReading() *EngineProgress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.latest
}

// recordFacts stores what PAIR measured of the response so far.
func (j *engineJob) recordFacts(prog LiveProgress) {
	j.mu.Lock()
	j.facts = prog
	j.hasFacts = true
	j.mu.Unlock()
}

// factsSnapshot returns what PAIR measured of the response so far, and
// whether the response has produced anything yet.
func (j *engineJob) factsSnapshot() (LiveProgress, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.facts, j.hasFacts
}

// engineActivity holds the pairing between the engine's request ids and
// PAIR's job ids.
//
// It is in memory and dies with the request: a pairing is forgotten when the
// response it belongs to is done.
type engineActivity struct {
	mu   sync.Mutex
	jobs map[string]*engineJob
	// cancel stops the poller, which runs only while at least one request
	// is being served.
	cancel context.CancelFunc
}

// engineProgressOf maps one engine row onto the frame's engine object. Only
// the readings the engine reported are carried, and each is copied so the
// published frame never aliases the cached row.
func engineProgressOf(row enginenative.ActivityRow) *EngineProgress {
	out := &EngineProgress{RequestID: row.RequestID, Phase: row.Phase}
	if row.GeneratedTokens != nil {
		value := *row.GeneratedTokens
		out.GeneratedTokens = &value
	}
	if row.TokensPerSecond != nil {
		value := *row.TokensPerSecond
		out.TokensPerSecond = &value
	}
	if row.PrefillProgress != nil {
		value := *row.PrefillProgress
		out.PrefillProgress = &value
	}
	if row.QueuePosition != nil {
		value := *row.QueuePosition
		out.QueuePosition = &value
	}
	return out
}

// bindEngineJob records the pairing between the engine's request id and
// PAIR's job id, and starts the poller when this is the first request being
// served.
func (p *Proxy) bindEngineJob(job *engineJob) {
	if job == nil || job.engineReqID == "" || job.jobID == "" {
		return
	}
	if job.limiter == nil {
		// A job always has a limiter: without one it would publish a
		// frame on every poll, beside the frames the PAIR-derived
		// publisher leaves.
		job.limiter = newProgressLimiter()
	}
	if job.nodeID == "" {
		// This node serves the request: a request a peer forwarded is
		// served here, and the frame names the node that does the work.
		job.nodeID = p.nodeID
	}
	p.engine.mu.Lock()
	if p.engine.jobs == nil {
		p.engine.jobs = make(map[string]*engineJob)
	}
	p.engine.jobs[job.engineReqID] = job
	var ctx context.Context
	start := false
	if p.engine.cancel == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		p.engine.cancel = cancel
		start = true
	}
	p.engine.mu.Unlock()
	if start {
		go p.pollEngineActivity(ctx)
	}
}

// releaseEngineJob forgets a request that has finished. The pairing dies
// with the request.
func (p *Proxy) releaseEngineJob(engineReqID string) {
	if engineReqID == "" {
		return
	}
	p.engine.mu.Lock()
	delete(p.engine.jobs, engineReqID)
	idle := len(p.engine.jobs) == 0
	cancel := p.engine.cancel
	p.engine.cancel = nil
	p.engine.mu.Unlock()
	if idle && cancel != nil {
		cancel()
	}
}

// engineJobID returns the PAIR job id paired with an engine request id.
func (p *Proxy) engineJobID(engineReqID string) (string, bool) {
	p.engine.mu.Lock()
	defer p.engine.mu.Unlock()
	job, ok := p.engine.jobs[engineReqID]
	if !ok {
		return "", false
	}
	return job.jobID, true
}

// pollEngineActivity refreshes the engine's live rows until the last
// request this proxy serves is done.
func (p *Proxy) pollEngineActivity(ctx context.Context) {
	ticker := time.NewTicker(engineActivityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			p.refreshEngineActivity(ctx, tick)
		}
	}
}

// refreshEngineActivity reads the engine's live rows once and publishes them
// for the requests this proxy is serving.
//
// The rows are published on their own, keyed by PAIR's job id: they are the
// engine's reading of a request, and the request's own frames carry the
// facts PAIR measured. A consumer merges the two under the same job id.
//
// The frames leave through the job's own limiter, the one the PAIR-derived
// publisher shares, so a job leaves at most one frame per window whichever
// publisher has something to say.
//
// The tick's own time is the frame's effective time, so the window is
// measured between two polls and a poll that runs a moment late does not
// eat the next poll's window.
func (p *Proxy) refreshEngineActivity(ctx context.Context, tick time.Time) {
	activity, err := p.fetchEngineActivity(ctx)
	if err != nil {
		slog.Debug("engine activity unavailable", "err", err)
		return
	}
	if !activity.Supported {
		return
	}
	rows := make(map[string]enginenative.ActivityRow, len(activity.Rows))
	for _, row := range activity.Rows {
		rows[row.RequestID] = row
	}
	p.engine.mu.Lock()
	jobs := make([]*engineJob, 0, len(p.engine.jobs))
	for _, job := range p.engine.jobs {
		jobs = append(jobs, job)
	}
	p.engine.mu.Unlock()

	for _, job := range jobs {
		row, ok := rows[job.engineReqID]
		if !ok {
			continue
		}
		// The reading is cached whatever the limiter says: the request's
		// own publisher attaches it to its frames, so a window the engine's
		// publisher does not win still carries the engine's numbers.
		reading := engineProgressOf(row)
		job.recordLatest(reading)
		if !job.limiter.due(tick) {
			continue
		}
		frame := ProgressFrame{
			JobID:          job.jobID,
			NodeID:         job.nodeID,
			OriginatedFrom: job.originNodeID,
			Engine:         workloadEngine,
			RunID:          job.runID,
			EngineProgress: reading,
		}
		// The facts PAIR measured ride on this frame too. The job's two
		// publishers share one window, so the frame that leaves must carry
		// both halves of the view: this process measured both, and the
		// broker merges the halves of a job's frames field by field.
		if facts, ok := job.factsSnapshot(); ok {
			frame.ElapsedMs = inferencemetrics.Int64Ptr(tick.Sub(job.start).Milliseconds())
			frame.Chunks = inferencemetrics.Int64Ptr(facts.Chunks)
			frame.Bytes = inferencemetrics.Int64Ptr(facts.Bytes)
			if facts.HasByte {
				frame.TTFBMs = inferencemetrics.Int64Ptr(facts.TTFBMs)
			}
		}
		p.emitProgress(frame)
	}
}

// fetchEngineActivity reads the engine's live rows.
//
// The engine-manager's em surface is preferred: it answers on loopback with
// PAIR's own type, needs no API key, and its loopback personality makes no
// trust decision. When it cannot be reached, the engine's own admin API is
// the fallback — the proxy already reads that API key for the engine's
// inference API, and the key never leaves this node.
func (p *Proxy) fetchEngineActivity(ctx context.Context) (enginenative.Activity, error) {
	activity, err := p.fetchEngineActivityFromManager(ctx)
	if err == nil {
		return activity, nil
	}
	target, ok := p.localBackendTarget()
	if !ok {
		return enginenative.Activity{}, err
	}
	return p.fetchEngineActivityFromEngine(ctx, target)
}

// fetchEngineActivityFromManager reads the em surface's engine-activity
// endpoint on loopback.
func (p *Proxy) fetchEngineActivityFromManager(ctx context.Context) (enginenative.Activity, error) {
	body, err := fetchEngineActivityBody(ctx, engineManagerActivityURL, "")
	if err != nil {
		return enginenative.Activity{}, err
	}
	var activity enginenative.Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		return enginenative.Activity{}, fmt.Errorf("the engine-activity answer is not readable: %w", err)
	}
	return activity, nil
}

// fetchEngineActivityFromEngine reads the engine's own admin API, which is
// the fallback when the em surface cannot be reached.
func (p *Proxy) fetchEngineActivityFromEngine(ctx context.Context, target *url.URL) (enginenative.Activity, error) {
	endpoint := target.Scheme + "://" + target.Host + engineAdminActivityPath
	body, err := fetchEngineActivityBody(ctx, endpoint, readOMLXAPIKey())
	if err != nil {
		return enginenative.Activity{}, err
	}
	activity, err := enginenative.ParseActivity(body)
	if err != nil {
		return enginenative.Activity{}, fmt.Errorf("the engine's activity answer is not readable: %w", err)
	}
	activity.Engine = enginenative.EngineOmlx
	activity.Supported = true
	return activity, nil
}

// fetchEngineActivityBody reads one activity endpoint. The API key is sent
// only when there is one, and it never appears in what this returns.
func fetchEngineActivityBody(ctx context.Context, endpoint, apiKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, engineActivityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the engine answered HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, engineActivityMaxBody))
}
