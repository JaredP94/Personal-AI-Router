# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Release harness tests; never launch PAIR, engines, or GUI applications."""

import ctypes
import os
from pathlib import Path
import tempfile
import time
import json
import sys
import unittest
from unittest.mock import patch

from scripts.perf.accounting import (
    Accounting, aggregate_runs, alternating_order, classify, render_markdown,
    spread, ticks_to_seconds,
)
from scripts.perf.libproc import Libproc, RusageV4, owned_processes, find_orphans
from scripts.perf.profile import (
    sandbox_environment, prepare_sandbox, build_parser, app_metadata,
    parse_low_power_modes, clean_quit, run_once, main,
)
from scripts.perf.inference_hook import stream_request, run_load


def sample(pid, start, user=0, system=0, name="PAIR", ppid=1, **extra):
    counters = dict(ri_proc_start_abstime=start, ri_user_time=user,
                    ri_system_time=system, ri_pkg_idle_wkups=0,
                    ri_interrupt_wkups=0, ri_phys_footprint=100,
                    ri_resident_size=200, ri_logical_writes=0,
                    ri_diskio_byteswritten=0, ri_billed_energy=0)
    counters.update(extra)
    return dict(pid=pid, ppid=ppid, name=name, path="/test/" + name,
                raw=counters)


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


class FakeClock:
    def __init__(self):
        self.value = 0
    def monotonic(self):
        return self.value
    def sleep(self, seconds):
        self.value += seconds


class FakeApp:
    pid = 1000
    returncode = None
    def poll(self):
        return self.returncode
    def terminate(self):
        self.returncode = 0


class FakeProc:
    numer, denom = 1, 1
    errors = []
    def __init__(self, clock, app):
        self.clock, self.app = clock, app
    def info(self, pid):
        return sample(pid, 1, user=int(self.clock.value * 1e9))
    def snapshot(self):
        return {} if self.app.returncode is not None else {1000: self.info(1000)}
    def absolute_time(self):
        return 100 + int(self.clock.value * 1e9)
    def seconds(self, ticks):
        return ticks / 1e9


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        probe = patch("scripts.perf.profile.command_evidence",
                      return_value=dict(exit_status=0, stdout="fixture state", stderr=""))
        probe.start()
        self.addCleanup(probe.stop)

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
            if clock.value >= 20 and app.poll() is None:
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
            result = clean_quit(proc, app, (1000, 1), {(2000, 20): child}, timeout=1)
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

    def test_missing_authorization_fails_without_launch(self):
        with patch("scripts.perf.profile.subprocess.Popen", side_effect=AssertionError("must not launch")):
            with self.assertRaises(SystemExit) as caught, patch("sys.stderr"):
                main(["--output", "/unused"])
        self.assertEqual(caught.exception.code, 2)


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
