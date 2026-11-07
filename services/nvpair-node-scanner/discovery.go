// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The shared mDNS browser owns scan/diff, order-insensitive comparison and
// per-record miss eviction. These aliases keep the scanner's local names;
// advertisement identity, admission and authentication budgets are configured
// in candidates.go. UUID claims are never browser identity keys.

import "nvpair-shared/discovery"

type (
	RawNode        = discovery.Node
	DiscoveryEvent = discovery.Event
	Discovery      = discovery.Browser
)

// NewDiscovery builds the node browser with the shared defaults.
var NewDiscovery = discovery.New

// WithLivenessProbe re-exports the shared option so the daemon can wire its
// TCP-probe-before-evict anti-flap guard — the guard the proxies
// used to run themselves before discovery was consolidated onto the daemon.
var WithLivenessProbe = discovery.WithLivenessProbe

// WithKeyFunc re-exports the shared option for advertisement endpoint keys.
// A claimed UUID is not an authenticated identity and cannot be the browser key.
var WithKeyFunc = discovery.WithKeyFunc

// UUIDFromTXT re-exports the shared claimed-UUID extractor.
var UUIDFromTXT = discovery.UUIDFromTXT

// sameStringSet re-exports the shared order-insensitive multiset compare so the
// daemon's model-inventory change detection (directory.applyModels / sameByEngine)
// uses one implementation rather than a local copy.
var sameStringSet = discovery.SameStringSet
