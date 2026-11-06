// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

const localNodeInfoStartupTimeout = 15 * time.Second

// fetchLocalNodeInfo gives only the first local inventory read a readiness
// grace. The broker spawns scanner before node-info's listener is ready. Reuse
// the existing telemetry backoff, bounded across requests and waits, so normal
// startup is quiet but a real outage still reaches noteNodeInfo's warning.
// Later outages retain the immediate transition report and last-good cache.
func (d *daemon) fetchLocalNodeInfo(host string, port int, expectedUUID string) (NodeInfoResponse, bool) {
	var info NodeInfoResponse
	var ok, initial bool
	d.localInfoStartup.Do(func() {
		initial = true
		if d.http == nil && d.tlsHTTP == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), localNodeInfoStartupTimeout)
		defer cancel()
		for attempt := 0; ctx.Err() == nil; attempt++ {
			info, ok = d.fetchNodeInfoWithin(ctx, host, port)
			if ok && (info.HostUUID == "" || info.HostUUID == expectedUUID) {
				return
			}
			ok = false
			timer := time.NewTimer(telemetryRetryDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
	if initial {
		return info, ok
	}
	return d.fetchNodeInfo(host, port)
}
