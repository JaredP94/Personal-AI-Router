// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strconv"
	"strings"
	"time"
)

// MemoryKind identifies the memory pool backing one adapter, not an additive
// contribution to a node-wide total.
type MemoryKind string

const (
	MemoryDedicated   MemoryKind = "dedicated"
	MemoryUnified     MemoryKind = "unified"
	MemoryShared      MemoryKind = "shared"
	MemoryUnavailable MemoryKind = "unavailable"
)

// byteCapacity omits unknown capacity; collectors use zero when detection fails.
func byteCapacity(bytes uint64) *uint64 {
	if bytes == 0 {
		return nil
	}
	return &bytes
}

// namesOnlyGPU is used by inventory sources without GPU metric support.
func namesOnlyGPU(name string) GPUInfo {
	return GPUInfo{Name: name, MemoryKind: MemoryUnavailable}
}

// staticGPUFromDXGI keeps dedicated capacity separate from the shared-system
// budget. Only adapters with no dedicated video memory use the shared budget as
// their primary capacity. A successful DXGI descriptor supplies known values,
// including zero; DedicatedSystemMemory is deliberately not part of either pool.
func staticGPUFromDXGI(name, key string, dedicatedBytes, sharedBytes uint64) GPUInfo {
	gpu := GPUInfo{Name: name, statsKey: key}
	if dedicatedBytes > 0 {
		gpu.MemoryKind = MemoryDedicated
		gpu.VRAMBytes = &dedicatedBytes
		gpu.SharedBytes = &sharedBytes
	} else {
		gpu.MemoryKind = MemoryShared
		gpu.VRAMBytes = &sharedBytes
	}
	return gpu
}

// decodeWindowsGPUCounters joins independently available PDH counters by LUID.
// Keep the raw dedicated/shared readings separate here; response assembly selects
// the primary used-memory reading according to the DXGI classification.
func decodeWindowsGPUCounters(dedicated, shared map[string]int64, engines map[string]float64) (map[string]gpuStat, bool) {
	out := make(map[string]gpuStat)
	for name, value := range dedicated {
		key := strings.ToLower(name)
		if value < 0 || !strings.HasPrefix(key, "luid_") {
			continue
		}
		stat := out[key]
		stat.VRAMUsed = uint64(value)
		stat.VRAMUsedValid = true
		out[key] = stat
	}
	for name, value := range shared {
		key := strings.ToLower(name)
		if value < 0 || !strings.HasPrefix(key, "luid_") {
			continue
		}
		stat := out[key]
		stat.SharedUsed = uint64(value)
		stat.SharedUsedValid = true
		out[key] = stat
	}
	utilization := aggregateUtilization(engines)
	for key, pct := range utilization {
		stat := out[key]
		stat.UtilizationPct = pct
		stat.UtilizationValid = true
		out[key] = stat
	}
	return out, len(utilization) > 0
}

// isNvidiaSmiNA reports whether an nvidia-smi CSV field is a "not
// applicable" sentinel rather than a numeric value. UMA platforms such as
// DGX Spark return [N/A] or [Not Supported] for GPU memory queries.
func isNvidiaSmiNA(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	switch strings.ToLower(s) {
	case "n/a", "not supported":
		return true
	default:
		return false
	}
}

// parseNvidiaStatic decodes the static query (uuid,name,memory.total) into
// GPUInfo records. memory.total is reported in MiB (because of -nounits); we
// convert to bytes. Rows missing a uuid or name are skipped. A malformed total
// remains an adapter with unavailable memory. Each
// unified-memory row is marked so response assembly can source its used bytes
// from system memory without depending on dynamic nvidia-smi collection. The
// second return value reports whether any row was unified, allowing the caller
// to fetch the shared system-memory total only when needed.
func parseNvidiaStatic(out string) ([]GPUInfo, bool) {
	var gpus []GPUInfo
	var unifiedMemory bool
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 3 {
			continue
		}
		uuid, name := fields[0], fields[1]
		if uuid == "" || name == "" {
			continue
		}
		var vramBytes *uint64
		kind := MemoryUnavailable
		usesUnifiedMemory := isNvidiaSmiNA(fields[2])
		if usesUnifiedMemory {
			unifiedMemory = true
			kind = MemoryUnified
		} else if mib, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			bytes := mib * 1024 * 1024
			vramBytes = &bytes
			kind = MemoryDedicated
		}
		gpus = append(gpus, GPUInfo{
			Name:                  name,
			MemoryKind:            kind,
			VRAMBytes:             vramBytes,
			statsKey:              uuid,
			usesSystemMemoryUsage: usesUnifiedMemory,
		})
	}
	return gpus, unifiedMemory
}

// splitCSVRow splits one nvidia-smi CSV row on commas and trims surrounding
// whitespace from each field (the tool emits ", " separators). Returns nil for
// a blank line so callers can skip it.
func splitCSVRow(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseNvidiaDynamic decodes the dynamic query (uuid,utilization.gpu,
// memory.used) into the per-GPU snapshot map keyed by UUID — the same statsKey
// gpu_linux.go stamped on each static GPUInfo. memory.used is in MiB
// (-nounits) and converted to bytes; utilization.gpu is an integer percent,
// clamped to 100. The returned sample count includes a parsed 0 % reading but
// excludes [N/A] and malformed utilization, so callers do not mistake
// memory-only rows for fresh utilization. Rows with a missing uuid are skipped.
// A memory.used value of [N/A] remains invalid here; buildResponse supplies
// system-memory usage for GPUs that static detection identified as
// unified-memory adapters.
func parseNvidiaDynamic(out string) (map[string]gpuStat, int) {
	res := map[string]gpuStat{}
	utilizationSamples := 0
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 3 {
			continue
		}
		uuid := fields[0]
		if uuid == "" {
			continue
		}
		var stat gpuStat
		if pct, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
			if pct > 100 {
				pct = 100
			}
			stat.UtilizationPct = uint32(pct)
			stat.UtilizationValid = true
			utilizationSamples++
		}
		if mib, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			stat.VRAMUsed = mib * 1024 * 1024
			stat.VRAMUsedValid = true
		}
		res[uuid] = stat
	}
	return res, utilizationSamples
}

type darwinGPUReading struct {
	inventory          []GPUInfo
	stats              map[string]gpuStat
	utilizationSamples int
	sampledAt          time.Time
	sampleSeq          uint64
}

func staticGPUsFromIORegistry(data []byte, systemMemory uint64) ([]GPUInfo, error) {
	records, err := parseIORegistryGPUs(data, systemMemory)
	if err != nil {
		return nil, err
	}
	gpus := make([]GPUInfo, 0, len(records))
	for _, record := range records {
		gpus = append(gpus, GPUInfo{
			Name:       record.name,
			MemoryKind: record.memoryKind,
			VRAMBytes:  byteCapacity(record.vramTotal),
			statsKey:   record.statsKey,
		})
	}
	return gpus, nil
}

func dynamicGPUReadingFromIORegistry(data []byte, systemMemory uint64) (darwinGPUReading, error) {
	records, err := parseIORegistryGPUs(data, systemMemory)
	if err != nil {
		return darwinGPUReading{}, err
	}
	reading := darwinGPUReading{
		inventory: make([]GPUInfo, 0, len(records)),
		stats:     make(map[string]gpuStat, len(records)),
	}
	for _, record := range records {
		reading.inventory = append(reading.inventory, GPUInfo{
			Name:       record.name,
			MemoryKind: record.memoryKind,
			VRAMBytes:  byteCapacity(record.vramTotal),
			statsKey:   record.statsKey,
		})
		if record.utilizationValid {
			reading.utilizationSamples++
		}
		reading.stats[record.statsKey] = gpuStat{
			VRAMUsed:         record.vramUsed,
			VRAMUsedValid:    record.vramUsedValid,
			UtilizationPct:   record.utilizationPct,
			UtilizationValid: record.utilizationValid,
		}
	}
	return reading, nil
}
