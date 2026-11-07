// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLlamaDownloadFreeSpaceDropsDuringStreaming(t *testing.T) {
	for _, interval := range []string{"bytes", "time"} {
		t.Run(interval, func(t *testing.T) {
			size := int64(257 << 20)
			if interval == "time" {
				size = 24
			}
			ex, cache := llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if interval == "time" {
					_, _ = w.Write(llamaTestBytes()[:8])
					w.(http.Flusher).Flush()
					select {
					case <-time.After(5100 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					_, _ = w.Write(llamaTestBytes()[8:16])
					w.(http.Flusher).Flush()
				} else {
					chunk := make([]byte, 128<<10)
					for i := 0; i < (256<<20)/len(chunk); i++ {
						if _, err := w.Write(chunk); err != nil {
							return
						}
					}
					w.(http.Flusher).Flush()
				}
				// Never send EOF: refusal must occur during streaming, not only at commit.
				<-r.Context().Done()
			}, func(repo *llamaHubRepo) {
				repo.Siblings[0].Size = size
				repo.Siblings[0].LFS.Size = size
			})
			checks := 0
			ex.llamaFreeSpace = func(f *os.File) (uint64, error) {
				checks++
				if checks == 1 {
					return 1 << 40, nil
				}
				info, err := f.Stat()
				if err != nil || !info.Mode().IsRegular() {
					t.Errorf("space recheck did not use transfer fd: %v", err)
				}
				return 0, nil
			}
			events, unsub := ex.progress.subscribe("llamacpp")
			defer unsub()
			observed := make(chan bool, 1)
			go func() {
				failed := false
				for ev := range events {
					if ev.Stage == "error" && strings.Contains(ev.Message, "not enough disk space") {
						failed = true
					}
				}
				observed <- failed
			}()
			done := make(chan error, 1)
			go func() { done <- pullLlamaTest(ex, "space-drop") }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
					t.Fatalf("disk-space refusal missing: %v", err)
				}
			case <-time.After(12 * time.Second):
				ex.CancelModelPull("llamacpp", llamaTestID, "space-drop")
				<-done
				t.Fatal("free space never rechecked during streaming")
			}
			if checks < 2 {
				t.Fatal("no streaming statfs")
			}
			files, err := os.ReadDir(cache)
			if err != nil || len(files) != 0 {
				t.Fatalf("own temporary file remains: %v %v", files, err)
			}
			unsub()
			if !<-observed {
				t.Fatal("disk-space error absent from progress/state")
			}
		})
	}
}
