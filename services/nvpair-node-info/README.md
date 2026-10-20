<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-info

A Go service that exposes this machine's hardware inventory (GPUs, CPU, physical memory) over a small HTTP API at `/v1/node-info`. It advertises nothing over mDNS itself — its parent (the broker) registers its `ni` port with the `nvpair-node-scanner` discovery daemon, which folds it into this node's single `_nvpair-node` record and fetches `/v1/node-info` to enrich the node for peers.

## Communication

Two surfaces:

- **HTTP(S)** — serves the node inventory at `/v1/node-info`. Plaintext HTTP on `:14318` by default; optional HTTPS (with optional mTLS) on `:14319` when a cert/key pair is supplied.
- **stdio JSON-RPC 2.0** — newline-delimited, used for lifecycle/control (`log/set-level`, `worker/health`, `nodeinfo:set-cluster-identity`, `stats/set-demand`) and shutdown via stdin EOF. The service is normally launched as a subprocess by the broker.

| Control request | Params | Result |
| --- | --- | --- |
| `worker/health` | — | `{"ok":true}` — control-loop responsiveness only |

An id-bearing `worker/health` request returns `{"ok":true}` through the shared
stdin control loop. This checks control-loop responsiveness, not HTTP listener
or telemetry readiness. Notifications receive no response.

It does **not** advertise itself over mDNS. Discovery is centralized in the `nvpair-node-scanner` daemon: the broker registers this service's `ni` port with the daemon, which carries it on the node's one `_nvpair-node` record and fetches `/v1/node-info` over plain HTTP to enrich each node.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--port <n>` | `14318` | HTTP port to listen on |
| `--tls-port <n>` | `14319` | HTTPS port (only used when `--cert` and `--key` are set) |
| `--cert <path>` | _(none)_ | TLS server certificate (PEM); enables HTTPS when set together with `--key` |
| `--key <path>` | _(none)_ | TLS server private key (PEM); enables HTTPS when set together with `--cert` |
| `--client-ca <path>` | _(none)_ | PEM bundle of CAs trusted to sign client certificates; enables mTLS when set |
| `--accept-http` | `false` | Keep the plaintext HTTP listener alive even when TLS is enabled (default: HTTPS-only once `--cert` is set) |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`); when set, the primary port carries one cluster-gated listener. While this node is a cluster member, `/v1/node-info` is served **only** over cluster-scoped pin-based mTLS, to pinned cluster peers — or this node itself via self-trust — and a plaintext caller is refused with `403`; while it is not a member the plaintext inventory is served as usual. Membership is re-read per request. Takes precedence over the `--cert` path |
| `--version` | | Print version and exit |

`--log-level` is also accepted (registered by `nvpair-shared/applog`).

### TLS / mTLS configuration

The cert flags follow a bring-your-own contract, validated at startup:

- `--cert` and `--key` must be supplied together; either alone is rejected.
- `--client-ca` requires `--cert` and `--key`. When set, the server requires and verifies a client certificate (`RequireAndVerifyClientCert`); connections without a trusted client cert are dropped at handshake time.
- TLS minimum version is pinned to TLS 1.2.

When TLS is enabled, the HTTP listener is dropped unless `--accept-http` is passed. Both listeners share the same handler, but bind separate ports so clients can migrate independently.

`--cluster-dir` replaces both with a single cluster-gated listener on the primary port (see the flag table): one bound port, both personalities, and the choice between them follows live cluster membership rather than being fixed when the listener was bound.

## HTTP API

### `GET /v1/node-info`

Returns the merged static identity (collected once at startup) and the latest dynamic stats snapshot.

```json
{
  "GPUs": [
    {
      "name": "NVIDIA GeForce RTX 3080",
      "memory_kind": "dedicated",
      "vram_bytes": 10737418240,
      "vram_used_bytes": 2147483648,
      "utilization_percent": 42
    }
  ],
  "telemetryValid": true,
  "msSince": 137,
  "cpu": {
    "name": "AMD Ryzen 9 5900X 12-Core Processor",
    "cores": 12,
    "utilization_percent": 7
  },
  "memory": {
    "total_bytes": 34359738368,
    "used_bytes": 12884901888
  },
  "hostUuid": "8661676a-0d1c-4bd3-ac5e-4d370e6f1a9c",
  "clusterUuid": ""
}
```

Field notes:

- Dynamic fields (`vram_used_bytes`, `shared_used_bytes`, `utilization_percent`, `cpu.utilization_percent`, `memory.used_bytes`) are populated by the Windows, Linux, and macOS per-tick stats collectors.
- `telemetryValid` and `msSince` describe the node-wide GPU utilization snapshot. `telemetryValid` is `true` after the collector has produced a usable GPU sample; `msSince` is that sample's age in milliseconds at response time. A failed collection retains the last usable sample and lets its age increase. Before the first usable sample, and on platforms without dynamic GPU telemetry, the response reports `telemetryValid:false` and `msSince:0`; consumers must ignore the age while validity is false.
- `clusterUuid` is the cluster principal this node currently holds. It has three distinct states on the wire: **absent** means unknown, **present and empty** means this node belongs to no cluster, and a value is that principal. A consumer must not read absent as unclustered — that is how a node too old to report the field answers, and also how this node answers before its parent has told it anything, so acting on it would clear a correct annotation elsewhere in the fleet.
- Under the broker, `clusterUuid` is pushed in over stdin (`nodeinfo:set-cluster-identity`) because node-info is spawned with no cluster dir and so cannot read membership itself; the field stays absent until the first push arrives. Standalone with `--cluster-dir`, it reads membership from the trust store per request instead and is therefore always known. The two sources are mutually exclusive by deployment, not a fallback chain.
- `clusterUuid` exists so a peer can learn this node's membership without its mDNS record. Membership otherwise travels only as the `cluster-uuid=` TXT key, which a consumer reads once per record *change*; a consumer that misses that change keeps the previous value indefinitely, and one still holding a departed node's principal will suppress the invite that would bring it back.
- GPU field presence and per-platform memory sources are defined in the [GPU wire contract](#gpu-wire-contract) below. `DedicatedSystemMemory` is never reported as video memory; macOS system allocation is never substituted for dedicated VRAM usage.
- CPU / memory fields retain their existing `omitempty` semantics; a genuinely idle CPU is still omitted.
- Telemetry sampling cadence is lease-driven via the `X-NVPAIR-Poll` request header:
  - `foreground` (visible UI) renews the **interactive** lease (2 s interval);
  - `scheduling` renews the **scheduling** lease (5 s interval);
  - `background` renews nothing and peeks the latest snapshot without waking the collector;
  - missing or unknown headers behave as `foreground`.
  - When neither lease is active and `localExecution` is false, sampling relaxes to idle (10 s interval).
  - Leases have a 6 s TTL (`LeaseTTL`). Any renewal that raises the sampling mode wakes the collector immediately.
- The scanner and `nvpair-manual-nodes` send `scheduling` only while the broker reports scheduling demand via `telemetry/set-scheduling-demand` `{"active": bool}`; otherwise they poll as `background` and renew no lease. The broker sends true while any workload in its store is non-terminal (queued or running, local or peer-origin) and false when the last one finishes or is removed, re-asserting the value on every worker (re)spawn. Both workers default to inactive, so an idle fleet renews no scheduling lease. Because their poll cadence is fixed, a cold start or a fresh transition can leave up to one poll interval at `background` before the next poll renews the lease; a sample from that gap is never reported as fresher than its true age.
- The parent broker pushes `stats/set-demand` with `{"localExecution": bool}` over stdin on start and on workload change. When `localExecution` is true (at least one active non-terminal workload is scheduled on this node), sampling stays in interactive mode (2 s) regardless of incoming poll headers.

### GPU wire contract

The same GPU object fields travel in `/v1/node-info` `GPUs[]`, shared
`noderec.GPUInfo` discovery records, and manual-nodes `gpus[]`:

| Field | Presence and meaning |
| --- | --- |
| `name` | String, always present. |
| `memory_kind` | Always present: `dedicated`, `unified`, `shared`, or `unavailable`. Classification belongs to Go, not the desktop. |
| `vram_bytes` | Optional capacity in bytes of **this adapter's reported kind**: dedicated VRAM, physical system RAM for unified memory, or the shared-system-memory budget for shared memory. Omitted when unknown. |
| `vram_used_bytes` | Optional measured used bytes for that kind; a measured `0` is present. Omitted when unknown. |
| `shared_bytes` | Windows dedicated adapters only: optional shared-system-memory budget, separate from dedicated capacity. Omitted elsewhere or when unknown. |
| `shared_used_bytes` | Windows dedicated adapters only: optional measured shared usage, separate from dedicated usage. A measured `0` is present; omitted elsewhere or when unknown. |
| `utilization_percent` | Optional measured utilization, including idle `0`; omitted when unknown. Independent of memory availability. |

Neither absent capacity nor absent usage means zero. Unknown fields are omitted,
not fabricated. Presence is independent for every adapter and metric, so one
unknown adapter does not hide another's measured utilization. Memory values are
per adapter, never summed across adapters or between dedicated and shared pools.
System RAM is never dedicated VRAM.

| Collector / classification condition | `memory_kind` | Capacity / used source |
| --- | --- | --- |
| macOS Apple GPU: AGX class or `Apple ` name | `unified` | Physical RAM / GPU-mapped `Alloc system memory`, not whole-system used RAM. |
| macOS non-Apple Intel GPU: class, model, or entry name contains `Intel` (case-insensitive) | `shared` | `VRAM,totalMB` shared budget / `vramUsedBytes` or `inUseVidMemoryBytes` only; otherwise usage omitted. Never `Alloc system memory`. |
| macOS other non-Apple GPU (AMD/NVIDIA discrete) | `dedicated` | `VRAM,totalMB` (or dedicated used + free when total is missing) / `vramUsedBytes` or `inUseVidMemoryBytes` only; otherwise usage omitted. |
| Linux NVIDIA: numeric `memory.total` | `dedicated` | `nvidia-smi` total / used, converted from MiB to bytes. |
| Linux NVIDIA: `[N/A]`, `N/A`, `[Not Supported]`, or `Not Supported` total | `unified` | Physical RAM / independently measured system-used RAM; sentinel-based classification, not a hardware memory-type query. |
| Linux NVIDIA: malformed total | `unavailable` | Memory capacity and usage omitted; measured utilization remains independent. |
| Linux names-only (`ghw`) and other OS inventories | `unavailable` | No memory readings or utilization. Linux AMD/Intel remain names-only; no new driver collectors. |
| Windows DXGI: `DedicatedVideoMemory > 0` | `dedicated` | Dedicated capacity / PDH `Dedicated Usage`; separately `shared_bytes` = `SharedSystemMemory`, `shared_used_bytes` = PDH `Shared Usage`. |
| Windows DXGI: `DedicatedVideoMemory == 0` | `shared` | `SharedSystemMemory` budget / PDH `Shared Usage`; separate `shared_*` fields omitted. |

Downstream Go decoding normalizes missing or empty `memory_kind` from older peers
to `unavailable`; the desktop also treats unrecognized kinds as `unavailable`
rather than inferring from names or numeric capacities. Unknown readings become
nullable desktop values: `—` in current displays and gaps in chart history.
Measured zero is retained independently on each adapter. Labels are `VRAM`,
`Unified memory`, `Shared memory`, and `Memory unavailable`. The unified detail
is `<used> used of <total> unified memory` on every platform; on Linux NVIDIA
unified adapters its used-byte source is **system-used RAM**, not a GPU-only
allocation measurement.

Memory semantics do not change scheduler policy: compact telemetry takes the
maximum **measured** utilization across adapters and retains the existing
node-wide validity, freshness, and sample-sequence rules. Invalid or stale samples
remain neutral pressure; memory kind/capacity do not affect ranking.

Windows classification and counter decoding have host-runnable fixture coverage
and Windows cross-compilation checks, but actual DXGI/PDH runtime behavior is
**untested on the macOS verification host**. These are source semantics, not
release-artifact certification.

## Discovery

The service no longer advertises itself over mDNS. Its parent (the broker) registers the `ni` port with the `nvpair-node-scanner` discovery daemon, which carries it on this node's single `_nvpair-node` record; a peer's daemon then fetches `/v1/node-info` over plain HTTP to enrich the node. Transport is derived from the shared per-service policy (`nvpair-shared/noderec`): node-info is served plain on the broker path. The standalone BYO-TLS / `--cluster-dir` mTLS serving above is retained for running node-info on its own, but is unused under the broker.

## Platform Notes

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes dedicated capacity and shared budget). Dynamic CPU / dedicated and shared GPU usage / utilization / system-memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, and utilization come from `nvidia-smi`; CPU and system-memory usage come from `/proc`. Unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable. Non-NVIDIA adapters fall back to names from `ghw` without dynamic GPU stats.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

## Shutdown

The service shuts down on:

- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`

On shutdown it gracefully stops the HTTP/HTTPS listeners (3 s timeout).
