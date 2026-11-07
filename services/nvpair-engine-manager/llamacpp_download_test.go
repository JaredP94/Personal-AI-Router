// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const llamaTestID = "bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M"

func llamaTestBytes() []byte {
	b := make([]byte, 24)
	copy(b, "GGUF")
	binary.LittleEndian.PutUint32(b[4:], 3)
	return b
}
func llamaDownloadExecutor(t *testing.T, serve func(http.ResponseWriter, *http.Request), change func(*llamaHubRepo)) (*Executor, string) {
	t.Helper()
	m := llamaManifest(t)
	p := m.Platforms["darwin/arm64"]
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = p
	reg := NewRegistry()
	reg.engines["llamacpp"] = m
	ex := NewExecutor(reg, NewReporter(nil), nil, t.TempDir())
	bytes := llamaTestBytes()
	sum := sha256.Sum256(bytes)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			repo := fixtureLlamaRepo(t)
			repo.Siblings[0].LFS.SHA256 = hex.EncodeToString(sum[:])
			if change != nil {
				change(&repo)
			}
			_ = json.NewEncoder(w).Encode(repo)
			return
		}
		if serve != nil {
			serve(w, r)
			return
		}
		_, _ = w.Write(bytes)
	}))
	t.Cleanup(server.Close)
	ex.llamaHub.endpoint = server.URL
	ex.llamaHub.client.Transport = server.Client().Transport
	ex.llamaFreeSpace = func(*os.File) (uint64, error) { return 1 << 40, nil }
	return ex, filepath.Join(ex.baseDir, "llamacpp-models")
}
func pullLlamaTest(ex *Executor, id string) error {
	_, err := ex.PullModelStream(context.Background(), "llamacpp", llamaTestID, nil, id)
	return err
}
func TestLlamaInstalledInventoryHonorsCancellation(t *testing.T) {
	ex, _ := llamaDownloadExecutor(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ex.Action(ctx, "llamacpp", "list_downloaded", nil); err != context.Canceled {
		t.Fatalf("ignored inventory cancellation: %v", err)
	}
}

func TestLlamaDownloadCommitSwapCannotClaimOrDeleteReplacement(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	ex.llamaCommit = func(dir *os.File, from, to string) error {
		if err := llamaCacheCommit(dir, from, to); err != nil {
			return err
		}
		path := filepath.Join(cache, to)
		if err := os.Rename(path, path+".saved"); err != nil {
			return err
		}
		return os.WriteFile(path, llamaTestBytes(), 0600)
	}
	if err := pullLlamaTest(ex, "commit-swap"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("commit replacement accepted: %v", err)
	}
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", llamaTestID); err == nil {
		t.Fatal("claimed and deleted a post-commit replacement")
	}
	if _, err := os.Stat(filepath.Join(cache, llamaOwnedFilename(llamaTestID))); err != nil {
		t.Fatal("replacement disappeared")
	}
}

func TestLlamaDownloadChecksumAndDelete(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "success"); err != nil {
		t.Fatal(err)
	}
	data, err := ex.Action(context.Background(), "llamacpp", "list_downloaded", nil)
	if err != nil || !strings.Contains(string(data), llamaTestID) {
		t.Fatalf("inventory: %s %v", data, err)
	}
	dir, err := openLlamaCache(cache, false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readLlamaManifest(dir)
	dir.Close()
	if err != nil || len(m.Models) != 1 || m.Models[0].FinalHost != "127.0.0.1" {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	st, _ := ex.state("llamacpp")
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", llamaTestID); err == nil {
		t.Fatal("deleted in-use model")
	}
	st.mu.Lock()
	st.running = false
	st.mu.Unlock()
	if err := os.WriteFile(filepath.Join(cache, "unrecorded.gguf"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", "a/unrecorded:Q4_K_M"); err == nil {
		t.Fatal("deleted unrecorded model")
	}
	if _, err := ex.ModelDelete(context.Background(), "llamacpp", llamaTestID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, llamaOwnedFilename(llamaTestID))); !os.IsNotExist(err) {
		t.Fatal("model remains")
	}
	b, err := os.ReadFile(filepath.Join(cache, "unrecorded.gguf"))
	if err != nil || string(b) != "untouched" {
		t.Fatal("unrecorded data changed")
	}
}
func TestLlamaDownloadMismatchPreservesOtherFiles(t *testing.T) {
	for _, which := range []string{"checksum", "size"} {
		t.Run(which, func(t *testing.T) {
			ex, cache := llamaDownloadExecutor(t, nil, func(r *llamaHubRepo) {
				if which == "checksum" {
					r.Siblings[0].LFS.SHA256 = strings.Repeat("b", 64)
				} else {
					r.Siblings[0].Size = 25
					r.Siblings[0].LFS.Size = 25
				}
			})
			if err := os.MkdirAll(cache, 0700); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(cache, "other.gguf")
			_ = os.WriteFile(other, []byte("untouched"), 0600)
			if err := pullLlamaTest(ex, "mismatch"); err == nil {
				t.Fatal("mismatch accepted")
			}
			files, err := os.ReadDir(cache)
			if err != nil || len(files) != 1 {
				t.Fatalf("temp/final leaked: %v %v", files, err)
			}
			b, _ := os.ReadFile(other)
			if string(b) != "untouched" {
				t.Fatal("other file changed")
			}
		})
	}
}
func TestLlamaDownloadCancelDuplicateAndInterrupted(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "restart"}[shutdown], func(t *testing.T) {
			started := make(chan struct{})
			ex, cache := llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write(llamaTestBytes()[:8])
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}, nil)
			events, unsub := ex.progress.subscribe("llamacpp")
			defer unsub()
			done := make(chan error, 1)
			go func() { done <- pullLlamaTest(ex, "first") }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("not started")
			}
			_ = os.WriteFile(filepath.Join(cache, "other.gguf"), []byte("untouched"), 0600)
			if err := pullLlamaTest(ex, "second"); err == nil {
				t.Fatal("duplicate accepted")
			}
			before := time.Now()
			if shutdown {
				ex.shuttingDown.Store(true)
			}
			if !ex.CancelModelPull("llamacpp", llamaTestID, "first") {
				t.Fatal("cancel refused")
			}
			if time.Since(before) > time.Second {
				t.Fatal("acceptance blocked")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancel not confirmed")
			}
			expected := "cancelled"
			if shutdown {
				expected = "interrupted"
			}
			found := false
			for len(events) > 0 {
				ev := <-events
				if ev.Stage == expected && ev.OpID == "first" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %s state", expected)
			}
			files, err := os.ReadDir(cache)
			if err != nil || len(files) != 1 || files[0].Name() != "other.gguf" {
				t.Fatalf("cleanup wrong: %v %v", files, err)
			}
		})
	}
}
func TestLlamaDownloadCancelCleanupFailureIsNotConfirmed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged user")
	}
	started := make(chan struct{})
	ex, cache := llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(llamaTestBytes()[:8])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}, nil)
	events, unsub := ex.progress.subscribe("llamacpp")
	defer unsub()
	done := make(chan error, 1)
	go func() { done <- pullLlamaTest(ex, "cleanup") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("not started")
	}
	if err := os.Chmod(cache, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cache, 0700) })
	if !ex.CancelModelPull("llamacpp", llamaTestID, "cleanup") {
		t.Fatal("cancel refused")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "could not be removed") {
			t.Fatalf("cleanup failure hidden: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not settle")
	}
	for len(events) > 0 {
		if ev := <-events; ev.Stage == "cancelled" {
			t.Fatal("claimed temp cleanup succeeded")
		}
	}
}

func TestLlamaDownloadFreeSpaceAndHTTPRedirect(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	ex.llamaFreeSpace = func(*os.File) (uint64, error) { return 0, nil }
	if err := pullLlamaTest(ex, "space"); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("space: %v", err)
	}
	files, _ := os.ReadDir(cache)
	if len(files) != 0 {
		t.Fatal("space refusal created files")
	}
	reached := false
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer httpServer.Close()
	ex, _ = llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, httpServer.URL, 302) }, nil)
	if err := pullLlamaTest(ex, "redirect"); err == nil {
		t.Fatal("unsafe redirect accepted")
	}
	if reached {
		t.Fatal("followed HTTP redirect")
	}
}
