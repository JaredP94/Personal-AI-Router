<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Release artifact profiling harness

Python 3.9+ standard library only; execution requires **macOS arm64**. Importing
modules, `--help`, and unit tests do not launch PAIR. Actual profiling explicitly
requires `--authorize-launch`. Run later, on an idle, authorized test machine;
do not run while someone is using the desktop. No screenshots are taken.

From the repository root, for example (**this launches GUI apps**):

```sh
python3 -m scripts.perf.profile --authorize-launch \
  --output /path/to/new-evidence-directory --scenario overview \
  --seed 20260421 --context 'Unclustered; tracing disabled; no loaded engine'
```

Default artifacts are `/Applications/PAIR.app` (baseline rc.1) and
`desktop/release/0.7.0-rc.2/mac/mac-arm64/PAIR.app` (candidate). Override with
`--baseline` / `--candidate`. Defaults: 60 s warm-up, 120 s steady sampling,
5 repetitions **per variant per scenario**, 10 s tray intervals or 5 s
Overview/inference intervals. The seed selects the first variant, then variants
strictly alternate. `--warmup`, `--duration`, `--interval`, `--repetitions` and
`--seed` are recorded. Shorter runs are marked diagnostic-only. Outputs must be
new directories; evidence is never silently overwritten.

## Isolation and scenarios

Each run launches `<app>/Contents/MacOS/PAIR` directly, with both `HOME` and
`CFFIXED_USER_HOME` pointing to a fresh retained directory inside its output.
Temporary files and XDG directories also point inside it; ambient credentials,
PAIR settings and telemetry environment variables are not inherited. This
starts a fresh, unclustered node, not the user's profile. `--template DIR`
copies trusted warm/upgrade state into **each fresh home** (template symlinks
and special files are rejected). Templates are not modified. Record template
provenance and tracing consent/configuration externally; do not seed real
cluster credentials or identities. The harness never starts engines itself.

Optionally use `--link-dotdir .omlx --link-dotdir .lmstudio` to detect installed
engines via named dot-directory symlinks from the original home. Opting into a
link exposes that directory to the launched app: HOME isolation is **not an
OS-enforced filesystem or network security sandbox**, and a symlink is not
read-only. Only use trusted detection-only state and hooks; do not ask the app
to install, start, stop, upgrade or mutate engines. Omit links for strongest
profile isolation. The harness's own writes stay inside output/sandbox paths.

`--scenario tray`, `overview`, and `inference` are labels, not visibility/load
assertions. PAIR opens Overview on launch. There is no headless close/hide
implementation here: the controller must establish actual tray-only state.
Use separate invocations/output directories for different scenario controls.
Without `--scenario`, all three labels run; that alone does not validate them.

`--pre-sample-hook 'COMMAND ARG ...'` starts after launch, **before warm-up**,
and may run concurrently through sampling. The hook must wait for app readiness
before hiding/showing a window or sending requests. There is no implicit shell;
use an explicit `/bin/sh -c ...` only with trusted commands. It receives:

- `PAIR_PERF_OUTPUT`: this run's output directory;
- `PAIR_PERF_SANDBOX`: isolated home (also hook cwd and HOME);
- `PAIR_PERF_APP_PID`, `PAIR_PERF_SCENARIO`.

Hook stdout/stderr and exit status are retained; do not put credentials,
response text or private prompts in arguments/output. PAIR stdout/stderr are
discarded, not copied into profiling logs. Hooks get SIGTERM after steady
sampling and must exit zero within `--hook-timeout` (default 30 s). Timed-out
hook groups are killed and the run fails.

### Example inference hook

Use an already authorized, running OpenAI-compatible endpoint; this example
**does not launch an engine**. Pass an absolute Python executable and script
path, because hooks run from the isolated home:

```sh
python3 -m scripts.perf.profile --authorize-launch \
  --output /path/to/new-inference-evidence --scenario inference \
  --pre-sample-hook '/absolute/path/to/python3 /absolute/path/to/repo/scripts/perf/inference_hook.py --base-url http://127.0.0.1:PORT/v1 --model MODEL --max-tokens 64' \
  --context 'Document endpoint ownership, engine/model settings and tracing mode here'
```

The sequential urllib streaming loop uses one fixed synthetic prompt and fixed
max_tokens (64 by default), consumes through `[DONE]`, and retains only request
count/status and end-to-end latency, not response contents. A failed request
fails the hook. Default request timeout is 15 s, delay is 1 s, and maximum hook
duration is 3600 s; these are configurable. It handles SIGTERM cleanly. Startup
readiness is the controller's responsibility; wrap the hook in a readiness
check rather than counting connection failures as successful inference.

`--post-run-check 'COMMAND ...'` is the explicit extension point for gate item
5: it runs **after steady sampling, before quit**, against the still-running
sandbox node. Status and stdout/stderr are retained; nonzero/timeout fails the
run. No configured hook is reported as such, not as a passing routing check.
The controller supplies eligibility/freshness/failover assertions.

## Evidence and limits

Each run retains `samples.jsonl` (frame timestamps, PID/PPID/executable and raw
V4 counters, plus final-known counter records for exits), `summary.json`, hook
evidence, and its home. The top-level `report.json` and `report.md` are updated
after each run, retaining failures/skips and partial measurements. App binary
and `Contents/Resources/cli-bin/manifest.json` SHA-256, version/service manifest,
CLI, harness source commit, OS/build, hardware, power source/modes and
best-effort display state are recorded, with power/display probes repeated
before and after each run. Unavailable probes remain explicit.
Retain complete artifact provenance separately; two file hashes are not a
whole-bundle signature. Review output for local paths and identifiers before
sharing; do not commit run evidence or seeded state to the repository.

The collector uses `proc_listallpids`, `proc_pidinfo`, `proc_pidpath`, and
`proc_pid_rusage` (`RUSAGE_INFO_V4`); ctypes layouts match the SDK headers.
CPU/start absolute ticks are converted through `mach_timebase_info`; Apple
Silicon must not assume a 1:1 tick-to-nanosecond ratio. Child rusage fields are
not added again to self counters. Processes are keyed by (PID, start time),
rediscovered each sample, and previously observed reparented children remain
owned. Sampling the system process table has observer overhead, not measured
as PAIR CPU. The system snapshot is non-atomic.

Exited processes keep their accumulated deltas/final known reading: CPU after
the last observation is unavailable. Processes **born and dead within one
interval** are invisible; an unobserved child reparented before discovery can
also be missed. Late discovery of an older task and regressing counters are
flagged, not treated as trustworthy totals. Known-task permission failures
fail a run; unrelated access/exit errors are retained with frames. A failed
collector cannot certify an empty process tree.

CPU is percent of one core, separately reporting `ioreg`, totals excluding it,
and individual executable components. Wakeups and writes are counter deltas
per elapsed second. Memory maxima/means are sampled sums, including the initial
boundary; absent components count as zero in arithmetic means. Aggregates use
successful runs only, with median, min/max and Tukey-half IQR (odd median
excluded; singleton IQR zero). Billed energy is labelled **kernel billed energy
counter — units/attribution unvalidated; directional only**; it is never
converted to physical energy, power or runtime. A zero observed write delta is
not a promise of absent I/O.

On quit, only the launched app PID is sent SIGTERM, with up to 30 s for app and
known descendants to disappear. Orphans/unverifiable shutdowns fail the run
and skip subsequent runs to avoid contamination. They are **not automatically
killed**: inspect retained PIDs/start identities before manual cleanup. This is
not an uninstaller and never replaces the installed app.

## Gate mapping and tests

1. Artifact identity, collector, machine/power/display and CLI evidence retained;
   controller adds baseline/candidate source identity, GPU, topology/peer versions,
   tracing configuration, model/engine settings and workload context.
2. Warm-up, steady windows, repetitions and seeded alternating order implemented;
   controller verifies actual window state, equivalent topology and tracing modes.
3. Owned-tree CPU/memory/writes/wakeups implemented; GPU counters and file growth
   require separate instrumentation. No claim of complete exit accounting.
4. Only explicitly labelled energy proxies; physical measurement is external.
5. Raw/per-run/statistical evidence retained; routing/freshness/alias checks are
   external via the post-run hook.
6. Re-run against final artifacts after corrections; unit tests are not a
   packaged-runtime or lifecycle certification.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/tests -v
node scripts/spdx-headers.mjs
```

Tests use fake process trees, clocks, app launchers and HTTP responses. Only one
macOS smoke test reads the test process's own rusage; no PAIR/engine/GUI launch
and no network requests are made by the test suite.
