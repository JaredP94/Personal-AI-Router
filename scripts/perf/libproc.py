# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Darwin libproc bindings. No subprocess launch or mutation of observed tasks."""

import ctypes
import errno
import os
import platform

from scripts.perf.accounting import identity, ticks_to_seconds


# Layout from macOS SDK sys/resource.h, RUSAGE_INFO_V4. Child counters are
# retained in raw evidence but never added to self counters (double counting).
RUSAGE_FIELDS = """
ri_user_time ri_system_time ri_pkg_idle_wkups ri_interrupt_wkups ri_pageins
ri_wired_size ri_resident_size ri_phys_footprint ri_proc_start_abstime
ri_proc_exit_abstime ri_child_user_time ri_child_system_time ri_child_pkg_idle_wkups
ri_child_interrupt_wkups ri_child_pageins ri_child_elapsed_abstime
ri_diskio_bytesread ri_diskio_byteswritten ri_cpu_time_qos_default
ri_cpu_time_qos_maintenance ri_cpu_time_qos_background ri_cpu_time_qos_utility
ri_cpu_time_qos_legacy ri_cpu_time_qos_user_initiated ri_cpu_time_qos_user_interactive
ri_billed_system_time ri_serviced_system_time ri_logical_writes
ri_lifetime_max_phys_footprint ri_instructions ri_cycles ri_billed_energy
ri_serviced_energy ri_interval_max_phys_footprint ri_runnable_time
""".split()


class RusageV4(ctypes.Structure):
    _fields_ = [("ri_uuid", ctypes.c_uint8 * 16)] + [
        (name, ctypes.c_uint64) for name in RUSAGE_FIELDS]


class BsdInfo(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint32) for name in (
        "flags", "status", "xstatus", "pid", "ppid", "uid", "gid", "ruid",
        "rgid", "svuid", "svgid", "reserved")] + [
        ("comm", ctypes.c_char * 16), ("name", ctypes.c_char * 32)] + [
        (name, ctypes.c_uint32) for name in (
            "nfiles", "pgid", "pjobc", "tdev", "tpgid", "nice")] + [
        ("start_sec", ctypes.c_uint64), ("start_usec", ctypes.c_uint64)]


def child_exited_libc(pid):
    """Darwin waitid for Python 3.9 builds that do not expose os.waitid.

    Darwin siginfo_t starts with four ints (signo, errno, code, pid). An
    oversized, int-aligned buffer accommodates the rest without depending on
    its union layout. WNOWAIT keeps the child PID/PGID reserved, even at exit.
    """
    if platform.system() != "Darwin":
        raise RuntimeError("libc waitid fallback requires Darwin")
    libc = ctypes.CDLL(None, use_errno=True)
    waitid = libc.waitid
    waitid.argtypes = [ctypes.c_uint, ctypes.c_uint, ctypes.c_void_p, ctypes.c_int]
    waitid.restype = ctypes.c_int
    info = (ctypes.c_int * 128)()
    if waitid(os.P_PID, pid, info, os.WEXITED | os.WNOHANG | os.WNOWAIT) != 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))
    return info[3] != 0


def child_exited(pid):
    """Observe exit without reaping; ECHILD means group signalling is unsafe."""
    if hasattr(os, "waitid"):
        return os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None
    return child_exited_libc(pid)


class Timeval(ctypes.Structure):
    _fields_ = [("sec", ctypes.c_long), ("usec", ctypes.c_int)]


class KinfoProc(ctypes.Structure):
    # Darwin LP64 sys/sysctl.h + sys/proc.h (arm64 and x86_64): sizeof 648,
    # kp_proc.p_un.__p_starttime at 0, kp_proc.p_pid at 40. The unused tail
    # stays opaque. Checked with sizeof/offsetof against the macOS SDK.
    _fields_ = [("start", Timeval), ("_prefix", ctypes.c_byte * 24),
                ("pid", ctypes.c_int), ("_tail", ctypes.c_byte * 604)]


class Timebase(ctypes.Structure):
    _fields_ = [("numer", ctypes.c_uint32), ("denom", ctypes.c_uint32)]


def owned_processes(table, root_identity, known):
    """Root and recursive descendants, plus previously observed reparented tasks.

    A reused PID must match the original start time to become a retained seed.
    A newly born descendant can use an arbitrary executable name.
    """
    owned = {pid: p for pid, p in table.items()
             if identity(p) == root_identity or identity(p) in known}
    while True:
        extra = {pid: p for pid, p in table.items()
                 if pid not in owned and p["ppid"] in owned}
        if not extra:
            return owned
        owned.update(extra)


def find_orphans(table, root_identity, known):
    return list(owned_processes(table, root_identity, known).values())


class Libproc:
    def __init__(self):
        if platform.system() != "Darwin":
            raise RuntimeError("libproc sampling requires macOS")
        self.lib = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        self.system = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
        self.lib.proc_listallpids.argtypes = [ctypes.c_void_p, ctypes.c_int]
        self.lib.proc_listallpids.restype = ctypes.c_int
        self.lib.proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
        self.lib.proc_pidpath.restype = ctypes.c_int
        self.lib.proc_pidinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
                                          ctypes.c_void_p, ctypes.c_int]
        self.lib.proc_pidinfo.restype = ctypes.c_int
        # rusage_info_t* is historically spelled void** but the API writes the
        # structure itself at this address, not a pointer to allocated memory.
        self.lib.proc_pid_rusage.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_void_p]
        self.lib.proc_pid_rusage.restype = ctypes.c_int
        if ctypes.sizeof(ctypes.c_void_p) != 8:
            raise RuntimeError("KERN_PROC_PGRP requires the Darwin LP64 ABI")
        self.system.sysctl.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint,
            ctypes.c_void_p, ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t]
        self.system.sysctl.restype = ctypes.c_int
        self.system.mach_timebase_info.argtypes = [ctypes.POINTER(Timebase)]
        self.system.mach_timebase_info.restype = ctypes.c_int
        self.system.mach_absolute_time.argtypes = []
        self.system.mach_absolute_time.restype = ctypes.c_uint64
        timebase = Timebase()
        if self.system.mach_timebase_info(ctypes.byref(timebase)) != 0:
            raise RuntimeError("mach_timebase_info failed")
        self.numer, self.denom = timebase.numer, timebase.denom
        self.errors = []

    def absolute_time(self):
        return self.system.mach_absolute_time()

    def seconds(self, ticks):
        return ticks_to_seconds(ticks, self.numer, self.denom)

    def pids(self):
        # proc_listallpids returns a count, not a byte count. Retry if it filled
        # our buffer; the extra slots accommodate forks during enumeration.
        for _ in range(4):
            count = self.lib.proc_listallpids(None, 0)
            if count <= 0:
                raise OSError(ctypes.get_errno(), "proc_listallpids failed")
            buffer = (ctypes.c_int * (count + 256))()
            received = self.lib.proc_listallpids(buffer, ctypes.sizeof(buffer))
            if received <= 0:
                raise OSError(ctypes.get_errno(), "proc_listallpids failed")
            if received < len(buffer):
                return [pid for pid in buffer[:received] if pid > 0]
        raise RuntimeError("process table grew repeatedly during enumeration")

    def group_members(self, pgid):
        """Authoritative membership, independent of per-process read permission.

        Identities use the kernel's timeval start time, not rusage mach ticks.
        Retry ENOMEM if the group grew between size and data queries. Never
        turn a query error or truncated record into an empty group.
        """
        if pgid <= 0:
            raise ValueError("a positive process group ID is required")
        mib = (ctypes.c_int * 4)(1, 14, 2, pgid)  # CTL_KERN, KERN_PROC, KERN_PROC_PGRP
        for _ in range(4):
            size = ctypes.c_size_t()
            if self.system.sysctl(mib, 4, None, ctypes.byref(size), None, 0) != 0:
                code = ctypes.get_errno()
                raise OSError(code, "KERN_PROC_PGRP size query failed")
            buffer = (KinfoProc * (size.value // ctypes.sizeof(KinfoProc) + 16))()
            size.value = ctypes.sizeof(buffer)
            if self.system.sysctl(mib, 4, buffer, ctypes.byref(size), None, 0) != 0:
                code = ctypes.get_errno()
                if code == errno.ENOMEM:
                    continue
                raise OSError(code, "KERN_PROC_PGRP query failed")
            if size.value > ctypes.sizeof(buffer) or size.value % ctypes.sizeof(KinfoProc):
                raise RuntimeError("invalid KERN_PROC_PGRP record size")
            result = {}
            for entry in buffer[:size.value // ctypes.sizeof(KinfoProc)]:
                if entry.pid <= 0 or entry.start.sec <= 0 or not 0 <= entry.start.usec < 1_000_000:
                    raise RuntimeError("invalid KERN_PROC_PGRP identity")
                if entry.pid in result:
                    raise RuntimeError("duplicate KERN_PROC_PGRP PID")
                result[entry.pid] = (entry.start.sec, entry.start.usec)
            return result
        raise RuntimeError("process group grew repeatedly during enumeration")

    def _bsd(self, pid):
        result = BsdInfo()
        size = self.lib.proc_pidinfo(pid, 3, 0, ctypes.byref(result), ctypes.sizeof(result))
        if size != ctypes.sizeof(result):
            raise OSError(ctypes.get_errno() or errno.ESRCH, "proc_pidinfo failed", pid)
        return result

    def info(self, pid):
        before = self._bsd(pid)
        raw = RusageV4()
        if self.lib.proc_pid_rusage(pid, 4, ctypes.byref(raw)) != 0:
            raise OSError(ctypes.get_errno() or errno.ESRCH, "proc_pid_rusage failed", pid)
        path = ctypes.create_string_buffer(4096)
        if self.lib.proc_pidpath(pid, path, len(path)) <= 0:
            raise OSError(ctypes.get_errno() or errno.ESRCH, "proc_pidpath failed", pid)
        after = self._bsd(pid)
        if (before.start_sec, before.start_usec) != (after.start_sec, after.start_usec):
            raise ProcessLookupError(errno.ESRCH, "PID reused during read", pid)
        executable = os.fsdecode(path.value)
        return dict(pid=pid, ppid=after.ppid, pgid=after.pgid, status=after.status,
                    start_time=(after.start_sec, after.start_usec), path=executable,
                    name=os.path.basename(executable),
                    raw={name: getattr(raw, name) for name in RUSAGE_FIELDS})

    def snapshot(self):
        """Best-effort system snapshot; the caller selects only owned evidence.

        Unrelated permission/exit races are retained as counts by the runner,
        rather than mistaking inaccessible processes for measured zeros.
        """
        result = {}
        self.errors = []
        for pid in self.pids():
            try:
                result[pid] = self.info(pid)
            except OSError as error:
                self.errors.append(dict(pid=pid, errno=error.errno, error=str(error)))
        return result
