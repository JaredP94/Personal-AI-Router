// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
)

const controlCancelPullPath = "/v1/models/cancel-pull"

type cancelPullRequest struct {
	Engine string `json:"engine"`
	Model  string `json:"model"`
	OpID   string `json:"opId"`
}

func (s *controlServer) handleCancelPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req cancelPullRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil || req.Engine == "" || req.Model == "" || req.OpID == "" {
		http.Error(w, "engine, model and opId are required", http.StatusBadRequest)
		return
	}
	accepted := s.exec.CancelModelPull(req.Engine, req.Model, req.OpID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": accepted})
}
