# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Pure counter accounting and report statistics (Python 3.9+)."""

from collections import defaultdict
import json
import random
from statistics import median


ENERGY_LABEL = "kernel billed energy counter — units/attribution unvalidated; directional only"
LIMITS = (
    "Processes born and dead within one interval are invisible. Exiting processes retain "
    "their final known counters, not an exit-time reading; work after their last sample is "
    "unobserved. Reparented children are retained only if previously observed. Discovery is "
    "not atomic; permission errors and PID races can omit processes. Snapshot memory means "
    "are arithmetic means over all steady snapshots, including the initial boundary, with "
    "absent components counted as zero; maxima are sampled, not lifetime peaks. CPU is "
    "percent of one core, not machine utilization. Logical writes are kernel counters, not "
    "physical storage traffic. No observed writes does not guarantee absence of I/O. "
    "Short windows, warm-up, background load, tracing, display and power state limit causal "
    "claims. Scenario labels do not establish window visibility or inference success. "
    "This harness does not measure GPU utilization, file growth, physical power, routing "
    "eligibility, telemetry freshness or alias failover; retain external evidence for those "
    "gate requirements. A successful run is not a release certification."
)
COUNTERS = ("ri_user_time", "ri_system_time", "ri_pkg_idle_wkups",
            "ri_interrupt_wkups", "ri_logical_writes", "ri_diskio_byteswritten",
            "ri_billed_energy")


def ticks_to_seconds(ticks, numer, denom):
    if numer <= 0 or denom <= 0:
        raise ValueError("mach timebase must be positive")
    return ticks * numer / denom / 1e9


def spread(values):
    """Median and Tukey hinges: exclude the median from odd-length halves."""
    values = sorted(values)
    if not values:
        return dict(n=0, median=None, min=None, max=None, q1=None, q3=None, iqr=None)
    middle = len(values) // 2
    lower = values[:middle] or values
    upper = values[(len(values) + 1) // 2:] or values
    q1, q3 = median(lower), median(upper)
    return dict(n=len(values), median=median(values), min=values[0], max=values[-1],
                q1=q1, q3=q3, iqr=q3 - q1)


def alternating_order(repetitions, seed):
    """A seeded first variant, then strict alternation, N runs of each variant."""
    if repetitions < 1:
        raise ValueError("repetitions must be positive")
    first = random.Random(seed).getrandbits(1)
    names = ("baseline", "candidate")
    return [names[(first + index) % 2] for index in range(2 * repetitions)]


def classify(name):
    if name == "PAIR" or name.startswith("PAIR Helper") or name == "ioreg":
        return name
    if name.startswith("nvpair-") or name.endswith("-proxy"):
        return name
    return "other"


def identity(process):
    return process["pid"], process["raw"]["ri_proc_start_abstime"]


class Accounting:
    """Add each (pid, start-abstime) delta once, retaining departed processes."""

    def __init__(self, start_abstime, numer=1, denom=1):
        self.start_abstime = start_abstime
        self.numer, self.denom = numer, denom
        ticks_to_seconds(0, numer, denom)
        self.last_known = {}
        self.active = set()
        self.totals = defaultdict(lambda: defaultdict(int))
        self.memory = []
        self.times = []
        self.warnings = []

    def observe(self, elapsed, processes):
        if self.times and elapsed <= self.times[-1]:
            raise ValueError("sample times must increase")
        initial = not self.times
        current = {identity(p): p for p in processes}
        exited = [self.last_known[key] for key in sorted(self.active - current.keys())]
        memory = defaultdict(lambda: [0, 0])
        for key, process in current.items():
            component = classify(process["name"])
            raw = process["raw"]
            previous = self.last_known.get(key)
            self.totals[component]  # Include components without a measured delta.
            if previous is not None:
                base = previous["raw"]
            elif not initial and key[1] >= self.start_abstime:
                base = dict.fromkeys(COUNTERS, 0)
            else:
                base = raw
                if not initial:
                    self.warnings.append("Late discovery predates steady boundary: %s" % (key,))
            for field in COUNTERS:
                delta = raw[field] - base[field]
                if delta < 0:
                    self.warnings.append("Counter regressed: %s %s" % (key, field))
                self.totals[component][field] += max(0, delta)
            memory[component][0] += raw["ri_phys_footprint"]
            memory[component][1] += raw["ri_resident_size"]
            self.last_known[key] = process
        self.active = set(current)
        self.memory.append(dict(memory))
        self.times.append(elapsed)
        return exited

    def summary(self):
        if len(self.times) < 2:
            return {}
        duration = self.times[-1] - self.times[0]
        metrics = {}
        total = defaultdict(int)
        components = sorted(self.totals)
        for component in components:
            counters = self.totals[component]
            for key, value in counters.items():
                total[key] += value
            self._rates(metrics, "components/%s/" % component, counters, duration)
            self._memory(metrics, "components/%s/" % component,
                         [snapshot.get(component, [0, 0]) for snapshot in self.memory])
        self._rates(metrics, "", total, duration)
        self._memory(metrics, "", [[sum(v[0] for v in s.values()), sum(v[1] for v in s.values())]
                                   for s in self.memory])
        ioreg_cpu = metrics.get("components/ioreg/cpu_percent_one_core", 0)
        metrics["cpu_percent_one_core_excl_ioreg"] = metrics["cpu_percent_one_core"] - ioreg_cpu
        metrics["ioreg_cpu_percent_one_core"] = ioreg_cpu
        return metrics

    def _rates(self, metrics, prefix, counters, duration):
        # RUSAGE_INFO_V4 CPU and *_abstime fields use mach absolute ticks.
        # The macOS self-PID smoke test guards against the 1:1 assumption.
        cpu_ticks = counters.get("ri_user_time", 0) + counters.get("ri_system_time", 0)
        metrics[prefix + "cpu_percent_one_core"] = (
            ticks_to_seconds(cpu_ticks, self.numer, self.denom) / duration * 100)
        for field, name in (("ri_pkg_idle_wkups", "package_idle_wakeups_per_second"),
                            ("ri_interrupt_wkups", "interrupt_wakeups_per_second"),
                            ("ri_logical_writes", "logical_writes_bytes_per_second"),
                            ("ri_diskio_byteswritten", "diskio_byteswritten_per_second")):
            metrics[prefix + name] = counters.get(field, 0) / duration
        metrics[prefix + "billed_energy_delta"] = counters.get("ri_billed_energy", 0)

    @staticmethod
    def _memory(metrics, prefix, observations):
        for index, name in enumerate(("physical_footprint_bytes", "rss_bytes")):
            values = [observation[index] for observation in observations]
            metrics[prefix + name + "_max"] = max(values, default=0)
            metrics[prefix + name + "_mean"] = sum(values) / len(values) if values else 0


def aggregate_runs(runs):
    groups = {}
    for run in runs:
        key = run["variant"] + "/" + run["scenario"]
        group = groups.setdefault(key, dict(ok=0, failed=0, skipped=0, metrics={}))
        group[run["status"]] += 1
    for key, group in groups.items():
        valid = [run for run in runs if run["status"] == "ok" and
                 run["variant"] + "/" + run["scenario"] == key]
        fields = set().union(*(run["metrics"] for run in valid))
        for field in sorted(fields):
            # Missing components are absent (zero), not excluded from the denominator.
            group["metrics"][field] = spread([run["metrics"].get(field, 0) for run in valid])
    return groups


def render_markdown(report):
    lines = ["# PAIR artifact profiling", "", ENERGY_LABEL, "", "## Sampling limits", "", LIMITS,
             "", "IQR uses Tukey halves, excluding the median for odd counts; one value has IQR 0.",
             "Failed/skipped runs are retained below and excluded from aggregate statistics.",
             "", "## Environment and protocol", "", "```json",
             json.dumps(report.get("metadata", {}), indent=2, sort_keys=True), "```", "",
             "## Per-run values", ""]
    for run in report["runs"]:
        lines.extend(["### %s / %s / %s" % (run["variant"], run["scenario"], run.get("id", "run")),
                      "", "```json", json.dumps(run, indent=2, sort_keys=True), "```", ""])
    lines.extend(["## Per variant/scenario", ""])
    for key, group in report["groups"].items():
        lines.extend(["### " + key, "", "ok: {ok}, failed: {failed}, skipped: {skipped}".format(**group),
                      "", "| Metric | n | Median | Min | Max | Q1 | Q3 | IQR |",
                      "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"])
        for field, stats in group["metrics"].items():
            label = ENERGY_LABEL if field == "billed_energy_delta" else field
            if field.endswith("/billed_energy_delta"):
                label = field.rsplit("/", 1)[0] + "/" + ENERGY_LABEL
            lines.append("| %s | %s |" % (label, " | ".join(str(stats[x]) for x in
                         ("n", "median", "min", "max", "q1", "q3", "iqr"))))
        lines.append("")
    return "\n".join(lines) + "\n"
