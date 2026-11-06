// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/netpick"
)

const (
	maxPeerAddresses   = 8 // includes the permanent pairing-time fallback
	peerAddressTTL     = 30 * 24 * time.Hour
	newAddressInterval = 30 * time.Second
)

// Address hints are local durable state, not membership authority and not part
// of ClusterNode's JSON-RPC projection. Only cluster-manager writes them.
type addressCandidate struct {
	Address     string `json:"address"`
	LearnedAt   int64  `json:"learnedAt"`
	LastSuccess int64  `json:"lastSuccess,omitempty"`
	LastAttempt int64  `json:"lastAttempt,omitempty"`
}

type memberAddresses struct {
	PairingAddress    string             `json:"pairingAddress,omitempty"`
	AddressCandidates []addressCandidate `json:"addressCandidates,omitempty"`
	NextAddressProbe  int64              `json:"nextAddressProbe,omitempty"`
}

type persistedMember struct {
	ClusterNode
	memberAddresses
}

// candidateEndpoint accepts only numeric unicast addresses, never a hostname
// whose subsequent resolution could redirect a learned hint. A zone is local
// routing state: accept it only on IPv6 link-local addresses. Self-reported
// zones are replaced with the receiving connection's zone before validation.
func candidateEndpoint(endpoint string, allowLoopback bool) (string, bool) {
	host, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", false
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() || (!ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback()) || (!allowLoopback && ip.IsLoopback()) {
		return "", false
	}
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 0 || b[0] >= 224 || ip.IsLinkLocalUnicast() {
			return "", false
		}
	}
	if ip.Is6() && ip.IsLinkLocalUnicast() && ip.Zone() == "" {
		return "", false
	}
	if zone := ip.Zone(); zone != "" {
		if !ip.Is6() || !ip.IsLinkLocalUnicast() || len(zone) > 64 || strings.ContainsAny(zone, "%/\\?#[]: \t\r\n") {
			return "", false
		}
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), true
}

func normalizeMemberAddresses(n *ClusterNode, now time.Time) {
	a := &n.addresses
	if a.PairingAddress == "" && len(a.AddressCandidates) == 0 {
		a.PairingAddress = joinHostPort(n.IPAddress, n.Port, n.Port)
	}
	if addr, ok := candidateEndpoint(a.PairingAddress, true); ok {
		a.PairingAddress = addr
	} else {
		a.PairingAddress = ""
	}
	seen := make(map[string]bool)
	kept := make([]addressCandidate, 0, maxPeerAddresses-1)
	for _, c := range a.AddressCandidates {
		addr, ok := candidateEndpoint(c.Address, true)
		if !ok || seen[addr] || max(c.LearnedAt, c.LastSuccess) < now.Add(-peerAddressTTL).UnixMilli() {
			continue
		}
		seen[addr] = true
		c.Address = addr
		// Disk state is also bounded and sanitized, including unreasonable clocks.
		c.LearnedAt = min(c.LearnedAt, now.UnixMilli())
		c.LastSuccess = min(c.LastSuccess, now.UnixMilli())
		kept = append(kept, c)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].LastSuccess > kept[j].LastSuccess })
	a.AddressCandidates = kept[:min(len(kept), maxPeerAddresses-1)]
	// A stale display/dial address must not bypass candidate expiry or the cap.
	current := joinHostPort(n.IPAddress, n.Port, n.Port)
	if current != a.PairingAddress && !slices.ContainsFunc(a.AddressCandidates, func(c addressCandidate) bool { return c.Address == current && c.LastSuccess > 0 }) {
		addr := a.PairingAddress
		if len(a.AddressCandidates) > 0 && a.AddressCandidates[0].LastSuccess > 0 {
			addr = a.AddressCandidates[0].Address
		}
		if addr != "" {
			n.IPAddress, n.Port = splitAddr(addr, n.Port)
		} else {
			n.IPAddress = ""
		}
	}
	a.NextAddressProbe = min(a.NextAddressProbe, now.Add(newAddressInterval).UnixMilli())
}

func memberCandidateAddrs(n ClusterNode, now time.Time) []string {
	normalizeMemberAddresses(&n, now)
	out := make([]string, 0, maxPeerAddresses)
	for _, c := range n.addresses.AddressCandidates {
		out = append(out, c.Address)
	}
	if n.addresses.PairingAddress != "" && !slices.Contains(out, n.addresses.PairingAddress) {
		out = append(out, n.addresses.PairingAddress)
	}
	return out
}

// learnPeerAddressesLocked is called only after rechecking the exact connection
// pin and admission under rosterMu. An authenticated sender can teach only its
// own member record. No removed member is created, and no mDNS/preview/gossip
// address reaches this function. observed carries the receiver's IPv6 zone.
func (m *Manager) learnPeerAddressesLocked(uuid, observed string, reports []string, successAddr string, now time.Time) bool {
	pin, pinned := m.trust.Get(uuid)
	if !pinned || m.teardownPending.Load() {
		return false
	}
	m.memMu.Lock()
	defer m.memMu.Unlock()
	n, ok := m.members[uuid]
	if !ok || n.State != stateMember || n.AdmissionEpoch != pin.AdmissionEpoch || n.ClusterID != pin.ClusterID {
		return false
	}
	normalizeMemberAddresses(n, now)
	before := *cloneClusterNode(n)
	add := func(addr string, success bool) {
		if addr == n.addresses.PairingAddress && !success {
			return
		}
		i := slices.IndexFunc(n.addresses.AddressCandidates, func(c addressCandidate) bool { return c.Address == addr })
		if i >= 0 {
			if !success {
				return
			} // a repeated claim never renews expiry or retry budget
			c := n.addresses.AddressCandidates[i]
			n.addresses.AddressCandidates = append(n.addresses.AddressCandidates[:i], n.addresses.AddressCandidates[i+1:]...)
			c.LastSuccess = now.UnixMilli()
			n.addresses.AddressCandidates = append([]addressCandidate{c}, n.addresses.AddressCandidates...)
		} else {
			c := addressCandidate{Address: addr, LearnedAt: now.UnixMilli()}
			if success {
				c.LastSuccess = now.UnixMilli()
				n.addresses.AddressCandidates = append([]addressCandidate{c}, n.addresses.AddressCandidates...)
			} else if len(n.addresses.AddressCandidates) < maxPeerAddresses-1 {
				n.addresses.AddressCandidates = append(n.addresses.AddressCandidates, c)
			}
		}
		n.addresses.AddressCandidates = n.addresses.AddressCandidates[:min(len(n.addresses.AddressCandidates), maxPeerAddresses-1)]
		if success {
			n.IPAddress, n.Port = splitAddr(addr, n.Port)
		}
	}
	if observed != "" {
		if addr, ok := candidateEndpoint(joinHostPort(observed, n.Port, m.port), true); ok {
			add(addr, true)
		}
	}
	if addr, ok := candidateEndpoint(successAddr, true); ok {
		add(addr, true)
	}
	// Bound processing as well as storage; a large body cannot churn the set.
	via := observed
	if via == "" {
		via = hostOnly(successAddr)
	}
	for _, addr := range reports[:min(len(reports), maxPeerAddresses)] {
		addr = localScope(addr, via)
		if addr, ok := candidateEndpoint(addr, false); ok {
			add(addr, false)
		}
	}
	return before.IPAddress != n.IPAddress || before.Port != n.Port || !slices.Equal(before.addresses.AddressCandidates, n.addresses.AddressCandidates)
}

// limitCandidateDials reserves at most one unproven endpoint per member per
// 30 seconds, shared by concurrent reconciles and persisted across restarts.
// A peer rotating its claims cannot reset this per-member budget. Previously
// authenticated sources/successes and the pairing fallback remain available.
func (m *Manager) limitCandidateDials(uuid string, addrs []string, now time.Time) ([]string, bool) {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	n, ok := m.members[uuid]
	if !ok {
		return nil, false
	}
	normalizeMemberAddresses(n, now)
	allowed := make(map[string]bool)
	for _, c := range n.addresses.AddressCandidates {
		if c.LastSuccess > 0 {
			allowed[c.Address] = true
		}
	}
	allowed[n.addresses.PairingAddress] = true
	var probe string
	if now.UnixMilli() >= n.addresses.NextAddressProbe {
		// Oldest attempted hint first prevents a dead first claim starving the rest.
		oldest := int64(1<<63 - 1)
		for _, c := range n.addresses.AddressCandidates {
			if c.LastSuccess == 0 && slices.Contains(addrs, c.Address) && c.LastAttempt < oldest {
				probe = c.Address
				oldest = c.LastAttempt
			}
		}
		if probe != "" {
			n.addresses.NextAddressProbe = now.Add(newAddressInterval).UnixMilli()
			for i := range n.addresses.AddressCandidates {
				if n.addresses.AddressCandidates[i].Address == probe {
					n.addresses.AddressCandidates[i].LastAttempt = now.UnixMilli()
				}
			}
			allowed[probe] = true
		}
	}
	out := make([]string, 0, maxPeerAddresses)
	for _, addr := range addrs {
		if allowed[addr] && !slices.Contains(out, addr) {
			out = append(out, addr)
		}
		if len(out) == maxPeerAddresses {
			break
		}
	}
	return out, probe != ""
}

// A reservation must reach disk before a new endpoint is dialed. Otherwise
// repeated restarts during a write failure could bypass the per-member budget.
func (m *Manager) reservePeerDialCandidates(uuid string, addrs []string) []string {
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	allowed, reserved := m.limitCandidateDials(uuid, addrs, time.Now())
	if reserved {
		if err := m.persistMembersErr(); err != nil {
			log.Printf("persist address dial budget: %v", err)
			// The in-memory reservation is retained. A second filter excludes
			// unproven hints while leaving proven endpoints/fallback available.
			allowed, _ = m.limitCandidateDials(uuid, allowed, time.Now())
		}
	}
	return allowed
}

func localRosterAddresses(port int) []string {
	var out []string
	// The shared publisher includes routed overlay addresses behind the LAN.
	for _, host := range netpick.LocalCandidates(netpick.Evidence{}) {
		if addr, ok := candidateEndpoint(net.JoinHostPort(host, strconv.Itoa(port)), false); ok {
			out = append(out, addr)
		}
		if len(out) == maxPeerAddresses {
			break
		}
	}
	return out
}
