// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBrokerPreservesFourProxyStartupPorts(t *testing.T) {
	for _, port := range []int{11434, 1234, 1236, 1237} {
		if portBusy(port) {
			t.Fatalf("startup regression requires free proxy port %d", port)
		}
	}
	root, cluster := t.TempDir(), t.TempDir()
	base := root
	if runtime.GOOS == "darwin" {
		base = filepath.Join(root, "Library", "Application Support")
	}
	store := filepath.Join(base, "Nvidia Corporation", "Personal AI Router")
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	// Model an upgrade: the component's saved port and the broker's baseline
	// agree on oMLX :1236. This is not an explicit settings Apply.
	for file, port := range map[string]int{
		"proxy-port.json": 11434, "lmstudio-proxy-port.json": 1234,
		"omlx-proxy-port.json": 1236, "llamacpp-proxy-port.json": 1237,
	} {
		if err := os.WriteFile(filepath.Join(store, file), []byte(fmt.Sprintf(`{"port":%d}`, port)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	journal := []byte(`{"omlx":{"snapshot":{"engine":"omlx","revision":1,"appliedRevision":1,"phase":"idle","settings":{"serverPort":1235,"proxyPort":1236,"launchText":""}},"explicit":false}}`)
	if err := os.WriteFile(filepath.Join(cluster, "engine-settings-operations.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	stdin, msgs, stderr, cleanup := startBrokerWithDirsAndEnv(t, root, cluster, []string{"OLLAMA_HOST="},
		"--settings-path", nodeSettingsBin, "--engine-manager-path", engineMgrBin,
		"--proxy-path", proxyBin, "--lmstudio-proxy-path", lmstudioProxyBin,
		"--omlx-proxy-path", omlxProxyBin, "--llamacpp-proxy-path", llamacppProxyBin,
	)
	defer cleanup()
	go func() {
		for range stderr {
		}
	}()
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	for _, proxy := range []struct {
		prefix string
		port   int
	}{
		{"proxy", 11434}, {"lmstudio-proxy", 1234}, {"omlx-proxy", 1236}, {"llamacpp-proxy", 1237},
	} {
		if got := waitRoutingProxyReady(t, stdin, msgs, proxy.prefix); got != proxy.port {
			t.Fatalf("%s started on %d, want %d", proxy.prefix, got, proxy.port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxy.port), time.Second)
		if err != nil {
			t.Fatalf("%s did not listen: %v", proxy.prefix, err)
		}
		_ = conn.Close()
	}
	data, err := os.ReadFile(filepath.Join(store, "omlx-proxy-port.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Port != 1236 {
		t.Fatalf("startup changed saved oMLX proxy port: %s (%v)", data, err)
	}
}
