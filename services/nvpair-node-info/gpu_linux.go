// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"log"
	"os/exec"
	"time"

	"github.com/jaypipes/ghw"
)

// nvidiaSmiTimeout caps how long we wait for a single nvidia-smi invocation.
// The tool is normally well under 100 ms, but on a wedged driver it can hang;
// a hard ceiling keeps both the one-shot startup detect and the per-tick stats
// collector from blocking indefinitely.
const nvidiaSmiTimeout = 3 * time.Second

// detectGPUs enumerates GPUs on Linux. It prefers nvidia-smi, which yields the
// marketing name, total VRAM, and a stable per-GPU UUID we reuse as the join
// key (statsKey) against the dynamic stats collector's snapshot. When
// nvidia-smi is absent — no NVIDIA driver, or an AMD/Intel-only host — it falls
// back to ghw, which reports adapter names but no VRAM and no join key, so
// those hosts list their GPUs without dynamic VRAM/utilization (matching the
// pre-existing non-Windows behavior).
//
// On unified-memory architectures (UMA, e.g. Grace-Blackwell / DGX Spark)
// nvidia-smi reports [N/A] for memory.total because the GPU shares system
// DRAM; in that case VRAMBytes is filled from detectMemoryTotal() instead.
func detectGPUs() []GPUInfo {
	if out, err := nvidiaSmiCSV("uuid,name,memory.total"); err == nil {
		if gpus, uma := parseNvidiaStatic(out); len(gpus) > 0 {
			if uma {
				if total := detectMemoryTotal(); total > 0 {
					for i := range gpus {
						if gpus[i].usesSystemMemoryUsage {
							gpus[i].VRAMBytes = byteCapacity(total)
						}
					}
				}
			}
			return gpus
		}
	}
	return detectGPUsGHW()
}

// detectGPUsGHW is the ghw-based fallback, identical in spirit to the
// non-Windows/non-Linux path in gpu_other.go: enumerate display adapters and
// return names only (memory is unavailable, statsKey stays empty).
func detectGPUsGHW() []GPUInfo {
	gpu, err := ghw.GPU()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	var gpus []GPUInfo
	for _, card := range gpu.GraphicsCards {
		name := "Unknown"
		if card.DeviceInfo != nil && card.DeviceInfo.Product != nil {
			name = card.DeviceInfo.Product.Name
		}
		gpus = append(gpus, namesOnlyGPU(name))
	}
	return gpus
}

// nvidiaSmiCSV runs `nvidia-smi --query-gpu=<fields> --format=csv,noheader,nounits`
// and returns raw stdout. The caller parses the comma-separated rows. A missing
// binary (not on PATH) surfaces as an exec error, which callers treat as "no
// NVIDIA GPU data available" and degrade silently.
func nvidiaSmiCSV(fields string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiTimeout)
	defer cancel()
	return nvidiaSmiCSVContext(ctx, fields)
}

func nvidiaSmiCSVContext(ctx context.Context, fields string) (string, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+fields,
		"--format=csv,noheader,nounits").Output()
	return string(out), err
}
