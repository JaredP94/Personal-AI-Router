<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tracing Settings, Privacy and Phoenix Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The tracing controls in Settings actually govern what every proxy exports; tracing and content capture are off until the user turns them on; traces stay on the node that originated them, in a loopback-only, pinned Phoenix whose storage the user can see and clear.

**Architecture:** `nvpair-node-settings` owns one revisioned telemetry configuration and notifies the broker on change. The broker reconciles that configuration into each proxy through an acknowledged `telemetry/configure` control request and reports desired versus effective state. A shared Go `telemetry.Manager` replaces global OpenTelemetry state: each configuration revision gets its own provider behind an export gate, so revoked revisions export nothing more. The broker owns one embedded, pinned, loopback-only Compose template and adds storage readout and scoped purge.

**Tech Stack:** Go (OpenTelemetry SDK v1.34, OTLP gRPC), Docker Compose, TypeScript/Electron/React, Vitest.

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §2, §3 (owner decisions: tracing off by default and reset; per-node collection with no cross-node export; keep traces until the user clears them, with an on-device storage readout).

**Codebase map:** `/tmp/pair-step2/research.md` is a scratch copy; the facts below are the binding ones.

## Rulings Made While Planning

- **Proxies no longer read `PAIR_TELEMETRY_*` / `OTEL_*` environment variables.** They start disabled and are configured only by the broker; a standalone proxy without a broker does not trace. (Spec §2.1: environment must not defeat consent.)
- **The in-app Phoenix image update feature is removed** (`telemetry/check-update`, `telemetry/update`, the update banner). A pinned, reviewed image is updated with PAIR releases. (Spec §3.1 pins image and digest; "check for latest" contradicts it.)
- **The configurable destination is a loopback port** (`collectorPort`, default 4317); exporters always dial `127.0.0.1:<port>`.
- **The repository-root `docker-compose.telemetry.yml`, `scripts/telemetry-phoenix.sh`, their `extraResources`, `--telemetry-compose-path` and `NVPAIR_TELEMETRY_COMPOSE_PATH` are removed**; the broker's embedded template is the single source.
- **Status reports delivery health** from a proxy `telemetry/status` request (last export success/failure), queried by the broker during `telemetry/get-status`.

## Global Constraints

- Work on branch `release/tracing-privacy` (from `main` @ `52cdd0d`); signed-off commits (`git commit -s`).
- Every new file carries the two-line SPDX header; `node scripts/spdx-headers.mjs` passes.
- Go owns policy; desktop only relays and renders. No TypeScript reimplementation of validation beyond UI input hints.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures; `@/` imports; static imports; no renderer imports from `src/electron/`; unused exports fail `npm run dead-code:check`.
- No legacy fallbacks: delete replaced methods, flags, env handling and files in the same task that replaces them.
- Never log prompts, completions, PINs, credentials or key material in `slog`/applog/stderr/file logs.
- Content capture is local-only: nothing in this plan may send spans to another node.
- JSON-RPC changes: update producer, broker relay, desktop bridge, tests and docs together; regenerate `desktop/docs/services-api.md` with `npm run service-contracts:write` (never by hand).
- Tests must not touch real Docker, real `~/Library/Application Support` data, or real engines; use the existing fakes (`mockDockerRunner`, `tracetest.InMemoryExporter`, `newTestManager`, capture codecs).

## Review Focus

1. **Opt-out leak:** after `telemetry/configure` disables tracing or capture, no span from the earlier revision is exported, including spans already queued in the batch processor and completion text buffered by an in-flight streaming response. Tests in Tasks 1 and 2.
2. **Fail closed when anything is missing:** proxy started but never configured, settings worker absent, `telemetry/configure` failing, or a stale revision arriving late — export stays disabled or at the newest acknowledged revision; never enabled by default. Tests in Tasks 2, 4 and 5.
3. **Migration does not keep implicit consent:** a pre-existing `settings.json` with `telemetry_enabled: true` and `telemetry_record_payloads: true` loads as disabled with no consent, while unrelated settings survive. Test in Task 4.
4. **Purge cannot delete the wrong thing:** a volume without PAIR's labels, a volume used by a running container, or a request without the confirmation token is refused. Tests in Task 7.
5. **Headers and metadata:** an `Authorization`, cookie, custom `X-Secret` header or query string never appears in any exported span attribute in metadata-only or capture mode. Tests in Tasks 1 and 2.

---

### Task 1: Shared telemetry manager with export gate and capture policy

**Files:**
- Modify: `services/shared/telemetry/telemetry.go`, `config.go`; replace `redact.go` with `capture.go`
- Test: `services/shared/telemetry/telemetry_test.go` (rewrite affected tests), `capture_test.go`

**Interfaces (Produces):**
- `type Settings struct { Enabled bool; CollectorPort int; RecordPayloads bool }`
- `func ValidateSettings(s Settings) error` — port 1–65535; `RecordPayloads` requires `Enabled`.
- `type ExporterFactory func(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error)`; `func OTLPExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error)` (OTLP gRPC, insecure transport, loopback only).
- `func NewManager(serviceName string, newExporter ExporterFactory) *Manager`
- `func (m *Manager) Apply(ctx context.Context, revision uint64, s Settings) (applied uint64, err error)` — `revision < current` → no change, returns current; `== current` → no-op; invalid settings → error, no change. Endpoint is `127.0.0.1:<CollectorPort>`.
- `func (m *Manager) Session() *Session` (never nil); `func (m *Manager) Status() Status`; `func (m *Manager) Shutdown(ctx context.Context) error`
- `type Session` (immutable per revision): `Tracer() trace.Tracer` (an OTel no-op tracer when disabled), `Revision() uint64`, `RecordPayloads() bool` (false once the session is superseded), `Active() bool`.
- `type Status struct { Revision uint64; Enabled bool; LastExportOK time.Time; LastExportError string; LastExportErrorAt time.Time }`
- Capture: `const MaxPayloadBytes = 16 << 10`; `func TruncateUTF8(s string, max int) (string, bool)`; `func RequestAttributes(h http.Header) []attribute.KeyValue` — only `http.request.header.content-type` and `http.request.header.content-length`, values truncated to 256 bytes.
- Delete: `Init`, `Provider`, `RedactHeaders`, any `otel.SetTracerProvider`/`otel.SetTextMapPropagator` call, `DefaultConfig`/env-oriented `Config`. Keep `ExtractHTTPContext` (local client `traceparent` only) using a package-level W3C propagator, not the global one. Delete `InjectHTTPContext`.
- Each session owns its `sdktrace.TracerProvider` whose exporter is wrapped by a gate: once the session is superseded or the manager shut down, `ExportSpans` drops everything and returns nil. Superseded providers are shut down asynchronously with a 2-second context, which also bounds any export RPC already in flight; shutdown flushes pass through the gate and are dropped. Spans already delivered before the switch cannot be recalled (documented, not tested).

- [ ] **Step 1: Failing tests** using a recording exporter via `ExporterFactory`:
  - `TestDisabledSessionUsesNoopTracer`: default manager → `Session().Tracer()` spans are not recorded, factory never called.
  - `TestApplyIgnoresStaleRevision`: apply 3 then 2 → status revision 3.
  - `TestRevokedRevisionExportsNothing`: apply rev 1 enabled; start+end a span; apply rev 2 disabled; force the old provider to flush → recording exporter received 0 spans from rev 1 after the switch (end spans before revoke but flush after).
  - `TestCaptureRevokedSessionReportsNoPayloads`: session from rev 1 with payloads → after apply rev 2, `RecordPayloads()` is false and `Active()` false.
  - `TestNoGlobalProviderMutation`: `otel.GetTracerProvider()` is identical before and after `Apply`.
  - `TestRequestAttributesAllowlist`: headers `Authorization`, `Cookie`, `X-Secret`, `Content-Type: application/json`, `Content-Length: 12` → exactly the two allowlisted attributes.
  - `TestTruncateUTF8`: `"héllo"` with max 2 → `"h"`, true; max 6 → unchanged, false.
  - `TestValidateSettings`: port 0 and 70000 rejected; payloads without enabled rejected.
- [ ] **Step 2:** `cd services/shared && go test ./telemetry/...` — FAIL.
- [ ] **Step 3:** Implement. The module will not compile its dependents until Tasks 2–3; that is expected.
- [ ] **Step 4:** `go test -race ./telemetry/...` — PASS.
- [ ] **Step 5: Commit** — `feat(telemetry): revision-gated tracer sessions without global state`

### Task 2: ollama-proxy adopts the manager and `telemetry/configure`

**Files:** `services/ollama-proxy/{main.go,proxy.go,telemetry.go,ingress.go}`, tests `services/ollama-proxy/telemetry_test.go` (+ any test constructing `NewProxy` with a provider)

**Interfaces:**
- Consumes Task 1.
- Proxy field `telemetry *telemetry.Manager`, constructed disabled in `main.go` with `telemetry.OTLPExporter`; delete `defaultTelemetryConfig` and every `PAIR_TELEMETRY_*`/`OTEL_*` read.
- JSON-RPC requests in `handleMessage`:
  - `telemetry/configure` params `{"revision": uint64, "enabled": bool, "collectorPort": int, "recordPayloads": bool}` → result `{"revision": uint64}` (the effective revision after apply; stale requests return the current one). Invalid params → error `-32602`.
  - `telemetry/status` → `{"revision": uint64, "enabled": bool, "lastExportOk": string|null, "lastExportError": string|null, "lastExportErrorAt": string|null}` (RFC 3339 UTC).
- Each inbound request takes one `Session` snapshot at the start and uses it for every span and capture decision of that request. `telemetryBodyReader` holds the session and stops appending completion text, and omits payload attributes at finalize, once `RecordPayloads()` is false.
- Replace `recordRequestHeaders` with `telemetry.RequestAttributes`. Prompt and completion attributes use `TruncateUTF8(..., telemetry.MaxPayloadBytes)` and add `gen_ai.prompt.truncated` / `gen_ai.completion.truncated` booleans when cut. Remove the previous 64 KiB completion cap constant. Free-form metadata attributes (model name, route, error message) are truncated to 256 bytes with `TruncateUTF8`; error attributes never include upstream response bodies.
- Remove the `pair.cluster.ingress` span, `ExtractHTTPContext` on ingress, and every `InjectHTTPContext` (outbound dispatch and local reverse proxy). Keep `ExtractHTTPContext` for the local loopback client request only.

- [ ] **Step 1: Failing tests** (in-memory exporter injected through the manager's factory):
  - `TestUnconfiguredProxyExportsNothing`.
  - `TestConfigureEnablesAndStaleRevisionIgnored`: configure rev 2 enabled → spans exported; configure rev 1 disabled → result revision 2, still exporting.
  - `TestDisableMidStreamDropsCompletion`: start a streaming request with capture on, configure capture off before the body finishes → no exported span contains `gen_ai.completion`/`output.value`.
  - `TestNoPeerTraceContext`: a request dispatched to a peer carries no `traceparent` header; cluster ingress creates no span.
  - `TestSensitiveHeadersNeverExported`: request with `Authorization`, `Cookie`, `X-Secret`, `?api_key=x` in the URL → no exported attribute value contains those strings, in both capture modes.
  - `TestPayloadTruncation`: 20 KiB prompt → attribute length ≤ 16 KiB and truncated flag true.
- [ ] **Step 2:** `cd services/ollama-proxy && go test ./...` — FAIL.
- [ ] **Step 3:** Implement; update existing telemetry tests that asserted ingress spans or traceparent injection to the new behavior (delete assertions for removed behavior rather than skipping them).
- [ ] **Step 4:** `go test -race ./...` — PASS.
- [ ] **Step 5: Commit** — `feat(ollama-proxy): broker-configured tracing with revocable capture`

### Task 3: lmstudio-proxy and omlx-proxy mirror Task 2

**Files:** the same files in `services/lmstudio-proxy/` and `services/omlx-proxy/`.

- [ ] **Step 1:** Port Task 2's tests (same names and assertions) to both proxies — FAIL.
- [ ] **Step 2:** Apply the identical changes; keep each proxy's service name.
- [ ] **Step 3:** `go test -race ./...` in both modules — PASS.
- [ ] **Step 4: Commit** — `feat(proxies): broker-configured tracing for LM Studio and oMLX proxies`

### Task 4: Revisioned telemetry configuration in node settings

**Files:** `services/nvpair-node-settings/manager.go`, `manager_test.go`, `README.md`

**Interfaces:**
- `settings.json` gains `"schema_version": 2`, `"telemetry_revision"`, `"telemetry_collector_port"`, `"telemetry_payload_consent_version"`; `telemetry_endpoint` is removed.
- `const TelemetryPayloadPolicyVersion = 1`
- Migration in `load()`: a file without `schema_version` (or `< 2`) keeps every non-telemetry field, sets telemetry `enabled=false`, `record_payloads=false`, `consent=0`, `collector_port=4317`, `revision=1`, `schema_version=2`, and persists immediately. Missing file → same telemetry defaults. Corrupt file → existing `.corrupt-<unix>` rename, then defaults.
- `settings/get-telemetry-config` → `{"revision", "enabled", "collectorPort", "recordPayloads", "payloadConsentVersion", "payloadPolicyVersion"}`.
- `settings/set-telemetry-config` params `{"enabled", "collectorPort", "recordPayloads", "payloadConsentVersion"}` (all required). Rules: invalid port or `recordPayloads && !enabled` → `-32602`; `recordPayloads && payloadConsentVersion != TelemetryPayloadPolicyVersion` → `-32602` with message `payload capture requires consent to policy version 1`. If anything changed: `revision++`, persist, notify `settings/telemetry-changed` with the same shape as the getter. Unchanged → no revision bump, no notification. Returns the getter shape.
- Delete the six `settings/{get,set}-telemetry-{enabled,endpoint,record-payloads}` methods.

- [ ] **Step 1: Failing tests:** `TestTelemetryMigrationDropsImplicitConsent` (fixture v1 file with telemetry true/true and `cluster_id` set → disabled, no consent, `cluster_id` preserved, file rewritten with schema 2), `TestTelemetryDefaultsOff`, `TestSetTelemetryConfigBumpsRevisionAndNotifies`, `TestSetTelemetryConfigUnchangedIsSilent`, `TestPayloadCaptureRequiresConsent`, `TestRemovedTelemetryMethodsAreUnknown` (`-32601`).
- [ ] **Step 2:** `go test ./...` — FAIL. **Step 3:** implement. **Step 4:** PASS (`-race`).
- [ ] **Step 5: Commit** — `feat(settings): revisioned telemetry configuration with explicit consent`

### Task 5: Broker reconciles telemetry into proxies and reports effective state

**Files:** `services/nvpair-ui-broker/{broker.go,broker_telemetry.go,proxy.go,lmstudioproxy.go,omlxproxy.go}`, new `telemetry_reconcile.go`, tests `telemetry_reconcile_test.go`, `broker_telemetry_test.go`

**Interfaces:**
- Consumes Task 2–4 methods.
- Desired config: loaded with `settings/get-telemetry-config` whenever the settings worker becomes ready (start and restart) and updated from `settings/telemetry-changed`; no settings worker → desired is disabled, revision 0.
- `reconcileTelemetry()` (serialized; latest desired wins): for each running proxy, `p.Call(ctx 5s, "telemetry/configure", desired)`; record `workerTelemetryState{Name string; AppliedRevision uint64; State string /* "applied"|"pending"|"failed"|"unavailable" */; Error string}`. Run after each proxy announces `ready`, including restarts, and after every desired change.
- `telemetry/get-status` result becomes: `{"revision","enabled","collectorPort","recordPayloads","payloadConsentVersion","payloadPolicyVersion","workers":[{"name","appliedRevision","state","error","lastExportOk","lastExportError","lastExportErrorAt"}],"containerState","collectorReachable","uiUrl","imageVersion","bindingsLoopbackOnly","managedConfig","storage"}`. `workers[].lastExport*` come from `telemetry/status` (2 s timeout each; failures leave them null). `uiUrl` is `http://127.0.0.1:6006`. Fields for `bindingsLoopbackOnly`, `managedConfig`, `storage` are filled by Tasks 6–7; until then emit `null`.
- Remove `endpoint`, `currentVersion`, `updateAvailable`, `latestVersion` from the result, and `deriveUIURL`.

- [ ] **Step 1: Failing tests** with fake proxy peers (follow `applyPriorityToProxy` tests): `TestReconcileAppliesDesiredToEveryProxy`, `TestProxyRestartGetsLatestConfig`, `TestSettingsMissingKeepsProxiesDisabled`, `TestConfigureFailureReportedAsFailed`, `TestLateOldReconcileCannotOverwriteNewer` (two desired revisions racing; final applied is the newer), `TestStatusReportsDesiredAndEffective`.
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test -race ./...` PASS.
- [ ] **Step 5: Commit** — `feat(broker): reconcile telemetry settings into proxies and report effective state`

### Task 6: Managed, pinned, loopback-only Phoenix

**Files:** `services/nvpair-ui-broker/broker_telemetry.go`, `main.go`, new `telemetry/phoenix-compose.yml.tmpl` (embedded with `go:embed`), tests; delete root `docker-compose.telemetry.yml`, `scripts/telemetry-phoenix.sh`; `desktop/electron-builder.config.ts` (remove their `extraResources`), `desktop/src/electron/service-bridge/modular-supervisor.ts` (remove `resolveTelemetryComposePath` and the flag), `desktop/tests/modular/telemetry-compose-path.test.ts` (delete)

**Interfaces:**
- Template renders: image `arizephoenix/phoenix:<version>@sha256:<digest>` — look up the current stable Phoenix release tag and its digest from Docker Hub at implementation time and record both in a comment with the lookup date; ports `127.0.0.1:6006:6006` and `127.0.0.1:{{.CollectorPort}}:4317`; container `nvpair-phoenix`; labels `nvpair.managed=true`, `nvpair.managed-config=<sha256 of rendered file>`; volume `nvpair-phoenix-data` with `name: nvpair-phoenix-data` and label `nvpair.managed=true`.
- The broker writes the rendered file to `appdir/telemetry/phoenix-compose.yml` (0644) and always runs Compose with `-p nvpair-telemetry -f <that file>`. Delete `resolveComposeFilePath`, `defaultTelemetryComposeYAML`, `--telemetry-compose-path`, `NVPAIR_TELEMETRY_COMPOSE_PATH`.
- `telemetry/start`: if a container named `nvpair-phoenix` exists with a Compose project other than `nvpair-telemetry`, stop and remove **that container only** (volumes untouched) before `compose up`.
- Status: `bindingsLoopbackOnly` from `docker inspect` `HostConfig.PortBindings` (true only if every binding's `HostIp` is `127.0.0.1`; null when no container); `managedConfig`: `"current"` when the running container's `nvpair.managed-config` label equals the rendered hash, `"outdated"` otherwise, `"missing"` with no container; `imageVersion` from the pinned tag.
- Delete `telemetry/check-update`, `telemetry/update`, the Docker Hub tag lookup and their tests.

- [ ] **Step 1: Failing tests** with `mockDockerRunner`: rendered template contains only `127.0.0.1:` bindings and a digest-pinned image; `TestStartReplacesForeignProjectContainerKeepsVolumes`; `TestStatusFlagsExposedBindings`; `TestStatusManagedConfigOutdated`; removed methods return `-32601`.
- [ ] **Step 2:** FAIL. **Step 3:** implement and delete the listed files/flags. **Step 4:** broker `go test -race ./...` PASS; from `desktop/`: `npm run typecheck && npm run test:unit`.
- [ ] **Step 5: Commit** — `feat(broker): own a pinned loopback-only Phoenix deployment`

### Task 7: Trace storage readout and scoped purge

**Files:** `services/nvpair-ui-broker/broker_telemetry.go` (+ `dockerRunner`), tests

**Interfaces:**
- `dockerRunner` gains `listVolumes(ctx, labelFilter string) ([]dockerVolume, error)` (`docker volume ls --filter label=… --format '{{json .}}'`), `volumeSizes(ctx) (map[string]int64, error)` (from `docker system df -v --format '{{json .}}'`), `volumeUsers(ctx, name string) ([]string, error)` (`docker ps -a --filter volume=<name> --format '{{.Names}}|{{.State}}'`), `removeVolume(ctx, name string) error`.
- PAIR trace volumes = volumes labelled `com.docker.compose.volume=nvpair-phoenix-data` (current and earlier PAIR Compose projects; earlier installs used a project name derived from the Compose file's directory, so their volumes are included in the readout and the purge). Ruling: these are PAIR-created Phoenix volumes, so clearing them is within "delete stored traces"; any other volume is never touched.
- Status `storage`: `{"bytes": int|null, "volumes": [{"name","bytes"}], "measuredAt": string|null, "error": string|null}`; measurement timeout 10 s; any failure → `bytes: null` with `error`.
- New method `telemetry/purge-storage` params `{"confirm": "delete-traces"}`: wrong/missing token → `-32602`; stop the managed container (20 s); for each PAIR trace volume refuse if any **running** container uses it; remove stopped `nvpair-phoenix` containers referencing it, then `docker volume rm`; never call `prune` or touch other volumes. Result `{"removed": [names], "refused": [{"name","reason"}], "storage": <status storage>}`.

- [ ] **Step 1: Failing tests:** storage sums only labelled volumes; Docker unavailable → `bytes: null` + error; purge refuses without token; purge refuses a volume in use by a running non-PAIR container; purge removes only labelled volumes; no `prune` invocation.
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS (`-race`).
- [ ] **Step 5: Commit** — `feat(broker): report and clear on-device trace storage`

### Task 8: Remove the `otel` discovery advertisement

**Files:** `services/shared/noderec/noderec.go` (+ test), `services/nvpair-ui-broker/advertiser.go` (+ `advertiser_test.go`), `broker_telemetry.go` call sites, `broker.go` (remove `runAutoAdvertiseTelemetry` start), `services/nvpair-node-scanner/{registry_test.go,directory_test.go}`

- [ ] **Step 1:** Delete `ServiceOTel`, `runAutoAdvertiseTelemetry`, `reconcileAdvertiseTelemetry` and all call sites/tests; an `otel=` TXT key from an older peer must parse without error and be ignored — add `TestUnknownOtelKeyIgnored` in noderec first and see it fail to compile/assert, then pass.
- [ ] **Step 2:** `go test ./...` in `shared`, `nvpair-ui-broker`, `nvpair-node-scanner` — PASS.
- [ ] **Step 3: Commit** — `refactor(discovery): drop the unused collector advertisement`

### Task 9: Desktop bridge and Observability card

**Files:** `desktop/src/shared/types/ws-channels.ts`, `desktop/src/electron/service-bridge/empty-handlers.ts`, preload telemetry API, `desktop/src/ui/components/ServiceSettings/ObservabilityCard.tsx`, new `desktop/src/ui/utils/observability-copy.ts`, tests `desktop/tests/modular/telemetry-card.test.ts`, regenerate `desktop/docs/services-api.md`

**Interfaces:**
- Channels: remove `telemetry:check-update`, `telemetry:update`, `telemetry:set-enabled`, `telemetry:set-endpoint`, `telemetry:set-record-payloads`; add `telemetry:set-config` (request `{enabled, collectorPort, recordPayloads, payloadConsentVersion}`, response telemetry config) and `telemetry:purge-storage` (request `{confirm: 'delete-traces'}`, response purge result). `TelemetryStatus` type mirrors Task 5–7's status exactly.
- `handleTelemetryGetStatus` no longer falls back to synthesizing status from settings; errors surface to the card.
- Card behavior: switches default off when status is unavailable; turning capture on opens a confirm modal and sends `payloadConsentVersion = status.payloadPolicyVersion` only after confirmation; per-worker apply state listed; storage line (`formatBytes` + measured time, or "Storage size unavailable"); **Clear stored traces** behind a confirm modal; warnings below.
- Exact copy in `observability-copy.ts` (tested):
  - Consent title `Capture prompts and responses?`; body `PAIR will attach the text of prompts and model responses (up to 16 KB each) to traces sent to Phoenix on this machine. Captured text may contain personal or confidential information and stays in Phoenix's storage until you clear it. Turning capture off stops new capture but can't remove text already stored.`; buttons `Capture text` / `Cancel`.
  - Clear title `Clear stored traces?`; body `This permanently deletes the traces stored by Phoenix on this machine. Phoenix stops while the data is deleted.`; buttons `Delete traces` / `Cancel`.
  - Dropping warning: `Tracing is on, but Phoenix isn't running, so this node's traces are being dropped.`
  - Exposed warning: `Phoenix is reachable from other machines on your network. Restart it to apply the local-only configuration.`
  - Outdated warning: `Phoenix is running an older PAIR configuration. Restart it to apply the current one.`
  - Apply failure: `Tracing settings didn't reach <worker>. Restart PAIR's services from this page if it persists.`
- Remove the update banner, version update check UI and endpoint text field; show a numeric collector-port field (validated 1–65535 for input hints only).

- [ ] **Step 1: Failing tests:** copy strings literal; `observabilityWarnings(status)` returns the right warnings for (enabled, stopped), (bindingsLoopbackOnly false), (managedConfig outdated), (worker failed); bridge `handleTelemetryGetStatus` rejects instead of synthesizing; set-config forwards the consent version only when capture is on.
- [ ] **Step 2:** FAIL. **Step 3:** implement; `npm run service-contracts:write`.
- [ ] **Step 4:** `npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check && npm run service-contracts:check` — PASS.
- [ ] **Step 5: Commit** — `feat(desktop): honest tracing controls with consent and storage management`

### Task 10: Versions, docs and full verification

**Files:** `services/versions.json`, `CHANGELOG.md` (Unreleased, with a **Migration** note: tracing and capture reset to off; `PAIR_TELEMETRY_*`/`OTEL_*` proxy variables, `--telemetry-compose-path`, `NVPAIR_TELEMETRY_COMPOSE_PATH`, the Phoenix update feature and the six settings methods removed; earlier trace volumes remain until cleared), `SECURITY.md` (replace the "current limitations" section with the new boundaries), `docs/architecture.mdx`, `docs/getting-started.mdx`, `docs/known-issues.mdx`, `docs/release-readiness.mdx`, `README.md` fork-enhancement 6, `services/nvpair-ui-broker/README.md`, `services/nvpair-node-settings/README.md`, `services/readme.md`, `desktop/docs/services-backend.md`, `AGENTS.md` (telemetry rule wording if affected).

- [ ] **Step 1:** Versions per `services/VERSIONING.md`: `nvpair-node-settings` and `nvpair-ui-broker` MAJOR (removed methods); the three proxies MINOR (new methods; removed environment handling noted in the changelog); `nvpair-node-scanner` PATCH; product/installer MAJOR. Desktop version is set at release time, not here.
- [ ] **Step 2:** Update the docs to match Tasks 1–9; remove every statement that tracing UI settings are not wired, that `otel=4317` is advertised, or that Compose ports are unrestricted.
- [ ] **Step 3: Full verification**

```bash
node scripts/spdx-headers.mjs
for m in services/*/; do [ -f "$m/go.mod" ] && (cd "$m" && go test ./...) || exit 1; done
(cd services/shared && go test -race ./telemetry/...)
cd desktop && npm run lint && npm run typecheck && npm run test:unit \
  && npm run dead-code:check && npm run service-contracts:check && npm run licenses:check
```

Expected: all pass (live opt-ins unset).

- [ ] **Step 4: Commit** — `docs(telemetry): document per-node tracing, consent and storage controls`
