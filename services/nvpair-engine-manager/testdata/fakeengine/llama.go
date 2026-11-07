// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
)

// Mirror b12171's API naming: an explicit alias replaces the descriptor path.
func serveLlamaAlias() {
	var model, alias string
	for i := 2; i+1 < len(os.Args); i += 2 {
		switch os.Args[i] {
		case "--model":
			model = os.Args[i+1]
		case "--alias":
			alias = os.Args[i+1]
		default:
			os.Exit(2)
		}
	}
	file, err := os.Open(model)
	if err != nil {
		os.Exit(2)
	}
	defer file.Close()
	if alias == "" {
		alias = model
	}
	mux := http.NewServeMux()
	inventory := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": alias}}})
	}
	mux.HandleFunc("/v1/models", inventory)
	mux.HandleFunc("/models", inventory)
	mux.HandleFunc("/props", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"model_alias": alias, "model_path": model})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != alias {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": alias, "choices": []any{}})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("http://%s\n", ln.Addr())
	_ = http.Serve(ln, mux)
}
