// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
)

// Reserve the generated download namespace: an unrecorded or rejected commit
// must not reappear as a manually supplied model in inventory or launch.
func llamaManagedFilename(name string) bool {
	return strings.HasSuffix(name, ".gguf") && llamaSHA.MatchString(strings.TrimSuffix(name, ".gguf"))
}

// Compare the actual launch descriptor, not a pathname checked earlier. No
// re-hash of a potentially 64 GiB file is needed for post-commit replacements.
func validateLlamaRecordedFile(dir *os.File, name string, file *os.File) error {
	manifest, err := readLlamaManifest(dir)
	if err != nil {
		return err
	}
	for _, m := range manifest.Models {
		if m.File != name {
			continue
		}
		info, err := file.Stat()
		if err != nil || info.Size() != m.Size || llamaCacheIdentity(file) != m.Identity {
			return fmt.Errorf("recorded model changed; refusing launch; delete the changed entry manually and download again")
		}
		return nil
	}
	if llamaManagedFilename(name) {
		return fmt.Errorf("model is not a verified PAIR-recorded download; refusing launch")
	}
	return nil
}
