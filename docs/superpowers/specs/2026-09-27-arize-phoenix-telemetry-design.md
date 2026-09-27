<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# OpenTelemetry & Arize Phoenix Observability Integration Design

## 1. Overview & Objectives

This design specifies the integration of an OpenTelemetry (OTel)-compatible observability and evaluation service into the **NVIDIA Personal AI Router (PAIR)**. 

### Core Goals:
1. **Inference Observability:** Measure and visualize operational performance metrics across all PAIR proxies (`ollama-proxy`, `lmstudio-proxy`, `omlx-proxy`), including Time to First Token (TTFT), token generation throughput (tokens/sec), prompt/completion token counts, total request latency, and HTTP error rates.
2. **Cluster Routing Traces:** Provide distributed tracing of routing decisions—capturing cache affinity matches, candidate nodes evaluated, queue depth, GPU pressure scoring, and failover events.
3. **AI Evaluations (Optional Opt-In):** Provide dual-mode telemetry payload recording. By default, operational metadata only is recorded. When explicitly enabled by the user, prompt and completion payloads are captured into the local Arize Phoenix database to power AI evaluation metrics (hallucination detection, QA accuracy, toxicity, prompt comparison).
4. **Lightweight Local & Cluster Deployment:**
   - Powered by **Arize Phoenix** running in a single Docker container.
   - For a multi-node cluster, **only one node** runs the Phoenix container; all peer nodes forward their OTLP spans across the LAN to the central instance.
   - Managed directly from the PAIR Desktop UI via **Start / Stop** buttons and real-time connection status monitoring.

---

## 2. Architecture & Topology

### A. Standalone vs. Multi-Node Cluster Topology

```
[ Standalone Node ]
┌──────────────────────────────────────────────┐
│ PAIR Node (Workstation)                      │
│                                              │
│ Client Request                               │
│      │                                       │
│      ▼                                       │
│ PAIR Proxies (Ollama / LM Studio / MLX)      │
│      │                                       │
│      ├─► OTLP gRPC (localhost:4317) ─────────┼──► [ Arize Phoenix Docker ]
│      │                                       │    Port 6006: Web Dashboard
│      ▼                                       │    Port 4317: OTLP Ingest
│ Local Engine (Ollama / LM Studio)            │
└──────────────────────────────────────────────┘

[ Multi-Node Cluster (Single Central Phoenix Instance) ]
     Node A (Laptop)                               Node B (Primary Workstation)
┌──────────────────────┐                     ┌──────────────────────────────────────┐
│ Client Request       │                     │ Target Engine (Ollama / MLX)         │
│      │               │                     │ Running 70B model                    │
│      ▼               │                     └──────────────────▲───────────────────┘
│ ollama-proxy         │                                        │
│ [Spans Generated]    │                     1. LAN HTTP + W3C Trace Context
│                      ├────────────────────────────────────────┘
│                      │                     2. OTLP gRPC (:4317)
│                      ├────────────────────────────────────────┐
└──────────────────────┘                                        ▼
                                             ┌──────────────────────────────────────┐
                                             │ Arize Phoenix (Docker Container)     │
                                             │ Port 6006: UI | Port 4317: OTLP      │
                                             │ • Cluster-wide unified metrics       │
                                             │ • Cross-node distributed traces      │
                                             └──────────────────────────────────────┘
```

- **Single Container Per Cluster:** Only one machine in the cluster hosts Phoenix. Peer nodes require zero container overhead.
- **W3C Distributed Tracing:** Forwarded requests carry standard W3C `traceparent` HTTP headers, stitching client ingress, router scheduling, network dispatch, and remote engine execution into a single unified trace.
- **Fail-Open Design:** If Phoenix is offline or unreachable, proxies drop telemetry in memory with zero impact on inference latency or routing reliability.

---

## 3. Docker Deployment & Lifecycle Management

### A. Docker Compose Specification (`docker-compose.telemetry.yml`)
Located at the repository root:
```yaml
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

### B. Broker Lifecycle Commands (`nvpair-ui-broker`)
The parent broker service orchestrates Docker lifecycle via JSON-RPC:

1. **`telemetry/get-status`**:
   - Probes TCP port `:4317` (OTLP) and `:6006` (UI) for reachability.
   - Inspects Docker daemon state and container state (`running`, `stopped`, `not_installed`).
   - Returns:
     ```json
     {
       "enabled": true,
       "endpoint": "localhost:4317",
       "uiUrl": "http://localhost:6006",
       "recordPayloads": false,
       "containerState": "running",
       "collectorReachable": true
     }
     ```
2. **`telemetry/start`**:
   - Executes `docker compose -f docker-compose.telemetry.yml up -d`.
   - Polls `:4317` until ready (bounded 10s timeout).
   - Returns human-friendly error if Docker is not installed or the Docker daemon is offline.
3. **`telemetry/stop`**:
   - Executes `docker compose -f docker-compose.telemetry.yml stop`.
   - Confirms port release and returns updated status.

---

## 4. Telemetry Data Model & Semantic Conventions

### A. Trace Structure per Inference Request

```
[Trace: pair.inference]
│
├── Span 1 (SERVER): "pair.proxy.inference"
│   │
│   ├── Span 2 (INTERNAL): "pair.router.schedule"
│   │
│   └── Span 3 (CLIENT): "pair.engine.dispatch"
│         Events: "first_token" (TTFT timestamp)
```

### B. Span Attributes

#### 1. Ingress Span (`pair.proxy.inference`)
* `gen_ai.system`: `"pair"`
* `gen_ai.request.model`: e.g. `"llama3.2:3b"`
* `http.request.method`: `"POST"`
* `http.route`: `"/api/chat"`, `"/v1/chat/completions"`, etc.
* `http.response.status_code`: HTTP status code
* `pair.client.protocol`: `"ollama"` | `"openai"` | `"mlx"`
* `pair.is_streaming`: `true` | `false`
* `pair.node.id`: Local node UUID
* `pair.node.name`: Friendly host name

#### 2. Scheduler Span (`pair.router.schedule`)
* `pair.route.candidate_count`: Total nodes hosting the requested model
* `pair.route.selected_node_id`: Target node ID
* `pair.route.is_local`: Boolean indicating if destination is local or remote LAN
* `pair.route.gpu_pressure`: Smoothed GPU pressure score (0–3)
* `pair.route.queue_depth`: Target node pending jobs count
* `pair.route.cache_affinity`: Boolean (true on KV cache prefix hit)
* `pair.route.decision_reason`: `"cache_hit"` | `"lowest_pressure"` | `"single_owner"`

#### 3. Engine Dispatch Span (`pair.engine.dispatch`)
* `gen_ai.response.model`: Model returned in response metadata
* `gen_ai.usage.input_tokens`: Prompt token count
* `gen_ai.usage.output_tokens`: Completion token count
* `pair.inference.ttft_ms`: Time to first token in milliseconds
* `pair.inference.tokens_per_second`: Computed generation rate
* `pair.route.failover`: `true` if this attempt followed an engine failover

---

## 5. Privacy, Redaction & Dual-Mode Payload Capture

### A. Dual-Mode Telemetry Controls
* **Mode 1 (`telemetry/record-payloads = false` - Default):**
  - Strictly operational metadata.
  - Zero prompt text, message contents, or completion bodies are recorded in OTel spans.
  - Safe for privacy-sensitive and production environments.
* **Mode 2 (`telemetry/record-payloads = true` - Opt-in for Evaluations):**
  - Proxies populate `input.value` (`gen_ai.prompt`) and `output.value` (`gen_ai.completion`) on the span.
  - Enables Arize Phoenix's Evaluation features (evaluating hallucination, toxicity, prompt variants).
  - Explicit warning in UI indicating that text is stored in the local Phoenix database.

### B. Mandatory Permanent Redaction
Regardless of the `record-payloads` setting:
- HTTP `Authorization` headers and bearer tokens are **never** captured.
- Cluster pairing PINs, EAP-NOOB credentials, and TLS private keys are **never** captured.

### C. Updates to `AGENTS.md`
`AGENTS.md` will be updated to document the distinction between system logs and optional local evaluation telemetry:
- Standard log sinks (`slog`, `applog`, stderr, file logs) must remain strictly zero-prompt and zero-body.
- Telemetry spans may include prompt/response payloads only when explicitly configured by the user (`RecordPayloads = true`) for local AI metrics and evaluation. PINs and auth credentials remain strictly forbidden in all sinks.

---

## 6. Implementation Components

### A. Go Services Tree
1. **`services/shared/telemetry` (New Package):**
   - Pure-Go implementation using `go.opentelemetry.io/otel` and `otlptracegrpc`.
   - `Init(serviceName string, cfg Config) (*ShutdownFunc, error)`
   - Non-blocking `sdktrace.NewBatchSpanProcessor`.
   - Redaction helpers and payload scrubbers.
2. **`services/ollama-proxy` & `services/lmstudio-proxy` & `services/omlx-proxy`:**
   - Initialize telemetry tracer from configuration.
   - Instrument request ingress, node selection, and streaming response readers.
   - Inject W3C `traceparent` headers when proxying to remote cluster nodes.
3. **`services/nvpair-node-settings`:**
   - Add typed keys: `telemetry/enabled` (`bool`), `telemetry/endpoint` (`string`), `telemetry/record-payloads` (`bool`).
4. **`services/nvpair-ui-broker`:**
   - Implement `telemetry/get-status`, `telemetry/start`, `telemetry/stop`.
   - Relay configuration changes to proxies.
5. **`services/nvpair-node-scanner`:**
   - Advertise `otel=4317` in mDNS TXT records when local Phoenix is running.
   - Detect advertised peer OTel endpoints for zero-config cluster discovery.

### B. Desktop UI (`desktop/`)
1. **`ObservabilityCard.tsx` in `desktop/src/ui/components/ServiceSettings/`:**
   - Status badge: `Connected ●` (green), `Stopped ○` (gray), or `Starting...` (yellow).
   - Action buttons: **[ Start Phoenix ▶ ]** / **[ Stop Phoenix ⏹ ]**, **[ Open Dashboard ↗ ]** (opens `http://localhost:6006`).
   - Toggles: "Enable OpenTelemetry Spans", "Capture Prompt & Response Text (For Evaluations)".
   - Error banner if Docker daemon is unreachable.
2. **Service Bridge & Store:**
   - IPC handlers in `desktop/src/electron/service-bridge/` for `telemetry/*` methods.
   - Status polling and store synchronization.

---

## 7. Testing & Verification

1. **Unit Tests:**
   - `services/shared/telemetry`: Test span creation, attribute formatting, batch shutdown, and verify secrets/auth headers are strictly redacted.
   - Dual-mode test: Confirm prompt/response text is absent when `RecordPayloads=false` and present when `RecordPayloads=true`.
   - Proxies: Mock tracer test verifying TTFT and token count extraction.
   - Broker: Test `telemetry/start` and `telemetry/stop` command wrappers.
2. **Service Contracts & Checks:**
   - Run `npm run service-contracts:write` and `npm run service-contracts:check`.
   - Run `npm run lint`, `npm run typecheck`, and `npm run dead-code:check`.
3. **End-to-End Live Validation:**
   - Click "Start Phoenix" in PAIR Desktop; confirm container starts.
   - Send requests via `ollama-proxy` and `lmstudio-proxy`.
   - Open `http://localhost:6006` and verify traces appear with correct TTFT, token counts, and routing attributes.
   - Test prompt/completion capture toggle and verify visibility in Phoenix Evals.
   - Click "Stop Phoenix" and confirm clean container shutdown.
