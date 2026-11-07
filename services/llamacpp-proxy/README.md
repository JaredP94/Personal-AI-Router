<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# llama.cpp proxy

A separate supervised OpenAI-compatible proxy for llama.cpp, copied from
[`lmstudio-proxy`](../lmstudio-proxy/README.md). Its control protocol, routing,
strict routing, streaming, cancellation, bounded request bodies, scheduler
priority, workload identity, telemetry, stall retry/cooldown and opt-in
`--first-body-timeout` behavior match that worker. LM Studio is unchanged.

Default proxy listener: **1237**. The managed engine listens on **127.0.0.1:8082**.
1237 is separate from Ollama (11434), LM Studio (1234), oMLX (1236), their
engine listeners, and node services (14318–14323). `--port` overrides the default;
`set-port` persists to `llamacpp-proxy-port.json`; `--ignore-persisted-port`
selects the broker's accepted settings instead. Stored engine port 8082 is not
restored as a proxy listener. Shared listen conflict checks apply.

`GET /v1/models` and `GET /models` aggregate the OpenAI `data` envelope from
candidate `/models` endpoints. IDs are exact, without implied tags. Model-bearing
inference only considers advertised llama.cpp owners, never another engine's
inventory. Supported inference routes are `POST /v1/chat/completions`,
`/v1/completions`, `/v1/embeddings`, and **`/v1/messages`**. The latter is native
in ggml-org/llama.cpp **b12171** (`tools/server/server.cpp` registers
`routes.post_anthropic_messages`); PAIR does not translate protocols. Streaming
responses pass through without retry after body commitment.

The worker subscribes to `discovery:subscribe {services:["lc"]}`. Only verified
cluster peers are routing candidates; previews are not. Manual bare engines
follow the existing explicit-manual rules. Pinned mTLS peer ingress is terminal:
it forwards only to the broker-supplied healthy loopback engine, never onward.

## Control protocol

Newline-delimited JSON-RPC 2.0 on stdio, or `--ipc`. The broker owns lifecycle
and relays client calls and subscribed events under **`llamacpp-proxy:`**.

- `worker/health` → `{ok:true}` (control-loop responsiveness only).
- `nodes/list`, `node/select`, `node/add-manual`, `node/remove-manual`.
- `node/set-priority`, `node/set-local-backend`, discovery snapshots.
- `set-port`, `port/cancel`, `log/set-level`, telemetry configuration.
- `ready`, `error`, `node/discovered`, `node/updated`, `node/removed`,
  `node/selection-changed`, `proxy/request-started`, `proxy/request`,
  `workload:started`, `workload:completed`, `workload:errored`, progress/activity.

Workloads use `engine:"llamacpp"`, with per-process run identity. The broker
stamps their origin and forwards them to workload-manager. See the LM Studio
worker reference for unchanged payload shapes and flags. `--version` is stamped
from `services/versions.json`; build with `go build .` in this module.

There is no model hub/download/delete adapter yet. Start llama.cpp with an
allowlisted `--model`/`-m` GGUF inside `{install_dir}-models`; see
[`LLAMACPP.md`](../nvpair-engine-manager/LLAMACPP.md). Starting without a model
retains router mode.

The model-list rewrite and exact-ID semantics are adapted from
NVIDIA/Personal-AI-Router@6d2d67f's engine profile.
