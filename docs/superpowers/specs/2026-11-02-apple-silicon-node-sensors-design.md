<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Apple Silicon Node Sensors — Temperature, Fans, Power, and Pressure

- **Date:** 2026-11-02
- **Status:** proposed — awaiting owner approval
- **Scope:** Apple Silicon (`darwin/arm64`) nodes only. Display-only.

---

## 1. Intent

A node card shows GPU utilization, unified memory, CPU utilization, and RAM, plus
the engine's own token rates. It does not show whether a Mac is **hot,
throttling, or under memory pressure**, so a node whose token rate has
collapsed looks the same as one that is just busy. On Apple Silicon, two of the
most common reasons inference gets slow are thermal throttling and a model load
that pushes the machine into swap.

The goal is to let the owner see, for every Apple Silicon node in the fleet,
including remote cluster peers:

- CPU and GPU temperature
- fan speed
- power draw
- the OS thermal pressure level, i.e. whether macOS is throttling
- the memory pressure level and swap use

The repository's rule for telemetry still applies: **a reading PAIR did not
measure is never shown as if it had been measured.**

## 2. Goals, non-goals, anti-goals

**Goals**

- Apple Silicon nodes report the readings above through `/v1/node-info`, so
  they reach every peer the same way GPU and memory telemetry does today.
- A new **Sensors** drawer on the node card shows them as tiles.
- Badges on the collapsed card show **"Throttling"** and **"Memory pressure"**
  when either is above normal.
- When macmon is missing, the card offers to install it through Homebrew,
  locally or on a remote cluster peer.
- Thermal pressure, memory pressure, and swap work **without** macmon.

**Non-goals**

- Linux, Windows, and Intel Macs. Other nodes do not report a `sensors` object,
  and the drawer button does not appear for them.
- Scheduling or routing changes. The scheduler does not read any of these
  readings.
- History charts for temperature, fan, or power. Tiles show current values only.
- Clock speeds and per-core data. macmon's CPU group labels are not stable
  across chips; see §3.
- Shipping macmon inside the app bundle.

**Anti-goals** (this would be a failure even if it technically worked)

- Showing `0 RPM`, `0 W`, or `0 °C` for a reading that was not measured. A
  fanless Mac must read "No fans", and a node without macmon must read "—".
- Colour thresholds PAIR invents for temperature. Apple publishes no safe
  limits, and a hot M-series GPU can be normal. Temperatures stay uncoloured.
- Running a sensor process continuously on an idle node faster than the
  existing sampling policy allows.
- Any privileged operation: `sudo`, `powermetrics`, or an admin-mode install.

## 3. Constraints and facts this design rests on

- **Builds have C interop disabled.** Every service is compiled with
  `CGO_ENABLED=0` (`desktop/scripts/build-modular-binaries.ts:373`), so
  node-info cannot call IOKit, IOReport, SMC, or `notify(3)` directly.
  Readings come from command-line tools or `sysctl`.
- **macmon** (<https://github.com/vladkens/macmon>, MIT, Homebrew formula
  `macmon`) reads Apple's private sensor interfaces without `sudo`. It was
  verified with macmon 0.10.0 on an Apple M5 Max running macOS 27.2.
  `macmon pipe -i <ms>` writes one JSON object per line. Each sample covers the
  preceding interval. The fields this design uses:

  | macmon field | Meaning |
  | --- | --- |
  | `temp.cpu_temp_avg`, `temp.gpu_temp_avg` | °C, float |
  | `fans[]` → `name`, `rpm`, `max_rpm` | per fan |
  | `sys_power`, `cpu_power`, `gpu_power`, `ane_power` | watts, float |
  | `timestamp` | RFC 3339 sample time |

  macmon's CPU group fields are **not** stable. On the M5 Max, `cpu_tiers`
  are labelled `P` and `S`, and the field named `ecpu_*` holds the P cores.
  Nothing in this design reads `cpu_tiers`, `ecpu_*`, `pcpu_*`, or `*_freq_mhz`.
  macmon's `memory` block is also ignored because `sysctl` provides swap (see
  below).
- **Thermal pressure** comes from
  `/usr/bin/notifyutil -g com.apple.system.thermalpressurelevel`. That prints
  `com.apple.system.thermalpressurelevel <n>`, where `n` is the OS thermal
  pressure level: `0` nominal, `1` moderate, `2` heavy, `3` trapping,
  `4` sleeping. It works unprivileged; `0` was read on the test machine.
  `pmset -g therm` is **not** used, because on Apple Silicon it reports only
  "No thermal warning level has been recorded" and gives no level.
- **Memory pressure** is the sysctl `kern.memorystatus_vm_pressure_level`:
  `1` normal, `2` warning, `4` critical. **Swap** is the sysctl `vm.swapusage`
  (`struct xsw_usage`). Both can be read in pure Go with `golang.org/x/sys/unix`,
  which node-info already depends on.
- **How readings reach the card.** The desktop polls **every** discovered
  node's `/v1/node-info` directly over HTTP. This includes remote peers. The
  poller is `desktop/src/electron/service-bridge/node-info-poller.ts`, which
  runs every 2 s and sends `X-NVPAIR-Poll: foreground` while a window is
  visible. That poll renews the remote node's interactive lease, so a visible
  card's sensors are about 2 s fresh. The poller hands each response to
  `mergeNodeInfoResponse` in `modular-state.ts`.

  Polling targets come from `getNodeInfoPollTargets`: every node with node-info
  up. That covers discovered peers (including cluster peers, because the broker
  starts node-info without `--cluster-dir`), this machine over loopback, and
  manual nodes added by IP address with plain HTTP.

  The poll does **not** reach two kinds of manual node. Hostname-only entries
  get no usable address, because `netpick.Candidates` accepts IP literals only.
  TLS-only entries can't be read either, because the poller always uses
  `http://`. These nodes already show no CPU, memory, or GPU numbers on the
  card, and they will show no sensors either. **Sensor coverage is exactly the
  coverage of today's card telemetry**; widening it is out of scope.

  The discovery directory is **not** how the card gets hardware telemetry. The
  broker's desktop snapshot (`AvailableNode`, built in
  `services/nvpair-ui-broker/discovery.go:392`) carries no GPU, CPU, or memory
  data. The scanner's 2 s telemetry loop forwards only compact GPU utilization
  for the scheduler. This design does not change any of these paths or
  cadences.

## 4. Approaches

### 4.1 Reading the sensors

| Approach | Trade-off |
| --- | --- |
| **macmon as an external process** *(chosen by owner)* | No private-API code in PAIR. Apple's interface changes become macmon's problem. Adds an external dependency and output that may drift. |
| Call IOReport/SMC/HID from Go through `purego` | No dependency, but PAIR owns undocumented Apple interfaces and has to fix them on every macOS change. |
| Separate cgo helper binary | Same ownership problem, plus a second build pipeline. |

### 4.2 Getting macmon onto a node

| Approach | Trade-off |
| --- | --- |
| **PAIR runs `brew install macmon` through engine-manager** *(chosen by owner)* | Homebrew owns upgrades and removal, and PAIR pins no version or checksum. Homebrew becomes a soft requirement. |
| PAIR downloads the GitHub release into its own support directory | No Homebrew needed, but PAIR owns version and checksum updates and its own uninstall. That is close to bundling. |
| Documented prerequisite only | Least code, but every node owner has to install macmon by hand. |

The owner also chose that the install works **on remote cluster peers**, the
same way `engine:remote-install` does.

### 4.3 Where the readings appear

| Approach | Trade-off |
| --- | --- |
| A — badges plus rows in the performance legend | Least new UI, but cramped. |
| B — always-visible line of readings on every card | Glanceable, but every card gets busier. |
| **C — separate Sensors drawer plus badges** *(chosen by owner)* | Room for the power breakdown and future readings. One more drawer button. |

## 5. Design

### 5.1 node-info: collecting (`services/nvpair-node-info`)

A new darwin-only sensor collector, alongside the existing `stats_darwin.go`
collector. It is built only for `darwin && arm64`; other platforms compile a
stub that reports no sensors.

**Direct readings (no macmon).** Memory pressure and swap are read on the
existing system tick, the 2 s `runSystem` loop. Thermal pressure runs
`notifyutil` on the same cadence as the GPU sample, i.e. the lease-driven
`SamplingPolicy` interval (2 s / 5 s / 10 s). It uses the existing
`CollectionTimeout`. A failed or unparseable read leaves that field absent. It
is never reported as nominal.

**macmon.** One long-running `macmon pipe -i <interval-ms>` child process:

- **Discovery:** the first executable found among `/opt/homebrew/bin/macmon`
  and `/usr/local/bin/macmon`. `PATH` is not searched, because the broker's
  environment under a GUI launch does not include Homebrew. While macmon is
  missing, both paths are re-checked on **every collector tick**, which costs
  two `stat` calls. A fresh install, local or remote, is therefore picked up
  within one sampling interval without any signal from engine-manager and
  without restarting anything.
- **Interval:** follows `SamplingPolicy.Interval()`. When the policy interval
  changes, macmon is restarted with the new `-i`.
- **Reading:** each stdout line is decoded into only the fields in §3. A line
  that does not decode, or decodes without any field PAIR uses, sets the status
  to `failed` and keeps no reading from it. The process keeps running.
- **Exit or crash:** restarted with backoff (1 s doubling to 30 s). Status is
  `failed` while it is down. stderr is logged at debug level only. macmon prints
  nothing sensitive, but an unbounded stderr stream is not wanted in the logs.
- **Staleness:** macmon readings older than 3 × the interval at which that
  sample was collected are dropped from the response. They are omitted, never
  sent as zero.
- **Shutdown:** the child process is killed when node-info shuts down. It must
  not outlive node-info.

### 5.2 node-info: wire contract (`/v1/node-info`)

A new top-level `sensors` object. It is **present only on `darwin/arm64`
nodes**. Absence means "this node reports no sensors": another platform, or a
node too old to know about sensors. The desktop shows the drawer only when
`sensors` is present.

```json
"sensors": {
  "macmon": "available",
  "thermalPressure": "nominal",
  "memoryPressure": "normal",
  "swapTotalBytes": 1073741824,
  "swapUsedBytes": 6291456,
  "cpuTempC": 36.0,
  "gpuTempC": 35.3,
  "fans": [
    { "name": "fan0", "rpm": 0, "maxRpm": 5349 },
    { "name": "fan1", "rpm": 0, "maxRpm": 5777 }
  ],
  "power": { "systemW": 3.86, "cpuW": 0.18, "gpuW": 0.12, "aneW": 0.0 },
  "msSince": 412
}
```

| Field | Presence and meaning |
| --- | --- |
| `macmon` | Always present: `available` (running and producing usable samples), `missing` (not found on the probed paths), or `failed` (found, but not producing usable samples). |
| `thermalPressure` | `nominal`, `moderate`, `heavy`, `trapping`, or `sleeping`. Omitted when unknown. |
| `memoryPressure` | `normal`, `warning`, or `critical`. Omitted when unknown. |
| `swapTotalBytes`, `swapUsedBytes` | Measured bytes from `vm.swapusage`. A measured `0` is present; omitted when unknown. |
| `cpuTempC`, `gpuTempC` | From macmon. Each is omitted independently when unknown. |
| `fans` | From macmon. **A present empty array means macmon reported no fans** (fanless Mac). Absent means unknown. |
| `power` | From macmon. Each sub-field is optional; the object is omitted when none was measured. |
| `msSince` | Age in milliseconds of the macmon sample that supplied the temperature, fan, and power fields. Present only while those fields are present. |

JSON names are camelCase, matching the newer node-info fields (`hostUuid`,
`clusterUuid`, `telemetryValid`). The existing snake_case GPU fields are not
renamed.

The node-info README gets a "Sensors wire contract" section next to the GPU
wire contract.

### 5.3 Getting the readings to the desktop

There is **one path**: node-info's `/v1/node-info` response, read by the
desktop poller (§3). That is the path the card's CPU, memory, and GPU numbers
already use.

- `mergeNodeInfoResponse` in
  `desktop/src/electron/service-bridge/modular-state.ts` parses `sensors` into a
  typed `NodeSensors` in `desktop/src/shared/types/`.
  - Missing fields become `null`, following the existing nullable GPU fields.
    A missing object becomes `null`.
  - The parser accepts only the known enum strings. An unknown `macmon`,
    `thermalPressure`, or `memoryPressure` value is treated as unknown, never
    coerced.
  - The node's sensor state counts as changed whenever any sensor field
    changes, alongside the existing `sameTelemetry` comparison, so the card
    re-renders.
- **When a node stops answering,** the card keeps the last sensors it read.
  This follows the poller's existing rule for CPU, memory, and GPU
  (`node-info-poller.ts`: "this node keeps the metrics last read from it").
  Nodes that go offline already render through `OfflineNode`.
- **No directory change.** `noderec`, the scanner, manual-nodes, the broker's
  relay comparison (`relay/relay.go:145`), and `AvailableNode` are untouched.
  Neither the TUI nor the scheduler needs sensors in this change, and adding
  them to the directory would mean new refresh and change-detection logic in
  four places for no consumer.

### 5.4 engine-manager: installing macmon (`services/nvpair-engine-manager`)

macmon is **not** an engine. It must not appear in `engine:get-installed`, on
the engine card, or in any engine manifest list. It gets a small, fixed,
in-code **tool** table, an allowlist with one entry:

| Tool | Platforms | Detect | Install |
| --- | --- | --- | --- |
| `macmon` | `darwin/arm64` | `/opt/homebrew/bin/macmon`, `/usr/local/bin/macmon` | `<brew> install macmon` |

`<brew>` is the first of `/opt/homebrew/bin/brew` or `/usr/local/bin/brew`.
The install runs in user mode as the engine-manager user. It never uses
`sudo`.

The existing `runCommand` (`install.go:318`) is not enough as it stands. It
puts the **entire** combined stdout and stderr into its error. The tool install
needs a bounded variant that keeps only the last lines of stderr, e.g. the last
20 lines or 4 KB, because Homebrew output can be long. Engine installs keep
using `runCommand` unchanged.

New JSON-RPC methods. They use the `engine:` prefix so the broker's existing
prefix relay (`nvpair-ui-broker/broker.go:3378`) forwards them unchanged.

| Method | Params | Result |
| --- | --- | --- |
| `engine:install-tool` | `{ tool }` | `{ tool, installed: true }`, or an error |
| `engine:remote-install-tool` | `{ node, tool }` | the remote node's result |

New `ec` route: `POST /v1/tools/install`. It uses the same NDJSON
progress/result/error framing as `POST /v1/engines/install`, and the same
pinned-mTLS gate.

Errors are typed by **JSON-RPC error code**. That is the only error field that
already survives every hop: the broker relays code and message
(`broker.go:3513`), and the desktop's `JsonRpcResponseError` keeps the code.
The codes come from the implementation-defined server range, which
engine-manager does not otherwise use beyond `-32000`:

| Code | Meaning | UI response |
| --- | --- | --- |
| `-32010` | `homebrew-missing`: no `brew` was found | inline "Install Homebrew, then run `brew install macmon`" with a copy button |
| `-32011` | `unsupported-platform`: the node is not `darwin/arm64` | inline message; should not happen, since the button only shows when `sensors` is present |
| `-32012` | `install-failed`: brew exited non-zero | reported error; the message carries brew's last stderr lines, which are bounded |

**On the remote path,** the `ec` NDJSON error frame (`streamFrame`,
`controlstream.go:34`) gains an optional `code` field. Today
`remoteclient.go:197` turns an error frame into a plain string error, so the
remote client must return an error that keeps the code. `engine:remote-install-tool`
then responds with that same code. Both ends of this frame are
engine-manager, so the change stays inside one binary. Error frames without a
code keep today's behaviour and map to `-32000`.

An unknown `tool` is rejected. Callers cannot run an arbitrary command through
this surface.

### 5.5 Desktop: Sensors drawer and badges (`desktop/src/ui/components/NodeList`)

- **Button:** a thermometer icon between the performance and engine-settings
  buttons in `NodeCardDetails.tsx`, in both the narrow and wide layouts. It is
  rendered only when the node's `sensors` is non-null.
- **Drawer:** a new `NodeSensors.tsx` using the same expanding-grid pattern as
  the existing drawers. It has four tiles:

  | Tile | Large value | Detail line |
  | --- | --- | --- |
  | Temperature | the higher of CPU and GPU, °C | `CPU 64 °C · GPU 71 °C` |
  | Fans | RPM of the fastest fan | `of <max> rpm` for each fan; a present empty `fans` reads **"No fans"** |
  | Power | system watts | `CPU · GPU · ANE` watts |
  | Memory | the pressure level | `swap <used> / <total>` |

  Unknown values render as `—`. Temperatures are never coloured. The Memory
  tile's value and the thermal badge are coloured as follows:

  | Level | Colour |
  | --- | --- |
  | `normal` / `nominal` | default |
  | `warning` / `moderate` / `heavy` | amber |
  | `critical` / `trapping` / `sleeping` | red |

- **macmon missing or failed:** the Temperature, Fans, and Power tiles show `—`
  and one line: "Sensors unavailable — **Install macmon**" when `missing`, or
  "Sensors not responding" when `failed`. The Install button calls
  `engine:install-tool` for the local node, or `engine:remote-install-tool` for
  a remote peer. While the call is in flight the button shows a spinner. On
  `homebrew-missing` it shows the copy-command fallback. The Memory tile and
  badges keep working.
- **Remote peers that are not trusted cluster members** cannot be installed
  on, because the `ec` route refuses them. The desktop already knows this
  through the node's `trusted` flag (`modular-state.ts`). For those nodes the
  button is replaced by the copy-command text.
- **Install failures** (`-32012`) are reported through the same error list
  engine installs use (`reportError` in `modular-supervisor.ts`), with
  `nodeId` set, the identity `engine-manager:install-failed:tool:macmon`, and
  **no retry action**.
  - The card's "Install macmon" button stays visible while the node reports
    `macmon: "missing"`, so it *is* the retry.
  - This leaves the shared `ServiceError` struct (`services/shared/errors`) and
    `buildRetry` (`ErrorModal.tsx:103`) unchanged. Adding a tool field there
    would have touched the shared package and all eight of its consumer
    services for a retry the card already offers.
  - `-32010` and `-32011` are not reported as errors. They are shown inline on
    the card.
- **Badges** on the collapsed card sit beside the engine pills:
  - "Throttling" when `thermalPressure` is anything but `nominal`.
  - "Memory pressure" when `memoryPressure` is `warning` or `critical`.
  - Each uses the colour from the table above. Nothing shows when the level is
    normal or unknown.
- The desktop bridge exposes the two install calls under
  `desktop/src/electron/service-bridge/`. The renderer does not import from
  `src/electron/`. Run `npm run service-contracts:write` to regenerate
  `desktop/docs/services-api.md`.

### 5.6 Versions, documentation, release notes

- Bump each service whose compiled output changes in `services/versions.json`:
  `nvpair-node-info` and `nvpair-engine-manager`. No shared package changes:
  the stream-frame `code` is engine-manager-local and `ServiceError` is
  untouched. Confirm the list by building.
- Update the README of node-info (wire contract) and engine-manager (tool
  methods, `ec` route, and tool error identity).
- Add a user-facing changelog entry under **Unreleased**, stating that macmon is
  a Homebrew dependency PAIR can install and is not bundled.
- Add macmon to the docs as an optional runtime dependency (MIT) in
  `services/installer/macos/INSTALL.md` "Requirements". It is not bundled, so
  `THIRD_PARTY_NOTICES.md` and the attribution manifest are unchanged.

## 6. Decided by the owner

- Scope: Apple Silicon only, display-only.
- Read temperature, fans, and power through macmon; read thermal state and
  memory pressure directly. Both sets are shown.
- macmon is an install dependency through Homebrew, not bundled.
- Install works on the local node and on remote cluster peers.
- UI: a separate Sensors drawer plus badges (mockup option C).
- Temperatures are uncoloured. Colours come only from macOS's own pressure
  levels.
- No history charts.

## 7. Calls made by the designer (check these)

- Thermal pressure comes from `notifyutil`, not `pmset` (§3).
- Memory pressure and swap come from `sysctl`, not macmon, so they work without
  macmon.
- macmon is found only on the two Homebrew paths, not on `PATH`.
- macmon follows the sampling-policy interval and is restarted when it changes.
  Readings older than 3 intervals are dropped.
- Staleness is measured against the interval the sample was collected at, so an idle→interactive restart does not blank fresh-enough readings.
- A fixed in-code tool allowlist in engine-manager, not an engine manifest, so
  macmon never shows up as an engine.
- The new methods use the `engine:` prefix so the broker relay needs no change.
- Wire fields are camelCase. The `sensors` object is present only on
  `darwin/arm64`.
- "Throttling" shows at any thermal level above nominal, including `moderate`.
- Sensors travel only through `/v1/node-info` and the desktop poller. They are
  not added to the discovery directory, so the TUI and scheduler do not see
  them (§5.3).
- A node that stops answering keeps its last-read sensors, matching existing
  telemetry.
- macmon discovery is re-checked on every collector tick while it is missing,
  so an install is detected within one tick. Readings follow roughly two
  intervals later (§9).
- Install errors are typed by JSON-RPC code (`-32010`/`-32011`/`-32012`),
  carried remotely by a new optional `code` on engine-manager's own NDJSON
  error frame.
- A failed install is reported with no Retry action, because the card's
  Install button is the retry. The shared `ServiceError` contract is
  unchanged.
- Sensor coverage matches today's card telemetry. Hostname-only and TLS-only
  manual nodes get no sensors, because they get no card telemetry today.
- `brew install macmon` runs with the user's own Homebrew environment. PAIR
  does not set `HOMEBREW_NO_AUTO_UPDATE` or other overrides.
- The tool install reuses the remote client's existing streaming timeouts. A
  Homebrew install can take minutes, and the engine-install route is already
  built for that.

## 8. Left to the builder

- Exact Go type names and file split inside node-info and engine-manager.
- Whether thermal pressure shares the GPU goroutine or gets its own.
- Tile styling, within the existing node-card visual language.
- Exact install progress UX beyond a spinner and the typed errors.
- The confirmed list of version bumps (§5.6).

## 9. Verification

- **Go unit tests:**
  - macmon line decoding against a recorded 0.10.0 fixture from the M5 Max,
    plus a fanless fixture (`fans: []`) and malformed or partial lines.
  - `notifyutil` and sysctl parsing for every level value, plus unknown values.
  - Staleness dropping.
  - Interval-change restart and crash backoff, using a fake `macmon` script.
  - Wire-contract tests that a present zero stays present and an unknown stays
    absent.
- **engine-manager tests:**
  - The tool allowlist rejects unknown tools.
  - `homebrew-missing`, `unsupported-platform`, and `install-failed` paths,
    using a fake `brew`.
  - The `ec` route is refused to unpinned callers, with the same tests the
    engine install route has.
- **engine-manager:** the bounded-output command helper keeps only the tail of
  stderr.
- **Desktop unit tests:**
  - The bridge parser: unknown enums and missing fields.
  - A sensor-only change triggers an update.
  - Install-button handling of `-32010`, `-32011`, and `-32012`, local and
    remote.
  - Drawer states: available, missing, failed, fanless, untrusted peer.
  - Badge levels.
- **Checks:** `make check`, `make test`, `npm run dead-code:check`, and
  `npm run service-contracts:check`.
- **engine-manager remote error code:** an `ec` error frame with `code`
  round-trips to the same JSON-RPC code on `engine:remote-install-tool`, and a
  frame without one maps to `-32000`.
- **Live check on an Apple Silicon Mac:** install macmon from the card without
  restarting PAIR.
  - node-info reports `macmon: "available"` within one collector tick of the
    binary appearing.
  - Readings then appear on the card after macmon's first sample (one
    interval) and the desktop's next poll (up to 2 s). Expect roughly two
    intervals end to end, not one. A real
  remote-peer install is a live multi-node test, so run it only on machines you
  control.
