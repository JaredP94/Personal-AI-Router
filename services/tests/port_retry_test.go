// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"nvpair-shared/jsonrpc"
)

// retryPortBind repeats the whole port selection and worker bind, not just the
// request. A probe cannot reserve a port for another process. Only a confirmed
// address-in-use error for that exact port is safe to retry.
func retryPortBind(t *testing.T, pick func() int, bind func(int) error) (int, error) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		port := pick()
		err := bind(port)
		if err == nil {
			return port, nil
		}
		if attempt == 5 || !portBindCollision(port, err) {
			return 0, err
		}
		t.Logf("port %d taken before worker bind; retrying with a fresh port (attempt %d/5): %v", port, attempt+1, err)
	}
}

func portBindCollision(port int, err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	var rpcErr *jsonrpc.RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code != -32000 {
			return false
		}
		prefix := fmt.Sprintf("failed to bind port %d: ", port)
		if !strings.HasPrefix(rpcErr.Message, prefix) {
			return false
		}
		message = strings.TrimPrefix(rpcErr.Message, prefix)
	}
	for _, host := range []string{"", "0.0.0.0"} {
		prefix := fmt.Sprintf("listen tcp %s:%d: bind: ", host, port)
		if !strings.HasPrefix(message, prefix) {
			continue
		}
		// The stdio boundary preserves the OS error text, not errno identity.
		switch strings.TrimPrefix(message, prefix) {
		case "address already in use", "Only one usage of each socket address (protocol/network address/port) is normally permitted.":
			return true
		}
	}
	return false
}
