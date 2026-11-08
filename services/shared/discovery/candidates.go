// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
)

// AdvertisementKey identifies an endpoint advertisement, not the principal it
// claims. PAIR's ip=/ips= describe the same endpoint set on every interface;
// prefer those to interface-specific A/AAAA subsets. Without them, use resolved
// addresses. Other TXT claims are part of the record variant: changing a
// principal or service-port hint must not overwrite a concurrently valid one.
func AdvertisementKey(n Node) string {
	var addresses, record []string
	for _, txt := range n.TXT {
		if ip, ok := strings.CutPrefix(txt, "ip="); ok {
			if ip != "" {
				addresses = append(addresses, ip)
			}
			continue
		}
		if ips, ok := strings.CutPrefix(txt, "ips="); ok {
			addresses = append(addresses, strings.Split(ips, ",")...)
			continue
		}
		record = append(record, txt)
	}
	if len(addresses) == 0 {
		addresses = append(addresses, n.Addresses...)
	}
	slices.Sort(addresses)
	addresses = slices.Compact(addresses)
	slices.Sort(record)
	return fmt.Sprintf("%s\x00%s\x00%d\x00%q\x00%q", strings.ToLower(n.ID), strings.ToLower(n.Host), n.Port, addresses, record)
}

// AdvertiserKey groups revisions from one mDNS responder by normalized instance
// and SRV host. It is an unauthenticated continuity hint, never a peer identity.
// Distinct hosts using the same instance remain independent advertisers.
func AdvertiserKey(n Node) string {
	return strings.ToLower(strings.TrimSuffix(n.ID, ".")) + "\x00" + strings.ToLower(strings.TrimSuffix(n.Host, "."))
}

// WithCandidateLimits bounds both a scan and the retained browser map. The
// optional preference must be a cheap, concurrency-safe lookup of previously
// authenticated endpoints, never a test of advertised identity. Excess unproven
// candidates are sampled with a fresh rank each scan, so a fixed flood cannot
// permanently occupy all admission slots. No unauthenticated discovery scheme
// can guarantee first discovery against an unlimited active LAN flood.
func WithCandidateLimits(perClaim, total int, preferred func(Node) bool) Option {
	return func(o *options) { o.perClaimLimit, o.candidateLimit, o.preferred = perClaim, total, preferred }
}

func (b *Browser) candidateRank(key string, n Node) (bool, uint64) {
	preferred := b.opt.preferred != nil && b.opt.preferred(n)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", b.candidateSalt, key)))
	return preferred, binary.BigEndian.Uint64(sum[:8])
}

// observeCandidate merges interface-specific resolved addresses within a scan.
// The advertised ip=/ips= endpoint set is already part of the key. Do not carry
// stale resolved addresses across reconciliation windows.
func (b *Browser) observeCandidate(set map[string]Node, n Node) bool {
	if prior, ok := set[b.key(n)]; ok {
		// A copied complete record with bogus resolved addresses cannot replace
		// a previously proven answering address. This is preference, not proof.
		if b.opt.preferred != nil && b.opt.preferred(prior) && !b.opt.preferred(n) {
			return true
		}
		for _, address := range prior.Addresses {
			if len(n.Addresses) >= 16 {
				break
			}
			if !slices.Contains(n.Addresses, address) {
				n.Addresses = append(n.Addresses, address)
			}
		}
	}
	return b.addCandidate(set, n)
}

// addCandidate keeps the best bounded sample without an unbounded side map of
// rejected names. Existing-key updates never consume another slot.
func (b *Browser) addCandidate(set map[string]Node, n Node) bool {
	key := b.key(n)
	if b.opt.perClaimLimit <= 0 && b.opt.candidateLimit <= 0 {
		set[key] = n
		return true
	}
	if old, ok := set[key]; ok {
		if b.opt.preferred != nil && b.opt.preferred(old) && !b.opt.preferred(n) {
			return true
		}
		if uuidFromTXT(old.TXT) == uuidFromTXT(n.TXT) {
			set[key] = n
			return true
		}
		delete(set, key)
	}
	claim := uuidFromTXT(n.TXT)
	count := 0
	for _, other := range set {
		if uuidFromTXT(other.TXT) == claim {
			count++
		}
	}
	sameClaimOnly := b.opt.perClaimLimit > 0 && count >= b.opt.perClaimLimit
	full := b.opt.candidateLimit > 0 && len(set) >= b.opt.candidateLimit
	if !sameClaimOnly && !full {
		set[key] = n
		return true
	}
	preferred, rank := b.candidateRank(key, n)
	worstKey := ""
	worstPreferred, worstRank := preferred, rank
	for k, other := range set {
		if sameClaimOnly && uuidFromTXT(other.TXT) != claim {
			continue
		}
		p, r := b.candidateRank(k, other)
		if (worstPreferred && !p) || (p == worstPreferred && r > worstRank) {
			worstKey, worstPreferred, worstRank = k, p, r
		}
	}
	if worstKey == "" {
		return false
	}
	delete(set, worstKey)
	set[key] = n
	return true
}
