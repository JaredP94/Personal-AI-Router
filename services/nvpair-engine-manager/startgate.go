// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"

	settings "nvpair-shared/enginesettings"
)

const settingsRecoveryError = "Engine settings could not be reconciled. This engine is unavailable; restart it to retry."

type startAuthorizationKey struct{}
type startAuthorization struct{ engine, token string }

// prepareStart retries broker-owned recovery BEFORE acquiring opMu: recovery
// can call ConfigureLaunch on this same engine. The returned authorization is
// checked again at doStart, so a concurrent apply cannot leave a stale grant.
func (e *Executor) prepareStart(ctx context.Context, engine string) (context.Context, error) {
	if e.settingsParent == nil {
		return ctx, errors.New(settingsRecoveryError)
	}
	raw, err := e.settingsParent(ctx, "prepare-start", settings.Request{Engine: engine}, "")
	var token string
	if err != nil || json.Unmarshal(raw, &token) != nil || token == "" {
		return ctx, errors.New(settingsRecoveryError)
	}
	return context.WithValue(ctx, startAuthorizationKey{}, startAuthorization{engine, token}), nil
}

// Every launch/adoption, including internal restarts, passes this fail-closed
// check. Only the broker can authorize a reconciled generation or an active
// configure operation. This callback must never acquire the broker's settings
// locks: both a live apply and its rollback already own that operation.
func (e *Executor) authorizeStart(ctx context.Context, engine string) error {
	if e.settingsParent == nil {
		return errors.New(settingsRecoveryError)
	}
	authorization, ok := ctx.Value(startAuthorizationKey{}).(startAuthorization)
	if !ok || authorization.engine != engine {
		return errors.New(settingsRecoveryError)
	}
	if _, err := e.settingsParent(ctx, "authorize-start", settings.Request{Engine: engine, RequestID: authorization.token}, ""); err != nil {
		return errors.New(settingsRecoveryError)
	}
	return nil
}
