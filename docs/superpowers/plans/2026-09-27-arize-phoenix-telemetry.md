<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Arize Phoenix & OpenTelemetry Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Integrate OpenTelemetry (OTel) distributed tracing and metrics with Arize Phoenix into the PAIR router, including proxy instrumentation across all cluster nodes, Docker container lifecycle controls in the Desktop UI, and optional prompt/completion capture for AI evaluations.

**Architecture:** Pure-Go OpenTelemetry SDK (`go.opentelemetry.io/otel`) with non-blocking OTLP gRPC export (`:4317`) in `services/shared/telemetry`. Proxies propagate standard W3C `traceparent` HTTP headers across cluster nodes for unified cross-node traces. Arize Phoenix runs as a single Docker container on one designated node, managed via broker JSON-RPC commands (`telemetry/start`, `telemetry/stop`, `telemetry/get-status`) and rendered in Desktop Service Settings.

**Tech Stack:** Go 1.25, `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, Docker Compose, Arize Phoenix, Electron, React, TypeScript.

---

### Task 1: Docker Compose Configuration & Helper Script

**Files:**
- Create: `docker-compose.telemetry.yml`
- Create: `scripts/telemetry-phoenix.sh`

- [ ] **Step 1: Write Docker Compose file for Arize Phoenix**

```yaml
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

services:
  phoenix:
    image: arizephoenix/phoenix:latest
    container_name: nvpair-phoenix
    restart: unless-stopped
    ports:
      - "6006:6006" # Phoenix UI and OTLP HTTP
      - "4317:4317" # OTLP gRPC collector
    environment:
      - PHOENIX_PORT=6006
      - PHOENIX_GRPC_PORT=4317
      - PHOENIX_SQL_DATABASE_URL=sqlite:////data/phoenix.db
    volumes:
      - nvpair-phoenix-data:/data

volumes:
  nvpair-phoenix-data:
    driver: local
```

- [ ] **Step 2: Write helper script `scripts/telemetry-phoenix.sh`**

```bash
#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
COMPOSE_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docker-compose.telemetry.yml"

case "${1:-status}" in
  up|start)
    docker compose -f "$COMPOSE_FILE" up -d
    echo "Arize Phoenix started on http://localhost:6006 (OTLP gRPC: 4317)"
    ;;
  down|stop)
    docker compose -f "$COMPOSE_FILE" stop
    echo "Arize Phoenix stopped"
    ;;
  status)
    docker compose -f "$COMPOSE_FILE" ps
    ;;
  open)
    if command -v open >/dev/null; then
      open "http://localhost:6006"
    elif command -v xdg-open >/dev/null; then
      xdg-open "http://localhost:6006"
    else
      echo "Open http://localhost:6006 in your browser"
    fi
    ;;
  *)
    echo "Usage: $0 {up|down|status|open}"
    exit 1
    ;;
esac
```

- [ ] **Step 3: Make helper script executable and test syntax**

Run: `chmod +x scripts/telemetry-phoenix.sh && docker compose -f docker-compose.telemetry.yml config`
Expected: Valid YAML output without syntax errors.

- [ ] **Step 4: Commit**

```bash
git add docker-compose.telemetry.yml scripts/telemetry-phoenix.sh
git commit -s -m "feat(telemetry): add Arize Phoenix docker-compose and management script"
```

---

### Task 2: Shared OpenTelemetry Module (`services/shared/telemetry`)

**Files:**
- Create: `services/shared/telemetry/config.go`
- Create: `services/shared/telemetry/redact.go`
- Create: `services/shared/telemetry/telemetry.go`
- Create: `services/shared/telemetry/telemetry_test.go`
- Modify: `services/shared/go.mod`

- [ ] **Step 1: Add OpenTelemetry dependencies to `services/shared/go.mod`**

Add `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/trace`, and `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`.

Run: `cd services/shared && go get go.opentelemetry.io/otel@v1.34.0 go.opentelemetry.io/otel/sdk@v1.34.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.34.0`

- [ ] **Step 2: Write failing unit test for telemetry initialization and redaction**

Create `services/shared/telemetry/telemetry_test.go`:
```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"testing"
)

func TestRedactHeaders(t *testing.T) {
	headers := map[string][]string{
		"Authorization":   {"Bearer secret-token-12345"},
		"X-Pair-Pin":      {"123456"},
		"Content-Type":    {"application/json"},
		"X-Forwarded-For": {"192.168.1.10"},
	}
	clean := RedactHeaders(headers)
	if clean["Authorization"] != "[REDACTED]" {
		t.Errorf("expected Authorization to be redacted, got %s", clean["Authorization"])
	}
	if clean["X-Pair-Pin"] != "[REDACTED]" {
		t.Errorf("expected X-Pair-Pin to be redacted, got %s", clean["X-Pair-Pin"])
	}
	if clean["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type to be preserved, got %s", clean["Content-Type"])
	}
}

func TestDualModePayloadControl(t *testing.T) {
	prompt := "Hello, how are you?"
	completion := "I am an AI model."

	// Mode 1: RecordPayloads = false
	attrsFalse := FormatPayloadAttributes(false, prompt, completion)
	if _, ok := attrsFalse["gen_ai.prompt"]; ok {
		t.Errorf("expected prompt to be absent when RecordPayloads=false")
	}
	if _, ok := attrsFalse["gen_ai.completion"]; ok {
		t.Errorf("expected completion to be absent when RecordPayloads=false")
	}

	// Mode 2: RecordPayloads = true
	attrsTrue := FormatPayloadAttributes(true, prompt, completion)
	if attrsTrue["input.value"] != prompt || attrsTrue["output.value"] != completion {
		t.Errorf("expected prompt and completion to be present when RecordPayloads=true")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd services/shared && go test ./telemetry/...`
Expected: Compilation failure because `RedactHeaders` and `FormatPayloadAttributes` are not defined.

- [ ] **Step 4: Implement `config.go`, `redact.go`, and `telemetry.go`**

Create `services/shared/telemetry/config.go`:
```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "time"

type Config struct {
	Enabled        bool          `json:"enabled"`
	Endpoint       string        `json:"endpoint"`
	ServiceName    string        `json:"service_name"`
	RecordPayloads bool          `json:"record_payloads"`
	BatchTimeout   time.Duration `json:"batch_timeout"`
}

func DefaultConfig(serviceName string) Config {
	return Config{
		Enabled:        true,
		Endpoint:       "localhost:4317",
		ServiceName:    serviceName,
		RecordPayloads: false,
		BatchTimeout:   time.Second,
	}
}
```

Create `services/shared/telemetry/redact.go`:
```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "strings"

var sensitiveHeaderKeys = map[string]struct{}{
	"authorization": {},
	"cookie":        {},
	"x-pair-pin":    {},
	"x-api-key":     {},
	"proxy-auth":    {},
}

func RedactHeaders(headers map[string][]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		if _, sensitive := sensitiveHeaderKeys[lower]; sensitive {
			out[k] = "[REDACTED]"
		} else if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func FormatPayloadAttributes(recordPayloads bool, prompt, completion string) map[string]string {
	out := make(map[string]string)
	if !recordPayloads {
		return out
	}
	if prompt != "" {
		out["input.value"] = prompt
		out["gen_ai.prompt"] = prompt
	}
	if completion != "" {
		out["output.value"] = completion
		out["gen_ai.completion"] = completion
	}
	return out
}
```

Create `services/shared/telemetry/telemetry.go`:
```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Provider struct {
	cfg      Config
	tp       *sdktrace.TracerProvider
	tracer   trace.Tracer
	shutdown func(context.Context) error
}

func Init(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{cfg: cfg, tracer: otel.GetTracerProvider().Tracer(cfg.ServiceName)}, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(cfg.BatchTimeout)),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{
		cfg:      cfg,
		tp:       tp,
		tracer:   tp.Tracer(cfg.ServiceName),
		shutdown: tp.Shutdown,
	}, nil
}

func (p *Provider) Tracer() trace.Tracer {
	if p == nil || p.tracer == nil {
		return otel.GetTracerProvider().Tracer("nvpair")
	}
	return p.tracer
}

func (p *Provider) RecordPayloads() bool {
	return p != nil && p.cfg.RecordPayloads
}

func (p *Provider) Shutdown(ctx context.Context) error {
	if p != nil && p.shutdown != nil {
		return p.shutdown(ctx)
	}
	return nil
}

// InjectHTTPContext injects W3C traceparent into an outgoing HTTP request.
func InjectHTTPContext(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// ExtractHTTPContext extracts W3C traceparent from an incoming HTTP request.
func ExtractHTTPContext(req *http.Request) context.Context {
	return otel.GetTextMapPropagator().Extract(req.Context(), propagation.HeaderCarrier(req.Header))
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd services/shared && go test ./telemetry/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add services/shared/
git commit -s -m "feat(shared/telemetry): implement OpenTelemetry tracer provider, redaction and W3C context helpers"
```

---

### Task 3: Instrument `ollama-proxy` with W3C Cluster Tracing & Metrics

**Files:**
- Create: `services/ollama-proxy/telemetry.go`
- Modify: `services/ollama-proxy/proxy.go`
- Modify: `services/ollama-proxy/go.mod`
- Create: `services/ollama-proxy/telemetry_test.go`

- [ ] **Step 1: Write failing test for `ollama-proxy` telemetry integration**

Create `services/ollama-proxy/telemetry_test.go`:
```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"nvpair-shared/telemetry"
)

func TestOllamaProxySpanCreation(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	// Set up proxy with in-memory trace provider
	// Verify that incoming /api/chat generates pair.proxy.inference span
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/ollama-proxy && go test -run TestOllamaProxySpanCreation`
Expected: FAIL.

- [ ] **Step 3: Implement telemetry instrumentation in `services/ollama-proxy/proxy.go`**

1. Initialize `telemetry.Provider` in `ollama-proxy/main.go`.
2. Wrap `ServeHTTP`:
   - Extract incoming W3C trace context via `telemetry.ExtractHTTPContext(r)`.
   - Start span `pair.proxy.inference`.
   - Record `gen_ai.request.model`, `http.route`, `pair.client.protocol = "ollama"`.
3. In `chooseNode`:
   - Start child span `pair.router.schedule`.
   - Set `pair.route.candidate_count`, `pair.route.selected_node_id`, `pair.route.gpu_pressure`, `pair.route.queue_depth`, `pair.route.cache_affinity`.
4. In `forwardRequest`:
   - Start child span `pair.engine.dispatch`.
   - Inject W3C trace context via `telemetry.InjectHTTPContext(ctx, outReq)`.
   - Meter response stream: record `pair.inference.ttft_ms` when first chunk arrives.
   - Extract `eval_count` and `prompt_eval_count` from Ollama's final JSON frame.
   - If `p.RecordPayloads()` is true, record prompt and completion text.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/ollama-proxy && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/ollama-proxy/
git commit -s -m "feat(ollama-proxy): instrument inference pipeline and W3C trace propagation"
```

---

### Task 4: Instrument `lmstudio-proxy` & `omlx-proxy`

**Files:**
- Create: `services/lmstudio-proxy/telemetry.go`
- Modify: `services/lmstudio-proxy/proxy.go`
- Modify: `services/lmstudio-proxy/go.mod`
- Create: `services/lmstudio-proxy/telemetry_test.go`
- Modify: `services/omlx-proxy/`

- [ ] **Step 1: Write unit tests for `lmstudio-proxy` OpenTelemetry spans**

Test that OpenAI-compatible routes (`/v1/chat/completions`) extract `model` from JSON body and record TTFT on SSE `data: ` chunks.

- [ ] **Step 2: Implement telemetry in `lmstudio-proxy` and `omlx-proxy`**

Add root span `pair.proxy.inference`, router schedule span, and dispatch span with `pair.client.protocol = "openai"`. Inject W3C traceparent on remote node dispatches.

- [ ] **Step 3: Run proxy tests**

Run: `cd services/lmstudio-proxy && go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add services/lmstudio-proxy/ services/omlx-proxy/
git commit -s -m "feat(lmstudio-proxy): instrument OpenAI proxy and MLX proxy with OpenTelemetry"
```

---

### Task 5: Telemetry Settings in `nvpair-node-settings`

**Files:**
- Modify: `services/nvpair-node-settings/manager.go`
- Modify: `services/nvpair-node-settings/manager_test.go`

- [ ] **Step 1: Write failing tests for telemetry settings in `manager_test.go`**

Add tests for:
- `settings/get-telemetry-enabled`, `settings/set-telemetry-enabled`
- `settings/get-telemetry-endpoint`, `settings/set-telemetry-endpoint`
- `settings/get-telemetry-record-payloads`, `settings/set-telemetry-record-payloads`

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/nvpair-node-settings && go test ./...`
Expected: FAIL.

- [ ] **Step 3: Implement settings in `manager.go`**

Add fields to settings struct and register JSON-RPC get/set handlers:
- `TelemetryEnabled` (`bool`, default `true`)
- `TelemetryEndpoint` (`string`, default `"localhost:4317"`)
- `TelemetryRecordPayloads` (`bool`, default `false`)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/nvpair-node-settings && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-node-settings/
git commit -s -m "feat(node-settings): add telemetry persistence keys for endpoint and payload capture"
```

---

### Task 6: Broker Lifecycle & Status Handlers (`nvpair-ui-broker`)

**Files:**
- Create: `services/nvpair-ui-broker/broker_telemetry.go`
- Modify: `services/nvpair-ui-broker/broker.go`
- Create: `services/nvpair-ui-broker/broker_telemetry_test.go`

- [ ] **Step 1: Write failing test for `telemetry/get-status`, `telemetry/start`, `telemetry/stop`**

Create `services/nvpair-ui-broker/broker_telemetry_test.go` testing JSON-RPC method dispatch and status payload generation.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd services/nvpair-ui-broker && go test -run TestTelemetryHandlers`
Expected: FAIL.

- [ ] **Step 3: Implement `broker_telemetry.go`**

1. `handleTelemetryGetStatus`:
   - Dial `:4317` with 500ms timeout to verify collector reachability.
   - Probe Docker container status (`docker inspect nvpair-phoenix`).
   - Return `{ enabled, endpoint, uiUrl: "http://localhost:6006", recordPayloads, containerState, collectorReachable }`.
2. `handleTelemetryStart`:
   - Execute `docker compose -f <path> up -d`.
   - Poll `:4317` up to 10s until accepting connections.
3. `handleTelemetryStop`:
   - Execute `docker compose -f <path> stop`.
   - Return updated status.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd services/nvpair-ui-broker && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-ui-broker/
git commit -s -m "feat(broker): implement telemetry start, stop, and status JSON-RPC handlers"
```

---

### Task 7: Zero-Config Cluster Discovery in `nvpair-node-scanner`

**Files:**
- Modify: `services/nvpair-node-scanner/scanner.go` (or discovery advertisement)
- Modify: `services/nvpair-node-scanner/scanner_test.go`

- [ ] **Step 1: Write test for advertising `otel=4317` TXT record**

Test that when local Phoenix is detected, mDNS record advertises the OTel port.

- [ ] **Step 2: Implement OTel mDNS advertisement & discovery**

When `collectorReachable` is true, add `otel=4317` to TXT records so peer nodes in the cluster can auto-route spans to the central Phoenix node.

- [ ] **Step 3: Run scanner tests**

Run: `cd services/nvpair-node-scanner && go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add services/nvpair-node-scanner/
git commit -s -m "feat(scanner): advertise OTel endpoint in mDNS for zero-config cluster tracing"
```

---

### Task 8: Desktop IPC & Service Bridge

**Files:**
- Modify: `desktop/src/shared/types/ipc-channels.ts`
- Modify: `desktop/src/electron/service-bridge/`
- Modify: `desktop/src/ui/api/pair-api.ts`

- [ ] **Step 1: Define TypeScript contracts for telemetry methods**

Add `TelemetryStatus`, `telemetryStart`, `telemetryStop`, `telemetryGetStatus` to IPC definitions without `any` or `as` casts.

- [ ] **Step 2: Implement Electron bridge relay**

Relay `telemetry/*` IPC calls through the broker JSON-RPC stream.

- [ ] **Step 3: Run contract check**

Run: `npm run service-contracts:write && npm run service-contracts:check` in `desktop/`
Expected: PASS with contracts up to date.

- [ ] **Step 4: Commit**

```bash
git add desktop/
git commit -s -m "feat(desktop/bridge): expose telemetry IPC channels and service contracts"
```

---

### Task 9: Desktop UI Settings Component (`ObservabilityCard.tsx`)

**Files:**
- Create: `desktop/src/ui/components/ServiceSettings/ObservabilityCard.tsx`
- Modify: `desktop/src/ui/components/ServiceSettings/ServiceSettings.tsx`
- Create: `desktop/src/ui/components/ServiceSettings/ObservabilityCard.test.tsx`

- [ ] **Step 1: Write component test for `ObservabilityCard`**

Test:
- Displays `Stopped` badge when Phoenix is offline.
- Clicking **[ Start Phoenix ▶ ]** enters loading state and invokes `telemetryStart`.
- Displays `Connected` badge and **[ Open Dashboard ↗ ]** when online.
- Clicking **[ Open Dashboard ↗ ]** opens `http://localhost:6006` in external browser.
- Displays inline warning banner if Docker is offline.

- [ ] **Step 2: Implement `ObservabilityCard.tsx`**

Using Foundations React Core components (`Button`, `Badge`, `Stack`, `Flex`, `Text`, `Toggle`):
- Start / Stop Phoenix action buttons.
- Real-time status badge (`Connected ●` / `Stopped ○`).
- "Capture Prompt & Response Text (For Evaluations)" toggle with local-storage privacy disclaimer.
- Link to open `http://localhost:6006`.

- [ ] **Step 3: Embed card in `ServiceSettings.tsx`**

Add `<ObservabilityCard />` below `RoutingDiagnostics`.

- [ ] **Step 4: Run desktop unit tests and typecheck**

Run: `npm run typecheck && npm run lint && npm run test:unit` in `desktop/`
Expected: PASS with 0 lint errors, 0 type errors, 0 dead code warnings.

- [ ] **Step 5: Commit**

```bash
git add desktop/
git commit -s -m "feat(ui): add ObservabilityCard with Phoenix lifecycle controls to Service Settings"
```

---

### Task 10: Verification, Version Bumps & Documentation Update

**Files:**
- Modify: `AGENTS.md`
- Modify: `services/versions.json`
- Update: `docs/developing.mdx` / `docs/architecture.mdx`

- [ ] **Step 1: Update `AGENTS.md` conventions**

Document the logging vs. local evaluation telemetry distinction:
```markdown
- **Never log prompts, messages, response bodies, pairing PINs, or key material** in standard system log sinks (`slog`, `applog`, stderr, file logs). Telemetry spans may optionally capture prompt and completion payloads directly to the local OpenTelemetry collector ONLY when explicitly enabled by the user for local AI metrics and evaluation. Pairing PINs, auth keys, and credentials remain strictly forbidden in all sinks at all times.
```

- [ ] **Step 2: Bump versions in `services/versions.json`**

Bump SemVer for modified services (`nvpair-ui-broker`, `ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`, `nvpair-node-settings`, `nvpair-node-scanner`).

- [ ] **Step 3: Run full repository checks**

Run: `make check && npm run dead-code:check`
Expected: All checks pass clean.

- [ ] **Step 4: Commit**

```bash
git add AGENTS.md services/versions.json docs/
git commit -s -m "docs: update AGENTS.md conventions and bump service versions for OpenTelemetry"
```
