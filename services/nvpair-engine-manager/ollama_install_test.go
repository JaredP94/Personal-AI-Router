// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestBundledOllamaDownloadsAreReleasePinned(t *testing.T) {
	m := bundledManifestSet(t)["ollama"]
	// Published v0.40.15/sha256sum.txt; independently downloaded and hashed.
	checksums := map[string]string{
		"Ollama-darwin.zip":          "de3298b9897828480c361c101e3efd2d3c499d08882c8ee972df524064abc3db",
		"ollama-linux-amd64.tar.zst": "628c018404a4206493efd38e46251c154ede5141b8b8dc85c659b3b027c47379",
		"ollama-linux-arm64.tar.zst": "7f699ead5cdb4ae1ed89b888b148a238af188708800871e1889ab9dcee95f247",
		"ollama-windows-amd64.zip":   "9ca74a7ab66ec86870b1eecac7df160df08dc4e3a291ab5d2d59bcf9310221c6",
		"ollama-windows-arm64.zip":   "35ce5195b57496dbc9ecbaa6593807e6ac26cbb53cf1f8bb57ac58daafa58574",
	}
	for platform, p := range m.Platforms {
		t.Run(platform, func(t *testing.T) {
			f := p.Install.Fetch
			const prefix = "https://github.com/ollama/ollama/releases/download/v0.40.15/"
			if f == nil || !strings.HasPrefix(f.URL, prefix) {
				t.Fatalf("download is not release-pinned: %+v", f)
			}
			artifact := strings.TrimPrefix(f.URL, prefix)
			expected, ok := checksums[artifact]
			if !ok || f.SHA256 != expected {
				t.Fatalf("checksum for %s = %q; want published %q", artifact, f.SHA256, expected)
			}
			sum, err := hex.DecodeString(f.SHA256)
			if err != nil || len(sum) != 32 {
				t.Fatalf("invalid SHA-256: %q", f.SHA256)
			}
		})
	}
}
