// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Real isolated workers: corrupt A's old address for B, let B's pinned inbound
// roster connection repair it, restart A, then prove A reaches B by delivering
// authenticated removal. No multicast prerequisite or environment skip.
func TestPeerRoamingLearnsInboundAndDialsAfterRestart(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	portA, portB := freePort(t), freePort(t)
	a, b := startCM(t, dirA, portA), startCM(t, dirB, portB)
	defer func() {
		if a != nil {
			a.stop()
		}
		if b != nil {
			b.stop()
		}
	}()
	waitForPort(t, "127.0.0.1", portA, 5*time.Second)
	waitForPort(t, "127.0.0.1", portB, 5*time.Second)
	pairNodes(t, a, b, portB, "peer-b")
	bid := decodeResult[cmNodeID](t, b.call("cluster:get-node-id", nil)).NodeUUID
	a.stop()
	a = nil
	b.stop()
	b = nil
	type diskMember struct {
		NodeUUID          string `json:"nodeUuid"`
		IPAddress         string `json:"ipAddress"`
		PairingAddress    string `json:"pairingAddress"`
		AddressCandidates []struct {
			Address string `json:"address"`
		} `json:"addressCandidates"`
	}
	path := filepath.Join(dirA, "cluster", "members.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	stale := "192.0.2.20:" + strconv.Itoa(portB)
	for _, member := range raw {
		var uuid string
		_ = json.Unmarshal(member["nodeUuid"], &uuid)
		if uuid == bid {
			member["ipAddress"], _ = json.Marshal("192.0.2.20")
			member["pairingAddress"], _ = json.Marshal(stale)
			delete(member, "addressCandidates")
		}
	}
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a = startCM(t, dirA, portA)
	b = startCM(t, dirB, portB)
	reached := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var members []diskMember
		if err := json.Unmarshal(data, &members); err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			if member.NodeUUID != bid || member.IPAddress != "127.0.0.1" {
				continue
			}
			if member.PairingAddress != stale || len(member.AddressCandidates) == 0 {
				t.Fatalf("repair lost pairing fallback or durable learned source: %+v", member)
			}
			reached = true
		}
		if reached {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.stop()
	a = nil
	if !reached {
		t.Fatal("B's authenticated inbound connection never repaired A's stale member address")
	}
	a = startCM(t, dirA, portA)
	waitForPort(t, "127.0.0.1", portA, 5*time.Second)
	a.call("nodes:remove", map[string]string{"nodeUuid": bid})
	waitMembers(t, b, 0)
}
