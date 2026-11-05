// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	settings "nvpair-shared/enginesettings"
)

func TestLoadProfileCLIHelper(t *testing.T) {
	capture := os.Getenv("PAIR_TEST_PROFILE_CAPTURE")
	if capture == "" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	file, err := os.OpenFile(capture, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(file).Encode(args)
	_ = file.Close()
	for _, arg := range args {
		if arg == "--fail-private" {
			fmt.Fprintln(os.Stderr, "echoed-private-value")
			os.Exit(1)
		}
	}
	fmt.Println("echoed-private-value")
	os.Exit(0)
}

func profileActionExecutor(t *testing.T) (*Executor, string, string) {
	t.Helper()
	capture := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("PAIR_TEST_PROFILE_CAPTURE", capture)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	m := deleteModelRestartManifest(t, root, fakeEngineBin)
	m.Engine = "lmstudio"
	m.Actions["load_model"] = Action{Cmd: []string{bin, "-test.run=^TestLoadProfileCLIHelper$", "--", "load", "{model}"}}
	m.Actions["unload_model"] = Action{Cmd: []string{bin, "-test.run=^TestLoadProfileCLIHelper$", "--", "unload", "{model}"}}
	ex := newTestExecutor(t, m)
	t.Cleanup(func() { _ = ex.Stop("lmstudio") })
	return ex, capture, root
}
func saveTestProfile(t *testing.T, e *Executor, model, text string) {
	t.Helper()
	snap, err := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.ApplyLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: model, LoadText: text, ExpectedRevision: snap.Revision, RequestID: fmt.Sprintf("%s-%d", model, snap.Revision)})
	if err != nil {
		t.Fatal(err)
	}
}
func capturedProfileArgs(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := [][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row []string
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}
func TestLoadProfileEveryLocalRemoteAndRestartLoad(t *testing.T) {
	e, capture, root := profileActionExecutor(t)
	ctx := context.Background()
	if err := e.Start(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--gpu max --context-length 200000 --parallel 2")
	if result, err := e.ModelLoad(ctx, "lmstudio", "keep"); err != nil || string(result) != "null" {
		t.Fatalf("load returned private output: %s %v", result, err)
	}
	if err := e.Restart(ctx, "lmstudio"); err != nil {
		t.Fatal(err)
	}
	// The authenticated route uses the same model-load adapter as a remote peer.
	request := httptest.NewRequest(http.MethodPost, controlLoadPath, strings.NewReader(`{"engine":"lmstudio","model":"keep"}`))
	response := httptest.NewRecorder()
	(&controlServer{exec: e}).handleLoad(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("remote load: %s", response.Body.String())
	}
	if err := os.MkdirAll(filepath.Join(root, "gone"), 0o700); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "gone", "--gpu off")
	if _, err := e.ModelLoad(ctx, "lmstudio", "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ModelDelete(ctx, "lmstudio", "gone"); err != nil {
		t.Fatal(err)
	}
	rows := capturedProfileArgs(t, capture)
	keep := []string{"load", "keep", "--gpu", "max", "--context-length", "200000", "--parallel", "2"}
	if len(rows) != 5 {
		t.Fatalf("load count = %d, want 5", len(rows))
	}
	for _, i := range []int{0, 1, 2, 4} {
		if !reflect.DeepEqual(rows[i], keep) {
			t.Fatalf("load %d did not apply profile: %v", i, rows[i])
		}
	}
	snap, err := e.GetLoadProfile(ctx, settings.Request{Engine: "lmstudio", Model: "gone"})
	if err != nil || snap.LoadText != "" {
		t.Fatal("delete did not remove profile")
	}
}
func TestLoadProfileFailureHasNoFallbackAndNoPrivateOutput(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	saveTestProfile(t, e, "private-model", "--fail-private")
	_, err := e.ModelLoad(context.Background(), "lmstudio", "private-model")
	if err == nil || strings.Contains(err.Error(), "echoed-private-value") || strings.Contains(err.Error(), "private-model") {
		t.Fatalf("unsafe failure: %v", err)
	}
	if rows := capturedProfileArgs(t, capture); len(rows) != 1 || rows[0][2] != "--fail-private" {
		t.Fatal("failed profile fell back or was omitted")
	}
}
func TestLoadProfileRestoreFailureFailsRestart(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	if err := e.Start(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--gpu max")
	if _, err := e.ModelLoad(context.Background(), "lmstudio", "keep"); err != nil {
		t.Fatal(err)
	}
	saveTestProfile(t, e, "keep", "--fail-private")
	if err := e.Restart(context.Background(), "lmstudio"); err == nil || err.Error() != profiledLoadFailed {
		t.Fatalf("restore failure not reported: %v", err)
	}
}
func TestLoadProfileCanceledApplyCannotPersistLate(t *testing.T) {
	e := &Executor{baseDir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e.loadProfilesMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := e.ApplyLoadProfile(ctx, settings.Request{Engine: "lmstudio", Model: "m", LoadText: "--gpu max", RequestID: "canceled"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("lock wait was unbounded")
	}
	e.loadProfilesMu.Unlock()
	snap, err := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "m"})
	if err != nil || snap.Revision != 0 || snap.LoadText != "" {
		t.Fatal("late canceled write persisted")
	}
}
func TestLoadProfileInventoryPrunesOnlyAfterSuccessfulQuery(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	st, _ := e.state("lmstudio")
	st.manifest.Actions["list_models"] = Action{HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}, Result: &ActionResult{Array: "models", Field: "name"}}
	saveTestProfile(t, e, "llama3.2:1b", "--gpu max")
	saveTestProfile(t, e, "gone", "--parallel 2")
	// A stopped engine contributes no authoritative inventory, so retain both.
	_, _ = e.modelsResult(context.Background())
	snap, _ := e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "gone"})
	if snap.LoadText == "" {
		t.Fatal("unknown inventory pruned configuration")
	}
	if err := e.Start(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.modelsResult(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, _ = e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "gone"})
	if snap.LoadText != "" {
		t.Fatal("authoritative inventory did not prune absent model")
	}
	snap, _ = e.GetLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "llama3.2:1b"})
	if snap.LoadText != "--gpu max" {
		t.Fatal("inventory pruned a present model")
	}
}

func TestLoadProfilePersistenceFailurePreservesPreviousChoice(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	saveTestProfile(t, e, "keep", "--gpu max")
	path := filepath.Join(e.baseDir, "lmstudio-load-profiles.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ApplyLoadProfile(context.Background(), settings.Request{Engine: "lmstudio", Model: "keep", LoadText: "--gpu off", RequestID: "blocked", ExpectedRevision: 1}); err == nil {
		t.Fatal("failed save reported success")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(data, after) {
		t.Fatal("failed persistence changed previous profile")
	}
}

func TestLoadProfileQueuedLoadCancellationIsFenced(t *testing.T) {
	e, capture, _ := profileActionExecutor(t)
	st, _ := e.state("lmstudio")
	st.lockOperation()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.ModelLoad(ctx, "lmstudio", "m"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled queued load succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("queued load wait unbounded")
	}
	st.unlockOperation()
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("canceled queued load executed late")
	}
}

func TestLoadProfilesOtherEnginesRejected(t *testing.T) {
	e, _, _ := profileActionExecutor(t)
	for _, engine := range []string{"ollama", "omlx"} {
		if _, err := e.ApplyLoadProfile(context.Background(), settings.Request{Engine: engine, Model: "m", LoadText: "--gpu max", RequestID: "one"}); err == nil {
			t.Fatalf("accepted %s", engine)
		}
	}
}
