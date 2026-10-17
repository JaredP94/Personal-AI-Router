<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Release Concerns Remediation — Proposed Design

**Status:** Owner decisions recorded 2026-10-17 (below).
**Baseline:** `release/performance-and-docs`, candidate metadata `0.7.0-rc.2`.
**Inputs:** [release checklist](../../release-readiness.mdx),
[security boundaries](../../../SECURITY.md), and
[performance evidence](../../performance-release-summary.mdx).

## Owner Decisions (2026-10-17)

| Topic | Decision | Effect on this design |
| --- | --- | --- |
| Tracing defaults | Off by default; existing "on" values reset and re-confirmed | §2.1 as written |
| Collection topology | Local only: each node traces only requests it originated, to its own Phoenix; a node without a running collector loses its own traces | §3.2 per-node model; no gateway, remote destination or cross-node export |
| Trace storage | Keep until the user deletes it; show on-device trace storage size in the UI with a clear action | §3.3 adds a storage-usage readout and a confirmed "Clear stored traces" action |
| Envelope parity | Owner delegated; **canonical-envelope enforcement** chosen | §5 recommended resolution adopted |
| Signing | No paid Apple Developer account, so **unsigned limited RC** | §8 unsigned path; helper/firewall automation reported unavailable, no repeated prompts |
| Known limitations | Fix now | §9 items are in scope for the next release |

Content capture therefore stays on the originating device, as current
contributor policy (`AGENTS.md`, `SECURITY.md`) already requires; no policy
change is needed.

## Intent and Scope

Make release claims match effective runtime behavior, protect trace content,
preserve correct scheduling while reducing idle work, close protocol-validation
and attribution gaps, and verify the exact artifacts that will be distributed.
This document proposes implementation and acceptance gates; it does not claim
that any concern is already fixed by writing the design.

The user's requested outcome is a concise performance summary and a design
covering all open release concerns. The security defaults, configuration
contracts, timing budgets, and release ordering below are **recommendations**,
not choices the user has already approved. No tag, publication, repository
setting change, real installation, engine operation, or trace deletion is
authorized by approving documentation alone.

### Constraints / Non-goals

- Go owns configuration policy, discovery, routing, cryptography, and scheduling;
  desktop/TUI relay commands and render authoritative state.
- One canonical path per capability; no insecure or legacy export fallback.
- No prompts, completions, PINs, keys, or credentials in ordinary logs. Content
  capture remains explicitly opted-in and **local-only** under existing policy.
- Do not weaken macOS trust checks, assume license permission, invent benchmark
  provenance, extend sample freshness to disguise old data, or certify untested
  platforms.
- Do not introduce unrelated engine integrations, pooling/sharding, or blanket
  rewrites. Existing product limitations require explicit disposition, not an
  implied promise of universal feature parity.

## 1. Coverage and Closure Map

| Open concern | Remedy | Closure evidence |
| --- | --- | --- |
| UI settings do not configure proxies; environment can override consent | Broker-reconciled, revisioned desired/effective settings; fail-closed provider lifecycle | UI → persistence → every proxy, restart, race and opt-out tests |
| Global tracer fallback, queued/in-flight content during opt-out | Explicit no-op state, generation-gated export and bounded cancellation | No newly exported revoked-generation spans; stale buffers discarded |
| Arbitrary headers/content and collector endpoints | Loopback-only destination, metadata allowlist, local payload consent | Synthetic-secret tests across every capture/export/log path |
| Unrestricted, duplicated/stale Phoenix Compose; plaintext OTLP | Versioned loopback-only managed deployment; per-node collection with no cross-node export | Real bind checks, migration tests, non-loopback endpoint rejection |
| Misleading collector advertisement/status and retention | Remove `otel` advertisement; distinguish settings, delivery, storage and deletion | Accurate status, storage readout and scoped purge tests |
| Idle sampling versus scheduling freshness | Independent telemetry delivery, bounded sampling budgets and demand leases | Phase/jitter/latency/expiry tests with real sample age and routing assertions |
| Desktop accepts missing/malformed envelope stamps | Lossless v2 decoder with shared Go/TypeScript conformance fixtures | Exact error/termination/liveness parity; owned cleanup and recovery tests |
| Private fork reporting disabled | Owner enables and tests private reporting or verifies a private contact | Owner/API/channel evidence; accurate SECURITY policy |
| README-only/missing license text and Kaizen attribution | Reviewed exact-version provenance manifest and fail-closed generation | No unresolved shipped component/asset; reviewer approval and retained texts |
| Standalone installer inventory and missing notices | Canonical inventory-driven staging/install/removal and archive inspection | Complete inventory, architecture/mode/version/hash/notice checks |
| Unsigned helper, platform gaps, lifecycle, update feeds | Explicit distribution capabilities and signed/unsigned policy; exact-artifact release gates | Native tests for advertised targets, signatures/limitations, install/lifecycle/feed evidence |
| Historical performance numbers do not certify corrected candidate | Retained raw baseline/final measurements and conservative release tables | Reproducible final-artifact comparison, including child CPU and physical energy limits |

## 2. Effective Telemetry and Privacy

### 2.1 One Desired Configuration, Explicit Effective State

Keep per-node settings in `nvpair-node-settings`, with a schema version and
monotonic revision. The canonical broker operation applies a **complete typed
configuration**, rather than unrelated setters racing on partial state:

- `enabled` defaults to **false**.
- `recordPayloads` defaults to **false** and is valid only for the managed local
  destination with explicit consent for the current policy version.
- The destination is always this node's own collector on a loopback port
  (§3.2); there is no remote destination and discovery never selects one.
- Store consent provenance/policy version, not prompts, credentials or tokens.
  Missing/corrupt settings and old implicit `true` defaults are not proof of
  consent. Migration preserves unrelated settings and starts tracing/capture off
  until the operator confirms the new effective controls.

The broker validates, persists and reconciles settings to all three proxies.
Workers start with export/capture **disabled**, receive an initial acknowledged
configuration before activation, and receive the latest revision after every
restart. Use id-bearing control requests for acknowledgement. A missing settings
worker or apply failure leaves export disabled, not inherited from environment.

Supervised workers must not interpret ambient `PAIR_TELEMETRY_*` or OTel values
as an independent authority that can enable tracing, select a remote destination,
or enable capture. Standalone configuration uses the same Go validator and
explicit operator configuration contract. Removal/change of existing environment
semantics requires a migration notice and version review, not a compatibility
branch that silently defeats consent.

Status exposes desired revision and each worker's effective revision, apply
state/error, capture mode, verified destination identity, and delivery health.
A persisted setter response or open TCP port must not be labelled “tracing
active.” Desktop renders partial/apply-failed states and the actual capture mode.
No TypeScript policy reimplementation.

### 2.2 Safe Provider Replacement and Opt-out

Introduce a shared Go provider manager, not unsynchronized reassignment of
`p.telemetry` or a fallback to the global OTel provider. A disabled manager returns
an explicit no-op tracer. Each inference request snapshots a configuration
revision; streaming readers carry that revision and capture policy consistently.

Reconfiguration uses a bounded privacy fence:

1. Close capture/export admission for the revoked revision before acknowledging
   an opt-out. Stop extending prompt/completion buffers immediately.
2. Gate batches at export time by revision and current consent. Discard revoked
   batches; do not flush captured content as a side effect of SDK shutdown.
3. Cancel outstanding exporter RPC contexts and await bounded local completion;
   close old transports. A provider shutdown must not bypass the export gate.
4. Activate the replacement only after validation, then acknowledge effective
   state. Concurrent updates and restarts cannot resurrect an older revision.

The UI must explain that opt-out cannot retract traces already delivered. It
must distinguish “saved,” “applying,” and “effective”; it cannot promise that
remote processing or stored traces have been undone.

### 2.3 Capture Policy

Replace capture-all-except-denylist header export with a minimal metadata
allowlist. Do not export arbitrary request/response headers, query strings,
credential-bearing URLs, raw error bodies, or filesystem paths. Engine/model
identifiers and node identity are still potentially sensitive metadata and must
be disclosed, length-bounded and justified. Keep fixed secret redaction as
defence in depth, never as a claim of general secret detection.

Local payload capture is separately consented, UTF-8-safe and bounded (proposed
maximum 16 KiB each for prompt and completion per span, with truncation metadata).
Do not capture PINs or credentials intentionally; arbitrary user content cannot
be certified secret-free. Metadata-only and payload modes need distinct tests.
Content never leaves the originating node: there is no remote destination, and
the serving node records nothing for peer-originated requests (§3.2).

**Source anchors:** `services/shared/telemetry/`, each proxy's `telemetry.go`,
`services/nvpair-ui-broker/proxy.go`, `broker.go`, `broker_telemetry.go`,
`services/nvpair-node-settings/manager.go`, and the desktop telemetry handlers.

## 3. Managed Phoenix, Secure Remote Collection and Storage

### 3.1 Local Service Is Loopback-only

Pin Phoenix to a reviewed image version **and digest** with recorded terms and
configuration compatibility. Publish managed UI and OTLP ports on `127.0.0.1`
only, using the explicit loopback endpoint consistently. Do not expose the UI on
the LAN by default. Loopback is a deliberate local-machine trust boundary, not
per-user authentication; document it accordingly.

Replace the two independently maintained Compose definitions with one canonical
versioned managed template used by both source and packaged builds. Do not keep
using an unsafe app-directory copy just because it already exists. Track managed
config version/hash and container/project ownership labels. Migration stages and
validates the new config, preserves the data volume, obtains explicit approval
for a restart, recreates the managed container, and verifies actual port bindings.
Merely rewriting YAML does not fix an already-running container.

Custom Compose paths are an explicit advanced/operator-managed mode, not silently
approved managed deployments. If bindings/image/identity cannot be verified, do
not mark them secure or enable content capture through the managed UI.

### 3.2 Per-node Collection: Each Node Traces What It Originated

Owner decision: there is no cross-node trace collection. A node traces only the
requests that entered through its own loopback proxy, and exports them only to
its own managed Phoenix on `127.0.0.1`. If that node's collector is not
running, those traces are dropped; another node's Phoenix never receives them.
This keeps all trace content on the device that originated it and matches the
existing local-only contributor policy without amendment.

- The originating node keeps the complete view of its own requests: root span,
  routing decision, and the dispatch span to whichever node served it, including
  serving-node identity, latency, TTFT and token counts as observed end to end.
- The serving node exports **nothing** for peer-originated requests. Remove the
  `pair.cluster.ingress` span and stop propagating trace context to peers
  (outbound dispatch injection and ingress extraction). Exporting metadata
  fragments of another node's sessions into the serving node's Phoenix would be
  confusing and leaks request metadata the originator never chose to share.
  Peer load remains visible through workloads, not traces.
- A client-supplied `traceparent` on the local loopback proxy may still parent
  the root span, so applications that trace themselves link to PAIR's spans.
- Remove the `otel` mDNS service key, its advertisement/reconcile path and
  scanner parsing: nothing consumes it, and advertising a plaintext collector
  invites misconfiguration. No remote destination, gateway or new executable is
  added; the service inventory stays at 14 executables.
- The only configurable destination is a loopback port, so an operator can run
  their own local collector; non-loopback hosts are rejected by the Go validator.

### 3.3 Honest Readiness, Retention and Deletion

Separate container-running, verified loopback bindings, OTLP ingestion success,
exporter delivery, and UI availability in status. When tracing is on but the
collector is not running, say so plainly: this node's traces are being dropped.

Report on-device trace storage in Settings → Service: the managed data volume's
size, measured by the broker from Docker for the labelled managed volume only,
with the time it was measured and a clear "unavailable" state when Docker or the
volume cannot be inspected. Pair it with a confirmed **Clear stored traces**
action (the scoped purge below), which reports the result and the new size.

“Stop tracing,” “stop Phoenix,” and “delete stored traces” are different actions.
Keep existing data on upgrade and ordinary stop. Show that persistent data
remains until deleted; do not promise automatic expiry without testing a
supported retention mechanism in the pinned Phoenix version.

Provide a separate, local-owner-only destructive purge operation with explicit
confirmation, expected managed project/volume identity, ownership-label checks,
container-stop verification and a clear outcome. Remove only that managed data
volume—never global Docker prune, another installation's volume, or arbitrary
SQLite files. Test wrong-project/volume refusal and retention across stop/update.
Old exported data requires deletion at its actual destination; opt-out is not a
remote erasure guarantee.

**Source anchors:** root Compose, `defaultTelemetryComposeYAML`,
`resolveComposeFilePath`, `reconcileAdvertiseTelemetry`, `getTelemetryStatus`,
and `services/shared/clustertrust/mesh.go`.

## 4. Scheduling-safe Adaptive Sampling

Retain the corrected foreground scheduling behavior until this complete change
is validated. Do not reinstate the superseded “all scheduler polls background”
patch or extend the 10-second scheduler freshness window to mask old samples.

### Timing and Age Contract

- Keep actual successful GPU sample time/identity separate from HTTP receipt,
  broker cache receipt, and scheduler delivery. Reads/replays must not reset age.
- Account for request duration, subsequent engine probes, queues and cache time.
  In particular, manual probes currently publish node-info age after later engine
  probes without adding that elapsed time; correct this asymmetry.
- Separate lightweight manual-node telemetry delivery from the 10-second full
  engine/model probe. Scheduling telemetry targets a two-second delivery cadence
  with at most 250 ms scheduling jitter, matching scanner behavior, without
  emitting topology updates for telemetry-only changes.
- Preserve a bounded-age observation and sample identity so repeated cached
  samples do not receive repeated EWMA weight merely because they were polled.
  Neither deduplication nor replay may renew freshness.

Introduce shared Go timing policy and bounded demand leases. An explicit
scheduling read renews a scheduling lease but not a visible-UI lease; background
UI reads renew neither. The local broker supplies pending/running execution
intent before dispatch, not only successful response-byte activity. Count work
scheduled **on this host**, not all work originated here. Cover cold starts,
queued work, cancelled dispatches, lost updates and lease expiry.

Recommended initial operating modes for macOS/Linux:

| Demand | Collection mode | Delivery |
| --- | --- | --- |
| Visible UI or local pending/running execution | 2-second active sampling | Scheduling observations about every 2 seconds |
| Scheduling consumers present, no active execution/UI | Proposed 5-second sampling | Independent 2-second scheduling delivery |
| No scheduling consumers and no foreground/UI demand | 10-second idle sampling | Background UI may use cached snapshots |

The proposed five-second mode is conditional on a tested budget:
`5s sampling interval + 1s collection limit + 2.25s delivery + 0.5s request/queue
budget + 1.25s margin <= 10s freshness`. Define these bounds and cancellation
semantics together; if a platform cannot meet them, keep its existing faster
collection rather than report fake freshness. This is a correctness condition,
not a measured performance claim. Windows currently collects every second and
must not inherit unimplemented macOS/Linux backoff claims.

On failure, suspend/resume, clock uncertainty, queue delay or expired leases,
report stale/unknown truthfully and use existing neutral-pressure handling.
Never make cached data “new” to avoid a neutral interval. Use deterministic
virtual-time phase sweeps, delayed collection/delivery, jitter, duplicate/replayed
samples and manual-only/mDNS scenarios. Verify routing and cold-start behavior,
then measure the complete CPU/network/energy tradeoff before enabling backoff.

**Source anchors:** node-info collectors, scanner telemetry loop, manual-node
probes, broker `telemetry.go` and workload store, scheduler `telemetry.go`.

## 5. Desktop Relay Protocol Conformance

Implement a validated, lossless envelope codec **before** updating liveness,
peer epoch or application state. Do not infer a missing envelope from
`broker:epoch` payload fields, add gap polling, or require a top-level `protocol`
field that v2 does not have. Version is reported in broker epoch/resync payloads.

Preserve the existing Go v2 contract rather than silently inventing stronger
rules in TypeScript: Go currently accepts epoch `""` and sequence `0`, treats
null stamps as absent, and checks stamp presence on notifications versus other
frames. Wrong stamp types/uint64 decoding failures are recoverable decode errors;
missing notification stamps or non-null stamps on requests/responses are terminal
protocol mismatches. A stricter shared contract would be a separate versioned
change, not an incidental desktop-only validation patch.

Sequence numbers are numeric uint64 on the wire, not IEEE-754 counters. Use native
JSON source-aware parsing to retain the top-level numeric lexeme, validate the
same integer spelling/range as Go, and represent it internally losslessly
(`bigint` or canonical decimal token). Keep payload interpretation unchanged.
For outbound stamps use exact numeric serialization, not rounded numbers or
wire strings. Native `JSON.parse` reviver source and `JSON.rawJSON` availability
was probed in the installed Node 26.7.0 and Electron 42.10.0 / Node 24.18.1; add
supported-runtime tests and fail clearly on an unsupported runtime rather than a
precision-losing fallback.

**Duplicate and case-variant members.** Native `JSON.parse` cannot reproduce Go's
struct decoding exactly. A probe of the current shared codec on 2026-10-17
confirmed that Go matches reserved fields case-insensitively (`"SEQ":1` is
accepted as `seq`), reports an earlier invalid duplicate (`"seq":"bad","seq":1`)
as a recoverable decode error, and lets a later `null` duplicate erase an earlier
stamp (then a terminal mismatch). `JSON.parse` sees only the last same-case value
and treats `SEQ` as unrelated. Exact parity is therefore impossible without
source-preserving member inspection.

Recommended resolution: define one **canonical envelope** and enforce it on both
sides. Reserved top-level members (`jsonrpc`, `id`, `method`, `params`, `result`,
`error`, `epoch`, `seq`) must be unique and exact-case. A frame violating that
is a recoverable malformed frame (`DecodeError` in Go; dropped without liveness
or state update in desktop), not a protocol-version mismatch. Go adds a bounded
top-level member pre-scan using `json.Decoder.Token`; desktop adds a small,
string/escape-aware, depth-tracking scanner of top-level member names only,
then uses source-aware `JSON.parse` for values. Conforming PAIR encoders already
emit unique exact-case members, so valid traffic is unchanged; this narrows
acceptance of malformed input only and is a patch-level codec change, not relay
protocol v3. Validate parity with the shared fixtures above plus duplicate,
case-variant, escaped-name (`"s\u0065q"`), nested-reserved-name and
differential fuzz inputs run through both implementations. No new parser
dependency is proposed. The owner may alternatively narrow the parity promise
to canonical frames only; that keeps today's undefined behavior for malformed
duplicates and must be stated explicitly rather than claimed as full parity.

A terminal mismatch fails pending calls, stops frame delivery and writes, and
hands teardown to the existing owning supervisor. It must not terminate another
session's workers or enter an automatic restart storm. Invalid JSON/frames do
not reset silence timers. Epoch changes still drive per-domain recovery;
sequence gaps/regressions do not initiate snapshot polling.

Create shared JSONL conformance fixtures consumed by Go and TypeScript: null,
missing/wrong fields, empty epoch, zero sequence, decimals/exponents, negative
values, exact safe-integer/uint64 boundaries, invalid JSON, requests/responses,
filtered notification sequences, new epochs, and shutdown with pending calls.
Fix unstamped fake brokers in tests rather than preserving a production fallback.

**Source anchors:** shared Go codec/envelope; desktop `json-rpc-subprocess.ts`
and `modular-supervisor.ts`.

## 6. Private Vulnerability Reporting

The owner enables GitHub private vulnerability reporting **or** supplies a
verified private maintainer address. Test the channel with a benign private
message, record the verification date, identify the fork triage owner and publish
realistic acknowledgement expectations. Keep NVIDIA PSIRT scoped to upstream.
Do not invent an address or imply that an enabled switch guarantees operational
triage. This is an owner/platform action; repository code alone cannot close it.

## 7. Attribution, Packaging and Supply Chain

### Reviewed Provenance Manifest

Add version-keyed attribution inputs consumed by the canonical notice generator:
component/asset identity, exact version or source commit, source URL/archive hash,
verbatim text-file hash/path, SPDX expression, required companion notices,
modifications, reviewer decision/reference and status. Store reviewed text inputs
in the repository. Builds do not fetch arbitrary license text or silently use
README fallback as approval.

Resolve all 23 README-only npm entries and `go-localereader v0.0.1` against their
exact source releases and permissions. If an authoritative grant cannot be
established, obtain permission, upgrade/replace/remove the dependency with
regression coverage, or block distribution. Do not manufacture a grant from an
MIT label. Resolve Kaizen redistribution/modification terms, pin CSS source
hashes, record font-removal modifications, and retain Lucide/Feather attribution
in the canonical notice and built assets as required by reviewed terms.

Add non-npm assets and bundled Go operator tools to the reviewed inventory, not
just service-module dependencies. Scan clean lockfile-matched dependencies;
prepopulate Go dependencies and explicitly enforce offline generation in the
release environment (`-mod=readonly` alone is not offline). Add a non-writing
`licenses:check` gate that fails on unresolved/unreviewed shipped entries,
version/hash drift, missing text or nondeterministic output. Human attribution
review is still required; automation is not legal certification.

### Canonical Packaging Inventory

Derive or validate all service archive/NSIS install/uninstall lists against the
canonical component inventory. Include oMLX, root `LICENSE` and the **actual
canonical notice**, not the services pointer. Keep the desktop's extra operator
tools separately inventoried. Inspect final archives/installers/app bundles for
versions, target architecture, executable modes, hashes and complete notices,
including Electron's separate `LICENSES.chromium.html`.

Test Windows install/uninstall and Linux/macOS archives; cross-compilation is
not runtime/installer proof. Until those reference paths pass, do not distribute
their outputs or describe them as supported release artifacts.

## 8. Distribution, Lifecycle and Publication

Choose and encode distribution capabilities explicitly:

- **Unsigned limited RC:** helper/firewall automation unavailable; expose that
  capability in UI without repeated futile helper-registration prompts. Use
  ordinary OS consent and manual firewall guidance; do not bypass trust checks.
- **Supported signed fork distribution:** owner-provided authorized Developer ID,
  notarization and CI secret handling. Configure coherent fork app/helper IDs,
  designated requirements and the fork's actual authorized Team ID together;
  update both client and helper verification. Never substitute an arbitrary
  identity for NVIDIA's credentials or weaken verification to “make it work.”

Use one non-secret distribution configuration to generate packaging identity and
capability metadata, with native tests of allowed/rejected identities. Signing
credentials remain outside the repository. Signed helper registration,
firewall application, normal exit, helper unregister and purge require explicitly
authorized OS-level testing. Gate native packages by actual tested platform;
Windows ARM remains experimental until decoder/extraction/install tests pass.

The exact final-candidate gate records source commit, clean tracked tree,
lockfile/toolchains, dependency/asset provenance, component versions and artifact
hashes. Run static/contracts/dead-code/SPDX checks, component and isolated
cross-process/race suites, and applicable live tests. Record skips by name.
On controlled hosts test fresh install, upgrade, corrupt/rejected download,
interrupted replacement, readiness, inference, normal quit/relaunch, owned-orphan
cleanup, epoch/silence recovery, consent/modals, uninstall retention and explicit
purge. Keep backups/restoration evidence; do not operate on a user's live cluster
merely to complete a checklist.

Re-profile the corrected final artifact using the retained-evidence protocol in
the performance summary. Publish only effects supported by that build's data;
no percentage from superseded Round 3, zero-I/O guarantee, or billed-counter-to-
battery extrapolation.

Stage a draft release first. Verify exact artifact, checksum and update-feed
consistency and installation from those assets. An RC is recommended as a
**prerelease**, not the stable/latest channel; test explicit opt-in candidate
updates separately from stable updates. Changing the prior RC's deliberately
regular/latest status requires owner approval—do not silently rewrite it.
Tag/publish only after the owner approves the release disposition and all
applicable gates. No unsigned package is described as signed/notarized.

## 9. Other Known Product Limitations: Explicit Disposition

These are not silently declared fixed by the release-readiness work:

| Existing limitation | Proposed remedy / gate |
| --- | --- |
| Worker alive but wedged | Add low-frequency standard control-health/progress checks and request deadlines; surface per-worker degraded state. Auto-restart only after worker-specific bounded teardown/orphan handling is verified, without killing a legitimate long engine operation or another session. |
| Optional worker initial spawn failure | Reuse the owning supervisor's bounded retry budget for transient initial failures; represent missing optional binaries distinctly and offer explicit retry after valid inventory is available. Invalid explicit paths remain fatal. |
| macOS unclustered LAN reachability | Reproduce with a controlled peer across idle, sleep/wake and interface/firewall transitions; correlate listener, advertisement and reachability evidence. Apply a source-backed backend fix and regression test. No guessed root cause or unexplained blanket restart as a claimed fix. |
| Misleading shared GPU memory / unavailable Linux metrics | Expose dedicated/shared/unified/unavailable semantics explicitly; do not call system RAM dedicated VRAM, sum shared budgets across adapters, or represent unknown utilization as measured zero. Add platform fixtures; full new driver support is separately scoped. |
| TUI operation gaps | Add capability-driven broker actions/views for supported model/lifecycle/remote operations and workload destination, with destructive confirmations and fake-broker/native integration tests. Do not invent unsupported oMLX pull/delete actions. |
| Installed engines not on PATH | Prefer an explicit per-user PAIR engine-launch command resolving the managed executable through services, rather than globally mutating PATH or guessing vendor installation paths. |

For the next release, the owner must classify each applicable known limitation as
resolved, explicitly accepted/documented with support scope, or release-blocking.
Investigation and external permission/signing tasks cannot be certified complete
in advance. This is not a commitment to add every roadmap feature before an RC.

## 10. Recommended Delivery Order and Approval

1. **Trustworthy release boundary:** verified reporting channel, attribution and
   packaging gates, truthful performance evidence, supported signing/platform
   scope. These can proceed independently of runtime implementation.
2. **Privacy/runtime core:** revisioned settings, no-op/provider/export fencing,
   allowlisted metadata, local-only payload consent, managed Compose migration,
   accurate status and scoped storage deletion.
3. **Protocol and scheduler correctness:** v2 conformance codec; end-to-end age
   accounting and delivery; only then enable measured adaptive collection.
4. **Exact-artifact verification:** native lifecycle/install/update/feed checks,
   new baseline/final profiling, notes and owner-approved tag/publication.

The owner decisions above settle the behavior-changing recommendations.
Implementation proceeds as separate plans, one per
subsystem, in the order above, with §9 limitations as their own plans. The
macOS unclustered-reachability item needs a controlled second machine to
reproduce; it cannot be scheduled as "fixed" before it is reproduced. The
candidate versions currently prepared are
provisional: re-evaluate component/product/protocol versions for the actual
approved contract changes, regenerate contracts/notices, and rebuild. No release
is authorized by this proposed design.
