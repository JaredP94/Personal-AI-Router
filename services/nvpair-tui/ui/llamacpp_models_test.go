// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	settings "nvpair-shared/enginesettings"
	"strings"
	"testing"
)

func TestLlamaCatalogBrowse(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	cmd := v.Update(engineKey("b"))
	if cmd == nil {
		t.Fatal("catalog browse must request the Go catalog")
	}
	v.Update(engineRequest(t, cmd, c, "engine:model-catalog", `{"query":""}`, `{"models":[{"id":"unsloth/Demo:Q4_K_M","file":"a.gguf"},{"id":"ggml-org/Small:Q4_K_M","file":"b.gguf"}]}`))
	if !strings.Contains(v.View(), "ggml-org/Small:Q4_K_M") || !strings.Contains(v.View(), "Catalog") {
		t.Fatalf("missing catalog: %s", v.View())
	}
}

func TestLlamaDeleteRequiresStoppedEngine(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(100, 20)
	modelEngine(v, "llamacpp")
	e := v.byName["llamacpp"]
	e.Running = true
	v.merge(e)
	v.models.open = true
	v.models.llama = []llamaModel{{ID: "ggml-org/Demo:Q4_K_M", File: "cache.gguf", SHA256: "verified"}}
	v.refreshModels()
	v.Update(engineKey("d"))
	if v.confirm.open() {
		t.Fatal("running llama.cpp must not offer deletion")
	}
	if !strings.Contains(v.status, "Stop llama.cpp") {
		t.Fatalf("missing reason: %s", v.status)
	}
}

func TestLlamaInstalledSelectionAndDelete(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	v.Update(engineRequest(t, v.Update(engineKey("m")), c, "engine:action", `{"engine":"llamacpp","action":"list_downloaded"}`, `{"models":[{"id":"ggml-org/Demo:Q4_K_M","file":"cache.gguf","sha256":"verified","size":42}]}`))
	if !strings.Contains(v.View(), "ggml-org/Demo:Q4_K_M") {
		t.Fatal(v.View())
	}
	msg := engineRequest(t, v.Update(engineKey("a")), c, "engine:get-settings", `{"engine":"llamacpp"}`, `{"editable":true,"format":"pair-arguments-v1","revision":2,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--threads 4"}}`)
	preview := v.Update(msg)
	v.Update(engineRequest(t, preview, c, "engine:preview-settings", `{"engine":"llamacpp","expectedRevision":2,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--threads 4 --model \"cache.gguf\""}}`, `{"revision":2,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--threads 4 --model \"cache.gguf\""}}`))
	if !v.confirm.open() || strings.Contains(v.View(), "/dev/fd/") {
		t.Fatal(v.View())
	}
	done := make(chan tea.Msg, 1)
	cmd := v.Update(engineKey("y"))
	go func() { done <- cmd() }()
	q, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	var request settings.Request
	if err := json.Unmarshal(q.Params, &request); err != nil {
		t.Fatal(err)
	}
	if q.Method != "engine:apply-settings" || request.RequestID == "" || request.ExpectedRevision != 2 || request.Settings.LaunchText != `--threads 4 --model "cache.gguf"` {
		t.Fatalf("wrong settings request: %+v", request)
	}
	if err := c.Respond(q.ID, json.RawMessage(`{"phase":"succeeded","revision":3}`)); err != nil {
		t.Fatal(err)
	}
	v.Update(<-done)
	if !strings.Contains(v.status, "startup model llamacpp ok") {
		t.Fatal(v.status)
	}
	v.Update(engineKey("d"))
	if !v.confirm.open() {
		t.Fatal("owned stopped model should offer delete")
	}
	v.Update(engineKey("n"))
	v.models.llama[0].SHA256 = ""
	v.Update(engineKey("d"))
	if v.confirm.open() {
		t.Fatal("unmanaged model cannot be deleted")
	}
}

func TestLlamaCatalogPullUsesExistingTracker(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	v.Update(engineRequest(t, v.Update(engineKey("b")), c, "engine:model-catalog", `{"query":""}`, `{"models":[{"id":"ggml-org/Demo:Q4_K_M","file":"cache.gguf"}]}`))
	cmd := v.Update(engineKey("p"))
	if len(v.downloads) != 1 {
		t.Fatal("missing operation tracking")
	}
	id := v.downloads[0].id
	params := fmt.Sprintf(`{"engine":"llamacpp","action":"pull_model","params":{"name":"ggml-org/Demo:Q4_K_M","model":"ggml-org/Demo:Q4_K_M"},"opId":%q}`, id)
	engineRequest(t, cmd, c, "engine:action", params, `{"status":"ok"}`)
	v.applyDownloadProgress(downloadProgress{OpID: id, Engine: "llamacpp", Model: "ggml-org/Demo:Q4_K_M", Stage: "downloading", Percent: 42, CanCancel: true})
	if !strings.Contains(v.View(), "42%") {
		t.Fatal(v.View())
	}
	v.downloads[0].status = "canceling"
	v.applyDownloadProgress(downloadProgress{OpID: id, Engine: "llamacpp", Model: "ggml-org/Demo:Q4_K_M", Stage: "downloading", Percent: 50, CanCancel: true})
	if !strings.Contains(v.View(), "canceling") {
		t.Fatal(v.View())
	}
	v.applyDownloadProgress(downloadProgress{OpID: id, Engine: "llamacpp", Model: "ggml-org/Demo:Q4_K_M", Stage: "cancelled"})
	if len(v.downloads) != 0 || strings.Contains(v.status, "Partial data") {
		t.Fatal(v.status)
	}
}

func TestLlamaInterruptedDownloadDoesNotPromiseResume(t *testing.T) {
	v := newEnginesView(nil)
	v.downloads = []engineDownload{{id: "old", engine: "llamacpp", model: "demo", status: "canceling"}}
	v.Update(epochChangedMsg{})
	if len(v.downloads) != 0 || v.status != "Download interrupted — retry the download" {
		t.Fatal(v.status)
	}
}

func TestLlamaCatalogSearchErrorsAndStaleResults(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	v.Update(engineRequest(t, v.Update(engineKey("b")), c, "engine:model-catalog", `{"query":""}`, `{"models":[]}`))
	v.Update(engineKey("/"))
	v.input.SetValue(strings.Repeat("x", 101))
	cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	msg := engineErrorReply(t, cmd, c, "engine:model-catalog", fmt.Sprintf(`{"query":%q}`, strings.Repeat("x", 101)), "query must not exceed 100 characters")
	v.Update(msg)
	if !strings.Contains(v.View(), "query must not exceed 100 characters") {
		t.Fatal(v.View())
	}
	v.Update(llamaModelsMsg{generation: v.models.generation - 1, catalog: true, models: []llamaModel{{ID: "stale"}}})
	if strings.Contains(v.View(), "stale") {
		t.Fatal("stale catalog replaced latest result")
	}
}

func TestLlamaCatalogSwitchClearsStaleActionRows(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	v.Update(engineRequest(t, v.Update(engineKey("b")), c, "engine:model-catalog", `{"query":""}`, `{"models":[{"id":"ggml-org/Demo:Q4_K_M","file":"catalog.gguf","sha256":"metadata"}]}`))
	v.Update(engineKey("b")) // cache request has not completed
	if v.selectedModel() != "" {
		t.Fatal("catalog result remained selectable as installed inventory")
	}
	if cmd := v.Update(engineKey("a")); cmd != nil {
		t.Fatal("stale catalog row became a startup choice")
	}
	v.Update(engineKey("d"))
	if v.confirm.open() {
		t.Fatal("stale catalog row became deletable")
	}
}

func TestLlamaCatalogIgnoresServedInventoryPush(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	v.Update(engineRequest(t, v.Update(engineKey("b")), c, "engine:model-catalog", `{"query":""}`, `{"models":[{"id":"ggml-org/Demo:Q4_K_M","file":"catalog.gguf"}]}`))
	generation := v.models.generation
	modelPush(v, `{"modelsByEngine":{"llamacpp":["served"]}}`)
	if v.models.generation != generation {
		t.Fatal("served inventory push invalidated the independent catalog")
	}
}

func TestLlamaRouterSelectionStartsWithSettingsRead(t *testing.T) {
	v, c := engineWire(t)
	modelEngine(v, "llamacpp")
	cmd := v.Update(engineKey("0"))
	if cmd == nil {
		t.Fatal("router selection must use existing settings protocol")
	}
	msg := engineRequest(t, cmd, c, "engine:get-settings", `{"engine":"llamacpp"}`, `{"engine":"llamacpp","editable":true,"format":"pair-arguments-v1","revision":2,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--model old.gguf --threads 4"}}`)
	preview := v.Update(msg)
	v.Update(engineRequest(t, preview, c, "engine:preview-settings", `{"engine":"llamacpp","expectedRevision":2,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--threads 4"}}`, `{"revision":2,"restart":true,"settings":{"serverPort":8082,"proxyPort":1237,"launchText":"--threads 4"}}`))
	if !v.confirm.open() || !strings.Contains(v.View(), "router mode") {
		t.Fatalf("missing selection confirmation: %s", v.View())
	}
}
