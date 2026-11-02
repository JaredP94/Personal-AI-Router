<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Apple Silicon Node Sensors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Apple Silicon node cards show temperature, fans, power, thermal pressure, memory pressure, and swap, with an on-demand Homebrew install of macmon (local or remote).

**Architecture:** node-info gains a darwin/arm64 sensor collector: direct `sysctl`/`notifyutil` reads plus one supervised `macmon pipe` child. It publishes a `sensors` object on `/v1/node-info`. engine-manager gains a one-entry tool allowlist with local (`engine:install-tool`) and remote (`engine:remote-install-tool`, `ec` `POST /v1/tools/install`) install, typed by JSON-RPC error code. The desktop poller parses `sensors` onto the per-poll `metrics:update` channel, and a new Sensors drawer and badges render it.

**Tech Stack:** Go 1.25 (`golang.org/x/sys/unix`, `os/exec`), Electron + React + TypeScript, vitest (+ jsdom for components), zustand stores, lucide icons.

**Spec:** `docs/superpowers/specs/2026-11-02-apple-silicon-node-sensors-design.md`

## Global Constraints

- Sensors exist only on `darwin/arm64`; every other platform omits `sensors` entirely.
- **A value PAIR did not measure is omitted, never zero.** A fanless Mac sends `"fans": []`, and an unknown fan list sends no `fans` key.
- The macmon binary is found only at `/opt/homebrew/bin/macmon` and `/usr/local/bin/macmon`, and `brew` only at `/opt/homebrew/bin/brew` and `/usr/local/bin/brew`. `PATH` is never searched.
- No `sudo`, no `powermetrics`, no admin install. Homebrew runs with the user's own environment (no `HOMEBREW_*` overrides).
- macmon fields used: `temp.cpu_temp_avg`, `temp.gpu_temp_avg`, `fans[].name/rpm/max_rpm`, `sys_power`, `cpu_power`, `gpu_power`, `ane_power`, `timestamp`. Nothing else is read.
- Thermal levels: `0 nominal, 1 moderate, 2 heavy, 3 trapping, 4 sleeping`. Memory levels: `1 normal, 2 warning, 4 critical`.
- Tool error codes: `-32010` homebrew-missing, `-32011` unsupported-platform, `-32012` install-failed.
- Install-failure error identity: `engine-manager:install-failed:tool:macmon`, `action: 'dismiss'`.
- Temperatures are never coloured. Amber for `warning`/`moderate`/`heavy`, red for `critical`/`trapping`/`sleeping`.
- Repo conventions (AGENTS.md): no TS casts, no `any`/`unknown` in signatures, `@/` imports, static imports only, SPDX two-line header on every new file, say "engine", not "backend", in copy, `git commit -s`.
- Version bumps in `services/versions.json`: `nvpair-node-info` 1.2.0 → 1.3.0, `nvpair-engine-manager` 1.2.0 → 1.3.0 (MINOR: additive HTTP/IPC).

## Review Focus

1. **macmon crashes or is uninstalled mid-run.** The status must go to `failed` or `missing`, and the old readings must disappear, not freeze. Pinned in Task 3: `TestMacmonSupervisorCrashBecomesFailedThenRecovers` and `TestMacmonSupervisorBinaryRemovedBecomesMissing`.
2. **Idle node, then a user opens the card.** macmon must switch from the 10 s to the 2 s interval within about 1 s, not after the next 10 s line. Pinned in Task 3: `TestMacmonSupervisorRestartsOnIntervalChange`.
3. **A node that never had sensors (Linux, Intel Mac, old peer)** must show no thermometer button and no badges, and must not crash the parser. Pinned in Task 7: `parses absent sensors as null`, and in Task 9: `renders no sensors button without sensors`.
4. **A remote install where the peer has no Homebrew** must show the inline copy-command, not an error-list entry. Pinned in Task 6 (`TestRemoteInstallToolPreservesErrorCode`) and Task 8 (`maps -32010 from a remote node to homebrew-missing`).
5. **node-info shutdown** must not leave an orphaned `macmon` process. Pinned in Task 3: `TestMacmonSupervisorKillsChildOnCancel`.

---

### Task 1: node-info sensor wire types and response

**Files:**
- Create: `services/nvpair-node-info/sensors.go`
- Modify: `services/nvpair-node-info/stats.go` (`statsSnapshot`), `services/nvpair-node-info/main.go:77-110` (`NodeInfoResponse`), `main.go:193-241` (`buildResponseAt`)
- Test: `services/nvpair-node-info/sensors_test.go`
- Docs: `services/nvpair-node-info/README.md`: add a "Sensors wire contract" section after the GPU wire contract, with the spec's §5.2 field table verbatim.

**Interfaces:**
- Produces (all in package `main`, no build tag):

```go
type MacmonStatus string
const (
    MacmonAvailable MacmonStatus = "available"
    MacmonMissing   MacmonStatus = "missing"
    MacmonFailed    MacmonStatus = "failed"
)

type SensorFan struct {
    Name   string `json:"name"`
    RPM    uint32 `json:"rpm"`
    MaxRPM uint32 `json:"maxRpm"`
}

type SensorPower struct {
    SystemW *float64 `json:"systemW,omitempty"`
    CPUW    *float64 `json:"cpuW,omitempty"`
    GPUW    *float64 `json:"gpuW,omitempty"`
    ANEW    *float64 `json:"aneW,omitempty"`
}

// SensorInfo is the /v1/node-info `sensors` wire object.
type SensorInfo struct {
    Macmon          MacmonStatus `json:"macmon"`
    ThermalPressure string       `json:"thermalPressure,omitempty"`
    MemoryPressure  string       `json:"memoryPressure,omitempty"`
    SwapTotalBytes  *uint64      `json:"swapTotalBytes,omitempty"`
    SwapUsedBytes   *uint64      `json:"swapUsedBytes,omitempty"`
    CPUTempC        *float64     `json:"cpuTempC,omitempty"`
    GPUTempC        *float64     `json:"gpuTempC,omitempty"`
    Fans            *[]SensorFan `json:"fans,omitempty"` // pointer: nil = unknown, &[] = no fans
    Power           *SensorPower `json:"power,omitempty"`
    MSSince         *int64       `json:"msSince,omitempty"`
}

// macmonSample is one decoded macmon line (macmon-sourced fields only).
type macmonSample struct {
    CPUTempC, GPUTempC *float64
    Fans               *[]SensorFan
    Power              *SensorPower
    SampledAt          time.Time
}

// directSensors are the readings PAIR takes without macmon.
type directSensors struct {
    ThermalPressure string // "" = unknown
    MemoryPressure  string // "" = unknown
    SwapTotalBytes  *uint64
    SwapUsedBytes   *uint64
}

// sensorState is what the collector publishes; wire() turns it into the response object.
type sensorState struct {
    Macmon   MacmonStatus
    Direct   directSensors
    Sample   *macmonSample // latest usable sample, nil when none
    Interval time.Duration // current macmon sampling interval
}

func (s *sensorState) wire(now time.Time) *SensorInfo // nil receiver → nil
```

- `statsSnapshot` gains `Sensors *sensorState`. `NodeInfoResponse` gains `Sensors *SensorInfo \`json:"sensors,omitempty"\``. `buildResponseAt` sets `resp.Sensors = snap.Sensors.wire(now)`.

- [ ] **Step 1: Write the failing tests** in `sensors_test.go`:

```go
func TestSensorWireNilStateOmitsSensors(t *testing.T) {
    body := buildResponseAt(nil, nil, 0, statsSnapshot{}, "h", nil, time.Unix(100, 0))
    if bytes.Contains(body, []byte(`"sensors"`)) { t.Fatalf("sensors present: %s", body) }
}

func TestSensorWireKeepsMeasuredZerosAndEmptyFans(t *testing.T) {
    zero := uint64(0); w := 0.0; noFans := []SensorFan{}
    now := time.Unix(100, 0)
    st := &sensorState{Macmon: MacmonAvailable, Interval: 2 * time.Second,
        Direct: directSensors{ThermalPressure: "nominal", MemoryPressure: "normal", SwapTotalBytes: &zero, SwapUsedBytes: &zero},
        Sample: &macmonSample{Fans: &noFans, Power: &SensorPower{ANEW: &w}, SampledAt: now.Add(-500 * time.Millisecond)}}
    got := st.wire(now)
    // marshal and assert exact substrings:
    // "swapUsedBytes":0  "fans":[]  "aneW":0  "msSince":500  "macmon":"available"
}

func TestSensorWireDropsStaleMacmonReadings(t *testing.T) {
    // Interval 2s, SampledAt = now-6001ms → CPUTempC, GPUTempC, Fans, Power, MSSince all nil;
    // Direct fields still present; Macmon status unchanged.
    // SampledAt = now-5999ms → readings present.
}

func TestSensorWireMissingMacmonHasDirectOnly(t *testing.T) {
    // Macmon: MacmonMissing, Sample nil → JSON has "macmon":"missing", "memoryPressure", no "fans", no "msSince".
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd services/nvpair-node-info && go test -run 'TestSensorWire' ./...`
Expected: FAIL, because the types are undefined.

- [ ] **Step 3: Implement the types and `wire`, then wire them into `statsSnapshot`, `NodeInfoResponse`, and `buildResponseAt`.** Staleness rule: drop the sample when `now - SampledAt > 3 * Interval`. Use `MSSince = now - SampledAt` in milliseconds.

- [ ] **Step 4: Run the package tests and confirm they pass**

Run: `cd services/nvpair-node-info && go test ./...`
Expected: PASS, including the existing `TestBuildResponse*` tests.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-node-info/sensors.go services/nvpair-node-info/sensors_test.go services/nvpair-node-info/stats.go services/nvpair-node-info/main.go services/nvpair-node-info/README.md
git commit -s -m "feat(node-info): add sensors wire object to /v1/node-info"
```

---

### Task 2: node-info sensor parsers

**Files:**
- Create: `services/nvpair-node-info/sensors_parse.go` (no build tag, so it is testable anywhere)
- Test: `services/nvpair-node-info/sensors_parse_test.go`

**Interfaces:**
- Consumes: `macmonSample`, `SensorFan`, `SensorPower` (Task 1).
- Produces:

```go
func parseMacmonLine(line []byte) (macmonSample, bool) // false = undecodable or no field PAIR uses
func parseThermalPressure(notifyutilOut string) (string, bool) // "com.apple.system.thermalpressurelevel 2" → "heavy", true
func memoryPressureName(level uint32) (string, bool)            // 1/2/4 → normal/warning/critical
func parseSwapUsage(raw []byte) (total, used uint64, ok bool)   // vm.swapusage xsw_usage: total@0, avail@8, used@16, little-endian uint64; ok=false if len<24
```

`parseMacmonLine` uses `SampledAt` from `timestamp` (RFC 3339). A missing or unparseable `timestamp` means false. Each field PAIR uses is optional and independent. `fans` present (even empty) → `&slice`, and absent → nil. `rpm`/`max_rpm` arrive as JSON numbers and are rounded to `uint32`.

- [ ] **Step 1: Write the failing tests.** Capture a real line with `macmon pipe -s 1 -i 500`. Strip `cpu_tiers`, `ecpu_cores`, and `pcpu_cores` to keep it short, and embed it as a Go raw-string const `macmonM5MaxLine`, with a comment that says "macmon 0.10.0, Apple M5 Max, macOS 27.2".

```go
func TestParseMacmonLineRealM5Max(t *testing.T) {
    s, ok := parseMacmonLine([]byte(macmonM5MaxLine))
    // ok; *s.CPUTempC > 0; *s.GPUTempC > 0; len(*s.Fans) == 2; (*s.Fans)[0].Name == "fan0";
    // (*s.Fans)[0].MaxRPM == 5349; s.Power.SystemW != nil; !s.SampledAt.IsZero()
}
func TestParseMacmonLineFanless(t *testing.T)       // `"fans":[]` → s.Fans != nil && len(*s.Fans) == 0
func TestParseMacmonLineFansAbsent(t *testing.T)    // no "fans" key → s.Fans == nil
func TestParseMacmonLineRejectsGarbage(t *testing.T) // "not json", `{}`, `{"timestamp":"x"}` → ok == false
func TestParseMacmonLineIgnoresUnstableCPUFields(t *testing.T) // a line with only ecpu_*/pcpu_*/cpu_tiers + timestamp → ok == false
func TestParseThermalPressure(t *testing.T) // 0..4 → nominal,moderate,heavy,trapping,sleeping; "…level 9", "", "garbage" → false
func TestMemoryPressureName(t *testing.T)   // 1,2,4 ok; 0,3,5 → false
func TestParseSwapUsage(t *testing.T)       // 32-byte buffer total=1<<30, used=6<<20 → (1<<30, 6<<20, true); 23 bytes → false
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd services/nvpair-node-info && go test -run 'TestParse|TestMemoryPressure' ./...`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement the four functions.** Decode macmon into a private struct with pointer fields so presence is preserved.

- [ ] **Step 4: Run them and confirm they pass.** Run the same command. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-node-info/sensors_parse.go services/nvpair-node-info/sensors_parse_test.go
git commit -s -m "feat(node-info): parse macmon, thermal pressure, memory pressure and swap"
```

---

### Task 3: node-info macmon supervisor

**Files:**
- Create: `services/nvpair-node-info/macmon_supervisor.go` (no build tag; it is started only by Task 4 on darwin/arm64)
- Test: `services/nvpair-node-info/macmon_supervisor_test.go` (`//go:build !windows`: it uses `/bin/sh` fake binaries)

**Interfaces:**
- Consumes: `parseMacmonLine` (Task 2), `MacmonStatus`, `macmonSample` (Task 1).
- Produces:

```go
type macmonConfig struct {
    Paths      []string              // probed in order; production: {"/opt/homebrew/bin/macmon", "/usr/local/bin/macmon"}
    Interval   func() time.Duration  // production: policy.Interval(time.Now())
    BackoffMin time.Duration         // production 1s
    BackoffMax time.Duration         // production 30s
    CheckEvery time.Duration         // interval-change check cadence; production 1s
}
type macmonSupervisor struct{ /* … */ }
func newMacmonSupervisor(cfg macmonConfig) *macmonSupervisor
func (s *macmonSupervisor) Run(ctx context.Context)          // blocks until ctx done; child killed and waited before return
func (s *macmonSupervisor) Latest() (MacmonStatus, *macmonSample, time.Duration) // status, last usable sample (nil if none or cleared), interval the child runs at
```

Behaviour:
- **No executable found:** status `missing`, sample cleared, and the paths are re-probed every `Interval()`.
- **Found:** start `<path> pipe -i <Interval().Milliseconds()>` with stdout piped and stderr sent to `slog.Debug`. Each line goes through `parseMacmonLine`. A good line stores the sample and sets status `available`. A bad line sets `failed` and keeps the previous sample (Task 1's staleness ages it out).
- **Every `CheckEvery`:** if `Interval()` differs from the running interval, kill the child and restart it with the new interval. This does not count as a failure.
- **Child exits:** status `failed`, then a restart with backoff that doubles from `BackoffMin` to `BackoffMax`. The backoff resets after the first good line. Before each restart, re-probe the paths; if none exist, the status is `missing`.
- **Process group:** use `exec.CommandContext` and kill on cancel.

- [ ] **Step 1: Write the failing tests.** The helper `fakeMacmon(t, script string) string` writes an executable `#!/bin/sh` script to `t.TempDir()` and returns its path. Scripts append `"$@"` to a sibling `args` file so tests can assert on `-i`.

```go
func TestMacmonSupervisorMissingBinary(t *testing.T)            // Paths={nonexistent} → status missing, sample nil
func TestMacmonSupervisorPublishesSamples(t *testing.T)         // script: loop printf '%s\n' "$LINE"; sleep 0.05 → eventually available, sample.CPUTempC != nil
func TestMacmonSupervisorPassesIntervalMs(t *testing.T)         // Interval→250ms → args file contains "pipe -i 250"
func TestMacmonSupervisorRestartsOnIntervalChange(t *testing.T) // Interval switches 250ms→100ms, CheckEvery 20ms → args file gains "pipe -i 100" within 500ms
func TestMacmonSupervisorCrashBecomesFailedThenRecovers(t *testing.T) // script prints 1 line, exits 1, BackoffMin 10ms → observe failed, then available again
func TestMacmonSupervisorBinaryRemovedBecomesMissing(t *testing.T)    // script exits; test deletes it → status missing, sample nil
func TestMacmonSupervisorBadLinesSetFailed(t *testing.T)        // script prints "garbage" forever → failed, sample nil
func TestMacmonSupervisorKillsChildOnCancel(t *testing.T)       // script writes $$ to pid file then sleeps; cancel ctx; Run returns; syscall.Kill(pid, 0) returns ESRCH
```

Use `require`-style polling with a 2 s deadline (`waitFor(t, func() bool)`), never a bare `time.Sleep`.

- [ ] **Step 2: Run them and confirm they fail.** Run `cd services/nvpair-node-info && go test -run TestMacmonSupervisor ./...`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement `macmonSupervisor`.** Guard state with a `sync.Mutex`. Read lines with `bufio.Scanner` and a 1 MiB buffer, because macmon lines with per-core arrays exceed 64 KiB on large chips.

- [ ] **Step 4: Run them and confirm they pass,** with the race detector. Run `cd services/nvpair-node-info && go test -race -run TestMacmonSupervisor -count=3 ./...`. Expected: PASS ×3.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-node-info/macmon_supervisor.go services/nvpair-node-info/macmon_supervisor_test.go
git commit -s -m "feat(node-info): supervise a long-running macmon pipe"
```

---

### Task 4: node-info darwin/arm64 collector and wiring

**Files:**
- Create: `services/nvpair-node-info/sensors_darwin_arm64.go` (`//go:build darwin && arm64`)
- Create: `services/nvpair-node-info/sensors_other.go` (`//go:build !(darwin && arm64)`)
- Modify: `services/nvpair-node-info/stats_darwin.go`: add a `sensors *sensorCollector` field. `startStatsCollector()` sets `c.sensors = startSensorCollector(c.policy)` after `newDarwinStatsCollector(...)` returns. `PeekSnapshot` sets `snap.Sensors = c.sensors.state()`. `Stop` calls `c.sensors.stop()` before `c.cancel()`. `newDarwinStatsCollector` itself does **not** start sensors, so the existing fake-reader tests never spawn macmon.
- Modify: `services/versions.json`: `nvpair-node-info` → `1.3.0`.
- Test: `services/nvpair-node-info/sensors_darwin_arm64_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces (both build variants define the same API):

```go
type sensorCollector struct{ /* … */ }
func startSensorCollector(policy *SamplingPolicy) *sensorCollector // other platforms: returns nil
func (c *sensorCollector) state() *sensorState                      // nil receiver → nil
func (c *sensorCollector) stop()                                    // nil receiver → no-op; waits for goroutines and the macmon child
```

darwin/arm64 internals:
- `readMemoryPressure()` uses `unix.SysctlUint32("kern.memorystatus_vm_pressure_level")` with `memoryPressureName`.
- `readSwap()` uses `unix.SysctlRaw("vm.swapusage")` with `parseSwapUsage`.
- `readThermalPressure(ctx)` runs `/usr/bin/notifyutil -g com.apple.system.thermalpressurelevel` under `CollectionTimeout`, then `parseThermalPressure`.
- One loop ticks every `statsTickInterval` (2 s). It reads memory pressure and swap on every tick, and thermal pressure only when at least `policy.Interval(now)` has passed since the last thermal read. It stores `directSensors` atomically.
- One goroutine runs `macmonSupervisor.Run` with production `macmonConfig` (§ Task 3).
- `state()` combines the latest `directSensors` with `supervisor.Latest()` into a fresh `*sensorState`.
- A failed read leaves only that field unknown. It is never reported as nominal or normal.

- [ ] **Step 1: Write the failing test** (darwin/arm64 only, using the live machine read-only):

```go
func TestSensorCollectorLiveDirectReadings(t *testing.T) {
    p := NewSamplingPolicy(func() {})
    c := startSensorCollector(p); defer c.stop()
    waitFor(t, func() bool { s := c.state(); return s != nil && s.Direct.MemoryPressure != "" && s.Direct.SwapTotalBytes != nil })
    // MemoryPressure ∈ {normal,warning,critical}; ThermalPressure ∈ {"",nominal,…}; Macmon ∈ {available,missing,failed}
}
func TestSensorCollectorStopIsIdempotentAndNilSafe(t *testing.T) // var nilC *sensorCollector; nilC.stop(); nilC.state()==nil; c.stop(); c.stop()
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd services/nvpair-node-info && go test -run TestSensorCollector ./...`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement both build variants and the `stats_darwin.go` wiring.**

- [ ] **Step 4: Verify the build matrix, the tests, and the live endpoint.**

Run: `cd services/nvpair-node-info && go test ./... && GOOS=linux GOARCH=amd64 go build ./... && GOOS=windows GOARCH=amd64 go build ./... && GOOS=darwin GOARCH=amd64 go build ./...`
Expected: PASS and three clean builds.

Run: `go run . --port 14398 & sleep 6; curl -s localhost:14398/v1/node-info | jq .sensors; kill %1`
Expected: an object with `"macmon":"available"`, numeric `cpuTempC`, and `fans` with 2 entries on this M5 Max.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-node-info/sensors_darwin_arm64.go services/nvpair-node-info/sensors_other.go services/nvpair-node-info/sensors_darwin_arm64_test.go services/nvpair-node-info/stats_darwin.go services/versions.json
git commit -s -m "feat(node-info): collect Apple Silicon sensors"
```

---

### Task 5: engine-manager local tool install

**Files:**
- Create: `services/nvpair-engine-manager/tools.go`
- Modify: `services/nvpair-engine-manager/install.go`: add `runCommandTail` next to `runCommand`, which stays unchanged.
- Modify: `services/nvpair-engine-manager/manager.go`: `case "engine:install-tool": go m.runInstallTool(ctx, msg)` in the dispatch switch near `:251`.
- Test: `services/nvpair-engine-manager/tools_test.go`
- Docs: `services/nvpair-engine-manager/README.md`: add `engine:install-tool` to the JSON-RPC table and a "Tool installs" subsection with the error-code table from the spec's §5.4.

**Interfaces:**
- Produces:

```go
const (
    codeHomebrewMissing     = -32010
    codeUnsupportedPlatform = -32011
    codeToolInstallFailed   = -32012
)

// toolError carries a JSON-RPC code through local and remote paths.
type toolError struct { Code int; Msg string }
func (e *toolError) Error() string

type toolSpec struct {
    Platforms []string // "darwin/arm64"
    Detect    []string // absolute paths
    Formula   string   // "macmon"
}

// toolEnv is injectable for tests; production values come from defaultToolEnv().
type toolEnv struct {
    Platform  string              // runtime.GOOS + "/" + runtime.GOARCH
    BrewPaths []string            // {"/opt/homebrew/bin/brew", "/usr/local/bin/brew"}
    Tools     map[string]toolSpec // {"macmon": {…}}
}
func defaultToolEnv() toolEnv

func (e *Executor) InstallTool(ctx context.Context, name string) error // nil if already detected
func (e *Executor) runCommandTail(ctx context.Context, argv []string, maxLines, maxBytes int) error // stderr tail only; 20 lines / 4096 bytes at call site

func (m *Manager) runInstallTool(ctx context.Context, msg *Message) // params {tool}; result {"tool":name,"installed":true}
func rpcCodeFor(err error) int // *toolError → its Code; otherwise -32000 (Task 6 extends it)
```

`Executor` gains a `tools toolEnv` field, set to `defaultToolEnv()` wherever `Executor` is constructed. `InstallTool` works in this order:

1. Unknown name → plain error. `runInstallTool` maps it to `-32602`.
2. Platform not listed → `codeUnsupportedPlatform`.
3. Already detected → nil.
4. No brew → `codeHomebrewMissing`.
5. `runCommandTail(brew, "install", Formula)` error → `codeToolInstallFailed` with the tail in `Msg`.
6. Not detected afterwards → `codeToolInstallFailed`.

- [ ] **Step 1: Write the failing tests.** Inject a `toolEnv` with fake `brew` and `Detect` paths in `t.TempDir()`; the fake brew is a `/bin/sh` script.

```go
func TestInstallToolUnknownTool(t *testing.T)            // "rm" → error, not *toolError
func TestInstallToolUnsupportedPlatform(t *testing.T)    // Platform "linux/amd64" → Code -32011
func TestInstallToolAlreadyInstalled(t *testing.T)       // detect file exists; brew script exits 99 → nil (brew not run)
func TestInstallToolHomebrewMissing(t *testing.T)        // BrewPaths nonexistent → Code -32010
func TestInstallToolSuccess(t *testing.T)                // brew script touches detect path, exits 0; args file == "install macmon"
func TestInstallToolBrewFailsKeepsBoundedTail(t *testing.T) // brew prints 100 numbered stderr lines, exits 1 → Code -32012; Msg contains "line 100", not "line 1\n"; len(Msg) <= 4096+len(prefix)
func TestInstallToolNotDetectedAfterInstall(t *testing.T)   // brew exits 0 without creating file → Code -32012
func TestRunInstallToolRespondsWithCode(t *testing.T)    // through Manager dispatch (follow manager_test.go's codec harness): homebrew-missing → JSON-RPC error code -32010; unknown tool → -32602; success → result {"tool":"macmon","installed":true}
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd services/nvpair-engine-manager && go test -run 'TestInstallTool|TestRunInstallTool' ./...`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement `tools.go`, `runCommandTail`, and the dispatch case.**

- [ ] **Step 4: Run the package tests and confirm they pass.** Run `cd services/nvpair-engine-manager && go test ./...`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-engine-manager/tools.go services/nvpair-engine-manager/tools_test.go services/nvpair-engine-manager/install.go services/nvpair-engine-manager/manager.go services/nvpair-engine-manager/README.md
git commit -s -m "feat(engine-manager): install allowlisted tools through Homebrew"
```

---

### Task 6: engine-manager remote tool install

**Files:**
- Modify: `services/nvpair-engine-manager/controlstream.go`:
  - `const controlToolInstallPath = "/v1/tools/install"`
  - `type toolInstallRequest struct { OpID string \`json:"opId"\`; Tool string \`json:"tool"\` }`
  - `func (s *controlServer) handleToolInstall(w, r)`: POST only; empty `tool` → 400; `streamOp(w, r, req.OpID, req.Tool, "install-tool", …)` whose result frame is `{Type:"result", Op:"install-tool", Result: {"tool":…,"installed":true}}`
  - `streamFrame` gains `Code int \`json:"code,omitempty"\``
  - `streamOp`'s error frame sets `Code` via `errors.As(err, *toolError)`
- Modify: `controlserver.go:54-63`: `mux.HandleFunc(controlToolInstallPath, s.requirePin(s.handleToolInstall))`.
- Modify: `remoteclient.go:197`: an error frame returns `&remoteFrameError{Code: f.Code, Msg: f.Message}`, whose `Error()` returns `Msg`, so existing callers see identical text.
- Modify: `remote.go`: `remoteParam` gains `Tool string \`json:"tool,omitempty"\``. Add `case "engine:remote-install-tool"` (empty `tool` → `-32602`), which streams `controlToolInstallPath` and responds with `terminal.Result` or an error coded by `rpcCodeFor(err)`.
- Modify: `manager.go:273`: add `"engine:remote-install-tool"` to the remote case list. Extend `rpcCodeFor`: `*remoteFrameError` with `Code != 0` → `Code`.
- Modify: `services/versions.json`: `nvpair-engine-manager` → `1.3.0`.
- Docs: README: add `engine:remote-install-tool` to the JSON-RPC table, `POST /v1/tools/install` to the `ec` route table, and the optional `code` on error frames.
- Test: `services/nvpair-engine-manager/controlstream_test.go`, `remote_test.go` (extend the existing harnesses)

**Interfaces:**
- Consumes: `toolError`, `InstallTool`, `rpcCodeFor` (Task 5).
- Produces: `type remoteFrameError struct { Code int; Msg string }`; the JSON-RPC method `engine:remote-install-tool {node, tool}` → `{opId, result:{tool, installed}}`.

- [ ] **Step 1: Write the failing tests.** Copy the setup of the existing install-route tests.

```go
func TestToolInstallRouteRefusesUnpinnedCaller(t *testing.T)    // same assertion as the engines/install unpinned test → 403
func TestToolInstallRouteStreamsCodedError(t *testing.T)        // executor env with no brew → final frame {"type":"error","code":-32010}
func TestToolInstallRouteResult(t *testing.T)                   // fake brew success → final frame type "result", result.installed == true
func TestRemoteInstallToolPreservesErrorCode(t *testing.T)      // engine:remote-install-tool against peer whose route returns code -32010 → JSON-RPC error code -32010
func TestRemoteStreamErrorWithoutCodeMapsToServerError(t *testing.T) // legacy error frame (no code) → -32000, message unchanged
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd services/nvpair-engine-manager && go test -run 'TestToolInstallRoute|TestRemoteInstallTool|TestRemoteStreamErrorWithoutCode' ./...`. Expected: FAIL.

- [ ] **Step 3: Implement the changes listed under Files.**

- [ ] **Step 4: Run the full package with the race detector and confirm it passes.** Run `cd services/nvpair-engine-manager && go test -race ./...`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/nvpair-engine-manager services/versions.json
git commit -s -m "feat(engine-manager): remote tool install over ec with coded errors"
```

---

### Task 7: Desktop sensor types, parsing, and metrics store

Sensors ride `metrics:update`, which fires on every successful poll, and **not** the node upsert. Temperatures change on almost every poll, so putting them in `sameTelemetry` would upsert, and fully re-render, every Apple node every 2 s. This meets the spec's §5.3 requirement that a sensor-only change re-renders the card.

**Files:**
- Modify: `desktop/src/shared/types/metrics.ts`: add the types below. `NodeItemMetricsEntry` gains `sensors: NodeSensors | null`.
- Modify: `desktop/src/electron/service-bridge/modular-state.ts`:
  - `ModularNode` gains `sensors: NodeSensors | null`; every literal that builds a `ModularNode` sets `null`.
  - Add `function sensorsValue(value: JsonValue | undefined): NodeSensors | null`.
  - In `mergeNodeInfoResponse`, parse `obj.sensors`. In the `sameTelemetry` early-return branch, first `this.nodes.set(nodeId, { ...node, sensors })`, then emit `toMetrics` of the updated node. In the upsert branch, include `sensors`.
  - `toMetrics` copies `node.sensors` into `current`.
- Modify: `desktop/src/ui/stores/metrics.store.ts`: `NodeMetricsHistory` gains `sensors: NodeSensors | null`, set from `metrics.current.sensors` on every accepted update. It holds the latest value only, with no history.
- Fix every `NodeItemMetricsEntry` / `NodeMetricsHistory` literal that `npm run typecheck` flags, in source and tests, by adding `sensors: null`.
- Test: `desktop/tests/modular/modular-state-sensors.test.ts` (copy the setup of `modular-state-gpu-metrics.test.ts`)

**Interfaces:**
- Produces:

```ts
export type MacmonStatus = 'available' | 'missing' | 'failed'
export type ThermalPressure = 'nominal' | 'moderate' | 'heavy' | 'trapping' | 'sleeping'
export type MemoryPressure = 'normal' | 'warning' | 'critical'
export interface SensorFan { name: string; rpm: number; maxRpm: number }
export interface SensorPower { systemW: number | null; cpuW: number | null; gpuW: number | null; aneW: number | null }
export interface NodeSensors {
    macmon: MacmonStatus | null            // null = unrecognised value
    thermalPressure: ThermalPressure | null
    memoryPressure: MemoryPressure | null
    swapTotalBytes: number | null
    swapUsedBytes: number | null
    cpuTempC: number | null
    gpuTempC: number | null
    fans: SensorFan[] | null               // [] = no fans, null = unknown
    power: SensorPower | null
}
```

Enum parsing uses type-guard functions such as `isThermalPressure(v: string): v is ThermalPressure`, with no casts. Fan entries missing `name`, `rpm`, or `maxRpm` are dropped.

- [ ] **Step 1: Write the failing tests:**

```ts
it('parses absent sensors as null')                       // merge {GPUs: []} → last metrics:update current.sensors === null
it('parses a full sensors object')                        // merge M5 Max-shaped sensors → cpuTempC 36, fans length 2, power.systemW 3.86, memoryPressure 'normal'
it('keeps fans [] distinct from absent fans')             // fans: [] → []; no fans key → null
it('treats unknown enum strings as null')                 // thermalPressure 'boiling', macmon 'maybe' → null, null
it('emits a metrics update when only sensors change')     // merge same GPUs twice with cpuTempC 40 then 41 → two updates, second has 41
it('does not upsert the node when only sensors change')   // subscribe 'nodes:upsert'; second merge differs only in sensors → no upsert pushed
it('stores latest sensors in the metrics store')          // useMetricsStore nodeMetrics.get(id).sensors.cpuTempC === 41
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd desktop && npx vitest run --project unit tests/modular/modular-state-sensors.test.ts`. Expected: FAIL.

- [ ] **Step 3: Implement the changes listed under Files.**

- [ ] **Step 4: Run the type check and unit tests and confirm they pass.** Run `cd desktop && npm run typecheck && npm run test:unit`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add desktop/src/shared/types/metrics.ts desktop/src/electron/service-bridge/modular-state.ts desktop/src/ui/stores/metrics.store.ts desktop/tests
git commit -s -m "feat(desktop): parse node sensors onto metrics updates"
```

---

### Task 8: Desktop install bridge

**Files:**
- Modify: `desktop/src/shared/types/metrics.ts`: add `export type MacmonInstallOutcome = 'installed' | 'homebrew-missing' | 'unsupported-platform' | 'failed'`.
- Modify: `desktop/src/shared/types/ws-channels.ts`: add `'nodes:install-macmon': { request: { nodeId: string }; response: MacmonInstallOutcome }` next to `nodes:remove-member`.
- Modify: `desktop/src/ui/api/pair-api.ts`: `INodesApi.installMacmon(nodeId: string): Promise<MacmonInstallOutcome>`, which calls `transport.invoke('nodes:install-macmon', { nodeId })`.
- Modify: `desktop/src/electron/service-bridge/empty-handlers.ts`: `'nodes:install-macmon': payload => (payload ? getModularSupervisor().installMacmon(payload.nodeId) : 'failed')`.
- Modify: `desktop/src/electron/service-bridge/modular-supervisor.ts`: add the method below.
- Regenerate: `desktop/docs/services-api.md` with `npm run service-contracts:write`. Never edit it by hand.
- Test: `desktop/tests/modular/macmon-install.test.ts`

**Interfaces:**
- Consumes: the JSON-RPC methods from Tasks 5–6 and `JsonRpcResponseError` (`json-rpc-subprocess.ts:26`).
- Produces:

```ts
async installMacmon(nodeId: string): Promise<MacmonInstallOutcome>
```

Behaviour:
- **Local node** (`nodeId === getModularBridgeState().getSelfId()`): `callProcess('broker', 'engine:install-tool', { tool: 'macmon' }, PULL_TIMEOUT_MS)`.
- **Any other node:** `callProcess('broker', 'engine:remote-install-tool', { node: nodeId, tool: 'macmon' }, PULL_TIMEOUT_MS)`.
- **Resolved** → `'installed'`.
- **`JsonRpcResponseError` with code `-32010`** → `'homebrew-missing'`, and **`-32011`** → `'unsupported-platform'`. Neither is reported.
- **Any other rejection:** `this.reportError(\`Failed to install macmon on ${nodeId}: ${getErrorString(err)}\`, 'error', \`macmon-install:${nodeId}\`, { id: 'engine-manager:install-failed:tool:macmon', nodeId, operation: 'install', action: 'dismiss' })`, then return `'failed'`.

- [ ] **Step 1: Write the failing tests.** Stub `callProcess` on the supervisor instance with `vi.spyOn`.

```ts
it('calls engine:install-tool for the local node')              // selfId === nodeId → method 'engine:install-tool', params { tool: 'macmon' }
it('calls engine:remote-install-tool for a peer')               // params { node: 'peer-1', tool: 'macmon' }
it('maps -32010 from a remote node to homebrew-missing')        // reject new JsonRpcResponseError('no brew', -32010) → 'homebrew-missing'; reportError not called
it('maps -32011 to unsupported-platform without reporting')
it('reports -32012 once with the tool identity and returns failed') // reportError called with id 'engine-manager:install-failed:tool:macmon', action 'dismiss'
it('reports a non-RPC failure as failed')                       // reject new Error('broker is not running') → 'failed', reported
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd desktop && npx vitest run --project unit tests/modular/macmon-install.test.ts`. Expected: FAIL.

- [ ] **Step 3: Implement the changes listed under Files, then regenerate the contract doc.** Run `cd desktop && npm run service-contracts:write`.

- [ ] **Step 4: Verify.** Run `cd desktop && npm run typecheck && npm run test:unit && npm run service-contracts:check && npm run dead-code:check`. Expected: all PASS. `services-api.md` lists `engine:install-tool` and `engine:remote-install-tool` as "✅ yes".

- [ ] **Step 5: Commit**

```bash
git add desktop/src desktop/tests/modular/macmon-install.test.ts desktop/docs/services-api.md
git commit -s -m "feat(desktop): bridge macmon install to engine-manager"
```

---

### Task 9: Desktop Sensors drawer and badges

**Files:**
- Modify: `desktop/src/ui/components/icons.tsx`: `export const Thermometer = appIcon(LucideThermometer)`, importing `Thermometer as LucideThermometer` from `lucide-react` the way neighbouring icons do.
- Create: `desktop/src/ui/components/NodeList/NodeSensors.tsx`: the drawer.
- Create: `desktop/src/ui/components/NodeList/NodeSensorBadges.tsx`: the collapsed-card badges.
- Create: `desktop/src/ui/utils/sensor-levels.ts`: the shared level → tone mapping.
- Modify: `desktop/src/ui/components/NodeList/NodeCardDetails.tsx`:
  - Read `sensors` from `useMetricsStore(state => state.nodeMetrics.get(node.id))?.sensors ?? null`.
  - Add a third button (`Thermometer`, tooltip "Show sensors", `aria-label` "Expand sensors"/"Collapse sensors") between performance and engine settings in `expandButtons`, rendered only when `sensors !== null`.
  - Add a third expanding-grid section that copies the performance drawer's pattern and renders `<NodeSensors node={node} sensors={sensors} isLocal={isLocal} />`.
  - Render `<NodeSensorBadges sensors={sensors} />` inside the engines `Flex`, after `<NodeEnginesInline />`.
- Test: `desktop/tests/modular/node-sensors.test.ts` (`// @vitest-environment jsdom`; copy the render harness in `NodeChartLegend-NodePerformance-radial-TrayNodeRow.test.ts`)

**Interfaces:**
- Consumes: `NodeSensors` (Task 7), `window.pairApi.nodes.installMacmon` (Task 8), and `useDiscoveredNodesStore` (`AvailableNode.id` matches `NodeItem.id`; `.trusted`).
- Produces:

```ts
export type SensorTone = 'default' | 'amber' | 'red'
export function thermalTone(level: ThermalPressure | null): SensorTone   // nominal/null → default; moderate/heavy → amber; trapping/sleeping → red
export function memoryTone(level: MemoryPressure | null): SensorTone     // normal/null → default; warning → amber; critical → red
export default function NodeSensors(props: { node: NodeItem; sensors: NodeSensors; isLocal: boolean }): JSX.Element
export default function NodeSensorBadges(props: { sensors: NodeSensors | null }): JSX.Element | null
```

**Exact copy and layout** (spec §5.5):
- **Tiles, in order:**

  | Tile | Large value | Detail line |
  | --- | --- | --- |
  | Temperature | the higher of CPU and GPU, `NN °C` (rounded) | `CPU NN °C · GPU NN °C` |
  | Fans | the fastest fan's `N,NNN` rpm | `of N,NNN rpm`, one per fan |
  | Power | `NNN W` system | `CPU N · GPU N · ANE N` W |
  | Memory | `Normal` / `Warning` / `Critical` | `swap 2.1 GB / 4.0 GB` via the existing `formatBytes(n, 1)` |

  An unknown value renders `—`. A present empty `fans` renders the large value **"No fans"** with no detail line. Temperature text never gets a tone class.
- **When `sensors.macmon` is `missing`:** the Temperature, Fans, and Power tiles render `—`, and a line below the tiles reads "Sensors unavailable —" followed by a button "Install macmon". The button shows only when `isLocal` or the node is `trusted`. Otherwise the line reads "Sensors unavailable — run `brew install macmon` on this node" with a copy button (`ContentCopy`).
- **When `sensors.macmon` is `failed`:** the same tiles render `—`, with the line "Sensors not responding". There is no button.
- **Install button flow:**
  - While awaiting the call, the button is disabled and shows a spinner.
  - `'homebrew-missing'`: replace the line with "Install Homebrew, then run `brew install macmon`" plus a copy button.
  - `'unsupported-platform'`: "macmon isn't supported on this Mac".
  - `'installed'`: "Installed — waiting for readings…" until `macmon` reports `available`.
  - `'failed'`: re-enable the button. The error is already in the error list (Task 8).
- **Badges:** "Throttling" when `thermalPressure` is set and not `nominal`, using `thermalTone`. "Memory pressure" when `memoryPressure` is `warning` or `critical`, using `memoryTone`. Return `null` when neither applies.

- [ ] **Step 1: Write the failing tests:**

```ts
it('renders no sensors button without sensors')           // NodeCardDetails with metrics.sensors null → no button labelled /sensors/i
it('renders the four tiles with M5 Max values')            // 36 °C max of 36.04/35.26; "of 5,349 rpm"; "4 W"; "Normal"
it('renders No fans for an empty fan list')
it('renders dashes and Install macmon when macmon is missing locally')
it('renders the copy command instead of Install for an untrusted peer')
it('shows the Homebrew copy command after a homebrew-missing outcome') // installMacmon mock resolves 'homebrew-missing'
it('never colours temperatures')                           // Temperature tile has no amber/red class even at 105 °C
it('shows Throttling amber at moderate and red at trapping')
it('shows Memory pressure only at warning or critical')
it('maps tones', () => { /* thermalTone/memoryTone tables for every level and null */ })
```

- [ ] **Step 2: Run them and confirm they fail.** Run `cd desktop && npx vitest run --project unit tests/modular/node-sensors.test.ts`. Expected: FAIL.

- [ ] **Step 3: Implement the components, the icon, the level helpers, and the `NodeCardDetails` wiring.** Match the existing card's visual language: `Text kind="body/regular/sm"`, `text-subtle-color`, and `tabular-nums`.

- [ ] **Step 4: Verify.** Run `cd desktop && npm run lint && npm run typecheck && npm run test:unit && npm run dead-code:check`. Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add desktop/src/ui desktop/tests/modular/node-sensors.test.ts
git commit -s -m "feat(desktop): Sensors drawer and pressure badges on node cards"
```

---

### Task 10: User docs, changelog, and full verification

**Files:**
- Modify: `services/installer/macos/INSTALL.md` "Requirements": add a bullet. "Optional: [macmon](https://github.com/vladkens/macmon) (MIT) for temperature, fan, and power readings on Apple Silicon. PAIR can install it through Homebrew from a node card; it is not bundled."
- Modify: `CHANGELOG.md` `## [Unreleased]`: add an `### Added` entry. "**Apple Silicon Node Sensors (`nvpair-node-info` 1.3.0, `nvpair-engine-manager` 1.3.0, `desktop`)**: Node cards on Apple Silicon gain a Sensors drawer (CPU/GPU temperature, fan speed, power draw, memory pressure and swap) and Throttling / Memory pressure badges. Temperature, fan, and power come from macmon, a Homebrew dependency PAIR can install locally or on a trusted cluster peer; it is not bundled. Thermal and memory pressure are read directly and work without macmon."

- [ ] **Step 1: Make the edits above.**

- [ ] **Step 2: Run the full gate.** Run `make check && make test && (cd desktop && npm run dead-code:check) && node scripts/spdx-headers.mjs`. Expected: all PASS, with 0 missing SPDX headers. Report any skipped Go tests as untested, not passing.

- [ ] **Step 3: Live check on this Mac.** Run `cd desktop && npm start`. Then:
  1. The local card shows the thermometer button, and its tiles show real values.
  2. Run `brew uninstall macmon`. Within one interval the tiles show `—` and "Install macmon".
  3. Click Install. node-info reports `available` within one tick of the binary appearing, and readings return within about two intervals.

  Skip the remote-peer live install unless a second Mac you control is in the cluster.

- [ ] **Step 4: Commit**

```bash
git add services/installer/macos/INSTALL.md CHANGELOG.md
git commit -s -m "docs: document macmon dependency and node sensors"
```
