// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestUnpairForgetsAllManualAddressesOnlyAfterSuccess(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not removed", true: "removed"}[removed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manual-nodes.json")
			entries := []persistedManualNode{{Kind: "pair", ID: "overlay", Address: "MagicDNS.example", PeerUUID: "peer"}, {Kind: "pair", ID: "wireguard", Address: "192.0.2.1", PeerUUID: "peer"}, {Kind: "engine", ID: "bare", Address: "bare.example"}}
			if err := writePersistedManualNodes(path, entries); err != nil {
				t.Fatal(err)
			}
			clientBroker, client := net.Pipe()
			defer clientBroker.Close()
			defer client.Close()
			clusterBroker, cluster := net.Pipe()
			defer clusterBroker.Close()
			defer cluster.Close()
			cm := &clusterManagerProcess{peer: NewPeer(NewCodec(clusterBroker))}
			go cm.peer.Serve(nil, nil)
			go func() {
				codec := NewCodec(cluster)
				msg, err := codec.Read()
				if err == nil {
					_ = codec.Respond(msg.ID, map[string]bool{"removed": removed})
				}
			}()
			b := &Broker{codec: NewCodec(clientBroker), manualNodesConfigPath: path}
			b.setClusterMgr(cm)
			id := json.RawMessage("1")
			go b.relayToClusterManager(&Message{JSONRPC: "2.0", ID: &id, Method: "nodes:remove", Params: json.RawMessage(`{"nodeUuid":"peer"}`)})
			response, err := NewCodec(client).Read()
			if err != nil || response.Error != nil {
				t.Fatalf("remove response: %+v, %v", response, err)
			}
			got, err := loadPersistedManualNodes(path)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if removed {
				want = 1
			}
			if len(got) != want {
				t.Fatalf("entries after removed=%v: %+v", removed, got)
			}
			if removed && got[0].Kind != "engine" {
				t.Fatalf("bare bypass entry removed by unpair: %+v", got)
			}
		})
	}
}
