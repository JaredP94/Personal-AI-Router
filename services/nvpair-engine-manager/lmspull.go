// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type downloadOutputKey struct{}

// LM Studio redraws using carriage returns and ANSI controls. Keep only a
// bounded tail; process output is not a system log. Callbacks run off mu.
type lmsDownloadOutput struct {
	mu                                       sync.Mutex
	text                                     string
	lastPercent                              int
	cancelled, completed, answered, disowned bool
	stdin                                    io.Writer
	progress                                 func(int)
}

var lmsPercentPattern = regexp.MustCompile(`\]\s+(\d+(?:\.\d+)?)%`)
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func (w *lmsDownloadOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	if w.disowned {
		w.mu.Unlock()
		return len(data), nil
	}
	w.text += string(data)
	clean := ansiPattern.ReplaceAllString(w.text, "")
	answer := strings.Contains(clean, "Continue to download in the background?") && !w.answered
	if answer {
		w.answered = true
	}
	w.cancelled = w.cancelled || strings.Contains(clean, "Download canceled.")
	w.completed = w.completed || strings.Contains(clean, "Download completed.")
	percent := -1
	matches := lmsPercentPattern.FindAllStringSubmatch(clean, -1)
	if len(matches) > 0 {
		if value, err := strconv.ParseFloat(matches[len(matches)-1][1], 64); err == nil {
			latest := max(0, min(100, int(value)))
			if latest != w.lastPercent {
				w.lastPercent = latest
				percent = latest
			}
		}
	}
	if len(w.text) > 8192 {
		w.text = w.text[len(w.text)-8192:]
	}
	stdin := w.stdin
	w.mu.Unlock()
	if answer && stdin != nil {
		answered := make(chan error, 1)
		go func() { _, err := io.WriteString(stdin, "n\n"); answered <- err }()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case err := <-answered:
			if err != nil {
				return 0, err
			}
		case <-timer.C:
			return 0, fmt.Errorf("LM Studio did not read cancellation response")
		}
	}
	if percent >= 0 && w.progress != nil {
		w.progress(percent)
	}
	return len(data), nil
}
func (w *lmsDownloadOutput) state() (bool, bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelled && !w.disowned, w.completed, w.text
}
func (w *lmsDownloadOutput) disown() { w.mu.Lock(); w.disowned = true; w.mu.Unlock() }

func (e *Executor) pullLMSModel(ctx context.Context, st *engineState, engine, model string) (json.RawMessage, error) {
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	params, _ := json.Marshal(map[string]string{"model": model})
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) {
		e.emitPullUpdate(ctx, ProgressEvent{Stage: "downloading", Percent: percent, Message: model})
	}}
	return e.runCmdAction(context.WithValue(ctx, downloadOutputKey{}, output), st, st.manifest.Actions[pullModelAction], port, params)
}

// The acceptance RPC never waits on these graces. Even a hung CLI must release
// this adapter in bounded time. WaitDelay bounds inherited stdio after reap.
const downloadWaitDelay = 5 * time.Second

type lmsCancelGraces struct{ interrupt, retry, reap time.Duration }
type lmsCancelGracesKey struct{}

func lmsCancelGracesFrom(ctx context.Context) lmsCancelGraces {
	if g, ok := ctx.Value(lmsCancelGracesKey{}).(lmsCancelGraces); ok {
		return g
	}
	return lmsCancelGraces{15 * time.Second, 5 * time.Second, 10 * time.Second}
}

func runLMSDownloadCommand(ctx context.Context, argv []string, output *lmsDownloadOutput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.WaitDelay = downloadWaitDelay
	if err := configureDownloadProcess(cmd); err != nil {
		return "", err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	defer stdin.Close()
	// Every retry owns a fresh writer, so output from an abandoned process can
	// neither confirm a newer run nor change its progress.
	current := &lmsDownloadOutput{lastPercent: -1, stdin: stdin, progress: output.progress}
	cmd.Stdout, cmd.Stderr = current, current
	if err := cmd.Start(); err != nil {
		return "", err
	}
	unpin, err := pinDownloadProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return "", fmt.Errorf("hold download process: %w", err)
	}
	defer unpin()
	finished := make(chan error, 1)
	var reaped atomic.Bool
	go func() { err := cmd.Wait(); reaped.Store(true); finished <- err }()
	var runErr error
	select {
	case runErr = <-finished:
	case <-ctx.Done():
		select {
		case runErr = <-finished:
		default:
			return stopLMSDownload(lmsCancelGracesFrom(ctx), cmd, finished, &reaped, current)
		}
	}
	_, _, text := current.state()
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, lmsErrorDetail(text))
	}
	return text, nil
}

func stopLMSDownload(g lmsCancelGraces, cmd *exec.Cmd, finished <-chan error, reaped *atomic.Bool, output *lmsDownloadOutput) (string, error) {
	interrupt := func() error {
		if reaped.Load() {
			return nil
		}
		return interruptDownload(cmd)
	}
	err := interrupt()
	exited := false
	if err == nil {
		exited, _ = waitLMSExit(finished, g.interrupt)
	}
	if !exited && err == nil {
		if interrupt() == nil {
			exited, _ = waitLMSExit(finished, g.retry)
		}
	}
	if exited {
		canceled, complete, text := output.state()
		if complete {
			return text, nil
		}
		if canceled {
			return "", context.Canceled
		}
		return "", fmt.Errorf("LM Studio exited without confirming download cancellation")
	}
	if !reaped.Load() {
		_ = killDownload(cmd)
	}
	_, _ = waitLMSExit(finished, g.reap)
	output.disown()
	return "", fmt.Errorf("LM Studio did not confirm download cancellation; the download may still be running in LM Studio")
}
func waitLMSExit(finished <-chan error, grace time.Duration) (bool, error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-finished:
		return true, err
	case <-timer.C:
		return false, nil
	}
}
func lmsErrorDetail(text string) string {
	clean := []rune(strings.TrimSpace(ansiPattern.ReplaceAllString(text, "")))
	if len(clean) > 200 {
		return "…" + string(clean[len(clean)-200:])
	}
	return string(clean)
}
