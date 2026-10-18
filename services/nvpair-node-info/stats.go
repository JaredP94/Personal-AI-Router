// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"strings"
	"time"
)

// gpuStat retains per-adapter metric presence independently of its value.
// A valid zero is a measurement, not an unavailable counter.
type gpuStat struct {
	VRAMUsed         uint64
	VRAMUsedValid    bool
	UtilizationPct   uint32
	UtilizationValid bool
}

// statsSnapshot is the dynamic bundle the HTTP handler reads without locking.
// Darwin combines independently published system and GPU samples so a slow
// ioreg call cannot delay CPU or memory telemetry.
// GPUInventory is normally nil. The Darwin collector populates it from its
// retrying ioreg sample so a transient startup enumeration failure can recover
// without restarting the service.
//
// Unsupported collectors publish a zero-valued snapshot with no valid GPU
// metrics. MemUsedValid distinguishes measured system-memory usage from unknown
// when a Linux unified-memory adapter uses that reading.
type statsSnapshot struct {
	GPU          map[string]gpuStat
	GPUInventory []GPUInfo
	// GPUSampledAt is the collection time of the latest usable GPU
	// utilization sample. A zero value means no usable sample has ever been
	// collected. Failed collection attempts retain the previous timestamp so
	// consumers can distinguish stale telemetry from freshly sampled data.
	GPUSampledAt time.Time
	GPUSampleSeq uint64
	CPUUtilPct   uint32
	MemUsedBytes uint64
	MemUsedValid bool
}

// applyGPUStats publishes a usable GPU sample or preserves the last usable
// sample after a failed collection. Before the first usable sample, callers may
// still publish partial dynamic fields (for example VRAM usage) with a zero
// sampledAt; those fields remain display-only until utilization becomes valid.
func applyGPUStats(previous statsSnapshot, next *statsSnapshot, sampled map[string]gpuStat, sampledAt time.Time) {
	if sampledAt.IsZero() && !previous.GPUSampledAt.IsZero() {
		next.GPU = previous.GPU
		next.GPUSampledAt = previous.GPUSampledAt
		next.GPUSampleSeq = previous.GPUSampleSeq
		return
	}
	next.GPU = sampled
	next.GPUSampledAt = sampledAt
	if !sampledAt.IsZero() {
		next.GPUSampleSeq = previous.GPUSampleSeq + 1
	}
}

// parseEngineInstance pulls the adapter-LUID key and the engine-type tag
// out of a PDH "GPU Engine" counter instance name, which looks like:
//
//	pid_1234_luid_0x00000000_0x000054f0_phys_0_eng_0_engtype_3D
//
// We need the LUID portion to join against luidKey() (which is how the
// DXGI-enumerated adapters identify themselves) and the engine-type to
// bucket utilization before the across-engine max. The returned luidKey
// is lowercase to match the normalization the VRAM counter reader does
// on its side, so both counter streams key into the same adapter map.
//
// Lives in a platform-neutral file (no build tag) so it's unit-testable
// on any host — the string format is stable across Windows versions and
// doesn't touch the PDH API itself.
func parseEngineInstance(s string) (luidKey, engtype string, ok bool) {
	s = strings.ToLower(s)
	luidIdx := strings.Index(s, "luid_")
	if luidIdx < 0 {
		return "", "", false
	}
	rest := s[luidIdx:]
	engIdx := strings.Index(rest, "_eng_")
	if engIdx < 0 {
		return "", "", false
	}
	luidKey = rest[:engIdx]
	const typeMarker = "_engtype_"
	typeIdx := strings.Index(rest, typeMarker)
	if typeIdx < 0 {
		return "", "", false
	}
	engtype = rest[typeIdx+len(typeMarker):]
	if engtype == "" {
		return "", "", false
	}
	return luidKey, engtype, true
}

// aggregateUtilization implements Task Manager's "overall GPU %" calc:
//
//  1. Sum per-process percentages within each (luid, engine-type) bucket.
//     Two apps each running the 3D engine at 30 % really is 60 % engine
//     load; the PDH counter is per-process-time, so summing is correct.
//  2. Clamp each bucket to 100. Sampling jitter can occasionally push a
//     bucket slightly over, and we don't want to surface >100 % to users.
//  3. Take the max across engine types for each luid. The number users
//     recognize as "the GPU is busy" is the busiest engine on that adapter,
//     not the average — a compute-bound ML job with an idle 3D engine is
//     still a fully-busy GPU.
//
// Result is rounded to whole percent. Sub-percent precision is below the
// visual resolution of any reasonable UI and would only generate noisy
// node/updated events from the change-detection plumbing downstream.
func aggregateUtilization(items map[string]float64) map[string]uint32 {
	byEngine := map[string]map[string]float64{}
	for instance, v := range items {
		luid, engtype, ok := parseEngineInstance(instance)
		if !ok || v < 0 {
			continue
		}
		m := byEngine[luid]
		if m == nil {
			m = map[string]float64{}
			byEngine[luid] = m
		}
		m[engtype] += v
	}
	out := make(map[string]uint32, len(byEngine))
	for luid, m := range byEngine {
		var maxPct float64
		for _, p := range m {
			if p > 100 {
				p = 100
			}
			if p > maxPct {
				maxPct = p
			}
		}
		out[luid] = uint32(math.Round(maxPct))
	}
	return out
}
