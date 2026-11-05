// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	settings "nvpair-shared/enginesettings"
)

const loadProfileError = "LM Studio load options could not be read or saved. No default load was attempted."
const loadProfileValidation = "Use long load-option flags with literal values. PAIR owns model identity and server addressing; file, execution and command controls are not allowed."

// This is a boundary denylist, not a catalogue of vendor tuning options.
// Unknown long flags are opaque. Short flags are ambiguous across CLI versions
// and are refused; positional tokens can only be values of a preceding flag.
func validateLoadText(text string) ([]string, error) {
	tokens, err := parseLaunchText(text)
	if err != nil {
		return nil, errors.New(loadProfileValidation)
	}
	args := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		name, value, equals := strings.Cut(token, "=")
		if !strings.HasPrefix(name, "--") || len(name) <= 2 {
			return nil, errors.New(loadProfileValidation)
		}
		for _, r := range name[2:] {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return nil, errors.New(loadProfileValidation)
			}
		}
		// Prefix matching also rejects abbreviated spellings and names that append
		// a suffix to a managed control. Vendor argument parsers may accept either.
		for _, control := range []string{"host", "port", "bind", "address", "listen", "server", "endpoint", "url", "api", "model", "identifier", "alias", "name", "path", "file", "config", "preset", "template", "prompt", "system", "lora", "adapter", "project", "exec", "command", "script", "plugin", "tool", "extension", "help", "version", "list", "directory", "dir", "remote", "log", "input", "output", "include", "require", "import", "export", "mcp", "proxy", "socket", "hook", "module", "resource", "attach", "estimate", "dry-run"} {
			if strings.Contains(name[2:], control) || strings.HasPrefix(control, name[2:]) {
				return nil, errors.New(loadProfileValidation)
			}
		}
		if name == "--load" || name == "--unload" || name == "--download" {
			return nil, errors.New(loadProfileValidation)
		}
		if !equals && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
			i++
			value = tokens[i]
			equals = true
			args = append(args, token, value)
		} else {
			args = append(args, token)
		}
		// A bare flag is a vendor boolean; scalar/path-free literal values cannot
		// introduce another command, response file, path or shell expression.
		if equals {
			if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "/\\@:=;$`|&<>\"'(){}\r\n") {
				return nil, errors.New(loadProfileValidation)
			}
			for _, r := range value {
				if unicode.IsControl(r) {
					return nil, errors.New(loadProfileValidation)
				}
			}
		}
	}
	return args, nil
}

type loadProfileRecord struct {
	Revision         uint64 `json:"revision"`
	LoadText         string `json:"loadText"`
	RequestID        string `json:"requestId"`
	Input            string `json:"input"`
	ExpectedRevision uint64 `json:"expectedRevision"`
}
type loadProfileFile struct {
	Revision uint64                       `json:"revision"`
	Profiles map[string]loadProfileRecord `json:"profiles"`
}

func (e *Executor) readLoadProfiles() (loadProfileFile, error) {
	state := loadProfileFile{Profiles: map[string]loadProfileRecord{}}
	if e.baseDir == "" {
		return state, nil
	}
	data, err := os.ReadFile(filepath.Join(e.baseDir, "lmstudio-load-profiles.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil || json.Unmarshal(data, &state) != nil || state.Profiles == nil {
		return state, errors.New(loadProfileError)
	}
	for model, record := range state.Profiles {
		if !validProfileModel(model) || record.Revision == 0 || record.Revision > state.Revision {
			return state, errors.New(loadProfileError)
		}
		if _, err := validateLoadText(record.LoadText); err != nil {
			return state, errors.New(loadProfileError)
		}
	}
	return state, nil
}

func validProfileModel(model string) bool {
	return model != "" && len(model) <= 4096 && !strings.HasPrefix(model, "-") && !strings.HasPrefix(model, "@") && !strings.ContainsFunc(model, unicode.IsControl)
}
func checkLoadProfileRequest(p settings.Request) error {
	if p.Engine != "lmstudio" || !validProfileModel(p.Model) || p.Format != "" && p.Format != launchTextFormat {
		return errors.New("Load options require an LM Studio model and a supported argument format.")
	}
	return nil
}
func (e *Executor) lockLoadProfiles(ctx context.Context) error {
	for !e.loadProfilesMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New(loadProfileError)
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		e.loadProfilesMu.Unlock()
		return errors.New(loadProfileError)
	}
	return nil
}
func profileRevision(state loadProfileFile, model string) uint64 {
	if record, ok := state.Profiles[model]; ok {
		return record.Revision
	}
	return state.Revision
}
func profileSnapshot(state loadProfileFile, model string) settings.LoadProfileSnapshot {
	return settings.LoadProfileSnapshot{Engine: "lmstudio", Model: model, Revision: profileRevision(state, model), LoadText: state.Profiles[model].LoadText, Format: launchTextFormat, Editable: true}
}
func (e *Executor) GetLoadProfile(ctx context.Context, p settings.Request) (settings.LoadProfileSnapshot, error) {
	if err := checkLoadProfileRequest(p); err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	if err := e.lockLoadProfiles(ctx); err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	defer e.loadProfilesMu.Unlock()
	state, err := e.readLoadProfiles()
	if err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	return profileSnapshot(state, p.Model), nil
}
func (e *Executor) PreviewLoadProfile(ctx context.Context, p settings.Request) (settings.LoadProfilePreview, error) {
	snap, err := e.GetLoadProfile(ctx, p)
	if err != nil {
		return settings.LoadProfilePreview{}, err
	}
	result := settings.LoadProfilePreview{Revision: snap.Revision, Format: launchTextFormat, Args: []string{}, Errors: map[string]string{}}
	args, err := validateLoadText(p.LoadText)
	if err != nil {
		result.Errors["loadText"] = err.Error()
		return result, nil
	}
	result.Args = args
	result.LoadText, _ = formatLaunchText(args)
	if p.ExpectedRevision != snap.Revision {
		result.Errors["loadText"] = "Load options changed. Reload before applying."
	}
	return result, nil
}
func (e *Executor) saveLoadProfiles(ctx context.Context, state loadProfileFile) error {
	if e.baseDir == "" || ctx.Err() != nil {
		return errors.New(loadProfileError)
	}
	if err := os.MkdirAll(e.baseDir, 0o700); err != nil {
		return errors.New(loadProfileError)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return errors.New(loadProfileError)
	}
	file, err := os.CreateTemp(e.baseDir, ".load-profiles-*")
	if err != nil {
		return errors.New(loadProfileError)
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil || ctx.Err() != nil {
		return errors.New(loadProfileError)
	}
	if err = os.Rename(file.Name(), filepath.Join(e.baseDir, "lmstudio-load-profiles.json")); err != nil {
		return errors.New(loadProfileError)
	}
	return nil
}
func (e *Executor) ApplyLoadProfile(ctx context.Context, p settings.Request) (settings.LoadProfileSnapshot, error) {
	if err := checkLoadProfileRequest(p); err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	args, err := validateLoadText(p.LoadText)
	if err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	if p.RequestID == "" || len(p.RequestID) > 128 {
		return settings.LoadProfileSnapshot{}, errors.New("A load options request ID is required.")
	}
	if err := e.lockLoadProfiles(ctx); err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	defer e.loadProfilesMu.Unlock()
	state, err := e.readLoadProfiles()
	if err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	old := state.Profiles[p.Model]
	if old.RequestID == p.RequestID {
		if old.Input != p.LoadText || old.ExpectedRevision != p.ExpectedRevision {
			return settings.LoadProfileSnapshot{}, errors.New("Load options request ID was already used.")
		}
		return profileSnapshot(state, p.Model), nil
	}
	if profileRevision(state, p.Model) != p.ExpectedRevision {
		return settings.LoadProfileSnapshot{}, errors.New("Load options changed. Reload before applying.")
	}
	normalized, _ := formatLaunchText(args)
	state.Revision++
	state.Profiles[p.Model] = loadProfileRecord{Revision: state.Revision, LoadText: normalized, RequestID: p.RequestID, Input: p.LoadText, ExpectedRevision: p.ExpectedRevision}
	if err := e.saveLoadProfiles(ctx, state); err != nil {
		return settings.LoadProfileSnapshot{}, err
	}
	return profileSnapshot(state, p.Model), nil
}
func (e *Executor) loadProfileArgs(ctx context.Context, model string) ([]string, error) {
	snap, err := e.GetLoadProfile(ctx, settings.Request{Engine: "lmstudio", Model: model})
	if err != nil {
		return nil, err
	}
	return validateLoadText(snap.LoadText)
}
func (e *Executor) loadProfileGeneration() uint64 {
	if !e.loadProfilesMu.TryLock() {
		return 0
	}
	defer e.loadProfilesMu.Unlock()
	state, err := e.readLoadProfiles()
	if err != nil {
		return 0
	}
	return state.Revision
}
func (e *Executor) pruneLoadProfiles(ctx context.Context, models []string, generation uint64) error {
	if err := e.lockLoadProfiles(ctx); err != nil {
		return err
	}
	defer e.loadProfilesMu.Unlock()
	state, err := e.readLoadProfiles()
	if err != nil {
		return err
	}
	changed := false
	for model, record := range state.Profiles {
		if record.Revision <= generation && !slices.Contains(models, model) {
			delete(state.Profiles, model)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	state.Revision++
	return e.saveLoadProfiles(ctx, state)
}
func (e *Executor) deleteLoadProfile(ctx context.Context, model string) error {
	if err := e.lockLoadProfiles(ctx); err != nil {
		return err
	}
	defer e.loadProfilesMu.Unlock()
	state, err := e.readLoadProfiles()
	if err != nil {
		return err
	}
	if _, exists := state.Profiles[model]; !exists {
		return nil
	}
	delete(state.Profiles, model)
	state.Revision++
	return e.saveLoadProfiles(ctx, state)
}

func (e *Executor) loadProfileOperation(ctx context.Context, method string, p settings.Request) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.LoadProfileBudget)
	defer cancel()
	switch method {
	case "get-load-profile":
		return e.GetLoadProfile(ctx, p)
	case "preview-load-profile":
		return e.PreviewLoadProfile(ctx, p)
	case "apply-load-profile":
		return e.ApplyLoadProfile(ctx, p)
	default:
		return nil, fmt.Errorf("unsupported load options method")
	}
}
