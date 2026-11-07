// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These exercise the public pull result as well as progress and the errors
// pipeline: merely suppressing the terminal error notification is not enough.
func TestLlamaDownloadCancellationPhases(t *testing.T) {
	for _, phase := range []string{"metadata", "free-space", "temp-creation", "streaming", "verification", "rename", "post-rename", "manifest"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			ex, cache := llamaDownloadExecutor(t, nil, nil)
			cancel := func() {
				if !ex.CancelModelPull("llamacpp", llamaTestID, "cancel-phase") {
					t.Error("cancel refused")
				}
			}
			switch phase {
			case "metadata":
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(started)
					<-r.Context().Done()
				}))
				t.Cleanup(server.Close)
				ex.llamaHub.endpoint = server.URL
				ex.llamaHub.client.Transport = server.Client().Transport
			case "free-space", "temp-creation":
				ex.llamaFreeSpace = func(*os.File) (uint64, error) {
					cancel()
					if phase == "free-space" {
						return 0, fmt.Errorf("space check interrupted")
					}
					// Deny staging creation after cancellation; no temp exists yet.
					if err := os.Chmod(cache, 0500); err != nil {
						t.Error(err)
					}
					t.Cleanup(func() { _ = os.Chmod(cache, 0700) })
					return 1 << 40, nil
				}
			case "streaming", "verification":
				ex, cache = llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
					if phase == "streaming" {
						_, _ = w.Write(llamaTestBytes()[:8])
						w.(http.Flusher).Flush()
						close(started)
						<-r.Context().Done()
						return
					}
					cancel()
					_, _ = w.Write(llamaTestBytes())
				}, func(repo *llamaHubRepo) {
					if phase == "verification" {
						repo.Siblings[0].LFS.SHA256 = strings.Repeat("b", 64)
					}
				})
			case "rename", "post-rename", "manifest":
				ex.llamaCommit = func(dir *os.File, from, to string) error {
					cancel()
					if phase == "rename" {
						return fmt.Errorf("rename interrupted")
					}
					if err := llamaCacheCommit(dir, from, to); err != nil {
						return err
					}
					if phase == "manifest" {
						return os.Mkdir(filepath.Join(cache, llamaManifestName), 0700)
					}
					return nil
				}
			}
			events, unsub := ex.progress.subscribe("llamacpp")
			defer unsub()
			type outcome struct {
				result json.RawMessage
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := ex.PullModelStream(context.Background(), "llamacpp", llamaTestID, nil, "cancel-phase")
				done <- outcome{result, err}
			}()
			if phase == "metadata" || phase == "streaming" {
				select {
				case <-started:
					cancel()
				case <-time.After(3 * time.Second):
					t.Fatal("request did not start")
				}
			}
			select {
			case got := <-done:
				if got.err != nil || string(got.result) != `{"status":"cancelled"}` {
					t.Errorf("pull result = %s, %v; want cancelled success response", got.result, got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("pull did not settle")
			}
			terminal := ""
			for len(events) > 0 {
				ev := <-events
				if ev.Stage == "cancelled" || ev.Stage == "error" || ev.Stage == "done" {
					terminal = ev.Stage
				}
			}
			if terminal != "cancelled" {
				t.Errorf("terminal stage = %q", terminal)
			}
			if errors := ex.reporter.snapshot(); len(errors) != 0 {
				t.Errorf("cancel reported engine failure: %+v", errors)
			}
			entries, err := os.ReadDir(cache)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") || strings.HasSuffix(entry.Name(), ".gguf") {
					t.Errorf("operation file leaked: %s", entry.Name())
				}
			}
			if phase == "post-rename" {
				dir, err := openLlamaCache(cache, false)
				if err != nil {
					t.Fatal(err)
				}
				defer dir.Close()
				manifest, err := readLlamaManifest(dir)
				if err != nil || len(manifest.Models) != 0 {
					t.Errorf("cancelled model recorded: %+v %v", manifest, err)
				}
			}
		})
	}
}

func TestLlamaDownloadFailureBeforeCancelRemainsError(t *testing.T) {
	ex, _ := llamaDownloadExecutor(t, nil, func(repo *llamaHubRepo) {
		repo.Siblings[0].LFS.SHA256 = strings.Repeat("b", 64)
	})
	_, err := ex.trackedPull(context.Background(), "llamacpp", llamaTestID, "failed", true, func(ctx context.Context) (json.RawMessage, error) {
		result, err := ex.pullLlamaModel(ctx, llamaTestID)
		if ex.CancelModelPull("llamacpp", llamaTestID, "failed") {
			t.Error("cancel accepted after primary failure")
		}
		return result, err
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("primary failure lost: %v", err)
	}
	if errors := ex.reporter.snapshot(); len(errors) != 1 || !strings.Contains(errors[0].ID, "pull-failed:llamacpp:") {
		t.Fatalf("primary failure not reported: %+v", errors)
	}
}

func TestLlamaDownloadCancelAfterCommitSucceeds(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	events, unsub := ex.progress.subscribe("llamacpp")
	defer unsub()
	result, err := ex.trackedPull(context.Background(), "llamacpp", llamaTestID, "committed", true, func(ctx context.Context) (json.RawMessage, error) {
		result, err := ex.pullLlamaModel(ctx, llamaTestID)
		if err != nil {
			return result, err
		}
		// The manifest is committed, but trackedPull has not published done yet.
		if ex.CancelModelPull("llamacpp", llamaTestID, "committed") {
			t.Error("cancel accepted after completion boundary")
		}
		return result, nil
	})
	if err != nil || !strings.Contains(string(result), llamaTestID) {
		t.Fatalf("completed pull failed: %s %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(cache, llamaOwnedFilename(llamaTestID))); err != nil {
		t.Fatal(err)
	}
	models, err := ex.listLlamaModels(context.Background())
	if err != nil || !strings.Contains(string(models), llamaTestID) {
		t.Fatalf("committed model missing: %s %v", models, err)
	}
	completed := false
	for len(events) > 0 {
		ev := <-events
		if ev.Stage == "cancelled" || ev.Stage == "error" {
			t.Fatalf("completed download changed outcome: %+v", ev)
		}
		if ev.Stage == "done" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("completed download did not report success")
	}
	if errors := ex.reporter.snapshot(); len(errors) != 0 {
		t.Fatalf("completed download reported failure: %+v", errors)
	}
}
