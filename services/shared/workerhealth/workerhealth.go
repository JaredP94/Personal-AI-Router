// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package workerhealth defines the shared worker control-loop health contract.
package workerhealth

const Method = "worker/health"

type Result struct {
	OK bool `json:"ok"`
}

func OK() Result { return Result{OK: true} }
