// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
const profiledListFailed = "LM Studio could not list loaded model identifiers. No automatic load or unload was attempted; retry after checking LM Studio."

// A successful load records only the tuning PAIR last submitted, NOT instance
// ownership. CLI 0.4.1 strips the immutable instanceReference from ps --json;
// its reusable identifier cannot authorize any automatic unload.
type lmsLoadedModel struct {
	Identifier string
	Args       []string
}

type lmsPSOutput struct{ buffer bytes.Buffer }

func (b *lmsPSOutput) Write(p []byte) (int, error) {
	if len(p) > 1024*1024-b.buffer.Len() {
		return 0, errors.New(profiledListFailed)
	}
	return b.buffer.Write(p)
}

func (e *Executor) listLMSInstances(ctx context.Context, cli string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, min(e.actionTimeout, 10*time.Second))
	defer cancel()
	command := exec.CommandContext(ctx, cli, "ps", "--json")
	configureSysProcAttr(command)
	command.WaitDelay = time.Second
	var output lmsPSOutput
	command.Stdout = &output
	// stderr is discarded; stdout is bounded private structured data, never logs.
	if err := command.Run(); err != nil || ctx.Err() != nil {
		return nil, errors.New(profiledListFailed)
	}
	var instances []struct {
		Identifier       string  `json:"identifier"`
		ModelKey         string  `json:"modelKey"`
		DeviceIdentifier *string `json:"deviceIdentifier"`
	}
	if json.Unmarshal(output.buffer.Bytes(), &instances) != nil || instances == nil {
		return nil, errors.New(profiledListFailed)
	}
	result := map[string]string{}
	for _, instance := range instances {
		if !validProfileModel(instance.Identifier) || !validProfileModel(instance.ModelKey) {
			return nil, errors.New(profiledListFailed)
		}
		// lms ps also includes linked devices; those aren't local resident loads.
		if instance.DeviceIdentifier != nil {
			continue
		}
		if _, duplicate := result[instance.Identifier]; duplicate {
			return nil, errors.New(profiledListFailed)
		}
		result[instance.Identifier] = instance.ModelKey
	}
	return result, nil
}

func runPrivateLMSCommand(ctx context.Context, argv []string) error {
	if ctx.Err() != nil {
		return errors.New(profiledLoadFailed)
	}
	// Nil streams go directly to the null device: never capture private vendor
	// diagnostics or wait for output pipes inherited by a vendor descendant.
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(command)
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil || ctx.Err() != nil {
		return errors.New(profiledLoadFailed)
	}
	return nil
}

func (e *Executor) runLMSLoad(ctx context.Context, st *engineState, model string) (json.RawMessage, error) {
	if !validProfileModel(model) {
		return nil, errors.New(profiledLoadFailed)
	}
	ctx, cancel := context.WithTimeout(ctx, min(e.actionTimeout, 4*time.Minute))
	defer cancel()
	args, err := e.loadProfileArgs(ctx, model)
	if err != nil {
		return nil, err
	}
	args = loadCLIArgs(args)
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	act := st.manifest.Actions["load_model"]
	vars := map[string]string{"model": model, "port": strconv.Itoa(port), "install_dir": st.installDir, "cli": controlCLIPath(st.plat, st.installDir, runtime.GOOS)}
	argv, err := resolveArgs(act.Cmd, vars)
	if err != nil || len(argv) == 0 {
		return nil, errors.New(profiledLoadFailed)
	}
	// server stop stops HTTP only, not resident instances. Query the daemon
	// before *every* load (including remote/restart loads), and fail closed.
	instances, err := e.listLMSInstances(ctx, argv[0])
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	forgetMissingLMSLoadsLocked(st, instances)
	st.mu.Unlock()
	identifier := model
	if key, resident := instances[identifier]; resident {
		if key != model || ctx.Err() != nil {
			return nil, errors.New(profiledLoadFailed)
		}
		// The original PAIR load may have expired and been replaced between
		// polls. Never unload even if the remembered arguments differ. The
		// user must explicitly unload/load to apply the saved profile.
		return json.RawMessage("null"), nil
	}
	// Pin the identifier rather than letting the CLI invent a duplicate.
	argv = append(argv, "--identifier", identifier)
	argv = append(argv, args...)
	if err := runPrivateLMSCommand(ctx, argv); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// Late/canceled completion cannot record tuning or replace the remembered
	// profile. The operation mutex serializes this with all lifecycle mutations.
	if ctx.Err() != nil {
		return nil, errors.New(profiledLoadFailed)
	}
	if st.loadModels == nil {
		st.loadModels = map[string]lmsLoadedModel{}
	}
	st.loadModels[model] = lmsLoadedModel{Identifier: identifier, Args: args}
	if st.restoreModels == nil {
		st.restoreModels = map[string]bool{}
	}
	st.restoreModels[model] = true
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
	delete(st.restoreModels, p.Model)
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
	models := make([]string, 0, len(st.restoreModels))
	for model := range st.restoreModels {
		models = append(models, model)
	}
	st.mu.Unlock()
	if len(models) == 0 {
		return nil
	}
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
