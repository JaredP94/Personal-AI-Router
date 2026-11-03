// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
	"testing"
)

func TestPeerMetadataAcceptsOnlyOwnedMatchingIdentity(t *testing.T) {
	metadata := &peerMetadata{}
	for _, body := range []string{`{`, `{"hostUuid":"other","services":{"ol":1}}`} {
		metadata.handle(applog.StdinMessage{Method: noderec.MethodSetPeerMetadata, Params: json.RawMessage(body)}, "self")
	}
	if metadata.get().HostUUID != "" {
		t.Fatal("malformed or mismatched metadata accepted")
	}
	data, _ := json.Marshal(noderec.PeerMetadataParams{HostUUID: "self", Name: "test-host", Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 14318, noderec.ServiceOMLX: 1236}})
	metadata.handle(applog.StdinMessage{Method: noderec.MethodSetPeerMetadata, Params: data}, "self")
	for _, verified := range []bool{false, true} {
		var response NodeInfoResponse
		if err := json.Unmarshal(metadata.decorate([]byte(`{"hostUuid":"self","GPUs":[]}`), "self", verified), &response); err != nil {
			t.Fatal(err)
		}
		if response.PeerMetadata == nil || response.PeerMetadata.Services[noderec.ServiceOMLX] != 1236 {
			t.Fatalf("broker metadata lost: %+v", response)
		}
		want := "unverified"
		if verified {
			want = "verified"
		}
		if response.IdentityVerification != want {
			t.Fatalf("response provenance=%q want %q", response.IdentityVerification, want)
		}
	}
}

func TestPublicHardwarePreviewRemainsAvailableAfterPairing(t *testing.T) {
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "test-cluster", "self", "peer")
	handler := nodeInfoHandler(clustertrust.Open(dir), func(*http.Request) []byte {
		return []byte(`{"GPUs":[{"name":"preview"}],"identityVerification":"unverified"}`)
	})
	req := httptest.NewRequest(http.MethodGet, "http://node/v1/node-info", nil)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("public preview status=%d", out.Code)
	}
	var response NodeInfoResponse
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.IdentityVerification != "unverified" || len(response.GPUs) != 1 {
		t.Fatalf("preview=%+v", response)
	}
}
