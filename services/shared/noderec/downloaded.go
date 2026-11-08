// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

// DownloadedModel describes a cache file, not a model currently served for routing.
// Owned means PAIR recorded and verified its file identity. Deletion still checks
// that identity again on the owning node.
type DownloadedModel struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Owned  bool   `json:"owned"`
}
