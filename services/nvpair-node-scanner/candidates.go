// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"time"

	"nvpair-shared/discovery"
	"nvpair-shared/noderec"
)

const (
	candidatePerClaimLimit   = 8
	candidateTotalLimit      = 256
	candidateGlobalAttempts  = 64
	candidateAttemptInterval = 15 * time.Second
)

func candidateRecordKey(raw RawNode) string {
	return noderec.PreviewRecordID(discovery.AdvertisementKey(raw), "")
}

// Production browse lists are already capped at four by netpick.Candidates.
// Defensively bound larger caller-supplied lists too, preferring a proven
// endpoint and otherwise rotating batches; this cannot recover addresses that
// netpick excluded from the production list.
func (d *daemon) candidateHosts(node noderec.DirectoryNode, hosts []string, now time.Time) []string {
	if len(hosts) <= 4 {
		return hosts
	}
	if d.knownCandidate(node) {
		peer, ok := d.dir.get(node.HostUUID)
		if ok && slices.Contains(hosts, peer.IP) {
			return []string{peer.IP}
		}
	}
	batches := (len(hosts) + 3) / 4
	start := int(now.Unix()/int64(candidateAttemptInterval/time.Second)%int64(batches)) * 4
	return hosts[start:min(start+4, len(hosts))]
}

type candidateAttemptWindow struct {
	start    time.Time
	attempts int
	proven   bool
}

// knownCandidate is only an admission preference. It confers no trust: a fresh
// pinned-mTLS read is still required to publish any new inventory.
func (d *daemon) knownCandidate(n noderec.DirectoryNode) bool {
	if d.dir == nil {
		return false
	}
	peer, ok := d.dir.get(n.HostUUID)
	return ok && peer.ServicePeer() && n.ClusterUUID == peer.ClusterUUID && slices.Contains(n.CandidateIPs(), peer.IP) &&
		peer.Services[noderec.ServiceNodeInfo].Port == n.Services[noderec.ServiceNodeInfo].Port
}

func (d *daemon) newCandidateBrowser() *Discovery {
	return NewDiscovery(noderec.ServiceType, noderec.Domain,
		WithLivenessProbe(func(n RawNode) bool { return d.reachable(n) }),
		WithKeyFunc(discovery.AdvertisementKey),
		discovery.WithCandidateLimits(candidatePerClaimLimit, candidateTotalLimit, func(raw RawNode) bool {
			if UUIDFromTXT(raw.TXT) == "" {
				return false
			}
			n, ok := toDirectoryNode(raw)
			return ok && d.knownCandidate(n)
		}),
	)
}

// rememberCandidate separately bounds the scanner, including direct browse
// events and candidates retained across scans. A proven endpoint can displace
// an unproven one, never the reverse. Revisions from the same normalized
// instance/SRV host retire pending candidates and previews, never a verified
// HostUUID record. Removed previews are withdrawn outside the candidate lock.
func (d *daemon) rememberCandidate(node noderec.DirectoryNode, advertiser string) bool {
	key := node.RecordKey()
	d.candidateMu.Lock()
	if d.candidates == nil {
		d.candidates = make(map[string]noderec.DirectoryNode)
	}
	if d.candidateAdvertisers == nil {
		d.candidateAdvertisers = make(map[string]string)
	}
	var retired []string
	for k, source := range d.candidateAdvertisers {
		if k != key && advertiser != "" && source == advertiser {
			delete(d.candidates, k)
			delete(d.candidateAdvertisers, k)
			retired = append(retired, k)
		}
	}
	_, exists := d.candidates[key]
	count := 0
	for k, n := range d.candidates {
		if k != key && n.HostUUID == node.HostUUID {
			count++
		}
	}
	droppedKey := ""
	admitted := true
	if count >= candidatePerClaimLimit || (!exists && len(d.candidates) >= candidateTotalLimit) {
		admitted = false
		if d.knownCandidate(node) {
			for k, n := range d.candidates {
				if k == key || (count >= candidatePerClaimLimit && n.HostUUID != node.HostUUID) {
					continue
				}
				if !d.knownCandidate(n) {
					droppedKey = k
					admitted = true
					break
				}
			}
		}
		// A changed UUID claim must not bypass the per-claim bound through an
		// already admitted advertisement key, or leave the old preview orphaned.
		if !admitted && exists {
			droppedKey = key
		}
	}
	if droppedKey != "" {
		delete(d.candidates, droppedKey)
		delete(d.candidateAdvertisers, droppedKey)
		retired = append(retired, droppedKey)
	}
	if admitted {
		d.candidates[key] = node
		d.candidateAdvertisers[key] = advertiser
	}
	d.candidateMu.Unlock()
	for _, retiredKey := range retired {
		if prior, ok := d.dir.get(retiredKey); ok {
			d.dir.remove(retiredKey)
			d.emit(noderec.NotifyNodeRemoved, prior)
		}
	}
	return admitted
}

// One UUID cannot buy unlimited work by changing names, TXT or endpoints. The
// bounded window table survives candidate removal; churning names cannot reset
// the budget. A previously proven endpoint has a reserved attempt, independent
// of the eight unproven attempts. Verified-directory refresh/telemetry retain
// their existing independent cadence and cannot be starved by previews.
func (d *daemon) claimCandidateAttempt(node noderec.DirectoryNode, now time.Time) bool {
	known := d.knownCandidate(node)
	d.candidateMu.Lock()
	defer d.candidateMu.Unlock()
	if now.Sub(d.candidateAttemptStart) >= candidateAttemptInterval {
		d.candidateAttemptStart = now
		d.candidateAttemptCount = 0
	}
	if d.candidateAttempts == nil {
		d.candidateAttempts = make(map[string]candidateAttemptWindow)
	}
	for claim, w := range d.candidateAttempts {
		if now.Sub(w.start) >= candidateAttemptInterval {
			delete(d.candidateAttempts, claim)
		}
	}
	w, exists := d.candidateAttempts[node.HostUUID]
	if !exists {
		if len(d.candidateAttempts) >= candidateTotalLimit {
			return false
		}
		w.start = now
	}
	if known {
		if w.proven {
			return false
		}
		w.proven = true
	} else {
		if w.attempts >= candidatePerClaimLimit || d.candidateAttemptCount >= candidateGlobalAttempts {
			return false
		}
		w.attempts++
		d.candidateAttemptCount++
	}
	d.candidateAttempts[node.HostUUID] = w
	return true
}
