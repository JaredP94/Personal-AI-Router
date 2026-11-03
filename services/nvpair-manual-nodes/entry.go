// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"strings"
)

// Missing historical intent is not consent to a plaintext engine bypass.
func validateManualEntry(entry *ManualEntry) error {
	if entry.Kind == "" {
		entry.Kind = "pair"
	}
	if entry.Kind != "pair" && entry.Kind != "engine" {
		return fmt.Errorf("kind must be pair or engine")
	}
	if entry.Address == "" {
		return fmt.Errorf("address is required")
	}
	if entry.TLSPort < 0 || entry.TLSPort > 65535 {
		return fmt.Errorf("tls_port must be between 0 and 65535")
	}
	if net.ParseIP(entry.Address) != nil {
		return nil
	}
	if strings.ContainsAny(entry.Address, ":/\\?#@[] \t\r\n") {
		return fmt.Errorf("enter an IP address or hostname without a scheme or port; use tls_port for a PAIR node-info port")
	}
	host := strings.TrimSuffix(entry.Address, ".")
	if len(host) == 0 || len(host) > 253 {
		return fmt.Errorf("invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("invalid hostname")
			}
		}
	}
	return nil
}
