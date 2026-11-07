// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"reflect"
	"testing"
)

func TestLlamaCPPServiceRoundTrip(t *testing.T) {
	record := ParseTXT([]string{"v=1", "uuid=fixture", "lc=1237", "lm=1234"})
	if port, ok := record.Port(ServiceLlamaCPP); !ok || port != 1237 {
		t.Fatalf("llama.cpp service: %+v", record)
	}
	if !reflect.DeepEqual(ParseTXT(record.TXT()), record) {
		t.Fatal("service lost during TXT round trip")
	}
	node := DirectoryNode{Models: []string{"same", "model.gguf"}, ModelsByEngine: map[string][]string{"lmstudio": {"same"}, "llamacpp": {"same", "model.gguf"}}}
	if !reflect.DeepEqual(node.EngineModels("llamacpp"), []string{"same", "model.gguf"}) || !reflect.DeepEqual(node.EngineModels("lmstudio"), []string{"same"}) {
		t.Fatal("engine inventories merged")
	}
}
