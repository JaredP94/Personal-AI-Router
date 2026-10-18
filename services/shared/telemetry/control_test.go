// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeConfigureRequest(t *testing.T) {
	for _, tt := range []struct {
		name     string
		raw      string
		revision uint64
		settings Settings
	}{
		{"enabled capture", `{"revision":2,"enabled":true,"collectorPort":4317,"recordPayloads":true}`, 2, Settings{Enabled: true, CollectorPort: 4317, RecordPayloads: true}},
		{"zero revision and false fields", `{"revision":0,"enabled":false,"collectorPort":1,"recordPayloads":false}`, 0, Settings{CollectorPort: 1}},
		{"max revision and port", `{"revision":18446744073709551615,"enabled":true,"collectorPort":65535,"recordPayloads":false}`, ^uint64(0), Settings{Enabled: true, CollectorPort: 65535}},
		{"unknown fields remain accepted", `{"revision":3,"enabled":false,"collectorPort":4317,"recordPayloads":false,"extra":true}`, 3, Settings{CollectorPort: 4317}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			revision, settings, err := DecodeConfigureRequest(json.RawMessage(tt.raw))
			if err != nil || revision != tt.revision || settings != tt.settings {
				t.Fatalf("got (%d, %+v, %v), want (%d, %+v, nil)", revision, settings, err, tt.revision, tt.settings)
			}
		})
	}
}

func TestDecodeConfigureRequestRejectsInvalidParams(t *testing.T) {
	const required = "expected revision, enabled, collectorPort and recordPayloads"
	for _, tt := range []struct{ name, raw, wantError string }{
		{"empty", ``, required},
		{"malformed", `{`, required},
		{"null", `null`, required},
		{"array", `[]`, required},
		{"scalar", `true`, required},
		{"empty object", `{}`, required},
		{"missing revision", `{"enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"missing enabled", `{"revision":1,"collectorPort":4317,"recordPayloads":false}`, required},
		{"missing port", `{"revision":1,"enabled":true,"recordPayloads":false}`, required},
		{"missing capture", `{"revision":1,"enabled":true,"collectorPort":4317}`, required},
		{"null revision", `{"revision":null,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"null enabled", `{"revision":1,"enabled":null,"collectorPort":4317,"recordPayloads":false}`, required},
		{"null port", `{"revision":1,"enabled":true,"collectorPort":null,"recordPayloads":false}`, required},
		{"null capture", `{"revision":1,"enabled":true,"collectorPort":4317,"recordPayloads":null}`, required},
		{"negative revision", `{"revision":-1,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"overflow revision", `{"revision":18446744073709551616,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"fractional revision", `{"revision":1.5,"enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"wrong revision type", `{"revision":"1","enabled":true,"collectorPort":4317,"recordPayloads":false}`, required},
		{"wrong enabled type", `{"revision":1,"enabled":"true","collectorPort":4317,"recordPayloads":false}`, required},
		{"wrong port type", `{"revision":1,"enabled":true,"collectorPort":"4317","recordPayloads":false}`, required},
		{"wrong capture type", `{"revision":1,"enabled":true,"collectorPort":4317,"recordPayloads":0}`, required},
		{"zero port", `{"revision":1,"enabled":true,"collectorPort":0,"recordPayloads":false}`, "collector port must be between 1 and 65535"},
		{"negative port", `{"revision":1,"enabled":true,"collectorPort":-1,"recordPayloads":false}`, "collector port must be between 1 and 65535"},
		{"overflow port", `{"revision":1,"enabled":false,"collectorPort":65536,"recordPayloads":false}`, "collector port must be between 1 and 65535"},
		{"capture without consent", `{"revision":1,"enabled":false,"collectorPort":4317,"recordPayloads":true}`, "payload recording requires telemetry to be enabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := DecodeConfigureRequest(json.RawMessage(tt.raw))
			if err == nil || err.Error() != tt.wantError {
				t.Fatalf("got error %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestConfigureResultJSON(t *testing.T) {
	got, err := json.Marshal(ConfigureResult{Revision: 2})
	if err != nil || string(got) != `{"revision":2}` {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestNewStatusResultJSON(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status Status
		want   string
	}{
		{"unconfigured", Status{}, `{"revision":0,"enabled":false,"lastExportOk":null,"lastExportError":null,"lastExportErrorAt":null}`},
		{"healthy", Status{Revision: 2, Enabled: true, LastExportOK: time.Date(2026, 10, 18, 5, 4, 3, 123, time.FixedZone("offset", 2*60*60))}, `{"revision":2,"enabled":true,"lastExportOk":"2026-10-18T03:04:03Z","lastExportError":null,"lastExportErrorAt":null}`},
		{"failed", Status{Revision: 3, LastExportError: "export failed", LastExportErrorAt: time.Date(2026, 10, 18, 1, 2, 3, 0, time.FixedZone("offset", -5*60*60))}, `{"revision":3,"enabled":false,"lastExportOk":null,"lastExportError":"export failed","lastExportErrorAt":"2026-10-18T06:02:03Z"}`},
		{"health history", Status{Revision: 4, Enabled: true, LastExportOK: time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC), LastExportError: "export failed", LastExportErrorAt: time.Date(2026, 10, 18, 4, 5, 6, 0, time.UTC)}, `{"revision":4,"enabled":true,"lastExportOk":"2026-10-18T01:02:03Z","lastExportError":"export failed","lastExportErrorAt":"2026-10-18T04:05:06Z"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(NewStatusResult(tt.status))
			if err != nil || string(got) != tt.want {
				t.Fatalf("got %s, %v; want %s", got, err, tt.want)
			}
		})
	}
}
