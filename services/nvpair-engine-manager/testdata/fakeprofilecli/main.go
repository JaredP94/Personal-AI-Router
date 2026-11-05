// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// A disposable argv recorder. Never invokes a real engine or reads user files.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	if os.Getenv("PAIR_TEST_PROFILE_HOLD_OUTPUT") == "1" {
		time.Sleep(2 * time.Second)
		return
	}
	file, err := os.OpenFile(os.Getenv("PAIR_TEST_PROFILE_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(file).Encode(os.Args[1:])
	_ = file.Close()
	switch os.Getenv("PAIR_TEST_PROFILE_MODE") {
	case "hold":
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
		if os.WriteFile(os.Getenv("PAIR_TEST_PROFILE_CAPTURE")+".ready", []byte("ready"), 0o600) != nil {
			os.Exit(2)
		}
		time.Sleep(4 * time.Second)
	case "fail":
		fmt.Fprintln(os.Stderr, "echoed-private-value")
		os.Exit(1)
	}
	fmt.Println("echoed-private-value")
}
