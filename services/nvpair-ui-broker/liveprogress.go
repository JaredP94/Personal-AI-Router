// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
	"sync"

	"nvpair-shared/inferencemetrics"
)

// The live progress frame's method names on the wire.
//
// methodProgress is the producer's own name: the proxy publishes a frame
// under it, the broker hands it to the workload-manager, and the manager
// relays it to the peers unchanged. methodWorkloadsProgress is the broker's
// translated name, the one the clients consume — the same translation the
// lifecycle events get (a proxy's workload:started reaches the client as a
// workloads:upsert).
const (
	methodProgress          = "workload:progress"
	methodWorkloadsProgress = "workloads:progress"
)

// liveViews returns the broker's map of live job views, creating it on first
// use so a Broker assembled without one still works.
func (b *Broker) liveViews() *liveProgressIndex {
	b.liveMu.Lock()
	defer b.liveMu.Unlock()
	if b.liveProgress == nil {
		b.liveProgress = newLiveProgressIndex(liveProgressCapacity)
	}
	return b.liveProgress
}

// liveJobKey identifies the live view of one in-flight job.
//
// It is the ORIGIN node plus the ENGINE plus PAIR's job id.
//
// The job id alone is not enough: every proxy counts its own requests from 1,
// so two origins can hold a job "1" at the same time, and — on one node —
// an oMLX job "1" and an Ollama job "1" are two different jobs as well. The
// origin tells two origins' jobs apart and the engine tells two engines'
// jobs apart; without the engine, one job's chunk count and the other job's
// engine token count would be shown on one card.
//
// The serving node is deliberately NOT part of the key: the origin, the
// engine and the job id already identify the job, and every frame of one job
// — the origin proxy's PAIR-derived facts and the serving node's engine
// reading — carries the same origin and the same engine.
type liveJobKey struct {
	origin string
	engine string
	jobID  string
}

// liveJob is one job's merged live view.
type liveJob struct {
	key   liveJobKey
	frame inferencemetrics.ProgressFrame
}

// stampProgressOrigin fills in the frame's origin node id when the frame
// carries none.
//
// The origin's own broker stamps its own id, the way it stamps
// `originatedFrom` onto a workload lifecycle event: the proxy that received
// the request cannot know it is the origin of a job it forwarded onward. A
// frame that already carries an origin — one the serving node published for a
// request a peer forwarded — is left alone, so the serving node's broker
// never re-stamps a peer's job with its own id.
func stampProgressOrigin(frame inferencemetrics.ProgressFrame, nodeID string) inferencemetrics.ProgressFrame {
	if frame.OriginatedFrom == "" {
		frame.OriginatedFrom = nodeID
	}
	return frame
}

// liveProgressCapacity bounds how many live job views the broker keeps. It is
// sized for a cluster running far more jobs at once than a desktop shows: the
// bound exists so a node that never sees a job's terminal event (a peer that
// joined late, a dropped frame) cannot accumulate views forever.
const liveProgressCapacity = 512

// retiredProgressCapacity bounds how many retired jobs the broker remembers.
// A retired job is one whose terminal event has been seen: a frame that
// arrives after that must not bring it back to life. The set ages out, so it
// only has to outlive the in-flight frames of a finished job.
const retiredProgressCapacity = 4096

// retiredRule is a rule that retires a job's live view.
//
// It names the job by the identity its frames carry: the job id, and the
// origin and the engine when the event that retired the job named them. An
// empty origin matches every origin and an empty engine matches every
// engine, because the removal wire carries only (workloadId, originatedFrom)
// — it cannot name the engine — and a retirement with no origin retires the
// job wherever it ran.
//
// The run id narrows a rule to one run of one proxy. A proxy that restarts
// counts its jobs from 1 again, so the job "1" of the new run is a different
// job from the job "1" of the old one; the run id is what tells them apart.
type retiredRule struct {
	origin string
	engine string
	runID  string
	jobID  string
}

// matches reports whether the rule covers the view of the given key, read by
// a frame of the given run.
//
// A rule that names a run covers a frame of that run and a frame that names
// no run (an older producer that does not carry one): a frame that cannot say
// which run it belongs to is not allowed to revive a job that is over.
func (r retiredRule) matches(key liveJobKey, runID string) bool {
	if r.jobID != key.jobID {
		return false
	}
	if r.origin != "" && r.origin != key.origin {
		return false
	}
	if r.engine != "" && r.engine != key.engine {
		return false
	}
	if r.runID != "" && runID != "" && r.runID != runID {
		return false
	}
	return true
}

// retiredEntry is one retired job: the rules that cover it, and its place in
// the ageing order.
type retiredEntry struct {
	jobID string
	rules []retiredRule
	// elem is this entry's place in the ageing order.
	elem *list.Element
}

// liveProgressIndex is the broker's map of live job views.
//
// The map is bounded and evicts the least recently updated view when it is
// full. Beside it the index remembers the jobs that have been retired, so a
// frame that arrives after a job's terminal event is dropped instead of
// resurrecting a job that is over.
type liveProgressIndex struct {
	mu       sync.Mutex
	capacity int
	// order holds the live views, most recently updated at the front.
	order   *list.List
	entries map[liveJobKey]*list.Element

	// retired remembers the jobs whose live view was retired, by job id.
	// The order ages the oldest retirement out first.
	retiredCapacity int
	retiredOrder    *list.List
	retired         map[string]*retiredEntry
}

// newLiveProgressIndex builds an empty index. A capacity of zero or less
// falls back to liveProgressCapacity.
func newLiveProgressIndex(capacity int) *liveProgressIndex {
	if capacity <= 0 {
		capacity = liveProgressCapacity
	}
	return &liveProgressIndex{
		capacity:        capacity,
		order:           list.New(),
		entries:         make(map[liveJobKey]*list.Element, capacity),
		retiredCapacity: retiredProgressCapacity,
		retiredOrder:    list.New(),
		retired:         make(map[string]*retiredEntry),
	}
}

// merge folds a live frame into the view of its job and returns the merged
// view.
//
// It reports false when the frame is rejected: a frame without a job id
// names no job, a frame without an engine cannot be told apart from another
// engine's job with the same id, and a frame for a job that has already
// finished is dropped — a progress frame must never resurrect a retired job.
func (i *liveProgressIndex) merge(frame inferencemetrics.ProgressFrame) (inferencemetrics.ProgressFrame, bool) {
	if frame.JobID == "" || frame.Engine == "" {
		return inferencemetrics.ProgressFrame{}, false
	}
	key := liveJobKey{origin: frame.OriginatedFrom, engine: frame.Engine, jobID: frame.JobID}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.isRetiredLocked(key, frame.RunID) {
		return inferencemetrics.ProgressFrame{}, false
	}

	el, ok := i.entries[key]
	if !ok {
		job := &liveJob{key: key}
		job.frame.JobID = key.jobID
		job.frame.OriginatedFrom = key.origin
		job.frame.Engine = key.engine
		el = i.order.PushFront(job)
		i.entries[key] = el
		i.evictLocked()
	} else {
		// A job that is still streaming moves to the front, so the view
		// that is being updated is the last one evicted.
		i.order.MoveToFront(el)
	}
	job := el.Value.(*liveJob)
	if job.frame.RunID != "" && frame.RunID != "" && job.frame.RunID != frame.RunID {
		// A proxy that restarted counts its jobs from 1 again, so this job
		// id now names a DIFFERENT job: the old view is replaced, not
		// merged, or one job's numbers would be shown on the other's card.
		job.frame = inferencemetrics.ProgressFrame{
			JobID:          key.jobID,
			OriginatedFrom: key.origin,
			Engine:         key.engine,
			RunID:          frame.RunID,
		}
	}
	mergeProgress(&job.frame, frame)
	return job.frame, true
}

// evictLocked drops the least recently updated views until the map is back
// within its bound. The caller holds the lock.
func (i *liveProgressIndex) evictLocked() {
	for i.order.Len() > i.capacity {
		oldest := i.order.Back()
		if oldest == nil {
			return
		}
		i.order.Remove(oldest)
		delete(i.entries, oldest.Value.(*liveJob).key)
	}
}

// mergeProgress folds an incoming frame into the stored view of one job.
//
// A frame is a PARTIAL view: the origin proxy's frame carries what PAIR
// measured of the client's stream, the serving node's frame carries what
// the engine reported, and neither carries the other's facts. So a field the
// incoming frame does not carry leaves the stored value alone — "not
// measured here" is not "zero" — and a field it does carry overwrites it.
//
// The engine's reading is the one exception: it is a single reading of a
// single engine row, so it is replaced as a unit rather than merged field by
// field.
func mergeProgress(stored *inferencemetrics.ProgressFrame, incoming inferencemetrics.ProgressFrame) {
	if incoming.OriginatedFrom != "" {
		stored.OriginatedFrom = incoming.OriginatedFrom
	}
	if incoming.Engine != "" {
		stored.Engine = incoming.Engine
	}
	if incoming.RunID != "" {
		stored.RunID = incoming.RunID
	}
	if incoming.NodeID != "" {
		stored.NodeID = incoming.NodeID
	}
	if incoming.ElapsedMs != nil {
		stored.ElapsedMs = incoming.ElapsedMs
	}
	if incoming.Chunks != nil {
		stored.Chunks = incoming.Chunks
	}
	if incoming.Bytes != nil {
		stored.Bytes = incoming.Bytes
	}
	if incoming.TTFBMs != nil {
		stored.TTFBMs = incoming.TTFBMs
	}
	if incoming.EngineProgress != nil {
		stored.EngineProgress = incoming.EngineProgress
	}
}

// retire drops the live view of a job that reached a terminal state and
// remembers the job, so a frame that arrives late cannot bring it back.
//
// The rule names the job the way the frames do: the origin and the engine
// when the event named them, and the job id always. A rule that names no
// origin retires the job on every origin, because the job is over wherever
// it ran and the event named no origin to retire; a rule that names no
// engine retires the job on every engine, because the removal wire names no
// engine.
//
// The rule covers exactly the views it removes, so a late frame of a retired
// job — on any origin, on any engine — is dropped instead of recreating a
// view of a job that is over.
func (i *liveProgressIndex) retire(rule retiredRule) {
	if rule.jobID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	for el := i.order.Front(); el != nil; {
		next := el.Next()
		job := el.Value.(*liveJob)
		if rule.matches(job.key, job.frame.RunID) {
			i.order.Remove(el)
			delete(i.entries, job.key)
		}
		el = next
	}
	i.markRetiredLocked(rule)
}

// isRetiredLocked reports whether a frame of the given key and run belongs
// to a retired job. The caller holds the lock.
func (i *liveProgressIndex) isRetiredLocked(key liveJobKey, runID string) bool {
	entry, ok := i.retired[key.jobID]
	if !ok {
		return false
	}
	for _, rule := range entry.rules {
		if rule.matches(key, runID) {
			return true
		}
	}
	return false
}

// markRetiredLocked remembers a retirement rule, ageing the oldest retired
// job out when the set is full. The caller holds the lock.
func (i *liveProgressIndex) markRetiredLocked(rule retiredRule) {
	entry, ok := i.retired[rule.jobID]
	if !ok {
		entry = &retiredEntry{jobID: rule.jobID}
		i.retired[rule.jobID] = entry
		entry.elem = i.retiredOrder.PushFront(entry)
	} else {
		// The job was retired again: it is the most recent retirement, so
		// it ages out last.
		i.retiredOrder.MoveToFront(entry.elem)
	}
	entry.rules = append(entry.rules, rule)
	for i.retiredOrder.Len() > i.retiredCapacity {
		oldest := i.retiredOrder.Back()
		if oldest == nil {
			return
		}
		i.retiredOrder.Remove(oldest)
		delete(i.retired, oldest.Value.(*retiredEntry).jobID)
	}
}

// get returns the merged live view of a job.
func (i *liveProgressIndex) get(origin, engine, jobID string) (inferencemetrics.ProgressFrame, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	el, ok := i.entries[liveJobKey{origin: origin, engine: engine, jobID: jobID}]
	if !ok {
		return inferencemetrics.ProgressFrame{}, false
	}
	return el.Value.(*liveJob).frame, true
}

// len returns the number of live job views held.
func (i *liveProgressIndex) len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.entries)
}
