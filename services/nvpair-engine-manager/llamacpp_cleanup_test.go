// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLlamaDownloadCleanupLeavesSubstitutedTemp(t *testing.T) {
	for _, cancelPull := range []bool{false, true} {
		name := "failure"
		if cancelPull {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan error, 1)
			var cache, substituted string
			ex, path := llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				files, err := os.ReadDir(cache)
				if err != nil {
					started <- err
					w.WriteHeader(500)
					return
				}
				for _, f := range files {
					if strings.HasPrefix(f.Name(), ".pair-") && strings.HasSuffix(f.Name(), ".tmp") {
						substituted = filepath.Join(cache, f.Name())
						if err = os.Rename(substituted, substituted+".saved"); err == nil {
							err = os.WriteFile(substituted, []byte("unrelated"), 0600)
						}
						break
					}
				}
				if substituted == "" && err == nil {
					err = os.ErrNotExist
				}
				started <- err
				if cancelPull {
					<-r.Context().Done()
				} else {
					w.WriteHeader(500)
				}
			}, nil)
			cache = path
			events, unsub := ex.progress.subscribe("llamacpp")
			defer unsub()
			done := make(chan error, 1)
			go func() { done <- pullLlamaTest(ex, "cleanup-swap") }()
			select {
			case err := <-started:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("transfer not started")
			}
			if cancelPull && !ex.CancelModelPull("llamacpp", llamaTestID, "cleanup-swap") {
				t.Fatal("cancel refused")
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "cleanup skipped") {
					t.Errorf("substituted cleanup not reported: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("transfer did not settle")
			}
			data, err := os.ReadFile(substituted)
			if err != nil || string(data) != "unrelated" {
				t.Fatalf("substituted file changed: %q %v", data, err)
			}
			failed := false
			for len(events) > 0 {
				ev := <-events
				if ev.Stage == "cancelled" || ev.Stage == "done" {
					t.Fatalf("claimed success: %+v", ev)
				}
				if ev.Stage == "error" {
					failed = true
				}
			}
			if !failed {
				t.Fatal("cleanup failure not emitted through progress")
			}
		})
	}
}

func TestLlamaDeleteLeavesReplacedCompletedModel(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "delete-swap"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, llamaOwnedFilename(llamaTestID))
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, llamaTestBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.deleteLlamaModel(context.Background(), llamaTestID); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement accepted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replacement deleted: %v", err)
	}
}
