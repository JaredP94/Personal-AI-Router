// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	settings "nvpair-shared/enginesettings"
)

func applyLiteralLaunch(rt Runtime, command launchCommand, vars map[string]string) (launchCommand, error) {
	if err := rejectManagedOverrides(rt); err != nil {
		return launchCommand{}, err
	}
	if rt.LaunchEnv != nil {
		env, err := literalEnvironment(*rt.LaunchEnv)
		if err != nil {
			return launchCommand{}, err
		}
		// Explicit assignments override manifest defaults. Omitted defaults
		// continue to resolve from the manifest on every launch.
		for key, template := range rt.Env {
			found := false
			for supplied := range env {
				if environmentKey(supplied) == environmentKey(key) {
					found = true
					break
				}
			}
			if !found {
				value, err := resolvePlaceholders(template, vars)
				if err != nil {
					return launchCommand{}, err
				}
				env[key] = value
			}
		}
		command.Env = env
	}
	p := rt.EditableLaunch
	if p == nil {
		if rt.LaunchArgs != nil {
			return launchCommand{}, fmt.Errorf("this engine has no editable launch definition")
		}
		return command, nil
	}
	if ip := net.ParseIP(vars["host"]); ip == nil {
		return launchCommand{}, fmt.Errorf("invalid bind address")
	}
	// Override every declared environment spelling, including inherited aliases.
	// This also makes the default preview agree with subsequent canonical launches.
	for _, control := range p.Controls {
		if value, managed := control.managedValue(vars["host"], vars["port"]); managed {
			for _, name := range control.Env {
				if command.Env == nil {
					command.Env = map[string]string{}
				}
				for supplied := range command.Env {
					if environmentKey(supplied) == environmentKey(name) {
						delete(command.Env, supplied)
					}
				}
				command.Env[name] = value
			}
		}
	}
	if rt.LaunchArgs == nil {
		return command, nil
	}
	command.Args = slices.Clone(p.FixedArgs)
	for _, control := range p.Controls {
		if value, managed := control.managedValue(vars["host"], vars["port"]); managed && control.flag() != "" {
			command.Args = append(command.Args, control.flag(), value)
		}
	}
	command.Args = append(command.Args, (*rt.LaunchArgs)...)

	return command, nil
}

func resolveRuntimeCommand(rt Runtime, index int, vars map[string]string) (launchCommand, error) {
	command, err := resolveCommandLaunch(rt.Start[index], vars)
	if err != nil {
		return launchCommand{}, err
	}
	if rt.EditableLaunch != nil && index == rt.EditableLaunch.StartIndex {
		return applyLiteralLaunch(rt, command, vars)
	}
	return command, nil
}

// launchForState is read-only; callers hold opMu, and neither preview nor a
// settings read probes ports, starts processes or writes configuration.
func launchForState(st *engineState, port int) (launchCommand, error) {
	rt := st.plat.Runtime
	vars := map[string]string{"host": effectiveBind(rt.Bind, ""), "port": strconv.Itoa(port), "install_dir": st.installDir}
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	st.mu.Lock()
	bin := st.binPath
	st.mu.Unlock()
	if rt.modeOrDefault() == "process" {
		return resolveProcessLaunch(rt, bin, vars)
	}
	index := 0
	if rt.EditableLaunch != nil {
		index = rt.EditableLaunch.StartIndex
	}
	if index < 0 || index >= len(rt.Start) {
		return launchCommand{}, fmt.Errorf("invalid editable start command")
	}
	return resolveRuntimeCommand(rt, index, vars)
}

func (e *Executor) launchStateLocked(engine string, st *engineState) settings.LaunchState {
	st.mu.Lock()
	result := settings.LaunchState{
		Engine:        engine,
		ServerPort:    st.plat.Runtime.Port,
		EffectivePort: st.port,
		Running:       st.running,
		Adopted:       st.adopted,
		Format:        launchTextFormat,
	}
	st.mu.Unlock()

	if engine == "omlx" {
		result.LaunchText = ""
		result.Editable = true
		return result
	}

	command, err := launchForState(st, result.ServerPort)
	if err == nil {
		result.LaunchText, err = command.argumentText(st.plat.Runtime.EditableLaunch)
	}
	switch {
	case err != nil:
		result.Reason = "The engine launch configuration cannot be displayed."
	case st.plat.Runtime.EditableLaunch == nil:
		result.Reason = "This engine does not support launch settings."
	case result.Running && result.Adopted && !canMoveAdoptedEngine(st.plat.Runtime):
		result.Reason = "Stop this engine in its own application before changing settings."
	default:
		result.Editable = true
	}
	return result
}

func (e *Executor) LaunchSettings(engine string) (settings.LaunchState, error) {
	st, err := e.state(engine)
	if err != nil {
		return settings.LaunchState{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	return e.launchStateLocked(engine, st), nil
}

func (e *Executor) PreviewLaunch(p settings.Request) (settings.Preview, error) {
	st, err := e.state(p.Engine)
	if err != nil {
		return settings.Preview{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	return e.previewLaunchLocked(st, p), nil
}

func (e *Executor) previewLaunchLocked(st *engineState, request settings.Request) settings.Preview {
	result := settings.Preview{Settings: request.Settings, Args: []string{}, Env: []string{}, Errors: map[string]string{}}
	fail := func(message string) settings.Preview { result.Errors["launchText"] = message; return result }

	if (request.Engine == "llamacpp" && request.Settings.ProxyPort != 0) || (request.Engine != "llamacpp" && (request.Settings.ProxyPort < 1 || request.Settings.ProxyPort > 65535)) {
		result.Errors["proxyPort"] = "Enter a proxy port from 1 to 65535."
		return result
	}
	if request.Engine == "omlx" {
		if request.Settings.ServerPort != st.plat.Runtime.Port {
			result.Errors["serverPort"] = "oMLX server settings are managed in the oMLX app; only the PAIR proxy port can be changed."
		}
		if strings.TrimSpace(request.Settings.LaunchText) != "" {
			result.Errors["launchText"] = "oMLX launch arguments and environment are managed in the oMLX app; only the PAIR proxy port can be changed."
		}
		if strings.TrimSpace(request.Settings.LaunchText) == "" {
			result.Settings.LaunchText = ""
		}
		current := e.launchStateLocked(request.Engine, st)
		result.Restart = false
		if !current.Editable {
			return fail(current.Reason)
		}
		return result
	}

	if request.Settings.ServerPort < 1 || request.Settings.ServerPort > 65535 {
		result.Errors["serverPort"] = "Enter a server port from 1 to 65535."
		return result
	}
	if err := e.reservedPortError(request.Settings.ServerPort); err != nil {
		result.Errors["serverPort"] = err.Error()
		return result
	}
	rt := st.plat.Runtime
	policy := rt.EditableLaunch
	if policy == nil {
		return fail("This engine does not support launch settings.")
	}
	if request.Resolution != "" && request.Resolution != "server" && request.Resolution != "launch" {
		return fail("Invalid port conflict resolution.")
	}
	base, err := launchForState(st, request.Settings.ServerPort)
	if err != nil {
		return fail("Cannot resolve the engine executable.")
	}
	host := effectiveBind(rt.Bind, "")
	tokens, err := parseLaunchText(request.Settings.LaunchText)
	if err != nil {
		return fail(err.Error())
	}
	for len(tokens) > 0 {
		key, _, ok := strings.Cut(tokens[0], "=")
		if !ok || strings.HasPrefix(key, "-") || strings.ContainsAny(key, "/\\") {
			break
		}
		if policy.protectedEnvironment(key) {
			return fail(managedLaunchMessage)
		}
		result.Env = append(result.Env, tokens[0])
		tokens = tokens[1:]
	}

	if request.Format != "" && request.Format != launchTextFormat {
		return fail("Unsupported engine argument format.")
	}
	// Every remaining token is validated, including anything after a "--"
	// separator. PAIR appends these after its own managed --port/--bind, so a
	// last-flag-wins engine would honour "-- --bind 0.0.0.0" if the separator
	// ended validation. "--" itself is passed through as a literal argument.
	for i := 0; i < len(tokens); i++ {
		control, _, last, err := policy.readControl(tokens, i)
		if err != nil {
			return fail(managedLaunchMessage)
		}
		i = last
		if control == nil {
			result.Args = append(result.Args, tokens[i])
			continue
		}
		return fail(managedLaunchMessage)
	}

	if err := e.reservedPortError(result.Settings.ServerPort); err != nil {
		result.Errors["serverPort"] = err.Error()
		return result
	}
	if _, err := literalEnvironment(result.Env); err != nil {
		return fail(err.Error())
	}
	// Unchanged manifest defaults remain templates, not literal overrides. This
	// keeps library paths following installation changes after a settings edit.
	overrides := result.Env[:0]
	for _, assignment := range result.Env {
		key, value, _ := strings.Cut(assignment, "=")
		if template, defaulted := rt.Env[key]; defaulted {
			inherited, err := resolvePlaceholders(template, map[string]string{"host": host, "port": strconv.Itoa(result.Settings.ServerPort), "install_dir": st.installDir, "bin": base.Bin})
			if err == nil && inherited == value {
				continue
			}
		}
		overrides = append(overrides, assignment)
	}
	result.Env = overrides
	rt.LaunchArgs = &result.Args
	rt.LaunchEnv = &result.Env
	vars := map[string]string{
		"host":        host,
		"port":        strconv.Itoa(result.Settings.ServerPort),
		"install_dir": st.installDir,
	}
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	command, err := applyLiteralLaunch(rt, base, vars)
	if err != nil {
		return fail(err.Error())
	}
	result.Settings.LaunchText, err = command.argumentText(policy)
	if err != nil {
		return fail(err.Error())
	}
	current := e.launchStateLocked(request.Engine, st)
	result.Restart = current.Running && (current.ServerPort != result.Settings.ServerPort || current.LaunchText != result.Settings.LaunchText)
	if !current.Editable {
		return fail(current.Reason)
	}
	return result
}

// ConfigureLaunch owns the entire engine operation lock, including the narrow
// parent rebind callback. The parent serializes settings operations, releasing
// its journal lock during this call; the callback uses only its operation token.
// Explicit ON/OFF intent is untouched.
func (e *Executor) ConfigureLaunch(ctx context.Context, p settings.Configure, rebind func(context.Context, bool) error) (settings.LaunchState, error) {
	st, err := e.state(p.Engine)
	if err != nil {
		return settings.LaunchState{}, err
	}
	// Invalidate passive observations across the entire settings operation,
	// just like Start/Stop/SetPort; a stale probe cannot adopt a vacated port.
	st.lockOperation()
	defer st.unlockOperation()
	if err := ctx.Err(); err != nil {
		return settings.LaunchState{}, err
	}
	preview := e.previewLaunchLocked(st, settings.Request{Engine: p.Engine, Settings: p.Settings})
	if err := preview.ValidationError(); err != nil {
		return settings.LaunchState{}, err
	}
	// This operation returns its one failure through the settings snapshot.
	// Do not also open the global lifecycle error dialog for the same attempt.
	ctx = context.WithValue(ctx, settingsApplyContextKey{}, true)
	// The broker's active operation authorizes candidate and compensation
	// starts without re-entering recovery while this engine lock is held.
	ctx = context.WithValue(ctx, startAuthorizationKey{}, startAuthorization{p.Engine, p.OperationID})
	e.reporter.clear(startFailedID(p.Engine))
	e.reporter.clear(exitedID(p.Engine))
	old := e.launchStateLocked(p.Engine, st)
	changed := old.ServerPort != preview.Settings.ServerPort || old.LaunchText != preview.Settings.LaunchText
	previousRuntime := st.plat.Runtime
	st.mu.Lock()
	previousBind := st.bind
	st.mu.Unlock()
	if old.Running && (old.Adopted || previousBind == "") {
		// An adopted daemon's actual listening scope is unknown. Restarting
		// it may narrow to loopback, but must never broaden its exposure.
		previousBind = "127.0.0.1"
	}
	previousFile, err := captureSettingsOverride(e.overrideDir, p.Engine)
	if err != nil {
		return old, fmt.Errorf("Settings could not be applied; no runtime changes were made.")
	}
	stopped := changed && old.Running
	startAttempted := false
	stopAttempt := func() error {
		err := e.doStop(st, p.Engine)
		// A failed control CLI can leave a detached daemon even though
		// running was never set. Verify both candidate and recovery cleanup.
		rt := st.plat.Runtime
		if rt.modeOrDefault() == "command" && rt.Ready != nil && !e.waitUnavailable(rt.Ready, st.port, time.Second) {
			err = e.runCommandStop(st, p.Engine, rt, st.port)
		}
		return err
	}
	rollback := func() (settings.LaunchState, error) {
		// Completion belongs to the node, not the canceled apply request. Keep
		// the same operation lock and reserve bounded time to restore service.
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		var stopErr error
		if startAttempted {
			stopErr = stopAttempt()
		}
		fileErr := previousFile.restore()
		proxyErr := rebind(restoreCtx, true)
		if stopErr == nil {
			st.mu.Lock()
			st.plat.Runtime = previousRuntime
			st.port = old.EffectivePort
			st.bind = previousBind
			st.mu.Unlock()
		}
		failed := stopErr != nil || fileErr != nil || proxyErr != nil
		if !failed && stopped {
			failed = e.doStart(restoreCtx, st, p.Engine, startOpts{RequireOwned: true, Bind: previousBind}) != nil
		}
		if failed && p.Engine != "omlx" {
			if err := stopAttempt(); err != nil {
				stopErr = err
			}
		}
		e.emitState(p.Engine)
		if failed {
			if stopErr != nil {
				return e.launchStateLocked(p.Engine, st), fmt.Errorf("Settings could not be applied and previous settings restoration failed; engine shutdown could not be confirmed. Stop it in its own application before retrying.")
			}
			if p.Engine == "omlx" {
				return e.launchStateLocked(p.Engine, st), fmt.Errorf("Settings could not be applied and previous settings restoration failed; inspect the PAIR proxy. The oMLX app was not changed.")
			}
			return e.launchStateLocked(p.Engine, st), fmt.Errorf("Settings could not be applied and previous settings restoration failed; the engine was left stopped. Reload to inspect the device.")
		}
		return e.launchStateLocked(p.Engine, st), fmt.Errorf("Settings could not be applied; previous settings were restored.")
	}
	// Stop before writing the override file. A stop failure returns with the
	// engine still on its old launch, so persisting first would leave disk on
	// the new one and the next start would silently adopt a configuration the
	// user was told had failed.
	if changed && old.Running {
		if err := e.doStop(st, p.Engine); err != nil {
			return e.launchStateLocked(p.Engine, st), err
		}
	}
	if p.Engine != "omlx" && (changed || !reflect.DeepEqual(st.plat.Runtime.LaunchArgs, &preview.Args) || !reflect.DeepEqual(st.plat.Runtime.LaunchEnv, &preview.Env)) {
		if err := e.persistRuntimeConfig(p.Engine, preview.Settings.ServerPort, &preview.Args, &preview.Env); err != nil {
			return rollback()
		}
	}
	st.mu.Lock()
	if p.Engine != "omlx" {
		st.plat.Runtime.Port = preview.Settings.ServerPort
		st.plat.Runtime.LaunchArgs = &preview.Args
		st.plat.Runtime.LaunchEnv = &preview.Env
		st.port = preview.Settings.ServerPort
	}
	st.mu.Unlock()
	if err := rebind(ctx, false); err != nil {
		return rollback()
	}
	resume := p.Resume
	if enabled, known, err := e.desired.get(p.Engine); err == nil && known && !enabled && !old.Running {
		resume = false
	}
	if p.Engine != "omlx" && ((changed && old.Running) || resume) {
		startAttempted = true
		opts := startOpts{RequireOwned: true}
		if old.Running {
			opts.Bind = previousBind
		}
		if err := e.doStart(ctx, st, p.Engine, opts); err != nil {
			return rollback()
		}
	}
	e.emitState(p.Engine)
	return e.launchStateLocked(p.Engine, st), nil
}

type settingsApplyContextKey struct{}

const managedLaunchMessage = "PAIR owns the engine bind address, ports and proxy wiring. Use the dedicated port fields; launch arguments and environment cannot override them."

// Validate persisted literal overrides too: a configuration file must not bypass
// the same boundary as the editor, even if it predates the current manifest.
func rejectManagedOverrides(rt Runtime) error {
	policy := rt.EditableLaunch
	if policy == nil {
		return nil
	}
	if rt.LaunchEnv != nil {
		for _, assignment := range *rt.LaunchEnv {
			key, _, _ := strings.Cut(assignment, "=")
			if policy.protectedEnvironment(key) {
				return fmt.Errorf("%s", managedLaunchMessage)
			}
		}
	}
	if rt.LaunchArgs != nil {
		for i := 0; i < len(*rt.LaunchArgs); i++ {
			control, _, _, err := policy.readControl(*rt.LaunchArgs, i)
			if err != nil || control != nil {
				return fmt.Errorf("%s", managedLaunchMessage)
			}
		}
	}
	return nil
}
