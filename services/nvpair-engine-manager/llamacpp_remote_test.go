// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLlamaRemoteDownloadAndDeleteVerifiedPeer(t *testing.T) {
	ex, _ := llamaDownloadExecutor(t, nil, nil)
	srv, mesh := newToolControlPeer(t, ex, true)
	params, _ := json.Marshal(map[string]string{"node": "host-server", "engine": "llamacpp", "model": llamaTestID, "opId": "llama-remote"})
	response := callToolRemote(t, srv, mesh, "engine:remote-pull-model", string(params))
	if response.Error != nil || !strings.Contains(string(response.Result), "sha256") {
		t.Fatalf("remote pull: %+v", response)
	}
	response = callToolRemote(t, srv, mesh, "engine:remote-delete-model", string(params))
	if response.Error != nil || !strings.Contains(string(response.Result), `"ok":true`) {
		t.Fatalf("remote delete: %+v", response)
	}
}
