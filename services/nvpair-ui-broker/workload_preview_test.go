// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/noderec"
)

func TestPreviewWithdrawalRetiresAlreadyTrackedWorkloads(t *testing.T) {
	for _, verifiedSurvives := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview-only", true: "verified-survives"}[verifiedSurvives], func(t *testing.T) {
			b := inventoryTestBroker(t)
			preview := noderec.DirectoryNode{HostUUID: "peer", Name: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}
			b.store.Upsert(directoryToEnriched(preview), sourceScanner)
			if verifiedSurvives {
				b.store.Upsert(directoryToEnriched(verifiedDirectoryForTest(b)), sourceManual)
			}
			// Seed a previously accepted record: the withdrawal must clean up
			// even state admitted before preview admission was guarded.
			b.workloads.Apply(storeIncoming("7", "peer", "ollama", "r1", "running", "peer"))
			sp := &scannerProcess{store: b.store, authorize: b.authorizedDirectoryNode, onNodeLost: func(uuid, name string) {
				if !b.hasDecisionInventory(uuid) {
					b.failWorkloadsForNode(uuid, name)
				}
			}}
			params, err := json.Marshal(noderec.NodeEvent{Node: preview})
			if err != nil {
				t.Fatal(err)
			}
			sp.handleNotify(noderec.NotifyNodeRemoved, params)
			want := "failed"
			if verifiedSurvives {
				want = "running"
			}
			if got, ok := b.workloads.Get("peer", "7"); !ok || got.State != want {
				t.Fatalf("workload after preview withdrawal = %+v, want %s", got, want)
			}
		})
	}
}

func TestScannerRetirementRetiresPreviewWorkloads(t *testing.T) {
	b := inventoryTestBroker(t)
	b.store.Upsert(EnrichedNode{HostUUID: "peer", ID: "preview", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("preview", "peer")}, sourceScanner)
	b.workloads.Apply(storeIncoming("7", "peer", "ollama", "r1", "running", "peer"))
	b.clearScannerInventory()
	if got, ok := b.workloads.Get("peer", "7"); !ok || got.State != "failed" {
		t.Fatalf("workload after scanner retirement = %+v, want failed", got)
	}
}

func TestActiveWorkloadsCannotUseUnverifiedPreview(t *testing.T) {
	for _, field := range []string{"origin", "executor", "preview-key"} {
		t.Run(field, func(t *testing.T) {
			b := inventoryTestBroker(t)
			preview := noderec.DirectoryNode{HostUUID: "peer", Name: "unproven", IdentityVerification: "unverified", PeerRecordID: noderec.PreviewRecordID("unproven", "peer")}
			b.store.Upsert(directoryToEnriched(preview), sourceScanner)
			origin, executor := "self", "peer"
			if field == "origin" {
				origin, executor = "peer", "self"
			} else if field == "preview-key" {
				executor = preview.RecordKey()
			}
			incoming := storeIncoming("7", origin, "ollama", "r1", "running", executor)
			params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": incoming.Info})
			if err != nil {
				t.Fatal(err)
			}
			b.forwardWorkloadManagerNotification("workloads:upsert", params)
			if _, ok := b.workloads.Get(origin, "7"); ok {
				t.Fatal("active workload was tracked against an unverified preview")
			}
			// A proven record sharing the claimed UUID must not be shadowed
			// by an unrelated preview. The preview key itself stays forbidden.
			b.store.Upsert(directoryToEnriched(verifiedDirectoryForTest(b)), sourceManual)
			b.forwardWorkloadManagerNotification("workloads:upsert", params)
			_, accepted := b.workloads.Get(origin, "7")
			if accepted != (field != "preview-key") {
				t.Fatalf("verified admission = %v for %s", accepted, field)
			}
		})
	}
}
