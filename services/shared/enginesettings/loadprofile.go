// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginesettings

import "time"

// Persistence-only operations are nested inside successively larger transport
// budgets. They never stop, restart or load an engine.
const LoadProfileBudget = 5 * time.Second
const LoadProfileRemoteBudget = 10 * time.Second
const LoadProfileRelayBudget = 15 * time.Second

// LM Studio model mutation may reload remaining models after delete/restart.
const LMStudioModelBudget = 6 * time.Minute
const LMStudioModelRemoteBudget = 7 * time.Minute
const LMStudioModelRelayBudget = 8 * time.Minute

type LoadProfileSnapshot struct {
	NodeID   string `json:"nodeId"`
	Engine   string `json:"engine"`
	Model    string `json:"model"`
	Revision uint64 `json:"revision"`
	LoadText string `json:"loadText"`
	Format   string `json:"format"`
	Editable bool   `json:"editable"`
	Reason   string `json:"reason,omitempty"`
}

type LoadProfilePreview struct {
	Revision uint64            `json:"revision"`
	LoadText string            `json:"loadText"`
	Format   string            `json:"format"`
	Args     []string          `json:"load_args"`
	Errors   map[string]string `json:"errors"`
}
