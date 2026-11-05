// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// A disposable resident-model simulator. Never invokes a real engine or reads user files.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type instance struct {
	Identifier       string  `json:"identifier"`
	ModelKey         string  `json:"modelKey"`
	DeviceIdentifier *string `json:"deviceIdentifier"`
}

func main() {
	if os.Getenv("PAIR_TEST_PROFILE_HOLD_OUTPUT") == "1" {
		time.Sleep(2 * time.Second)
		return
	}
	capture := os.Getenv("PAIR_TEST_PROFILE_CAPTURE")
	file, err := os.OpenFile(capture, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(file).Encode(os.Args[1:])
	_ = file.Close()
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	models := []instance{}
	if data, err := os.ReadFile(capture + ".models"); err == nil {
		if json.Unmarshal(data, &models) != nil {
			os.Exit(2)
		}
	}
	mode := os.Getenv("PAIR_TEST_PROFILE_MODE")
	if os.Args[1] == "ps" {
		switch mode {
		case "fail-list":
			fmt.Fprintln(os.Stderr, "echoed-private-value")
			os.Exit(1)
		case "malformed-list":
			fmt.Println(`{"private":"echoed-private-value"}`)
			return
		case "oversized-list":
			fmt.Println(strings.Repeat("x", 2*1024*1024))
			return
		case "hold-list":
			hold(capture)
		}
		_ = json.NewEncoder(os.Stdout).Encode(models)
		return
	}
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "load":
		switch mode {
		case "hold":
			hold(capture)
		case "fail":
			fmt.Fprintln(os.Stderr, "echoed-private-value")
			os.Exit(1)
		}
		identifier := os.Args[2]
		for i := 3; i+1 < len(os.Args); i++ {
			if os.Args[i] == "--identifier" {
				identifier = os.Args[i+1]
			}
		}
		models = append(models, instance{Identifier: identifier, ModelKey: os.Args[2]})
	case "unload":
		remaining := []instance{}
		for _, model := range models {
			if model.Identifier != os.Args[2] {
				remaining = append(remaining, model)
			}
		}
		models = remaining
	default:
		os.Exit(2)
	}
	data, _ := json.Marshal(models)
	if os.WriteFile(capture+".models", data, 0o600) != nil {
		os.Exit(2)
	}
	fmt.Println("echoed-private-value")
}

func hold(capture string) {
	bin, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	child := exec.Command(bin)
	child.Env = append(os.Environ(), "PAIR_TEST_PROFILE_HOLD_OUTPUT=1")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if child.Start() != nil {
		os.Exit(2)
	}
	if os.WriteFile(capture+".ready", []byte("ready"), 0o600) != nil {
		os.Exit(2)
	}
	time.Sleep(4 * time.Second)
}
