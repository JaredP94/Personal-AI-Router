// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type llamaCauseTransport func(*http.Request) (*http.Response, error)

func (f llamaCauseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type llamaCauseBody struct {
	read  func([]byte) (int, error)
	close func() error
}

func (b llamaCauseBody) Read(p []byte) (int, error) { return b.read(p) }
func (b llamaCauseBody) Close() error               { return b.close() }

// Losing the metadata cause used to make the live metadata cancel report a
// pull-failed engine error once order-based classification was removed.
func TestLlamaMetadataPreservesCancellationCause(t *testing.T) {
	for _, phase := range []string{"request", "body-wrapped", "body-unwrapped"} {
		t.Run(phase, func(t *testing.T) {
			h := newLlamaHub()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.client.Transport = llamaCauseTransport(func(r *http.Request) (*http.Response, error) {
				if phase == "request" {
					cancel()
					return nil, fmt.Errorf("transport stopped: %w", r.Context().Err())
				}
				body := llamaCauseBody{read: func([]byte) (int, error) {
					cancel()
					if phase == "body-wrapped" {
						return 0, fmt.Errorf("read stopped: %w", r.Context().Err())
					}
					return 0, io.ErrUnexpectedEOF
				}, close: func() error { return nil }}
				return &http.Response{StatusCode: 200, Body: body, Request: r}, nil
			})
			var repo llamaHubRepo
			if err := h.get(ctx, "/api/models/test", &repo); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}

// The same accepted cancel must not relabel a real filesystem failure, but a
// filesystem helper returning the cancellation cause must still be cancelled.
func TestLlamaDownloadClassifiesByCause(t *testing.T) {
	for _, phase := range []string{"free-space", "commit"} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancellation=%v", phase, cancelled), func(t *testing.T) {
				ex, _ := llamaDownloadExecutor(t, nil, nil)
				cause := errors.New("filesystem failed")
				if cancelled {
					cause = context.Canceled
				}
				fail := func() error {
					if !ex.CancelModelPull("llamacpp", llamaTestID, "cause") {
						t.Error("cancel refused")
					}
					return fmt.Errorf("phase failed: %w", cause)
				}
				if phase == "free-space" {
					ex.llamaFreeSpace = func(*os.File) (uint64, error) { return 0, fail() }
				} else {
					ex.llamaCommit = func(*os.File, string, string) error { return fail() }
				}
				result, err := ex.PullModelStream(context.Background(), "llamacpp", llamaTestID, nil, "cause")
				if cancelled {
					if err != nil || string(result) != `{"status":"cancelled"}` {
						t.Fatalf("want cancelled, got %s %v", result, err)
					}
					if len(ex.reporter.snapshot()) != 0 {
						t.Fatal("cancellation reported as failure")
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "filesystem failed") {
						t.Fatalf("real failure lost: %s %v", result, err)
					}
					if len(ex.reporter.snapshot()) != 1 {
						t.Fatal("failure not reported")
					}
				}
			})
		}
	}
}

// A real blocked HTTP client request must wrap context.Canceled; plain body
// read errors need the operation context as their cause at the read boundary.
func TestLlamaDownloadCancellationCausePhases(t *testing.T) {
	for _, phase := range []string{"metadata-blocked", "request-blocked", "streaming-wrapped", "streaming-unwrapped", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			ex, _ := llamaDownloadExecutor(t, nil, nil)
			cancel := func() bool { return ex.CancelModelPull("llamacpp", llamaTestID, "cause-phase") }
			if strings.HasSuffix(phase, "blocked") {
				started := make(chan struct{})
				if phase == "metadata-blocked" {
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						close(started)
						<-r.Context().Done()
					}))
					t.Cleanup(server.Close)
					ex.llamaHub.endpoint = server.URL
					ex.llamaHub.client.Transport = server.Client().Transport
				} else {
					ex, _ = llamaDownloadExecutor(t, func(w http.ResponseWriter, r *http.Request) {
						close(started)
						<-r.Context().Done()
					}, nil)
				}
				done := make(chan struct{})
				defer close(done)
				go func() {
					select {
					case <-started:
						if !cancel() {
							t.Error("blocked request cancel refused")
						}
					case <-done:
					}
				}()
			} else {
				transport := ex.llamaHub.client.Transport
				ex.llamaHub.client.Transport = llamaCauseTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err != nil || strings.HasPrefix(r.URL.Path, "/api/models/") {
						return resp, err
					}
					body := resp.Body
					if phase == "after-commit" {
						resp.Body = llamaCauseBody{read: body.Read, close: func() error {
							if cancel() {
								t.Error("cancel accepted after manifest commit")
							}
							return body.Close()
						}}
					} else {
						resp.Body = llamaCauseBody{read: func([]byte) (int, error) {
							if !cancel() {
								t.Error("streaming cancel refused")
							}
							if phase == "streaming-wrapped" {
								return 0, fmt.Errorf("transport read: %w", r.Context().Err())
							}
							return 0, io.ErrUnexpectedEOF
						}, close: body.Close}
					}
					return resp, nil
				})
			}
			events, unsub := ex.progress.subscribe("llamacpp")
			defer unsub()
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			result, err := ex.PullModelStream(ctx, "llamacpp", llamaTestID, nil, "cause-phase")
			want := "cancelled"
			if phase == "after-commit" {
				want = "done"
			}
			if err != nil {
				t.Fatalf("pull failed: %v", err)
			}
			if want == "cancelled" && string(result) != `{"status":"cancelled"}` {
				t.Fatalf("cancellation lost: %s", result)
			}
			if want == "done" && !strings.Contains(string(result), llamaTestID) {
				t.Fatalf("commit lost: %s", result)
			}
			terminal := ""
			for len(events) > 0 {
				ev := <-events
				if ev.Stage == "done" || ev.Stage == "cancelled" || ev.Stage == "error" {
					terminal = ev.Stage
				}
			}
			if terminal != want {
				t.Fatalf("terminal = %q, want %q", terminal, want)
			}
			if len(ex.reporter.snapshot()) != 0 {
				t.Fatal("cancellation or success reported a pull failure")
			}
		})
	}
}

func TestLlamaDownloadChecksumCancelDuringTeardown(t *testing.T) {
	for _, point := range []string{"body-close", "cache-close", "manifest-read"} {
		t.Run(point, func(t *testing.T) {
			ex, cache := llamaDownloadExecutor(t, nil, func(repo *llamaHubRepo) {
				repo.Siblings[0].LFS.SHA256 = strings.Repeat("b", 64)
			})
			transport := ex.llamaHub.client.Transport
			ex.llamaHub.client.Transport = llamaCauseTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := transport.RoundTrip(r)
				if err != nil || strings.HasPrefix(r.URL.Path, "/api/models/") {
					return resp, err
				}
				body := resp.Body
				resp.Body = llamaCauseBody{read: body.Read, close: func() error {
					// These nested teardown operations all run before pullLlamaModel
					// returns. Cancellation must not depend on a helper's Close order.
					if point != "body-close" {
						dir, err := openLlamaCache(cache, false)
						if err != nil {
							t.Error(err)
							return body.Close()
						}
						if !ex.CancelModelPull("llamacpp", llamaTestID, "teardown") {
							t.Error("cancel refused during nested teardown")
						}
						if point == "manifest-read" {
							_, err = readLlamaManifest(dir)
							if err != nil {
								t.Error(err)
							}
						}
						_ = dir.Close()
					} else if !ex.CancelModelPull("llamacpp", llamaTestID, "teardown") {
						t.Error("cancel refused during body teardown")
					}
					return body.Close()
				}}
				return resp, nil
			})
			if err := pullLlamaTest(ex, "teardown"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("checksum failure lost: %v", err)
			}
			if len(ex.reporter.snapshot()) != 1 {
				t.Fatal("checksum failure not reported")
			}
		})
	}
}
