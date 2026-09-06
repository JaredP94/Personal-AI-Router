// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestPrepareOMLXProxyPortWhenLMStudioPresent(t *testing.T) {
	free := func(port int) bool { return true }
	b := &Broker{
		lmstudioProxyPath: "/path/to/lmstudio-proxy",
	}

	b.prepareOMLXProxyPortWithPortCheck(free)
	if got := int(b.omlxProxyStartupPort.Load()); got != 1236 {
		t.Fatalf("expected fallback port 1236, got %d", got)
	}

	args := b.omlxProxyArgs()
	want := []string{"--port", "1236", "--ignore-persisted-port"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestPrepareOMLXProxyPortWhenStandaloneLMStudioOccupies1234(t *testing.T) {
	free := func(port int) bool {
		return port != 1234
	}
	b := &Broker{}

	b.prepareOMLXProxyPortWithPortCheck(free)
	if got := int(b.omlxProxyStartupPort.Load()); got != 1236 {
		t.Fatalf("expected fallback port 1236, got %d", got)
	}
}

func TestPrepareOMLXProxyPortWhen1234FreeAndNoLMStudio(t *testing.T) {
	free := func(port int) bool { return true }
	b := &Broker{}

	b.prepareOMLXProxyPortWithPortCheck(free)
	if got := int(b.omlxProxyStartupPort.Load()); got != 0 {
		t.Fatalf("expected no startup port override (0), got %d", got)
	}

	args := b.omlxProxyArgs()
	if len(args) != 0 {
		t.Fatalf("expected empty args, got %v", args)
	}
}
