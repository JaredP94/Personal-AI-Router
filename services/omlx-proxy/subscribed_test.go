// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// TestSubscribedToNode covers the DirectoryNode -> routable Node projection for
// the om service, including per-engine model attribution: the proxy ranks on the
// node's oMLX models only, never the cross-engine union.
func TestSubscribedToNode(t *testing.T) {
	withOM := noderec.DirectoryNode{
		HostUUID: "uuid-a",
		Name:     "host-a",
		IP:       "10.0.0.5",
		Models:   []string{"mlx-model"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOMLX: {Port: 1234},
		},
	}
	got, ok := subscribedToNode(withOM)
	if !ok {
		t.Fatal("node with om + IP should project")
	}
	// No attribution -> fall back to the flat union (single-engine peer).
	if got.ID != "uuid-a" || got.Port != 1234 || got.IP != "10.0.0.5" ||
		len(got.Models) != 1 || got.Models[0] != "mlx-model" {
		t.Fatalf("unexpected projection: %+v", got)
	}

	noIP := withOM
	noIP.IP = ""
	if _, ok := subscribedToNode(noIP); ok {
		t.Fatal("node without IP should not project")
	}

	// A node advertising only a non-om service must not be an om routing target.
	olOnly := noderec.DirectoryNode{
		Name:     "host-b",
		IP:       "10.0.0.6",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
	if _, ok := subscribedToNode(olOnly); ok {
		t.Fatal("node without om should not project")
	}

	// Per-engine attribution: a dual-engine node projects ONLY its oMLX
	// models, never the union — so an Ollama-only model isn't ranked as an
	// oMLX owner.
	dual := noderec.DirectoryNode{
		HostUUID: "uuid-d",
		Name:     "host-d",
		IP:       "10.0.0.7",
		Models:   []string{"ollama-model", "omlx-model"},
		ModelsByEngine: map[string][]string{
			"ollama": {"ollama-model"},
			"omlx":   {"omlx-model"},
		},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOMLX: {Port: 1234},
		},
	}
	got, ok = subscribedToNode(dual)
	if !ok {
		t.Fatal("dual-engine node with om should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "omlx-model" {
		t.Fatalf("dual-engine projection Models = %v, want [omlx-model] only", got.Models)
	}
}

// TestSubscribedToNodeKeysByHostUUID: the routable Node keys on the stable
// hostUuid, not the hostname, so routing/scheduledOn/selection survive a PC
// rename and never conflate same-named machines. Host stays the hostname for
// display.
func TestSubscribedToNodeKeysByHostUUID(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	n := noderec.DirectoryNode{
		HostUUID: uuid,
		Name:     "host-a",
		IP:       "10.0.0.5",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOMLX: {Port: 1234}},
	}
	got, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("node with om + IP should project")
	}
	if got.ID != uuid {
		t.Fatalf("ID = %q, want hostUuid %q", got.ID, uuid)
	}
	if got.Host != "host-a" {
		t.Fatalf("Host = %q, want hostname for display", got.Host)
	}
}
