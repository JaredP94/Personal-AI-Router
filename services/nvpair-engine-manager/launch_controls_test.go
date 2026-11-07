// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestGenericControlSourcesAndLaunchConstruction(t *testing.T) {
	for _, definition := range []string{
		`[{"value":"{server.port}","env":["SERVER_PORT","PORT_ALIAS"]},{"value":"{server.host}","env":["SERVER_HOST","HOST_ALIAS"]}]`,
		`[{"value":"{server.host}:{server.port}","flags":["--listen","-l"],"env":["LISTEN","LISTEN_ALIAS"]}]`,
	} {
		t.Run(definition, func(t *testing.T) {
			e := settingsExecutor(t, true)
			rt := &settingsState(t, e).plat.Runtime
			if err := json.Unmarshal([]byte(definition), &rt.EditableLaunch.Controls); err != nil {
				t.Fatal(err)
			}
			request := settingsRequest(t, e)
			request.Settings.ServerPort = 23456
			request.Settings.LaunchText = "--future opaque"
			preview := previewSettings(t, e, request)
			if len(preview.Errors) > 0 {
				t.Fatal(preview.Errors)
			}
			rt.LaunchArgs, rt.LaunchEnv = &preview.Args, &preview.Env
			launch, err := launchForState(settingsState(t, e), 23456)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateEffectiveLaunch(*rt, launch, "127.0.0.1", "23456"); err != nil {
				t.Fatal(err)
			}
			for _, control := range rt.EditableLaunch.Controls {
				value, _ := control.managedValue("127.0.0.1", "23456")
				for _, env := range control.Env {
					if launch.Env[env] != value {
						t.Fatal("managed alias lost")
					}
					request.Settings.LaunchText = env + "=" + value
					if len(previewSettings(t, e, request).Errors) == 0 {
						t.Fatal("managed alias accepted")
					}
				}
			}
			if !slices.Equal(launch.Args[len(launch.Args)-2:], []string{"--future", "opaque"}) {
				t.Fatal("opaque arguments changed")
			}
		})
	}
}

func TestSavedControlsCannotBypassLaunchValidation(t *testing.T) {
	for _, command := range []bool{false, true} {
		e := settingsExecutor(t, command)
		rt := settingsState(t, e).plat.Runtime
		request := settingsRequest(t, e)
		launch, err := launchForState(settingsState(t, e), request.Settings.ServerPort)
		if err != nil {
			t.Fatal(err)
		}
		if command {
			launch.Args = append(launch.Args, "-p0")
		} else {
			launch.Env["OLLAMA_HOST"] = "0.0.0.0:11434"
		}
		if err := validateEffectiveLaunch(rt, launch, "127.0.0.1", fmt.Sprint(request.Settings.ServerPort)); err == nil {
			t.Fatal("unsafe saved launch accepted")
		}
		if command {
			args := []string{"-p0"}
			rt.LaunchArgs = &args
		} else {
			env := []string{"OLLAMA_HOST=0.0.0.0:11434"}
			rt.LaunchEnv = &env
		}
		settingsState(t, e).plat.Runtime = rt
		if err := e.Start(context.Background(), "fake"); err == nil {
			t.Fatal("unsafe saved overrides reached launch")
		}
		err = e.Start(context.Background(), "fake")
		if err == nil || !strings.Contains(err.Error(), "PAIR owns") {
			t.Fatalf("Start did not reject the unsafe saved networking control: %v", err)
		}
	}
}

func TestBundledNetworkingControls(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	names := reg.Names()
	slices.Sort(names)
	for _, name := range names {
		if name == "omlx" {
			continue // omlx keeps app launch without editable_launch
		}
		manifest, _ := reg.Get(name)
		for platform, config := range manifest.Platforms {
			t.Run(name+"/"+platform, func(t *testing.T) {
				e := settingsExecutor(t, config.Runtime.modeOrDefault() == "command")
				settingsState(t, e).plat.Runtime = config.Runtime
				policy := config.Runtime.EditableLaunch
				if policy == nil {
					t.Fatal("missing reviewed networking controls")
				}
				var valid, invalid []string
				if name == "lmstudio" {
					if !reflect.DeepEqual(policy.Controls, []LaunchControl{
						{Value: "{server.port}", Flags: []string{"--port", "-p"}, Env: []string{"LMS_SERVER_PORT"}},
						{Value: "{server.host}", Flags: []string{"--bind", "--host"}, Env: []string{"LMS_SERVER_HOST"}},
					}) {
						t.Fatalf("incomplete LM Studio controls: %+v", policy.Controls)
					}
					valid = []string{"--port 23456", "--port=23456", "-p 23456", "-p23456", "-p=23456", `"-p" "23456"`, "--port 23456 -p23456", "-- -p23456", "--host 127.0.0.1"}
					invalid = []string{"-p0", "-p65536", "-p", "-pno", "--port 23456 -p23457", "-vp23456", "-vp=23456", "--bind 0.0.0.0", "--bind=::", "LMS_SERVER_HOST=0.0.0.0", "--host 0.0.0.0", "--host=0.0.0.0", "-- --bind 0.0.0.0"}
				} else if name == "llamacpp" {
					valid = []string{"--host 127.0.0.1", "--port 23456", "LLAMA_ARG_HOST=127.0.0.1", "LLAMA_ARG_PORT=23456"}
					invalid = []string{"--host /tmp/server.sock", "--port 0", "--reuse_port", "LLAMA_ARG_REUSE_PORT=1"}
				} else {
					if !reflect.DeepEqual(policy.Controls, []LaunchControl{
						{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}},
					}) {
						t.Fatalf("incomplete Ollama controls: %+v", policy.Controls)
					}
					valid = []string{`OLLAMA_HOST="127.0.0.1:23456"`, `OLLAMA_HOST='127.0.0.1:23456'`, "OLLAMA_HOST=127.0.0.1:23456"}
					invalid = []string{"OLLAMA_HOST=0.0.0.0:23456", "OLLAMA_HOST=127.0.0.1:0", "OLLAMA_HOST=127.0.0.1:65536", "OLLAMA_HOST=127.0.0.1:23456 OLLAMA_HOST=0.0.0.0:23456"}
				}
				invalid = append(invalid, valid...)
				for _, text := range invalid {
					p := settingsRequest(t, e)
					p.Settings.LaunchText = text
					preview := previewSettings(t, e, p)
					if len(preview.Errors) == 0 && preview.Conflict == nil {
						t.Fatalf("invalid networking text accepted: %q", text)
					}
				}
			})
		}
	}
}

func TestNetworkingManifestRejectsAmbiguousDeclarations(t *testing.T) {
	for _, controls := range [][]LaunchControl{
		{{Value: "{server.port}"}},
		{{Value: "{server.port}", Flags: []string{"--port"}}, {Value: "{server.host}", Flags: []string{"--port"}}},
		{{Value: "{server.port}", Flags: []string{"--port", "--port"}}},
		{{Value: "{server.port}", Env: []string{"PORT"}}, {Value: "{server.host}", Env: []string{"PORT"}}},
		{{Value: "{unknown}", Flags: []string{"--x"}}},
		{{Value: "{server.port}", Flags: []string{"--port"}}},
	} {
		policy := EditableLaunch{Controls: controls}
		if err := policy.validateControls(); err == nil {
			t.Errorf("accepted %+v", policy)
		}
	}
}
