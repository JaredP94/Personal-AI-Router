// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newToolTestExecutor(t *testing.T) (*Executor, string, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Homebrew requires /bin/sh")
	}
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	detect := filepath.Join(dir, "macmon")
	args := filepath.Join(dir, "args")
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, dir)
	ex.tools = toolEnv{
		InstallTimeout: 15 * time.Minute,
		Platform:       "darwin/arm64",
		BrewPaths:      []string{brew},
		Tools: map[string]toolSpec{
			"macmon": {Platforms: []string{"darwin/arm64"}, Detect: []string{detect}, Formula: "macmon"},
		},
	}
	return ex, brew, detect, args
}

func writeToolFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func requireToolCode(t *testing.T, err error, code int) *toolError {
	t.Helper()
	var te *toolError
	if !errors.As(err, &te) || te.Code != code {
		t.Fatalf("error = %v, want *toolError with code %d", err, code)
	}
	return te
}

func TestInstallToolUnknownTool(t *testing.T) {
	ex, _, _, _ := newToolTestExecutor(t)
	err := ex.InstallTool(context.Background(), "rm")
	var te *toolError
	if err == nil || errors.As(err, &te) {
		t.Fatalf("unknown tool error = %v, want plain error", err)
	}
}

func TestInstallToolUnsupportedPlatform(t *testing.T) {
	ex, _, detect, _ := newToolTestExecutor(t)
	ex.tools.Platform = "linux/amd64"
	// Platform validation must precede the already-installed check.
	writeToolFile(t, detect, "", 0o755)
	requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), -32011)
}

func TestInstallToolAlreadyInstalled(t *testing.T) {
	ex, brew, detect, args := newToolTestExecutor(t)
	writeToolFile(t, detect, "", 0o755)
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\nprintf ran > %q\nexit 99\n", args), 0o755)
	if err := ex.InstallTool(context.Background(), "macmon"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatalf("brew ran for an already-installed tool: %v", err)
	}
}

func TestInstallToolHomebrewMissing(t *testing.T) {
	ex, brew, _, _ := newToolTestExecutor(t)
	// A brew on PATH must not be used in place of the fixed candidates.
	writeToolFile(t, brew, "#!/bin/sh\nexit 99\n", 0o755)
	t.Setenv("PATH", filepath.Dir(brew))
	ex.tools.BrewPaths = []string{brew + "-missing"}
	requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), -32010)
}

func TestDefaultToolAllowlist(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	want := toolEnv{
		InstallTimeout: 15 * time.Minute,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		BrewPaths:      []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"},
		Tools: map[string]toolSpec{
			"macmon": {Platforms: []string{"darwin/arm64"}, Detect: []string{"/opt/homebrew/bin/macmon", "/usr/local/bin/macmon"}, Formula: "macmon"},
		},
	}
	if !reflect.DeepEqual(ex.tools, want) {
		t.Fatalf("unsafe or missing tool defaults: %+v", ex.tools)
	}
}

func TestInstallToolSuccess(t *testing.T) {
	ex, brew, detect, args := newToolTestExecutor(t)
	t.Setenv("PAIR_TOOL_TEST", "inherited")
	t.Setenv("HOMEBREW_NO_AUTO_UPDATE", "user-value")
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\n[ \"$PAIR_TOOL_TEST\" = inherited ] || exit 90\n[ \"$HOMEBREW_NO_AUTO_UPDATE\" = user-value ] || exit 91\nprintf '%%s %%s' \"$1\" \"$2\" > %q\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", args, detect, detect), 0o755)
	if err := ex.InstallTool(context.Background(), "macmon"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(args)
	if err != nil || string(got) != "install macmon" {
		t.Fatalf("brew args = %q, err = %v", got, err)
	}
	if installed := ex.GetInstalled(); len(installed) != 0 {
		t.Fatalf("tool leaked into engine list: %+v", installed)
	}
	if _, ok := ex.reg.Get("macmon"); ok {
		t.Fatal("tool leaked into engine registry")
	}
}

func TestInstallToolBrewFailsKeepsBoundedTail(t *testing.T) {
	ex, brew, _, _ := newToolTestExecutor(t)
	writeToolFile(t, brew, "#!/bin/sh\nprintf 'stdout must be discarded\\n'\ni=1\nwhile [ $i -le 100 ]; do printf 'line %s\\n' \"$i\" >&2; i=$((i+1)); done\nexit 1\n", 0o755)
	te := requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), -32012)
	for _, want := range []string{"line 81\n", "line 100"} {
		if !strings.Contains(te.Msg, want) {
			t.Fatalf("missing %q in %q", want, te.Msg)
		}
	}
	for _, unwanted := range []string{"line 1\n", "line 80\n", "stdout must be discarded"} {
		if strings.Contains(te.Msg, unwanted) {
			t.Fatalf("unwanted %q in %q", unwanted, te.Msg)
		}
	}
	if len(te.Msg) > 4096+128 {
		t.Fatalf("error too long: %d bytes", len(te.Msg))
	}
}

func TestInstallToolConcurrentCallsRunBrewOnce(t *testing.T) {
	ex, brew, detect, args := newToolTestExecutor(t)
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\nprintf 'run\\n' >> %q\n/bin/sleep 0.2\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", args, detect, detect), 0o755)
	results := make(chan error, 2)
	go func() { results <- ex.InstallTool(context.Background(), "macmon") }()
	deadline := time.Now().Add(time.Second)
	for {
		if data, _ := os.ReadFile(args); len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first brew did not start")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { results <- ex.InstallTool(context.Background(), "macmon") }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(args)
	if err != nil || string(data) != "run\n" {
		t.Fatalf("brew invocations = %q, err = %v; want one", data, err)
	}
}

func TestInstallToolTimeout(t *testing.T) {
	ex, brew, detect, _ := newToolTestExecutor(t)
	ex.tools.InstallTimeout = 100 * time.Millisecond
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\n/bin/sleep 0.3\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", detect, detect), 0o755)
	te := requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), -32012)
	if !strings.Contains(te.Msg, "timed out") {
		t.Fatalf("timeout error = %q, want timed out", te.Msg)
	}
	if _, err := os.Stat(detect); !os.IsNotExist(err) {
		t.Fatalf("brew completed after timeout: %v", err)
	}
}

func TestInstallToolNotDetectedAfterInstall(t *testing.T) {
	ex, brew, _, _ := newToolTestExecutor(t)
	writeToolFile(t, brew, "#!/bin/sh\nexit 0\n", 0o755)
	requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), -32012)
}

func TestInstallToolRequiresExecutableRegularFiles(t *testing.T) {
	for _, candidate := range []string{"tool", "brew"} {
		for _, kind := range []string{"directory", "non-executable"} {
			t.Run(candidate+"/"+kind, func(t *testing.T) {
				ex, brew, detect, _ := newToolTestExecutor(t)
				path, want := detect, -32010
				if candidate == "brew" {
					path = brew
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					writeToolFile(t, path, "", 0o644)
				}
				requireToolCode(t, ex.InstallTool(context.Background(), "macmon"), want)
			})
		}
	}
}

func TestInstallToolUsesLaterExecutableCandidates(t *testing.T) {
	ex, brew, detect, _ := newToolTestExecutor(t)
	ex.tools.BrewPaths = []string{brew + "-missing", brew}
	spec := ex.tools.Tools["macmon"]
	spec.Detect = []string{detect + "-missing", detect}
	ex.tools.Tools["macmon"] = spec
	writeToolFile(t, detect, "", 0o755)
	// Even with no brew, the later detect path makes this a no-op.
	if err := ex.InstallTool(context.Background(), "macmon"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(detect); err != nil {
		t.Fatal(err)
	}
	writeToolFile(t, brew, fmt.Sprintf("#!/bin/sh\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", detect, detect), 0o755)
	if err := ex.InstallTool(context.Background(), "macmon"); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommandTailCapsBytes(t *testing.T) {
	ex, brew, _, _ := newToolTestExecutor(t)
	writeToolFile(t, brew, "#!/bin/sh\nprintf 'stdout secret'\nprintf '%s' '"+strings.Repeat("x", 5000)+"end' >&2\nexit 1\n", 0o755)
	err := ex.runCommandTail(context.Background(), []string{brew}, 20, 4096)
	if err == nil {
		t.Fatal("expected command failure")
	}
	want := "exit status 1: " + strings.Repeat("x", 4093) + "end"
	if err.Error() != want {
		t.Fatalf("bounded error length = %d, want %d; suffix = %q", len(err.Error()), len(want), err.Error()[len(err.Error())-3:])
	}
}

func TestCommandTailRetainsOnlyByteSuffixAcrossWrites(t *testing.T) {
	tail := &commandTail{maxBytes: 10}
	for _, chunk := range []string{"123456", "789", "abc", "defghijklmnop", "qr"} {
		n, err := tail.Write([]byte(chunk))
		if err != nil || n != len(chunk) || len(tail.data) > 10 {
			t.Fatalf("Write(%q) = %d, %v; retained %d bytes", chunk, n, err, len(tail.data))
		}
	}
	if string(tail.data) != "ijklmnopqr" {
		t.Fatalf("tail = %q, want ijklmnopqr", tail.data)
	}
}

func TestRPCCodesForToolErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&toolError{Code: -32010, Msg: "missing"}, -32010},
		{fmt.Errorf("wrapped: %w", &toolError{Code: -32012, Msg: "failed"}), -32012},
		{errors.New("ordinary error"), -32000},
	} {
		if got := rpcCodeFor(tc.err); got != tc.want {
			t.Fatalf("rpcCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestRunInstallToolRespondsWithCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		params    string
		platform  string
		installed bool
		install   bool
		wantCode  int
	}{
		{name: "homebrew missing", params: `{"tool":"macmon"}`, wantCode: -32010},
		{name: "unknown tool", params: `{"tool":"rm"}`, wantCode: -32602},
		{name: "missing tool", params: `{}`, wantCode: -32602},
		{name: "empty tool", params: `{"tool":""}`, wantCode: -32602},
		{name: "invalid params", params: `{"tool":7}`, wantCode: -32602},
		{name: "no params", wantCode: -32602},
		{name: "unsupported", params: `{"tool":"macmon"}`, platform: "linux/amd64", wantCode: -32011},
		{name: "install failed", params: `{"tool":"macmon"}`, install: true, wantCode: -32012},
		{name: "already installed", params: `{"tool":"macmon"}`, installed: true},
		{name: "success", params: `{"tool":"macmon"}`, install: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, brew, detect, _ := newToolTestExecutor(t)
			if tc.install {
				body := "#!/bin/sh\nexit 1\n"
				if tc.wantCode == 0 {
					body = fmt.Sprintf("#!/bin/sh\nprintf '#!/bin/sh\\n' > %q\n/bin/chmod +x %q\n", detect, detect)
				}
				writeToolFile(t, brew, body, 0o755)
			}
			if tc.platform != "" {
				ex.tools.Platform = tc.platform
			}
			if tc.installed {
				writeToolFile(t, detect, "", 0o755)
			}
			managerConn, clientConn := net.Pipe()
			defer managerConn.Close()
			defer clientConn.Close()
			if err := clientConn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			m := NewManager(NewCodec(managerConn), ex, nil)
			id := json.RawMessage("1")
			returned := make(chan struct{})
			go func() {
				m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:install-tool", Params: json.RawMessage(tc.params)})
				close(returned)
			}()
			// net.Pipe blocks response writes until the read below. Dispatch
			// must return independently rather than run the operation inline.
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("tool install blocked message dispatch")
			}
			line, err := bufio.NewReader(clientConn).ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				ID    int `json:"id"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(line, &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != 1 {
				t.Fatalf("response id = %d", response.ID)
			}
			if tc.wantCode != 0 {
				if response.Error == nil || response.Error.Code != tc.wantCode {
					t.Fatalf("response = %s, want code %d", line, tc.wantCode)
				}
				return
			}
			var result map[string]any
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if response.Error != nil || !reflect.DeepEqual(result, map[string]any{"tool": "macmon", "installed": true}) {
				t.Fatalf("unexpected success response: %s", line)
			}
		})
	}
}
