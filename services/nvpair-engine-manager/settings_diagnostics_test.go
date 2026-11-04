// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	settings "nvpair-shared/enginesettings"
)

// An opaque token that resembles a flag can contain a secret too. Do not rely
// on a vendor-specific flag/value classifier to decide what is safe to log.
func TestSettingsFailureDoesNotEchoOpaqueArgument(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		p.Settings.LaunchText = "--fail-launch --private-launch-value"
		if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func(context.Context, bool) error { return nil }); err != nil {
			t.Fatal(err)
		}
		err := e.Start(context.Background(), "fake")
		if err == nil || strings.Contains(err.Error(), "private-launch-value") {
			t.Fatal("private argument echoed in startup error")
		}
		for _, line := range settingsState(t, e).logs.snapshot() {
			if strings.Contains(line.Text, "private-launch-value") {
				t.Fatal("private argument entered exported engine log")
			}
		}
	})
}
