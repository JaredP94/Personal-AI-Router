// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"time"

	settings "nvpair-shared/enginesettings"
)

const profiledLoadFailed = "LM Studio could not load the model with its saved load options. Check the options before retrying; no default load was attempted."

func (e *Executor) runLMSLoad(ctx context.Context, st *engineState, model string) (json.RawMessage, error) {
	if !validProfileModel(model) {
		return nil, errors.New(profiledLoadFailed)
	}
	budget := min(e.actionTimeout, 4*time.Minute)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	args, err := e.loadProfileArgs(ctx, model)
	if err != nil {
		return nil, errors.New(loadProfileError)
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	act := st.manifest.Actions["load_model"]
	vars := map[string]string{"model": model, "port": strconv.Itoa(port), "install_dir": st.installDir, "cli": controlCLIPath(st.plat, st.installDir, runtime.GOOS)}
	argv, err := resolveArgs(act.Cmd, vars)
	if err != nil || len(argv) == 0 {
		return nil, errors.New(profiledLoadFailed)
	}
	argv = append(argv, args...)
	// Nil output streams go directly to the null device: do not capture private
	// diagnostics or wait for output pipes inherited by a vendor descendant.
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(command)
	command.WaitDelay = time.Second
	if err = command.Run(); err != nil {
		return nil, errors.New(profiledLoadFailed)
	}
	if ctx.Err() != nil {
		return nil, errors.New(profiledLoadFailed)
	}
	st.mu.Lock()
	if st.loadModels == nil {
		st.loadModels = map[string]bool{}
	}
	st.loadModels[model] = true
	st.mu.Unlock()
	return json.RawMessage("null"), nil
}

// The lifecycle operation lock serializes model mutation with restart. The
// profile store lock is released before any CLI, lifecycle call or callback.
func (e *Executor) lmsModelAction(ctx context.Context, st *engineState, action string, act Action, params json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.LMStudioModelBudget)
	defer cancel()
	var p struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(params, &p) != nil || !validProfileModel(p.Model) {
		return nil, errors.New("An LM Studio model is required.")
	}
	if act.RestartAfter {
		var err error
		ctx, err = e.prepareStart(ctx, "lmstudio")
		if err != nil {
			return nil, err
		}
	}
	if err := lockLMSOperation(ctx, st); err != nil {
		return nil, err
	}
	defer st.unlockOperation()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if action == "load_model" {
		return e.runLMSLoad(ctx, st, p.Model)
	}
	result, err := e.dispatchAction(ctx, st, "lmstudio", action, act, params)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	delete(st.loadModels, p.Model)
	running := st.running
	st.mu.Unlock()
	if action == "delete_model" {
		if err := e.deleteLoadProfile(ctx, p.Model); err != nil {
			return nil, err
		}
	}
	if act.RestartAfter && running {
		if err := e.doStop(st, "lmstudio"); err != nil {
			return nil, fmt.Errorf("action %q: engine failed to restart", action)
		}
		if err := e.doStart(ctx, st, "lmstudio", startOpts{}); err != nil {
			return nil, fmt.Errorf("action %q: engine failed to restart: %w", action, err)
		}
	}
	return result, nil
}

func lockLMSOperation(ctx context.Context, st *engineState) error {
	for !st.opMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New(profiledLoadFailed)
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		st.opMu.Unlock()
		return errors.New(profiledLoadFailed)
	}
	st.mu.Lock()
	st.opDone = make(chan struct{})
	st.revision++
	st.mu.Unlock()
	return nil
}

func (e *Executor) restoreLMSModels(ctx context.Context, st *engineState) error {
	st.mu.Lock()
	models := make([]string, 0, len(st.loadModels))
	for model := range st.loadModels {
		models = append(models, model)
	}
	st.mu.Unlock()
	slices.Sort(models)
	// A batch shares one deadline; an arbitrary number of models cannot extend
	// restart beyond the caller's lifecycle budget.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, model := range models {
		if _, err := e.runLMSLoad(ctx, st, model); err != nil {
			return err
		}
	}
	return nil
}
