# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Release harness tests; never launch PAIR, engines, or GUI applications."""

import ctypes
import errno
import os
from pathlib import Path
import tempfile
import time
import json
import hashlib
import signal
import subprocess
import sys
import unittest
from unittest.mock import patch

from scripts.perf.accounting import (
    Accounting, aggregate_runs, alternating_order, classify, render_markdown,
    spread, ticks_to_seconds,
)
from scripts.perf.libproc import (
    Libproc, RusageV4, owned_processes, find_orphans, child_exited, child_exited_libc,
)
from scripts.perf.profile import (
    sandbox_environment, prepare_sandbox, build_parser, app_metadata,
    parse_low_power_modes, clean_quit, run_once, main,
)
from scripts.perf.inference_hook import stream_request, run_load
from scripts.perf import profile


def sample(pid, start, user=0, system=0, name="PAIR", ppid=1, **extra):
    counters = dict(ri_proc_start_abstime=start, ri_user_time=user,
                    ri_system_time=system, ri_pkg_idle_wkups=0,
                    ri_interrupt_wkups=0, ri_phys_footprint=100,
                    ri_resident_size=200, ri_logical_writes=0,
                    ri_diskio_byteswritten=0, ri_billed_energy=0)
    counters.update(extra)
    return dict(pid=pid, ppid=ppid, name=name, path="/test/" + name,
                start_time=(start, 0), raw=counters)


class StatisticsTests(unittest.TestCase):
    def test_spread_tukey_halves_odd_even_single_and_empty(self):
        self.assertEqual(spread([1, 2, 3, 4, 100]),
                         dict(n=5, median=3, min=1, max=100, q1=1.5, q3=52, iqr=50.5))
        self.assertEqual(spread([1, 2, 3, 4])["iqr"], 2)
        self.assertEqual(spread([7])["iqr"], 0)
        self.assertIsNone(spread([])["median"])

    def test_mach_conversion_not_one_to_one(self):
        self.assertAlmostEqual(ticks_to_seconds(24_000_000, 125, 3), 1)
        with self.assertRaises(ValueError):
            ticks_to_seconds(1, 1, 0)

    def test_alternation_reproducible_balanced_and_seeded(self):
        order = alternating_order(5, 42)
        self.assertEqual(len(order), 10)
        self.assertEqual(order.count("baseline"), 5)
        self.assertTrue(all(a != b for a, b in zip(order, order[1:])))
        self.assertEqual(order, alternating_order(5, 42))
        self.assertNotEqual(order, alternating_order(5, 43))

    def test_classification_preserves_worker_identity(self):
        for name in ["PAIR", "PAIR Helper (Renderer)", "nvpair-node-info", "omlx-proxy", "ioreg"]:
            self.assertEqual(classify(name), name)
        self.assertEqual(classify("nvidia-smi"), "other")

    def test_grouped_report_omits_failed_runs_from_statistics(self):
        rows = [dict(variant="baseline", scenario="tray", status="ok", metrics={"cpu": x})
                for x in (1, 2, 10)]
        rows.append(dict(variant="baseline", scenario="tray", status="failed", metrics={"cpu": 900}))
        rows.append(dict(variant="candidate", scenario="overview", status="skipped", metrics={}))
        groups = aggregate_runs(rows)
        self.assertEqual(groups["baseline/tray"]["metrics"]["cpu"]["median"], 2)
        self.assertEqual(groups["baseline/tray"]["failed"], 1)
        self.assertEqual(groups["candidate/overview"]["skipped"], 1)
        rendered = render_markdown(dict(runs=rows, groups=groups, metadata={}))
        self.assertIn("directional only", rendered)
        self.assertIn("units/attribution unvalidated", rendered)
        self.assertIn("born and dead", rendered)
        self.assertIn("900", rendered)  # Failed per-run observations remain inspectable.
        self.assertNotIn("watts", rendered.lower())
        self.assertNotIn("battery", rendered.lower())
        self.assertNotIn("zero-i/o", rendered.lower())


class AccountingTests(unittest.TestCase):
    def test_churn_retains_exited_cpu_and_keys_by_start_time(self):
        ledger = Accounting(start_abstime=100)
        ledger.observe(0, [sample(1, 1, user=5_000_000_000)])
        ledger.observe(10, [sample(1, 1, user=6_000_000_000),
                            sample(2, 110, user=500_000_000, name="ioreg")])
        exited = ledger.observe(20, [sample(1, 1, user=7_000_000_000),
                                    sample(2, 120, user=250_000_000, name="ioreg")])
        self.assertEqual(exited[0]["raw"]["ri_user_time"], 500_000_000)
        self.assertEqual(exited[0]["raw"]["ri_proc_start_abstime"], 110)
        metrics = ledger.summary()
        self.assertAlmostEqual(metrics["cpu_percent_one_core_excl_ioreg"], 10)
        self.assertEqual(metrics["ioreg_cpu_percent_one_core"], 3.75)
        self.assertEqual(metrics["components/ioreg/cpu_percent_one_core"], 3.75)
        self.assertEqual(len(ledger.last_known), 3)

    def test_newly_discovered_old_process_does_not_count_lifetime(self):
        ledger = Accounting(100)
        ledger.observe(0, [])
        ledger.observe(10, [sample(2, 90, user=90_000_000_000)])
        ledger.observe(20, [sample(2, 90, user=91_000_000_000)])
        self.assertEqual(ledger.summary()["cpu_percent_one_core_excl_ioreg"], 5)
        self.assertTrue(ledger.warnings)

    def test_rates_and_snapshot_memory_include_absence_as_zero(self):
        ledger = Accounting(100)
        ledger.observe(0, [sample(1, 1)])
        ledger.observe(10, [sample(1, 1, user=1_000_000_000,
                                  ri_pkg_idle_wkups=20, ri_interrupt_wkups=30,
                                  ri_logical_writes=1000, ri_billed_energy=50,
                                  ri_phys_footprint=300, ri_resident_size=600)])
        ledger.observe(20, [])
        m = ledger.summary()
        self.assertEqual(m["package_idle_wakeups_per_second"], 1)
        self.assertEqual(m["interrupt_wakeups_per_second"], 1.5)
        self.assertEqual(m["logical_writes_bytes_per_second"], 50)
        self.assertEqual(m["billed_energy_delta"], 50)
        self.assertEqual(m["physical_footprint_bytes_max"], 300)
        self.assertEqual(m["physical_footprint_bytes_mean"], 400 / 3)
        self.assertEqual(m["rss_bytes_mean"], 800 / 3)
        self.assertEqual(m["components/PAIR/rss_bytes_mean"], 800 / 3)

    def test_cpu_uses_mach_timebase_not_nanosecond_assumption(self):
        ledger = Accounting(100, numer=125, denom=3)
        ledger.observe(0, [sample(1, 1)])
        ledger.observe(1, [sample(1, 1, user=24_000_000)])
        self.assertEqual(ledger.summary()["cpu_percent_one_core_excl_ioreg"], 100)

    def test_counter_regression_is_flagged_not_negative(self):
        ledger = Accounting(100)
        ledger.observe(0, [sample(1, 1, user=10)])
        ledger.observe(1, [sample(1, 1, user=5)])
        self.assertEqual(ledger.summary()["cpu_percent_one_core_excl_ioreg"], 0)
        self.assertIn("regressed", " ".join(ledger.warnings))


class OwnershipTests(unittest.TestCase):
    def test_dynamic_tree_and_reparented_known_child_not_reused_pid(self):
        root = sample(1, 10)
        child = sample(2, 20, ppid=1, name="nvpair-ui-broker")
        grandchild = sample(3, 30, ppid=2, name="ioreg")
        other = sample(4, 40, ppid=0)
        table = {p["pid"]: p for p in [root, child, grandchild, other]}
        self.assertEqual(set(owned_processes(table, (1, 10), {})), {1, 2, 3})
        grandchild["ppid"] = 1
        table = {3: grandchild, 2: sample(2, 99), 4: other}
        known = {(2, 20): child, (3, 30): grandchild}
        self.assertEqual(set(owned_processes(table, (1, 10), known)), {3})
        self.assertEqual([p["pid"] for p in find_orphans(table, (1, 10), known)], [3])

    def test_fake_libproc_drives_tree_discovery(self):
        class Fake:
            def pids(self):
                return [1, 2, 3]
            def info(self, pid):
                return sample(pid, pid * 10, ppid=pid - 1)
        self.assertEqual(set(Libproc.snapshot(Fake())), {1, 2, 3})

    @unittest.skipUnless(sys.platform == "darwin", "macOS libproc only")
    def test_real_self_rusage_cpu_and_start_use_mach_ticks(self):
        proc = Libproc()
        before = proc.info(os.getpid())
        started = time.process_time()
        while time.process_time() - started < 0.03:
            pass
        after = proc.info(os.getpid())
        raw_delta = sum(after["raw"][k] - before["raw"][k]
                        for k in ("ri_user_time", "ri_system_time"))
        self.assertGreater(proc.seconds(raw_delta), 0.015)
        self.assertLess(proc.seconds(raw_delta), 0.5)
        self.assertGreater(after["raw"]["ri_proc_start_abstime"], 0)
        self.assertLess(after["raw"]["ri_proc_start_abstime"], proc.absolute_time())
        self.assertEqual(after["pid"], os.getpid())
        self.assertEqual(after["ppid"], os.getppid())
        self.assertEqual(ctypes.sizeof(RusageV4), 296)


class ChildWaitTests(unittest.TestCase):
    def test_native_and_darwin_fallback_wait_leave_own_child_unreaped(self):
        # These tiny console children are the only subprocesses this test starts.
        for probe in ([child_exited, child_exited_libc] if sys.platform == "darwin" else [child_exited]):
            with self.subTest(probe=probe.__name__), \
                 subprocess.Popen([sys.executable, "-c", "pass"]) as child:
                deadline = time.monotonic() + 5
                while not probe(child.pid):
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(0.01)
                self.assertTrue(probe(child.pid))  # Still waitable, not reaped.
                self.assertIsNone(child.returncode)
                self.assertEqual(child.wait(timeout=5), 0)
                with self.assertRaises(ChildProcessError):
                    probe(child.pid)


class FakeClock:
    def __init__(self):
        self.value = 0
    def monotonic(self):
        return self.value
    def sleep(self, seconds):
        self.value += seconds


class FakeApp:
    instances = []
    pid = 1000
    def __init__(self):
        self.returncode = None
        self.exitcode = None
        self.reaped = False
        self.instances.append(self)
    def poll(self):
        if self.exitcode is not None:
            self.wait()
        return self.returncode
    def terminate(self):
        self.exitcode = 0
    def wait(self, timeout=None):
        if self.exitcode is None:
            raise subprocess.TimeoutExpired("fake app", timeout)
        self.reaped = True
        self.returncode = self.exitcode
        return self.returncode
    def kill(self):
        self.exitcode = -signal.SIGKILL


def fake_child_exited(pid):
    app = next(app for app in reversed(FakeApp.instances) if app.pid == pid)
    if app.reaped:
        raise ChildProcessError
    return app.exitcode is not None


def fake_kill(pid, signum):
    app = next(app for app in reversed(FakeApp.instances) if app.pid == pid)
    if app.reaped:
        raise AssertionError("signal sent after reap")
    if signum == signal.SIGTERM:
        app.terminate()
    elif signum == signal.SIGKILL:
        app.kill()


class FakeProc:
    numer, denom = 1, 1
    errors = []
    def __init__(self, clock, app):
        self.clock, self.app = clock, app
    def info(self, pid):
        return dict(sample(pid, 1, user=int(self.clock.value * 1e9)), pgid=pid)
    def group_members(self, pgid):
        return {} if self.app.exitcode is not None else {self.app.pid: (1, 0)}
    def snapshot(self):
        return {} if self.app.exitcode is not None else {self.app.pid: self.info(self.app.pid)}
    def absolute_time(self):
        return 100 + int(self.clock.value * 1e9)
    def seconds(self, ticks):
        return ticks / 1e9


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        for target, replacement in (("child_exited", fake_child_exited), ("os.kill", fake_kill)):
            guard = patch("scripts.perf.profile." + target, side_effect=replacement)
            guard.start()
            self.addCleanup(guard.stop)
        probe = patch("scripts.perf.profile.command_evidence",
                      return_value=dict(exit_status=0, stdout="fixture state", stderr=""))
        probe.start()
        self.addCleanup(probe.stop)
        access = patch("scripts.perf.profile.os.access", return_value=False)
        access.start()
        self.addCleanup(access.stop)
        groups = patch("scripts.perf.profile.os.killpg", side_effect=ProcessLookupError)
        groups.start()
        self.addCleanup(groups.stop)
        sandbox = tempfile.TemporaryDirectory()
        self.addCleanup(sandbox.cleanup)
        launcher = patch("scripts.perf.profile.SYSTEM_LAUNCHER", Path(sandbox.name) / "nvpair")
        launcher.start()
        self.addCleanup(launcher.stop)

    def test_fake_run_writes_raw_summary_and_terminates_only_fake_app(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        args = build_parser().parse_args(["--output", "/unused", "--warmup", "0",
                                         "--duration", "20", "--interval", "10"])
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp).resolve() / "run"
            with patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
                 patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                 patch("scripts.perf.profile.time.sleep", clock.sleep):
                result = run_once(args, proc, "baseline", "tray", run_dir, 1, {"binary": "/never-run"})
            self.assertEqual(result["status"], "ok")
            self.assertEqual(result["metrics"]["cpu_percent_one_core_excl_ioreg"], 100)
            self.assertEqual(result["snapshots"], 3)
            raw = [json.loads(line) for line in (run_dir / "samples.jsonl").read_text().splitlines()]
            self.assertEqual(sum(row["event"] == "sample" for row in raw), 3)
            self.assertEqual(raw[-1]["raw"]["ri_user_time"], 20_000_000_000)
            self.assertEqual(json.loads((run_dir / "summary.json").read_text())["status"], "ok")
            self.assertTrue(result["quit"]["ok"])
            self.assertEqual(result["environment_before"]["power_source"]["stdout"], "fixture state")
            self.assertEqual(result["environment_after"]["power_source"]["stdout"], "fixture state")

    def test_full_steady_window_starts_at_first_completed_snapshot(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        original = proc.snapshot
        first = True
        def expensive_first_snapshot():
            nonlocal first
            if first:
                clock.value += 5
                first = False
            return original()
        proc.snapshot = expensive_first_snapshot
        args = build_parser().parse_args(["--output", "/unused", "--warmup", "0",
                                         "--duration", "20", "--interval", "10"])
        with tempfile.TemporaryDirectory() as temp:
            with patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
                 patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                 patch("scripts.perf.profile.time.sleep", clock.sleep):
                result = run_once(args, proc, "baseline", "tray", Path(temp).resolve() / "run", 1, {"binary": "/never-run"})
            self.assertGreaterEqual(result["measured_seconds"], 20)

    def test_failed_run_preserves_partial_accounting_and_summary(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        original = proc.snapshot
        def failing():
            if clock.value >= 20 and app.exitcode is None:
                raise OSError("fake collector failed")
            return original()
        proc.snapshot = failing
        args = build_parser().parse_args(["--output", "/unused", "--warmup", "0",
                                         "--duration", "30", "--interval", "10"])
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp).resolve() / "run"
            with patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
                 patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                 patch("scripts.perf.profile.time.sleep", clock.sleep):
                result = run_once(args, proc, "candidate", "overview", run_dir, 1, {"binary": "/never-run"})
            self.assertEqual(result["status"], "failed")
            self.assertEqual(result["metrics"]["cpu_percent_one_core_excl_ioreg"], 100)
            self.assertEqual(result["measured_seconds"], 10)
            self.assertTrue(result["quit"]["ok"])
            self.assertTrue((run_dir / "summary.json").exists())

    def test_orphan_is_failure_without_signalling_an_unrelated_process(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        child = sample(2000, 20, ppid=1, name="nvpair-ui-broker")
        proc.snapshot = lambda: {2000: child}
        with patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            result = clean_quit(proc, profile.ProcessGroup(app, proc), (1000, 1), {(2000, 20): child}, timeout=1)
        self.assertFalse(result["ok"])
        self.assertEqual(result["orphans"][0]["pid"], 2000)
        self.assertEqual(app.returncode, 0)

    def test_nonfinite_durations_are_rejected_before_metadata_or_launch(self):
        for field, value in [("--duration", "nan"), ("--warmup", "inf"), ("--interval", "nan")]:
            with self.subTest(field=field), tempfile.TemporaryDirectory() as temp:
                with patch("scripts.perf.profile.environment_metadata", side_effect=AssertionError("must not probe")):
                    with self.assertRaises(SystemExit) as caught, patch("sys.stderr"):
                        main(["--output", str(Path(temp) / "out"), "--authorize-launch", field, value])
                    self.assertEqual(caught.exception.code, 2)

    def test_libproc_initialization_failure_retains_report_and_skips_runs(self):
        metadata = dict(apps={"baseline": {}, "candidate": {}})
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "out"
            with patch("scripts.perf.profile.environment_metadata", return_value=metadata), \
                 patch("scripts.perf.profile.platform.system", return_value="Darwin"), \
                 patch("scripts.perf.profile.platform.machine", return_value="arm64"), \
                 patch("scripts.perf.profile.Libproc", side_effect=OSError("collector unavailable")), \
                 patch("scripts.perf.profile.subprocess.Popen", side_effect=AssertionError("must not launch")):
                status = main(["--output", str(output), "--authorize-launch",
                               "--repetitions", "1", "--scenario", "overview"])
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(status, 1)
            self.assertEqual([run["status"] for run in report["runs"]], ["skipped", "skipped"])
            self.assertIn("collector unavailable", report["metadata"]["collector_error"])
            self.assertTrue((output / "report.md").exists())

    def fake_run(self, temp, app, proc, clock, extra=()):
        args = build_parser().parse_args(["--output", "/unused", "--warmup", "0",
                                         "--duration", "20", "--interval", "10"] + list(extra))
        with patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            return run_once(args, proc, "baseline", "tray", Path(temp) / "run", 1,
                            {"binary": "/never-run"})

    def test_writable_system_launcher_directory_refuses_launch_without_opt_in(self):
        clock, app = FakeClock(), FakeApp()
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.os.access", return_value=True):
            result = self.fake_run(temp, app, FakeProc(clock, app), clock)
        self.assertEqual(result["status"], "failed")
        self.assertTrue(any("--allow-system-launcher-write" in str(f) for f in result["failures"]))
        self.assertNotIn("quit", result)

    def test_writable_directory_opt_in_permits_fake_launch_but_still_verifies_launcher(self):
        clock, app = FakeClock(), FakeApp()
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.os.access", return_value=True):
            result = self.fake_run(temp, app, FakeProc(clock, app), clock,
                                   ["--allow-system-launcher-write"])
        self.assertEqual(result["status"], "ok")
        self.assertTrue(result["system_launcher"]["allow_write"])
        self.assertEqual(result["system_launcher"]["before"], {"state": "absent"})
        self.assertEqual(result["system_launcher"]["after"], {"state": "absent"})
        self.assertFalse(result["launcher_changed"])
        self.assertEqual(app.wait(), 0)

    def test_launcher_snapshot_detects_mutation_even_with_opt_in(self):
        for allowed in (False, True):
            with self.subTest(allowed=allowed), tempfile.TemporaryDirectory() as temp:
                launcher = Path(temp) / "nvpair"
                clock, app = FakeClock(), FakeApp()
                proc = FakeProc(clock, app)
                original = proc.snapshot
                def mutate():
                    launcher.write_text("unexpected launcher")
                    return original()
                proc.snapshot = mutate
                with patch("scripts.perf.profile.SYSTEM_LAUNCHER", launcher):
                    result = self.fake_run(temp, app, proc, clock,
                        ["--allow-system-launcher-write"] if allowed else [])
                self.assertEqual(result["status"], "failed")
                self.assertTrue(result["launcher_changed"])
                self.assertEqual(result["system_launcher"]["before"], {"state": "absent"})
                self.assertEqual(result["system_launcher"]["after"]["sha256"],
                                 hashlib.sha256(b"unexpected launcher").hexdigest())

    def test_signals_cancel_sampling_and_repeated_cleanup_signals_do_not_strand_app(self):
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            with self.subTest(signal=signum), tempfile.TemporaryDirectory() as temp:
                clock, app = FakeClock(), FakeApp()
                proc = FakeProc(clock, app)
                handlers = {}
                def install(sig, handler):
                    previous = handlers.get(sig, signal.SIG_DFL)
                    handlers[sig] = handler
                    return previous
                def terminate_with_repeated_signals():
                    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                        handlers[sig](sig, None)
                    app.exitcode = 0
                app.terminate = terminate_with_repeated_signals
                with patch("scripts.perf.profile.signal.signal", side_effect=install):
                    original_sleep = clock.sleep
                    def interrupted_sleep(seconds):
                        original_sleep(seconds)
                        handlers[signum](signum, None)
                    clock.sleep = interrupted_sleep
                    result = self.fake_run(temp, app, proc, clock)
                self.assertTrue(result["interrupted"])
                self.assertEqual(app.poll(), 0)
                self.assertTrue(result["quit"]["ok"])
                self.assertTrue((Path(temp) / "run/summary.json").exists())
                self.assertTrue(all(value == signal.SIG_DFL for value in handlers.values()))

    def test_signal_at_launch_returns_owned_child_before_cleanup(self):
        clock, app = FakeClock(), FakeApp()
        handlers = {}
        def install(sig, handler):
            previous = handlers.get(sig, signal.SIG_DFL)
            handlers[sig] = handler
            return previous
        def launch(*args, **kwargs):
            handlers[signal.SIGTERM](signal.SIGTERM, None)
            return app
        args = build_parser().parse_args(["--output", "/unused"])
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.signal.signal", side_effect=install), \
             patch("scripts.perf.profile.subprocess.Popen", side_effect=launch):
            result = run_once(args, FakeProc(clock, app), "baseline", "tray",
                              Path(temp) / "run", 1, {"binary": "/fake"})
        self.assertTrue(result["interrupted"])
        self.assertEqual(app.wait(), 0)
        self.assertFalse(result["quit"]["ok"])  # root discovery never completed

    def test_repeated_signals_during_hook_teardown_still_reap_app_and_hook(self):
        clock, app = FakeClock(), FakeApp()
        hook = FakeApp()
        hook.pid = 2000
        handlers = {}
        def install(sig, handler):
            previous = handlers.get(sig, signal.SIG_DFL)
            handlers[sig] = handler
            return previous
        def group_signal(pid, sig):
            if pid == hook.pid and sig == signal.SIGTERM:
                for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                    handlers[signum](signum, None)
                hook.terminate()
            if pid != hook.pid or hook.exitcode is not None:
                raise ProcessLookupError
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.signal.signal", side_effect=install), \
             patch("scripts.perf.profile.start_hook", side_effect=lambda *args: (profile.ProcessGroup(hook, FakeProc(clock, hook)), {"pid": hook.pid})), \
             patch("scripts.perf.profile.os.killpg", side_effect=group_signal):
            result = self.fake_run(temp, app, FakeProc(clock, app), clock,
                                   ["--pre-sample-hook", "/fake-hook"])
        self.assertTrue(result["interrupted"])
        self.assertEqual(hook.wait(), 0)
        self.assertEqual(app.wait(), 0)
        self.assertTrue(result["hooks"]["pre_sample"]["cleanup_ok"])
        self.assertTrue(result["quit"]["ok"])

    def test_failing_collector_cannot_prevent_child_shutdown_and_failure_is_retained(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        proc.info = lambda pid: (_ for _ in ()).throw(OSError("info unavailable"))
        proc.snapshot = lambda: (_ for _ in ()).throw(OSError("snapshot unavailable"))
        with tempfile.TemporaryDirectory() as temp:
            result = self.fake_run(temp, app, proc, clock)
        self.assertEqual(app.poll(), 0)
        self.assertFalse(result["quit"]["ok"])
        self.assertIn("snapshot unavailable", str(result["quit"]))

    def test_collector_failure_with_known_root_does_not_block_direct_child_termination(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        def unavailable(*unused):
            raise OSError("collector unavailable")
        proc.info = proc.snapshot = unavailable
        result = clean_quit(proc, profile.ProcessGroup(app, proc), (1000, 1), {})
        self.assertEqual(app.wait(), 0)
        self.assertFalse(result["ok"])
        self.assertIn("collector unavailable", result["collector_error"])

    def test_signal_during_final_metadata_is_retained_and_stops_experiment(self):
        clock, app = FakeClock(), FakeApp()
        handlers = {}
        def install(sig, handler):
            previous = handlers.get(sig, signal.SIG_DFL)
            handlers[sig] = handler
            return previous
        calls = 0
        def probe():
            nonlocal calls
            calls += 1
            if calls == 2:
                handlers[signal.SIGTERM](signal.SIGTERM, None)
            return {}
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.signal.signal", side_effect=install), \
             patch("scripts.perf.profile.power_display_metadata", side_effect=probe):
            result = self.fake_run(temp, app, FakeProc(clock, app), clock)
            stored = json.loads((Path(temp) / "run/summary.json").read_text())
        self.assertTrue(result.get("interrupted"))
        self.assertEqual(stored["status"], "failed")

    def test_signal_during_experiment_metadata_skips_all_launches(self):
        handlers = {}
        def install(sig, handler):
            previous = handlers.get(sig, signal.SIG_DFL)
            handlers[sig] = handler
            return previous
        def metadata(*unused):
            handlers[signal.SIGHUP](signal.SIGHUP, None)
            return dict(apps={v: {"binary": "/fake"} for v in ("baseline", "candidate")})
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.signal.signal", side_effect=install), \
             patch("scripts.perf.profile.environment_metadata", side_effect=metadata), \
             patch("scripts.perf.profile.platform.system", return_value="Darwin"), \
             patch("scripts.perf.profile.platform.machine", return_value="arm64"), \
             patch("scripts.perf.profile.Libproc", return_value=FakeProc(FakeClock(), FakeApp())), \
             patch("scripts.perf.profile.subprocess.Popen", side_effect=AssertionError("must not launch")):
            output = Path(temp) / "out"
            status = main(["--output", str(output), "--authorize-launch", "--scenario", "overview",
                           "--repetitions", "1"])
            report = json.loads((output / "report.json").read_text())
        self.assertEqual(status, 1)
        self.assertEqual([r["status"] for r in report["runs"]], ["skipped", "skipped"])

    def test_launcher_mutation_skips_subsequent_runs(self):
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        with tempfile.TemporaryDirectory() as temp:
            launcher = Path(temp) / "nvpair"
            original = proc.snapshot
            def mutate():
                launcher.write_bytes(b"modified")
                return original()
            proc.snapshot = mutate
            metadata = dict(apps={v: {"binary": "/fake"} for v in ("baseline", "candidate")})
            with patch("scripts.perf.profile.SYSTEM_LAUNCHER", launcher), \
                 patch("scripts.perf.profile.environment_metadata", return_value=metadata), \
                 patch("scripts.perf.profile.platform.system", return_value="Darwin"), \
                 patch("scripts.perf.profile.platform.machine", return_value="arm64"), \
                 patch("scripts.perf.profile.Libproc", return_value=proc), \
                 patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
                 patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                 patch("scripts.perf.profile.time.sleep", clock.sleep):
                output = Path(temp) / "out"
                status = main(["--output", str(output), "--authorize-launch", "--scenario", "overview",
                               "--repetitions", "1", "--warmup", "0", "--duration", "20"])
            report = json.loads((output / "report.json").read_text())
        self.assertEqual(status, 1)
        self.assertEqual([r["status"] for r in report["runs"]], ["failed", "skipped"])
        self.assertTrue(report["runs"][0]["launcher_changed"])
        self.assertTrue(report["runs"][0]["quit"]["ok"])

    def test_timed_out_post_hook_gets_termination_grace_before_group_kill(self):
        clock, hook = FakeClock(), FakeApp()
        terminated_at = None
        killed_at = None
        def group_signal(pid, sig):
            nonlocal terminated_at, killed_at
            if hook.exitcode is not None:
                raise ProcessLookupError
            if sig == signal.SIGTERM:
                terminated_at = clock.value
            elif sig == signal.SIGKILL:
                killed_at = clock.value
                hook.kill()
        with patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            profile.finish_hook(profile.ProcessGroup(hook, FakeProc(clock, hook)), {}, 1)
        self.assertGreaterEqual(killed_at - terminated_at, 1)
        self.assertEqual(hook.wait(), -signal.SIGKILL)

    def test_hook_group_permission_failure_is_unverified_even_if_leader_exits_zero(self):
        clock, hook = FakeClock(), FakeApp()
        evidence = {}
        def denied(pid, sig):
            hook.exitcode = 0
            raise PermissionError("denied")
        with patch("scripts.perf.profile.os.killpg", side_effect=denied), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            profile.finish_hook(profile.ProcessGroup(hook, FakeProc(clock, hook)), evidence, 1)
        self.assertFalse(evidence["cleanup_ok"])
        self.assertTrue(evidence["group_errors"])

    def test_unresponsive_child_is_group_killed_and_reaped_without_collector(self):
        clock, app = FakeClock(), FakeApp()
        app.terminate = lambda: None
        proc = FakeProc(clock, app)
        proc.snapshot = lambda: (_ for _ in ()).throw(OSError("collector failed"))
        signals = []
        def group_signal(pid, sig):
            self.assertEqual(pid, 1000)
            signals.append(sig)
            if sig == signal.SIGKILL:
                app.kill()
            if app.exitcode is not None:
                raise ProcessLookupError
        with patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            result = clean_quit(proc, profile.ProcessGroup(app, proc), (1000, 1), {}, timeout=1)
        self.assertEqual(app.wait(), -signal.SIGKILL)
        self.assertIn(signal.SIGKILL, signals)
        self.assertFalse(result["ok"])

    def test_both_hook_groups_are_cleaned_after_leader_exit_and_kill_escalation(self):
        for stop in (False, True):
            with self.subTest(pre_hook=stop):
                clock, hook = FakeClock(), FakeApp()
                living = True
                signals = []
                def group_signal(pid, sig):
                    nonlocal living
                    self.assertEqual(pid, hook.pid)
                    if not living:
                        raise ProcessLookupError
                    signals.append(sig)
                    if sig == signal.SIGKILL:
                        living = False
                evidence = {}
                proc = FakeProc(clock, hook)
                child = dict(sample(1001, 2), pgid=1000)
                proc.snapshot = lambda: {1001: child} if living else {}
                proc.group_members = lambda pgid: {1001: (2, 0)} if living else {}
                proc.info = lambda pid: child
                with patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
                     patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                     patch("scripts.perf.profile.time.sleep", clock.sleep):
                    group = profile.ProcessGroup(hook, proc)
                    hook.exitcode = 0  # observed descendant survives its leader
                    profile.finish_hook(group, evidence, 1, stop=stop)
                self.assertFalse(living)
                self.assertIn(signal.SIGTERM, signals)
                self.assertIn(signal.SIGKILL, signals)
                self.assertTrue(evidence["cleanup_ok"])
                self.assertTrue(evidence["forced_kill"])

    def test_unverified_hook_cleanup_aborts_remaining_runs(self):
        metadata = dict(apps={"baseline": {"binary": "/fake"}, "candidate": {"binary": "/fake"}})
        clock, app = FakeClock(), FakeApp()
        proc = FakeProc(clock, app)
        def cannot_clean(hook, evidence, *args, **kwargs):
            evidence.update(cleanup_ok=False, exit_status=0)
        with tempfile.TemporaryDirectory() as temp, \
             patch("scripts.perf.profile.environment_metadata", return_value=metadata), \
             patch("scripts.perf.profile.platform.system", return_value="Darwin"), \
             patch("scripts.perf.profile.platform.machine", return_value="arm64"), \
             patch("scripts.perf.profile.Libproc", return_value=proc), \
             patch("scripts.perf.profile.subprocess.Popen", return_value=app), \
             patch("scripts.perf.profile.start_hook", return_value=(app, {})), \
             patch("scripts.perf.profile.finish_hook", side_effect=cannot_clean), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            output = Path(temp) / "out"
            status = main(["--output", str(output), "--authorize-launch", "--scenario", "overview",
                           "--repetitions", "1", "--warmup", "0", "--duration", "20",
                           "--post-run-check", "/fake-check"])
            report = json.loads((output / "report.json").read_text())
        self.assertEqual(status, 1)
        self.assertEqual([r["status"] for r in report["runs"]], ["failed", "skipped"])
        self.assertTrue(report["runs"][0]["hook_cleanup_unverified"])

    def test_never_sampled_inaccessible_hook_member_is_killed_and_aborts_experiment(self):
        for hook_flag in ("--pre-sample-hook", "--post-run-check"):
            with self.subTest(hook=hook_flag):
                clock, app, hook = FakeClock(), FakeApp(), FakeApp()
                hook.pid = 2000
                signals = []

                class Collector(FakeProc):
                    def group_members(self, pgid):
                        if pgid == 2000:
                            return {2001: (2, 0)}
                        return super().group_members(pgid)
                    def info(self, pid):
                        if pid == 2001:
                            raise PermissionError(errno.EACCES, "unreadable member")
                        return super().info(pid)
                    def snapshot(self):
                        self.errors = [dict(pid=2001, errno=errno.EACCES, error="unreadable member")]
                        return {p.pid: self.info(p.pid) for p in (app, hook)
                                if p.exitcode is None}

                def group_signal(pid, sig):
                    self.assertEqual(pid, 2000)
                    self.assertFalse(hook.reaped)
                    signals.append((sig, clock.value))
                    hook.exitcode = 0  # The leader exits; its unreadable child ignores SIGTERM.

                proc = Collector(clock, app)
                metadata = dict(apps={v: {"binary": "/fake"} for v in ("baseline", "candidate")})
                with tempfile.TemporaryDirectory() as temp, \
                     patch("scripts.perf.profile.environment_metadata", return_value=metadata), \
                     patch("scripts.perf.profile.platform.system", return_value="Darwin"), \
                     patch("scripts.perf.profile.platform.machine", return_value="arm64"), \
                     patch("scripts.perf.profile.Libproc", return_value=proc), \
                     patch("scripts.perf.profile.subprocess.Popen", side_effect=[app, hook]) as launch, \
                     patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
                     patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                     patch("scripts.perf.profile.time.sleep", clock.sleep):
                    output = Path(temp) / "out"
                    status = main(["--output", str(output), "--authorize-launch", "--scenario", "overview",
                                   "--repetitions", "1", "--warmup", "0", "--duration", "20",
                                   "--hook-timeout", "1", hook_flag, "/fake-hook"])
                    report = json.loads((output / "report.json").read_text())
                self.assertIn(signal.SIGKILL, [sig for sig, unused in signals])
                self.assertGreaterEqual(signals[1][1] - signals[0][1], 1)
                self.assertEqual(status, 1)
                self.assertEqual([r["status"] for r in report["runs"]], ["failed", "skipped"])
                self.assertEqual(launch.call_count, 2)  # Only the first fake app and hook.
                first = report["runs"][0]
                self.assertTrue(first["hook_cleanup_unverified"])
                key = "pre_sample" if hook_flag == "--pre-sample-hook" else "post_run_check"
                self.assertFalse(first["hooks"][key]["cleanup_ok"])
                self.assertTrue(first["hooks"][key]["unverified_members"])
                self.assertTrue(first["quit"]["ok"])

    def test_missing_authorization_fails_without_launch(self):
        with patch("scripts.perf.profile.subprocess.Popen", side_effect=AssertionError("must not launch")):
            with self.assertRaises(SystemExit) as caught, patch("sys.stderr"):
                main(["--output", "/unused"])
        self.assertEqual(caught.exception.code, 2)


class GroupIdentityTests(unittest.TestCase):
    """Only fake signals: a reused numeric group must never be targeted."""

    def exercise(self, kind, reaped=False):
        clock, leader = FakeClock(), FakeApp()
        events = []
        child = dict(sample(1001, 2), pgid=1000)
        reused = dict(sample(1000, 99), pgid=1000)
        living = True

        class Collector:
            errors = []
            def snapshot(self):
                return {1000: reused} if leader.reaped else ({1001: child} if living else {})
            def group_members(self, pgid):
                return {pid: p["start_time"] for pid, p in self.snapshot().items()}
            def info(self, pid):
                return self.snapshot()[pid]

        def exited(pid):
            if leader.reaped:
                raise ChildProcessError
            return leader.exitcode is not None

        def wait(timeout=None):
            events.append(("reap", leader.reaped))
            leader.reaped = True
            leader.returncode = leader.exitcode
            return leader.returncode

        def group_signal(pid, signum):
            nonlocal living
            if signum:
                events.append((signum, leader.reaped))
                living = False
            elif not living:
                raise ProcessLookupError

        leader.wait = wait
        with patch.object(profile, "child_exited", side_effect=exited), \
             patch("scripts.perf.profile.os.kill", side_effect=AssertionError("unexpected individual signal")), \
             patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            group = profile.ProcessGroup(leader, Collector())
            leader.exitcode = 0
            leader.reaped = reaped
            if kind == "hook":
                evidence = {}
                profile.finish_hook(group, evidence, 1)
                ok = evidence["cleanup_ok"]
            else:
                evidence = clean_quit(Collector(), group, (1000, 1), {(1001, 2): child}, timeout=1)
                ok = evidence["ok"]
        return ok, events, leader.reaped

    def test_incomplete_members_force_pinned_group_kill_and_fail_cleanup(self):
        cases = [
            ("permission", errno.EACCES, 1000, None),
            ("read race", errno.ESRCH, 1000, None),
            ("other read error", errno.EIO, 1000, None),
            ("group query permission", errno.EACCES, PermissionError("query denied"), None),
            ("group query error", errno.EIO, OSError("query failed"), None),
            ("missing start", None, 1000, dict(sample(1001, 2), raw={})),
            ("zero start", None, 1000, sample(1001, 0)),
            ("null start", None, 1000, sample(1001, None)),
            ("missing pgid", None, 1000, sample(1001, 2)),
        ]
        for kind in ("hook", "app"):
            for name, code, pgid, partial in cases:
                with self.subTest(kind=kind, member=name):
                    clock, leader = FakeClock(), FakeApp()
                    signals = []
                    if partial is not None and name != "missing pgid":
                        partial["pgid"] = 1000

                    class Collector(FakeProc):
                        def group_members(self, group_id):
                            if isinstance(pgid, Exception):
                                raise pgid
                            return {1001: (2, 0)}
                        def info(self, pid):
                            if pid == 1001:
                                if code is not None:
                                    raise OSError(code, name)
                                return partial
                            return super().info(pid)
                        def snapshot(self):
                            self.errors = ([dict(pid=1001, errno=code, error=name)]
                                           if code is not None else [])
                            table = super().snapshot()
                            if partial is not None:
                                table[1001] = partial
                            return table

                    def group_signal(pid, sig):
                        self.assertEqual(pid, 1000)
                        self.assertFalse(leader.reaped)
                        signals.append((sig, clock.value))
                        leader.exitcode = 0

                    proc = Collector(clock, leader)
                    with patch("scripts.perf.profile.child_exited", side_effect=fake_child_exited), \
                         patch("scripts.perf.profile.os.kill", side_effect=fake_kill), \
                         patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
                         patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                         patch("scripts.perf.profile.time.sleep", clock.sleep):
                        group = profile.ProcessGroup(leader, proc)
                        self.assertNotIn((1001, 0), group.known)
                        self.assertNotIn((1001, None), group.known)
                        if kind == "hook":
                            evidence = {}
                            profile.finish_hook(group, evidence, 1, stop=True)
                            ok = evidence["cleanup_ok"]
                        else:
                            evidence = clean_quit(proc, group, (1000, 1), {}, timeout=1)
                            ok = evidence["ok"]
                    self.assertIn(signal.SIGKILL, [sig for sig, unused in signals])
                    self.assertGreaterEqual(signals[-1][1], 1)
                    self.assertFalse(ok)
                    self.assertTrue(evidence.get("unverified_members") or evidence.get("collector_error"))
                    self.assertTrue(leader.reaped)

    def test_enumerated_members_still_present_after_kill_fail_cleanup(self):
        for status in (1, 5):
            with self.subTest(status=status):
                clock, leader = FakeClock(), FakeApp()
                child = dict(sample(1001, 2), pgid=1000, status=status)
                proc = FakeProc(clock, leader)
                proc.snapshot = lambda: {1001: child}
                proc.group_members = lambda pgid: {1001: (2, 0)}
                proc.info = lambda pid: child
                signals = []
                def group_signal(pid, sig):
                    self.assertEqual(pid, 1000)
                    self.assertFalse(leader.reaped)
                    signals.append(sig)
                with patch("scripts.perf.profile.child_exited", side_effect=fake_child_exited), \
                     patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
                     patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
                     patch("scripts.perf.profile.time.sleep", clock.sleep):
                    group = profile.ProcessGroup(leader, proc)
                    leader.exitcode = 0
                    evidence = {}
                    profile.finish_hook(group, evidence, 1)
                self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
                self.assertFalse(evidence["cleanup_ok"])

    def test_read_errors_for_kernel_nonmembers_do_not_force_kill(self):
        clock, leader = FakeClock(), FakeApp()
        proc = FakeProc(clock, leader)
        proc.errors = [dict(pid=2001, errno=code, error="unrelated read error")
                       for code in (errno.EACCES, errno.ESRCH)]
        signals = []
        def group_signal(pid, sig):
            self.assertEqual(pid, 1000)
            self.assertFalse(leader.reaped)
            signals.append(sig)
            leader.exitcode = 0
        with patch("scripts.perf.profile.child_exited", side_effect=fake_child_exited), \
             patch("scripts.perf.profile.os.killpg", side_effect=group_signal), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            group = profile.ProcessGroup(leader, proc)
            evidence = {}
            profile.finish_hook(group, evidence, 1, stop=True)
        self.assertEqual(signals, [signal.SIGTERM])
        self.assertTrue(evidence["cleanup_ok"])

    def test_app_orphan_ledger_cannot_seed_signal_targets_after_leader_reap(self):
        clock, leader = FakeClock(), FakeApp()
        leader.returncode = leader.exitcode = 0
        leader.reaped = True
        reused_member = dict(sample(1001, 99), pgid=1000)
        signals = []

        class Collector:
            errors = []
            def snapshot(self):
                return {1001: reused_member}
            def group_members(self, pgid):
                return {1001: (99, 0)}
            def info(self, pid):
                return reused_member

        with patch("scripts.perf.profile.os.kill",
                   side_effect=lambda pid, sig: signals.append((pid, sig))), \
             patch("scripts.perf.profile.os.killpg",
                   side_effect=AssertionError("must not signal reused group")), \
             patch("scripts.perf.profile.time.monotonic", clock.monotonic), \
             patch("scripts.perf.profile.time.sleep", clock.sleep):
            collector = Collector()
            group = profile.ProcessGroup(leader, collector)
            # The orphan ledger is not proof this identity was recorded in the
            # original group while its leader was alive.
            evidence = clean_quit(collector, group, (1000, 1),
                                  {(1001, 99): reused_member}, timeout=1)
        self.assertEqual(signals, [])
        self.assertFalse(evidence["ok"])
        self.assertEqual(evidence["orphans"], [reused_member])

    def test_reaped_leader_fallback_only_signals_revalidated_member_identities(self):
        for current, expected in (
            (dict(sample(1001, 2), pgid=1000), [(1001, signal.SIGTERM)]),
            (dict(sample(1001, 99), pgid=1000), []),
            (dict(sample(1001, 2), pgid=999), []),
            (PermissionError("cannot verify member"), []),
        ):
            with self.subTest(current=current):
                leader = FakeApp()
                known = dict(sample(1001, 2), pgid=1000)
                signals, group_signals = [], []

                class Collector:
                    errors = []
                    def snapshot(self):
                        return {} if leader.reaped else {1001: known}
                    def group_members(self, pgid):
                        return {} if leader.reaped else {1001: (2, 0)}
                    def info(self, pid):
                        if not leader.reaped:
                            return known
                        if isinstance(current, Exception):
                            raise current
                        return current

                with patch("scripts.perf.profile.child_exited", side_effect=fake_child_exited), \
                     patch("scripts.perf.profile.os.kill",
                           side_effect=lambda pid, sig: signals.append((pid, sig))), \
                     patch("scripts.perf.profile.os.killpg",
                           side_effect=lambda pid, sig: group_signals.append((pid, sig))):
                    group = profile.ProcessGroup(leader, Collector())
                    leader.returncode = leader.exitcode = 0
                    leader.reaped = True
                    evidence = {}
                    profile.finish_hook(group, evidence, 1, stop=True)
                self.assertEqual(signals, expected)
                self.assertEqual(group_signals, [])
                self.assertFalse(evidence["cleanup_ok"])
                self.assertIn("identity_unverified", evidence)

    def test_reaped_hook_never_signals_reused_group_or_reports_success(self):
        ok, events, unused = self.exercise("hook", reaped=True)
        self.assertEqual([event for event in events if event[0] != "reap"], [])
        self.assertFalse(ok)

    def test_reaped_app_never_signals_reused_group_or_reports_success(self):
        ok, events, unused = self.exercise("app", reaped=True)
        self.assertEqual([event for event in events if event[0] != "reap"], [])
        self.assertFalse(ok)

    def test_hook_signals_with_zombie_leader_pinned_then_reaps(self):
        ok, events, reaped = self.exercise("hook")
        self.assertTrue(ok)
        self.assertEqual(events, [(signal.SIGTERM, False), ("reap", False)])
        self.assertTrue(reaped)

    def test_app_signals_with_zombie_leader_pinned_then_reaps(self):
        ok, events, reaped = self.exercise("app")
        self.assertFalse(ok)  # app needed forced cleanup of a surviving member
        self.assertEqual(events, [(signal.SIGKILL, False), ("reap", False)])
        self.assertTrue(reaped)


class MetadataAndHookTests(unittest.TestCase):
    def test_low_power_modes_are_source_specific_and_missing_is_unknown(self):
        self.assertEqual(parse_low_power_modes("Battery Power:\n lowpowermode 1\nAC Power:\n lowpowermode 0\n"),
                         {"Battery Power": True, "AC Power": False})
        self.assertEqual(parse_low_power_modes("Battery Power:\n sleep 0\n"), {})

    def test_app_identity_hashes_and_versions_without_running_binary(self):
        import plistlib
        import hashlib
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp) / "PAIR.app"
            binary = app / "Contents/MacOS/PAIR"
            manifest = app / "Contents/Resources/cli-bin/manifest.json"
            binary.parent.mkdir(parents=True)
            manifest.parent.mkdir(parents=True)
            binary.write_bytes(b"never execute me")
            binary.chmod(0o700)
            manifest.write_text('{"version":"fixture"}')
            with (app / "Contents/Info.plist").open("wb") as out:
                plistlib.dump({"CFBundleShortVersionString": "0.7.0-rc.2"}, out)
            info = app_metadata(app)
            self.assertEqual(info["binary_sha256"], hashlib.sha256(b"never execute me").hexdigest())
            self.assertEqual(info["manifest_sha256"], hashlib.sha256(b'{"version":"fixture"}').hexdigest())
            self.assertEqual(info["version"], "0.7.0-rc.2")
            self.assertNotIn("error", info)

    def test_whole_bundle_manifest_is_deterministic_and_sensitive_to_content_mode_and_links(self):
        with tempfile.TemporaryDirectory() as temp:
            tree = Path(temp) / "PAIR.app"
            tree.mkdir()
            (tree / "z").write_bytes(b"payload")
            (tree / "a").mkdir()
            (tree / "a/file").write_bytes(b"hello")
            (tree / "link").symlink_to("a")
            output = Path(temp) / "manifest.json"
            first = profile.bundle_manifest(tree, output)
            self.assertEqual(first, profile.bundle_manifest(tree, output))
            self.assertEqual(first, hashlib.sha256(output.read_bytes()).hexdigest())
            entries = json.loads(output.read_text())
            self.assertEqual([row["path"] for row in entries], [".", "a", "a/file", "link", "z"])
            link = entries[3]
            self.assertEqual(link["target"], "a")
            self.assertEqual(link["sha256"], hashlib.sha256(b"a").hexdigest())
            self.assertNotIn("link/file", [row["path"] for row in entries])
            (tree / "a/file").write_bytes(b"jello")
            second = profile.bundle_manifest(tree, output)
            self.assertNotEqual(first, second)
            (tree / "z").chmod(0o700)
            third = profile.bundle_manifest(tree, output)
            self.assertNotEqual(second, third)
            (tree / "link").unlink()
            (tree / "link").symlink_to("z")
            self.assertNotEqual(third, profile.bundle_manifest(tree, output))

    def test_launcher_lstat_snapshot_tracks_symlink_and_content_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "nvpair"
            self.assertEqual(profile.launcher_snapshot(path), {"state": "absent"})
            target = Path(temp) / "target"
            target.write_bytes(b"launcher")
            path.symlink_to("target")
            before = profile.launcher_snapshot(path)
            self.assertEqual(before["target"], "target")
            self.assertEqual(before["sha256"], hashlib.sha256(b"launcher").hexdigest())
            target.write_bytes(b"changed")
            self.assertNotEqual(before, profile.launcher_snapshot(path))
            path.unlink()
            path.symlink_to("missing")
            self.assertEqual(profile.launcher_snapshot(path)["content_state"], "absent")

    def test_artifact_metadata_retains_whole_bundle_and_optional_archive_hash(self):
        import plistlib
        with tempfile.TemporaryDirectory() as temp:
            tree = Path(temp) / "PAIR.app"
            binary = tree / "Contents/MacOS/PAIR"
            binary.parent.mkdir(parents=True)
            binary.write_bytes(b"fake")
            binary.chmod(0o700)
            manifest = tree / "Contents/Resources/cli-bin/manifest.json"
            manifest.parent.mkdir(parents=True)
            manifest.write_text("{}")
            with (tree / "Contents/Info.plist").open("wb") as stream:
                plistlib.dump({}, stream)
            archive = Path(temp) / "release.zip"
            archive.write_bytes(b"archive")
            output = Path(temp) / "bundle.json"
            info = app_metadata(tree, output, archive)
            self.assertEqual(info["bundle_manifest_sha256"], hashlib.sha256(output.read_bytes()).hexdigest())
            self.assertEqual(info["bundle_manifest_path"], str(output))
            self.assertEqual(info["archive_sha256"], hashlib.sha256(b"archive").hexdigest())

    def test_stream_consumed_without_persisting_response_and_requires_done(self):
        class Response:
            def __init__(self, lines):
                self.lines = lines
            def __enter__(self):
                return iter(self.lines)
            def __exit__(self, *unused):
                pass
        captured = []
        def opener(request, timeout):
            captured.append(request)
            return Response([b'data: {"choices":[{"delta":{"content":"secret"}}]}\n',
                             b'data: [DONE]\n'])
        with patch("scripts.perf.inference_hook.urllib.request.urlopen", opener):
            result = stream_request("http://localhost:9000/v1", "test-model", 16, 2)
        body = json.loads(captured[0].data)
        self.assertEqual(captured[0].full_url, "http://localhost:9000/v1/chat/completions")
        self.assertTrue(body["stream"])
        self.assertEqual(body["max_tokens"], 16)
        self.assertEqual(result["status"], "ok")
        self.assertNotIn("secret", json.dumps(result))
        with patch("scripts.perf.inference_hook.urllib.request.urlopen", return_value=Response([])):
            self.assertEqual(stream_request("http://local/v1", "model", 16, 2)["status"], "failed")

    def test_load_retains_counts_latency_and_no_content(self):
        with tempfile.TemporaryDirectory() as temp:
            rows = iter([dict(status="ok", latency_seconds=0.1),
                         dict(status="failed", latency_seconds=0.3, error="URLError")])
            with patch("scripts.perf.inference_hook.stream_request", side_effect=lambda *args: next(rows)):
                summary = run_load(Path(temp), "http://local/v1", "model", 16, 2,
                                   duration=10, delay=0, max_requests=2)
            self.assertEqual(summary["requests"], 2)
            self.assertEqual(summary["succeeded"], 1)
            self.assertEqual(summary["failed"], 1)
            self.assertEqual(summary["latency_seconds"]["median"], 0.2)
            self.assertEqual(len((Path(temp) / "requests.jsonl").read_text().splitlines()), 2)


class SandboxTests(unittest.TestCase):
    def test_environment_drops_ambient_state_and_redirects_writes(self):
        with tempfile.TemporaryDirectory() as temp:
            sandbox = Path(temp).resolve() / "home"
            sandbox.mkdir()
            with patch.dict(os.environ, {"HOME": "/real", "CFFIXED_USER_HOME": "/real",
                                         "OTEL_EXPORTER_OTLP_ENDPOINT": "http://real",
                                         "NVPAIR_TEST": "danger"}):
                env = sandbox_environment(sandbox)
            self.assertEqual(env["HOME"], str(sandbox))
            self.assertEqual(env["CFFIXED_USER_HOME"], str(sandbox))
            self.assertTrue(Path(env["TMPDIR"]).is_relative_to(sandbox))
            self.assertNotIn("NVPAIR_TEST", env)
            self.assertNotIn("OTEL_EXPORTER_OTLP_ENDPOINT", env)
            self.assertEqual(env["PATH"], "/usr/bin:/bin:/usr/sbin:/sbin")

    def test_template_copied_and_named_links_only(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            template, real = root / "template", root / "real"
            template.mkdir()
            real.mkdir()
            (template / "state").write_text("warm")
            (real / ".omlx").mkdir()
            sandbox = prepare_sandbox(root / "output", template, [".omlx"], real)
            self.assertEqual((sandbox / "state").read_text(), "warm")
            self.assertEqual((sandbox / ".omlx").resolve(), real / ".omlx")
            self.assertFalse((template / ".omlx").exists())
            for bad in ["../escape", "/absolute", "normal", ".", "..", ".a/b"]:
                with self.assertRaises(ValueError):
                    prepare_sandbox(root / "output", None, [bad], real)

    def test_template_cannot_contain_output_and_reserved_links_cannot_redirect_writes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            template, real = root / "template", root / "real"
            template.mkdir()
            real.mkdir()
            for name in (".cache", ".config", ".local"):
                (real / name).mkdir()
                with self.assertRaises(ValueError):
                    prepare_sandbox(root / "out", None, [name], real)
            with self.assertRaises(ValueError):
                prepare_sandbox(template / "output", template, [], real)
            self.assertEqual(list((real / ".local").iterdir()), [])

    def test_template_symlink_escape_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            template = root / "template"
            template.mkdir()
            (template / "escape").symlink_to(root)
            with self.assertRaises(ValueError):
                prepare_sandbox(root / "output", template, [], root)

    def test_cli_defaults_and_short_runs_marked_diagnostic(self):
        args = build_parser().parse_args(["--output", "/tmp/example"])
        self.assertEqual(args.repetitions, 5)
        self.assertEqual(args.warmup, 60)
        self.assertEqual(args.duration, 120)
        self.assertEqual(args.baseline, Path("/Applications/PAIR.app"))
        self.assertEqual(args.scenario, ["tray", "overview", "inference"])


if __name__ == "__main__":
    unittest.main()
