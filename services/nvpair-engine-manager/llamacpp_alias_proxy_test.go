// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exercise the real descriptor launch and real proxy, not a hand-written alias
// in discovery. Without --alias the fake advertises /dev/fd/3, like b12171.
func TestLlamaModelAliasProxyRouting(t *testing.T) {
	ex, cache := llamaDownloadExecutor(t, nil, nil)
	if err := pullLlamaTest(ex, "alias-proxy"); err != nil {
		t.Fatal(err)
	}
	args, err := normalizeLlamaOptions([]string{"--model", llamaOwnedFilename(llamaTestID)}, cache)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	proc, err := startManagedProcWithEnvironment(fakeEngineForTest(t), append([]string{"llama-alias"}, args...), map[string]string{
		"LLAMA_CACHE": cache, "PAIR_TEST_PROCESS_ROOT": testProcessRoot,
	}, false, func(stream, line string) {
		if stream == "stdout" {
			select {
			case ready <- line:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.stop)
	var engineURL string
	select {
	case engineURL = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("fake llama server did not become ready")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	readModels := func(url string) []string {
		t.Helper()
		resp, err := client.Get(url + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Data []struct{ ID string } }
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("model inventory: status=%d error=%v", resp.StatusCode, err)
		}
		var ids []string
		for _, m := range body.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	ids := readModels(engineURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proxy := filepath.Join(t.TempDir(), "llamacpp-proxy")
	build := exec.CommandContext(ctx, "go", "build", "-o", proxy, ".")
	build.Dir = filepath.Join("..", "llamacpp-proxy")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v\n%s", err, out)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, proxyPort, err := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(proxy, "--port", proxyPort, "--ignore-persisted-port")
	// The real proxy must never read or write the developer's settings.
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "APPDATA="+home, "LOCALAPPDATA="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	type frame struct {
		ID     int
		Method string
		Params struct {
			Port         int
			WorkloadInfo struct{ Model, Engine string }
		}
		Error json.RawMessage
	}
	frames := make(chan frame, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var f frame
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				frames <- f
			}
		}
	}()
	wait := func(match func(frame) bool) frame {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatal("proxy exited before expected notification")
				}
				if match(f) {
					return f
				}
			case <-deadline.C:
				t.Fatal("timed out waiting for proxy")
			}
		}
	}
	port := wait(func(f frame) bool { return f.Method == "ready" }).Params.Port
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(engineURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	enginePort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "node/add-manual", "params": map[string]any{
		"id": "alias-node", "host": host, "addresses": []string{host}, "port": enginePort, "models": ids,
	}}); err != nil {
		t.Fatal(err)
	}
	if f := wait(func(f frame) bool { return f.ID == 1 }); len(f.Error) != 0 && string(f.Error) != "null" {
		t.Fatalf("add manual node: %s", f.Error)
	}
	proxyURL := "http://127.0.0.1:" + strconv.Itoa(port)
	if got := readModels(proxyURL); len(got) != 1 || got[0] != llamaTestID {
		t.Fatalf("proxy inventory = %v, want catalog ID %s", got, llamaTestID)
	}
	resp, err := client.Post(proxyURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+llamaTestID+`","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var completion struct{ Model string }
	if err != nil || json.Unmarshal(data, &completion) != nil || resp.StatusCode != http.StatusOK || completion.Model != llamaTestID {
		t.Fatalf("routed response: status=%d model=%q error=%v", resp.StatusCode, completion.Model, err)
	}
	for _, method := range []string{"workload:started", "workload:completed"} {
		f := wait(func(f frame) bool { return f.Method == method })
		if f.Params.WorkloadInfo.Model != llamaTestID || f.Params.WorkloadInfo.Engine != "llamacpp" {
			t.Fatalf("%s used wrong identity: %+v", method, f.Params.WorkloadInfo)
		}
	}
}
