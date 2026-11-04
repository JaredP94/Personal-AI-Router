// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"strconv"
)

// These are PAIR's policy fields, independent of any engine or CLI syntax.
// Their validators cannot be weakened by a manifest. Proxy ports belong to the
// broker and are deliberately not injectable into an engine's arguments.
type launchField struct {
	match     func(host string) string
	normalize func(value, host string) (string, error)
	canonical func(string) string
	managed   func(host, port string) string
	localOnly bool
}

var launchFields = map[string]launchField{
	"server.port": {
		match:     func(string) string { return `[+-]?[0-9]+` },
		normalize: normalizeServerPort, managed: func(_, port string) string { return port },
	},
	"server.host": {
		match:     regexp.QuoteMeta,
		normalize: normalizeServerHost, managed: func(host, _ string) string { return host },
	},
}

func normalizeServerPort(value, _ string) (string, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("the launch text contains an invalid server port")
	}
	return strconv.Itoa(port), nil
}

func normalizeServerHost(value, host string) (string, error) {
	if value != host {
		return "", fmt.Errorf("the engine bind address is fixed to loopback")
	}
	return host, nil
}
