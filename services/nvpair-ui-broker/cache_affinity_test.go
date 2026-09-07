// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestCacheAffinityCompletionRelay(t *testing.T) {
	for _, engine := range []string{"proxy", "lmstudio-proxy", "omlx-proxy"} {
		t.Run(engine, func(t *testing.T) {
			brokerSide, clientSide := net.Pipe()
			defer brokerSide.Close()
			defer clientSide.Close()
			_ = clientSide.SetDeadline(time.Now().Add(2 * time.Second))
			b := &Broker{codec: NewCodec(brokerSide), proxySubscribed: true, lmstudioProxySubscribed: true, omlxProxySubscribed: true}
			params := json.RawMessage(`{"id":"17","method":"POST","path":"/v1/chat/completions","status":200,"cache_affinity":true}`)
			go func() {
				switch engine {
				case "proxy":
					b.forwardProxyNotification("proxy/request", params)
				case "lmstudio-proxy":
					b.forwardLMStudioProxyNotification("proxy/request", params)
				case "omlx-proxy":
					b.forwardOMLXProxyNotification("proxy/request", params)
				}
			}()
			msg, err := NewCodec(clientSide).Read()
			if err != nil {
				t.Fatal(err)
			}
			if msg.Method != engine+":proxy/request" {
				t.Fatalf("unexpected method: %s", msg.Method)
			}
			if string(msg.Params) != string(params) {
				t.Fatalf("completion payload changed: %s", msg.Params)
			}
		})
	}
}
