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
// It is PAIR's job id plus the node serving the request. The job id alone
// is not enough: every proxy counts its own requests from 1, so two nodes
// can hold a job "1" at the same time, and the workload catalog tells them
// apart by the origin. A live frame carries no origin — it carries the job
// id and the node the work runs on — so (job id, serving node) is the
// finest identity a frame can carry. It is also the identity every frame of
// one job shares: the origin proxy's frame and the serving node's engine
// frame name the same job id and the same serving node, which is what lets
// the two merge into one view.
type liveJobKey struct {
	jobID  string
	nodeID string
}

// liveJob is one job's merged live view.
type liveJob struct {
	key   liveJobKey
	frame inferencemetrics.ProgressFrame
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

	// retired remembers the jobs whose live view was retired. The order
	// ages the oldest retirement out first.
	retiredCapacity int
	retiredOrder    *list.List
	retired         map[liveJobKey]struct{}
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
		retired:         make(map[liveJobKey]struct{}),
	}
}

// merge folds a live frame into the view of its job and returns the merged
// view.
//
// It reports false when the frame is rejected: a frame without a job id
// names no job, and a frame for a job that has already finished is dropped —
// a progress frame must never resurrect a retired job.
func (i *liveProgressIndex) merge(frame inferencemetrics.ProgressFrame) (inferencemetrics.ProgressFrame, bool) {
	if frame.JobID == "" {
		return inferencemetrics.ProgressFrame{}, false
	}
	key := liveJobKey{jobID: frame.JobID, nodeID: frame.NodeID}

	i.mu.Lock()
	defer i.mu.Unlock()

	if _, retired := i.retired[key]; retired {
		return inferencemetrics.ProgressFrame{}, false
	}

	el, ok := i.entries[key]
	if !ok {
		job := &liveJob{key: key}
		job.frame.JobID = key.jobID
		el = i.order.PushFront(job)
		i.entries[key] = el
		i.evictLocked()
	} else {
		// A job that is still streaming moves to the front, so the view
		// that is being updated is the last one evicted.
		i.order.MoveToFront(el)
	}
	job := el.Value.(*liveJob)
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
// The engine object is the one exception: it is a single reading of a single
// engine row, so it is replaced as a unit rather than merged field by field.
func mergeProgress(stored *inferencemetrics.ProgressFrame, incoming inferencemetrics.ProgressFrame) {
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
	if incoming.Engine != nil {
		stored.Engine = incoming.Engine
	}
}

// retire drops the live view of a job that reached a terminal state and
// remembers the job, so a frame that arrives late cannot bring it back.
//
// The serving node is part of the key: the frame of a peer-served job names
// the node the work runs on, and that is the node the terminal event names
// as the job's scheduledOn. A terminal event that names no node retires the
// job on every node, because the job is over wherever it ran.
func (i *liveProgressIndex) retire(jobID, nodeID string) {
	if jobID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	key := liveJobKey{jobID: jobID, nodeID: nodeID}
	if el, ok := i.entries[key]; ok {
		i.order.Remove(el)
		delete(i.entries, key)
	}
	if nodeID == "" {
		// The job is over wherever it ran: drop every view of it.
		for el := i.order.Front(); el != nil; {
			next := el.Next()
			job := el.Value.(*liveJob)
			if job.key.jobID == jobID {
				i.order.Remove(el)
				delete(i.entries, job.key)
			}
			el = next
		}
	}
	i.markRetired(key)
}

// markRetired remembers a retired job, ageing the oldest retirement out when
// the set is full. The caller holds the lock.
func (i *liveProgressIndex) markRetired(key liveJobKey) {
	if _, ok := i.retired[key]; ok {
		return
	}
	i.retired[key] = struct{}{}
	i.retiredOrder.PushFront(key)
	if i.retiredOrder.Len() > i.retiredCapacity {
		oldest := i.retiredOrder.Back()
		if oldest != nil {
			i.retiredOrder.Remove(oldest)
			delete(i.retired, oldest.Value.(liveJobKey))
		}
	}
}

// get returns the merged live view of a job.
func (i *liveProgressIndex) get(jobID, nodeID string) (inferencemetrics.ProgressFrame, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	el, ok := i.entries[liveJobKey{jobID: jobID, nodeID: nodeID}]
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
