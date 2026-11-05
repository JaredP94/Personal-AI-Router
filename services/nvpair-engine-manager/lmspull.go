// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
const downloadWaitDelay = time.Second

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
	process, err := configureDownloadProcess(cmd)
	if err != nil {
		return "", err
	}
	defer process.close()
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
	unpin, err := pinDownloadProcess(process)
	if err != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return "", fmt.Errorf("hold download process: %w", err)
	}
	defer unpin()
	finished := make(chan struct{})
	var runErr error
	go func() { runErr = cmd.Wait(); close(finished) }()
	select {
	case <-finished:
		if ctx.Err() != nil {
			return stopLMSDownload(ctx, lmsCancelGracesFrom(ctx), process, finished, current)
		}
	case <-ctx.Done():
		return stopLMSDownload(ctx, lmsCancelGracesFrom(ctx), process, finished, current)
	}
	_, _, text := current.state()
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, lmsErrorDetail(text))
	}
	return text, nil
}

func stopLMSDownload(ctx context.Context, g lmsCancelGraces, process *downloadProcess, finished <-chan struct{}, output *lmsDownloadOutput) (string, error) {
	// Shutdown interrupts ordinary cancellation graces and Windows helpers.
	normal, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdown := pullShutdown(ctx)
	go func() {
		select {
		case <-shutdown:
			cancel()
		case <-normal.Done():
		}
	}()
	stopping := func() bool {
		select {
		case <-shutdown:
			return true
		default:
			return false
		}
	}
	stop := func() (string, error) { return stopDownloadForShutdown(process, finished, output) }
	if stopping() {
		return stop()
	}
	interrupt := func() error {
		select {
		case <-finished:
			return nil
		default:
			return interruptDownload(normal, process)
		}
	}
	err := interrupt()
	if stopping() {
		return stop()
	}
	exited := false
	if err == nil {
		exited = waitLMSExitUnlessShutdown(normal, finished, g.interrupt)
	}
	if stopping() {
		return stop()
	}
	if !exited && err == nil {
		if interrupt() == nil {
			exited = waitLMSExitUnlessShutdown(normal, finished, g.retry)
		}
	}
	if stopping() {
		return stop()
	}
	// Launcher exit (including a cancellation acknowledgement) does not prove
	// its children exited. Always kill and verify the owned group/job.
	cleanupErr := killAndWaitDownload(normal, process, finished, g.reap)
	if stopping() {
		return stop()
	}
	if cleanupErr != nil {
		output.disown()
		return "", cleanupErr
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
	output.disown()
	return "", fmt.Errorf("LM Studio did not confirm download cancellation; the download may still be running in LM Studio")
}

func stopDownloadForShutdown(process *downloadProcess, finished <-chan struct{}, output *lmsDownloadOutput) (string, error) {
	signalCtx, cancel := context.WithTimeout(context.Background(), downloadShutdownSignalBudget)
	_ = terminateDownload(signalCtx, process)
	cancel()
	waitLMSExitUnlessShutdown(context.Background(), finished, downloadShutdownGrace)
	// Kill even if the launcher exited during grace: the group/job may survive.
	err := killAndWaitDownload(context.Background(), process, finished, downloadShutdownReap)
	output.disown()
	if err != nil {
		// Only process identity and OS errors, never downloader output.
		slog.Warn("download process cleanup failed during shutdown", "err", err)
		return "", err
	}
	return "", context.Canceled
}

// Group/job emptiness and launcher reap share one deadline. No lock is held
// while waiting; closing finished makes repeated exit checks safe.
func killAndWaitDownload(parent context.Context, process *downloadProcess, finished <-chan struct{}, reap time.Duration) error {
	killCtx, cancelKill := context.WithTimeout(parent, downloadShutdownSignalBudget)
	err := killDownload(killCtx, process)
	cancelKill()
	if err != nil {
		return fmt.Errorf("kill download process tree: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, reap)
	defer cancel()
	if err := waitDownloadGroup(ctx, process); err != nil {
		return err
	}
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("download launcher did not exit: %w", ctx.Err())
	}
}

func waitLMSExitUnlessShutdown(ctx context.Context, finished <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-finished:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func lmsErrorDetail(text string) string {
	clean := []rune(strings.TrimSpace(ansiPattern.ReplaceAllString(text, "")))
	if len(clean) > 200 {
		return "…" + string(clean[len(clean)-200:])
	}
	return string(clean)
}
