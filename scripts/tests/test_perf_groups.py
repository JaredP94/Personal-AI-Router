# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Group cleanup regressions; only our own console children may be signalled."""

import ctypes
import errno
import os
from pathlib import Path
import signal
import struct
import subprocess
import sys
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from scripts.perf import profile
from scripts.perf.libproc import Libproc, child_exited
from scripts.tests.test_release_profiling import FakeApp, FakeClock, fake_child_exited, fake_kill, sample


class GroupMembershipTests(unittest.TestCase):
    def test_unrelated_unreadable_pids_do_not_fail_quit_or_hook(self):
        for kind in ("app", "hook"):
            with self.subTest(kind=kind):
                leader, clock = FakeApp(), FakeClock()
                class Collector:
                    errors = [dict(pid=1, errno=errno.EPERM, error="unrelated system task")]
                    def group_members(self, pgid):
                        return {1000: (10, 20)}
                    def snapshot(self):
                        return {}
                with patch.object(profile, "child_exited", side_effect=fake_child_exited), \
                     patch.object(profile.os, "kill", side_effect=fake_kill), \
                     patch.object(profile.os, "getpgid", side_effect=PermissionError(errno.EPERM, "denied")), \
                     patch.object(profile.os, "killpg", side_effect=PermissionError(errno.EPERM, "empty zombie-only group")), \
                     patch.object(profile.time, "monotonic", clock.monotonic), \
                     patch.object(profile.time, "sleep", clock.sleep):
                    leader.exitcode = 0
                    group = profile.ProcessGroup(leader, Collector())
                    if kind == "app":
                        evidence = profile.clean_quit(group.proc, group, (1000, 1), {}, timeout=1)
                        self.assertTrue(evidence["ok"], evidence)
                    else:
                        evidence = {}
                        profile.finish_hook(group, evidence, 1, stop=True)
                        self.assertTrue(evidence["cleanup_ok"], evidence)
                self.assertTrue(leader.reaped)

    def test_quit_still_checks_recorded_descendants_outside_leader_group(self):
        for state, expected in (("present", False), ("unreadable", False), ("pid reused", True)):
            with self.subTest(state=state):
                leader, clock = FakeApp(), FakeClock()
                known = dict(sample(2000, 2), pgid=2000)
                current = dict(sample(2000, 99 if state == "pid reused" else 2), pgid=2000)
                class Collector:
                    errors = ([dict(pid=2000, errno=errno.EPERM, error="recorded descendant denied")]
                              if state == "unreadable" else [])
                    def group_members(self, pgid):
                        return {}
                    def snapshot(self):
                        return {} if state == "unreadable" else {2000: current}
                leader.exitcode = 0
                with patch.object(profile, "child_exited", side_effect=fake_child_exited), \
                     patch.object(profile.time, "monotonic", clock.monotonic), \
                     patch.object(profile.time, "sleep", clock.sleep):
                    proc = Collector()
                    evidence = profile.clean_quit(proc, profile.ProcessGroup(leader, proc),
                        (1000, 1), {(2000, 2): known}, timeout=1)
                self.assertEqual(evidence["ok"], expected, evidence)

    def test_failed_hook_cleanup_is_not_repeated_after_reap(self):
        leader, clock = FakeApp(), FakeClock()
        member = dict(sample(1001, 2), pgid=1000, start_time=(11, 20))
        class Collector:
            errors = []
            def snapshot(self):
                return {1001: member}
            def group_members(self, pgid):
                return {1001: (11, 20)}
            def info(self, pid):
                return member
        with patch.object(profile, "child_exited", side_effect=fake_child_exited), \
             patch.object(profile.os, "killpg", side_effect=PermissionError(errno.EPERM, "denied")), \
             patch.object(profile.time, "monotonic", clock.monotonic), \
             patch.object(profile.time, "sleep", clock.sleep):
            leader.exitcode = 4
            group = profile.ProcessGroup(leader, Collector())
            evidence = {}
            profile.finish_hook(group, evidence, 1, stop=True)
            self.assertFalse(evidence["cleanup_ok"])
            original = dict(evidence)
            profile.finish_hook(group, evidence, 1, stop=True)
            self.assertEqual(evidence, original)
            self.assertNotIn("identity_unverified", evidence)
            self.assertIn("group_errors", evidence)  # EPERM is never disappearance.

    def test_reservation_lost_during_membership_query_forbids_group_signal(self):
        leader = FakeApp()
        class Collector:
            def group_members(self, pgid):
                leader.reaped = True
                return {1001: (11, 20)}
            def info(self, pid):
                return dict(sample(1001, 2), pgid=1000, start_time=(11, 20))
        with patch.object(profile, "child_exited", side_effect=fake_child_exited), \
             patch.object(profile.os, "killpg", side_effect=AssertionError("unreserved group signal")), \
             patch.object(profile.os, "kill", side_effect=AssertionError("unrecorded member signal")):
            # Construct alive first; lose reservation only inside send's query.
            collector = Collector()
            with patch.object(collector, "group_members", return_value={}):
                group = profile.ProcessGroup(leader, collector)
            group.send(signal.SIGTERM)
        self.assertFalse(group.pinned)
        self.assertIn("identity_unverified", group.errors)

    def test_leader_exit_during_member_read_cannot_seed_signal_fallback(self):
        leader = FakeApp()
        class Collector:
            def group_members(self, pgid):
                return {1001: (11, 20)}
            def info(self, pid):
                leader.exitcode = 3
                return dict(sample(1001, 2), pgid=1000, start_time=(11, 20))
        with patch.object(profile, "child_exited", side_effect=fake_child_exited):
            group = profile.ProcessGroup(leader, Collector())
        self.assertEqual(group.known, {})

    def test_hook_fork_then_exit_requires_post_exit_membership_before_reap(self):
        for transition in ("membership query", "member read"):
            with self.subTest(transition=transition):
                leader, clock = FakeApp(), FakeClock()
                class Collector:
                    armed = False
                    child_alive = False
                    def fork_then_exit(self):
                        if self.armed and leader.exitcode is None:
                            self.child_alive = True
                            leader.exitcode = 3
                    def group_members(self, pgid):
                        members = {1000: (10, 20)}
                        if self.child_alive:
                            members[1001] = (11, 20)
                        if transition == "membership query":
                            self.fork_then_exit()  # Snapshot predates the fork.
                        return members
                    def info(self, pid):
                        if transition == "member read":
                            self.fork_then_exit()
                        return dict(sample(pid, 1 if pid == 1000 else 2), pgid=1000,
                                    start_time=(10, 20) if pid == 1000 else (11, 20))
                collector = Collector()
                def group_signal(pgid, signum):
                    self.assertEqual(pgid, 1000)
                    self.assertFalse(leader.reaped, "group signalled after reap")
                    if signum == signal.SIGTERM:
                        collector.armed = True
                    elif signum == signal.SIGKILL:
                        collector.child_alive = False
                with patch.object(profile, "child_exited", side_effect=fake_child_exited), \
                     patch.object(profile.os, "killpg", side_effect=group_signal), \
                     patch.object(profile.os, "kill", side_effect=AssertionError("unexpected individual signal")), \
                     patch.object(profile.time, "monotonic", clock.monotonic), \
                     patch.object(profile.time, "sleep", clock.sleep):
                    group = profile.ProcessGroup(leader, collector)
                    evidence = {}
                    profile.finish_hook(group, evidence, 1, stop=True)
                self.assertFalse(collector.child_alive,
                                 "cleanup reaped the leader while its raced child survived")
                self.assertTrue(evidence["cleanup_ok"], evidence)
                self.assertTrue(evidence.get("forced_kill"), evidence)
                self.assertTrue(leader.reaped)
                self.assertEqual(evidence["exit_status"], 3)

    def test_group_empty_after_raced_member_read_resolves_uncertainty(self):
        leader = FakeApp()
        class Collector:
            members = {1001: (11, 20)}
            def group_members(self, pgid):
                return self.members
            def info(self, pid):
                raise ProcessLookupError(errno.ESRCH, "exited during read")
        collector = Collector()
        with patch.object(profile, "child_exited", side_effect=fake_child_exited):
            group = profile.ProcessGroup(leader, collector)
            self.assertTrue(group.errors.get("unverified_members"))
            self.assertFalse(group.observe())  # Still enumerated: fail closed.
            leader.exitcode = 0
            collector.members = {}
            self.assertTrue(group.observe())
            self.assertFalse(group.errors)

    def test_member_start_time_mismatch_blocks_identity_adoption(self):
        leader = FakeApp()
        member = dict(sample(1001, 2), pgid=1000, start_time=(99, 0))
        class Collector:
            errors = []
            def snapshot(self):
                return {1001: member}
            def group_members(self, pgid):
                return {1001: (11, 20)}
            def info(self, pid):
                return member
        with patch.object(profile, "child_exited", side_effect=fake_child_exited):
            group = profile.ProcessGroup(leader, Collector())
        self.assertEqual(group.known, {})
        self.assertTrue(group.errors.get("unverified_members"))


class KernelQueryTests(unittest.TestCase):
    def query(self, payload, grow_once=False, failure=None):
        calls = []
        def sysctl(mib, count, buffer, size_ptr, new, new_size):
            if list(mib) != [1, 14, 2, 1234] or count != 4 or new is not None or new_size != 0:
                raise AssertionError("wrong read-only PGRP MIB")
            size = ctypes.cast(size_ptr, ctypes.POINTER(ctypes.c_size_t))
            if buffer is None:
                size[0] = len(payload)
                return 0
            calls.append(1)
            if failure or (grow_once and len(calls) == 1):
                ctypes.set_errno(failure or errno.ENOMEM)
                return -1
            ctypes.memmove(buffer, payload, len(payload))
            size[0] = len(payload)
            return 0
        proc = Libproc.__new__(Libproc)
        proc.system = SimpleNamespace(sysctl=sysctl)
        return proc.group_members(1234)

    def test_kernel_pid_and_timeval_identity_survive_buffer_growth(self):
        # Independent SDK ABI fixture: 648-byte LP64 kinfo_proc, timeval at
        # offset 0 (long seconds, int microseconds), pid_t at offset 40.
        record = bytearray(648)
        struct.pack_into("=qi", record, 0, 1700000000, 123456)
        struct.pack_into("=i", record, 40, 1235)
        self.assertEqual(self.query(bytes(record), grow_once=True), {1235: (1700000000, 123456)})
        self.assertEqual(self.query(b""), {})

    def test_query_errors_and_malformed_records_cannot_mean_empty_group(self):
        with self.assertRaises(PermissionError):
            self.query(b"", failure=errno.EPERM)
        for payload in (b"x", bytes(648)):
            with self.subTest(size=len(payload)), self.assertRaises(RuntimeError):
                self.query(payload)


@unittest.skipUnless(sys.platform == "darwin", "KERN_PROC_PGRP requires macOS")
class RealHookGroupTests(unittest.TestCase):
    def test_self_exiting_hook_keeps_leader_pinned_until_sleep_group_is_empty(self):
        proc = Libproc()
        with tempfile.TemporaryDirectory() as temp:
            handles = []
            group, evidence = profile.start_hook("/bin/sh -c 'sleep 30 & exit 3'", "hook",
                Path(temp), dict(os.environ, HOME=temp), handles, proc)
            try:
                deadline = time.monotonic() + 5
                while not group.exited():
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(0.01)
                self.assertIsNone(group.process.returncode)
                members = proc.group_members(group.pid)
                self.assertIn(group.pid, members)
                children = set(members) - {group.pid}
                self.assertEqual(len(children), 1)
                child = children.pop()
                self.assertEqual(members[child], tuple(proc.info(child)["start_time"]))
                # Metadata Popen creation invokes subprocess._cleanup; it must
                # not reap this retained hook. This mirrors between-run probes.
                subprocess.run(["/bin/sh", "-c", "exit 0"], check=True)
                self.assertTrue(child_exited(group.pid))
                profile.finish_hook(group, evidence, 5)
                self.assertTrue(evidence["cleanup_ok"], evidence)
                self.assertEqual(evidence["exit_status"], 3)
                self.assertFalse(evidence.get("forced_kill"), evidence)
                self.assertEqual(proc.group_members(group.pid), {})
                with self.assertRaises(ChildProcessError):
                    child_exited(group.pid)
            finally:
                # On a failed assertion only our still-waitable test group may
                # be signalled; never use its number after final reaping.
                if group.process.returncode is None:
                    try:
                        if child_exited(group.pid) or group.pinned:
                            os.killpg(group.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    group.process.wait(timeout=5)
                for handle in handles:
                    handle.close()
