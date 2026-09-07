// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package prefixhash

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMatchesEarlierRequestAcrossAssistantAndUserTurns(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/api/chat"} {
		t.Run(path, func(t *testing.T) {
			first := Extract("POST", path, []byte(`{"model":"model","messages":[{"role":"system","content":"instruction"},{"role":"user","content":"first"}]}`))
			next := Extract("POST", path, []byte(`{"stream":true,"model":"model","messages":[{"content":"instruction","role":"system"},{"content":"first","role":"user"},{"content":"answer","role":"assistant"},{"role":"user","content":"next"}]}`))
			if first.Model != "model" || first.Full == (Digest{}) || len(next.Prefixes) != 3 || next.Prefixes[1] != first.Full {
				t.Fatal("next turn does not include the previous full context among its longest-first prefixes")
			}
			shorter := Extract("POST", path, []byte(`{"model":"model","messages":[{"role":"system","content":"instruction"}]}`))
			if len(shorter.Prefixes) != 0 || shorter.Full != next.Prefixes[2] {
				t.Fatal("single-message context should be recorded without predicting affinity")
			}
		})
	}
}

func TestCanonicalizationAndPromptIsolation(t *testing.T) {
	base := `{"model":"model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:synthetic"}}]}],"tools":[{"type":"function","function":{"name":"example","parameters":{"large":9007199254740992}}}]}`
	original := Extract("POST", "/v1/chat/completions", []byte(base))
	for _, extra := range []string{`,"stream":true`, `,"temperature":0.2,"max_tokens":32`} {
		changed := strings.TrimSuffix(base, "}") + extra + "}"
		if Extract("POST", "/v1/chat/completions", []byte(changed)).Full != original.Full {
			t.Fatal("generation settings changed prompt identity")
		}
	}
	for _, replacement := range [][2]string{{"9007199254740992", "9007199254740993"}, {"data:synthetic", "data:other"}, {"example", "other"}, {`"role":"user"`, `"role":"system"`}} {
		changed := strings.Replace(base, replacement[0], replacement[1], 1)
		if Extract("POST", "/v1/chat/completions", []byte(changed)).Full == original.Full {
			t.Fatal("different prompt-affecting data shared a digest")
		}
	}
}

func TestChatCanonicalizesNestedObjectsAndKeepsTemplateOptions(t *testing.T) {
	a := Extract("POST", "/v1/chat/completions", []byte(`{"model":"m","chat_template_kwargs":{"thinking":true,"mode":"brief"},"tool_choice":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"function":{"name":"lookup","parameters":{"type":"object","properties":{}}},"type":"function"}]}`))
	b := Extract("POST", "/v1/chat/completions", []byte(`{
		"tools": [{"type":"function","function":{"parameters":{"properties":{},"type":"object"},"name":"lookup"}}],
		"messages": [{"content":"hell\u006f", "role":"user"}], "model":"m",
		"tool_choice":"auto", "chat_template_kwargs":{"mode":"brief","thinking":true}
	}`))
	if a.Full == (Digest{}) || a.Full != b.Full {
		t.Fatal("object ordering, whitespace or equivalent string escapes changed identity")
	}
	base := `{"model":"m","messages":[{"role":"user","content":"hello"}],"chat_template_kwargs":{"thinking":true},"tool_choice":"auto"}`
	original := Extract("POST", "/v1/chat/completions", []byte(base))
	for _, changed := range []string{strings.Replace(base, "true", "false", 1), strings.Replace(base, "auto", "none", 1), strings.TrimSuffix(base, "}") + `,"custom_prompt_option":true}`} {
		if Extract("POST", "/v1/chat/completions", []byte(changed)).Full == original.Full {
			t.Fatal("prompt-affecting option change shared identity")
		}
	}
	if Extract("POST", "/api/chat", []byte(base)).Full == original.Full {
		t.Fatal("different API formats shared identity")
	}
}

func TestUnsupportedAndMalformedRequestsHaveNoContext(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/chat/completions", `{}`},
		{"POST", "/v1/embeddings", `{}`},
		{"POST", "/v1/chat/completions", `null`},
		{"POST", "/v1/chat/completions", `{`},
		{"POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]} {}`},
		{"POST", "/v1/chat/completions", `{"model":"m","messages":[]}`},
		{"POST", "/v1/chat/completions", `{"model":"m","messages":[null]}`},
		{"POST", "/v1/chat/completions", `{"model":"m","messages":["hi"]}`},
		{"POST", "/v1/chat/completions", `{"model":2,"messages":[{"role":"user","content":"hi"}]}`},
		{"POST", "/api/generate", `{"model":"m","context":[1.5,2]}`},
	} {
		got := Extract(tc.method, tc.path, []byte(tc.body))
		if got.Model != "" || got.Full != (Digest{}) || len(got.Prefixes) != 0 {
			t.Errorf("unexpected context for %s %s: %s", tc.method, tc.path, tc.body)
		}
	}
}

func TestCompletionUsesOnlyLongPromptPrefix(t *testing.T) {
	for _, path := range []string{"/v1/completions", "/api/generate"} {
		makeContext := func(prompt string) Context {
			body, _ := json.Marshal(map[string]string{"model": "model", "prompt": prompt})
			return Extract("POST", path, body)
		}
		if got := makeContext(strings.Repeat("a", 1023)); got.Full != (Digest{}) {
			t.Fatal("short prompts must not acquire affinity")
		}
		a := makeContext(strings.Repeat("a", 1024) + "first")
		b := makeContext(strings.Repeat("a", 1024) + "second")
		if a.Full == (Digest{}) || a.Full != b.Full || len(a.Prefixes) != 1 || a.Prefixes[0] != a.Full {
			t.Fatal("long completions must share an initial 1024-byte prefix bucket")
		}
	}
}

func TestOllamaContextDoesNotPredictResponseTokens(t *testing.T) {
	a := Extract("POST", "/api/generate", []byte(`{"model":"m","context":[1,2,3],"prompt":"first"}`))
	b := Extract("POST", "/api/generate", []byte(`{"model":"m","context":[1,2,3],"prompt":"second"}`))
	c := Extract("POST", "/api/generate", []byte(`{"model":"m","context":[1,2,3,4],"prompt":"third"}`))
	if a.Full == (Digest{}) || len(a.Prefixes) != 1 || a.Prefixes[0] != a.Full || b.Full != a.Full || c.Full == a.Full {
		t.Fatal("only repeated supplied Ollama context is a known affinity hint")
	}
}
