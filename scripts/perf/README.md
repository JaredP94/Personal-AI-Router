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

Default artifacts are `/Applications/PAIR.app` (the installed baseline) and
`desktop/release/<desktop version>/mac/mac-arm64/PAIR.app` (candidate, the path
follows `desktop/package.json`). Override with
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

Packaged macOS startup can create or rewrite `/usr/local/bin/nvpair` despite
HOME redirection. **Before every launch the harness refuses to proceed if
`/usr/local/bin` is writable by the current user**, unless explicitly authorized
with `--allow-system-launcher-write`. This is separate from `--authorize-launch`:
the override accepts a possible system launcher write, not filesystem confinement.
With or without that override, each run records `/usr/local/bin/nvpair` before
and after: absent state or lstat metadata plus SHA-256 of file contents (including
a symlink's referent contents and its target string). An unreadable/unverifiable
snapshot or any change fails the run and skips further launches. The harness
never restores/deletes the launcher; inspect evidence and arrange any repair
separately. This guard is not a general OS sandbox and cannot prevent arbitrary
writes by trusted app code or hooks.

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
hook groups are killed and the run fails. Both pre- and post-hooks retain their
process-group ownership through cleanup, even if the leader exits first. The
harness sends group SIGTERM, allows `--hook-timeout` seconds of termination
grace (separate from a post-hook's execution timeout), escalates to SIGKILL,
and verifies disappearance/reaps the leader with a bounded 5 s wait.
For both hooks and the app, exit observation uses `waitid(..., WNOWAIT)`
(including a libc wrapper on Python 3.9 builds without `os.waitid`). The leader
stays unreaped, even as a zombie, until group verification or escalation is
finished, reserving its PID/PGID against reuse. A retained `Popen` reference
prevents subprocess destructor/background reaping; no poll/wait is used before
verification ends. Hook cleanup is finalized once, including failed cleanup:
the run's finalizer never retries group signalling after reaping.

For both hooks and the app's leader group, the authoritative membership list
comes from the kernel's `sysctl({CTL_KERN, KERN_PROC, KERN_PROC_PGRP, pgid})`.
Its `kinfo_proc` PID + timeval start identity is checked against each member's
libproc identity/PGID before accepting a full read. New fallback signal targets
are retained only while the leader is alive. Unrelated system PIDs with denied
libproc reads cannot block group verification. A query failure, or a listed
member whose full read fails (including ESRCH), is not proof of disappearance.
An inaccessible/changed member remains unverified until a later successful
membership query resolves it; persistent uncertainty requires full grace,
SIGKILL while pinned, and failed cleanup/skip if still unresolved. Only the
waitid-confirmed exited leader is exempt; other enumerated zombies must vanish.

No group signal is needed once waitid and the kernel membership query establish
that only the exited leader (or no members) remains. Darwin can return EPERM
for a zombie-only group; EPERM alone does **not** prove reuse or disappearance
(ESRCH is the usual absent-group result). Actual signalling EPERM remains failed
evidence, never success. If another path has already reaped the leader, the
harness never signals that numeric group: only previously recorded, individually
revalidated members may be signalled, and cleanup is always unverified.
Unverified cleanup fails the run and skips subsequent runs. Hooks and their
children must not detach into different process groups/sessions; escaped,
unobserved descendants cannot be certified by process-group probes.

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

`baseline-bundle-manifest.json` and `candidate-bundle-manifest.json` inventory
the entire `.app`, including relative paths, entry types, permission modes,
per-file SHA-256, and symlink targets plus their SHA-256. Symlinks are not
followed. Entries are sorted by relative path; SHA-256 of the retained canonical
JSON bytes is recorded as `bundle_manifest_sha256` in app metadata. Timestamps,
ownership and extended attributes are not included. Optional `--baseline-archive`
and `--candidate-archive` paths add the original archive's SHA-256 to metadata.
Keep artifacts immutable during profiling: the inventory identifies content at
preflight, not changes made later. Retain provenance/signature/notarization
evidence separately; a content digest is not a signing certificate. Review output
for local paths and identifiers before sharing; do not commit run evidence or
seeded state to the repository.

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

SIGINT, SIGTERM and SIGHUP request controlled cancellation; repeated signals
during teardown do not interrupt app/hook cleanup. The harness retains partial
measurements and cancellation evidence, and skips remaining runs. SIGKILL,
host failure and deliberately detached/unobserved children are outside this
guarantee.

On quit, the harness signals its own unreaped child directly with SIGTERM.
Shutdown requests and final reaping do not depend on collector success, but
verification fails closed on collector errors. The app's new process
group gets up to 30 s to disappear, then SIGKILL and a bounded verification/reap
wait. Forced termination fails the run. Collector failures are retained and
cannot prevent requesting shutdown. Orphans/unverifiable shutdowns fail the run
and skip subsequent runs to avoid contamination. Recorded descendants outside
the leader's group (including Electron helpers/workers that change groups) still
undergo the existing descendant/orphan verification using sampled PID + mach
start-time identities, independent of the kernel leader-group query. Those
outside-group descendants are **not automatically killed**: inspect retained
PIDs/start identities before manual cleanup. This is not an uninstaller and
never replaces the installed app.

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

Tests use fake process trees, clocks, app launchers and HTTP responses. macOS
integration coverage reads the test process's own rusage and starts only trivial
console children: a self-exiting `/bin/sh -c 'sleep 30 & exit 3'` hook proves
non-reaping observation, kernel member/start-time discovery, group SIGTERM,
verified disappearance, then final reap. No PAIR/engine/GUI launch or network
requests are made by the test suite.
