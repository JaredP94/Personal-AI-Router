// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestLMSCommandCancellationRequiresConfirmation(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "unconfirmed"}[confirmed], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, lmsCancelGracesKey{}, lmsCancelGraces{2 * time.Second, time.Second, time.Second})
			started := make(chan struct{}, 1)
			output := &lmsDownloadOutput{lastPercent: -1, progress: func(p int) {
				if p == 10 {
					select {
					case started <- struct{}{}:
					default:
					}
				}
			}}
			done := make(chan error, 1)
			argv := []string{fakeEngineForTest(t), "download-cancel-fixture"}
			if !confirmed {
				argv = append(argv, "unconfirmed")
			}
			go func() { _, err := runLMSDownloadCommand(ctx, argv, output); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("no progress")
			}
			cancel()
			select {
			case err := <-done:
				if confirmed && !errors.Is(err, context.Canceled) {
					t.Fatalf("confirmed stop: %v", err)
				}
				if !confirmed && (err == nil || errors.Is(err, context.Canceled)) {
					t.Fatalf("invented confirmation: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("unbounded cancellation")
			}
		})
	}
}

func TestLMSProgressSplitRedrawAndConfirmation(t *testing.T) {
	var mu sync.Mutex
	var percents []int
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	answers := make(chan string, 1)
	go func() { b := make([]byte, 2); n, _ := io.ReadFull(r, b); answers <- string(b[:n]) }()
	out := &lmsDownloadOutput{lastPercent: -1, stdin: w, progress: func(p int) { mu.Lock(); percents = append(percents, p); mu.Unlock() }}
	for _, s := range []string{"\x1b[32m[====] 12", ".5%", "\r[====] 27%", "\r[====] 27%\r", "Continue to download in the back", "ground?", "Download canceled."} {
		if _, err := out.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got := <-answers; got != "n\n" {
		t.Fatalf("background response %q", got)
	}
	canceled, complete, _ := out.state()
	if !canceled || complete {
		t.Fatalf("state %v %v", canceled, complete)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(percents) != 2 || percents[0] != 12 || percents[1] != 27 {
		t.Fatalf("progress %v", percents)
	}
	out.disown()
	canceled, _, _ = out.state()
	if canceled {
		t.Fatal("abandoned writer confirmed cancellation")
	}
}
