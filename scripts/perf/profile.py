# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Opt-in macOS artifact profiler. Importing this module never starts an app.

Invoke from the repository root: python3 -m scripts.perf.profile --help
"""

import argparse
import errno
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import plistlib
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

from scripts.perf.accounting import (
    Accounting, ENERGY_LABEL, LIMITS, aggregate_runs, alternating_order, identity,
    render_markdown,
)
from scripts.perf.libproc import (
    Libproc, child_exited, find_orphans, owned_processes,
)


ROOT = Path(__file__).resolve().parents[2]
SYSTEM_LAUNCHER = Path("/usr/local/bin/nvpair")


class Cancelled(Exception):
    pass


class Cancellation:
    """Signals only set state: never interrupt Popen assignment or teardown."""
    def __init__(self):
        self.signum = None
        self.previous = {}

    def __enter__(self):
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            self.previous[signum] = signal.signal(signum, self.request)
        return self

    def __exit__(self, *unused):
        for signum, handler in self.previous.items():
            signal.signal(signum, handler)

    def request(self, signum, frame):
        if self.signum is None:
            self.signum = signum

    def check(self):
        if self.signum is not None:
            raise Cancelled(signal.Signals(self.signum).name)

    def sleep(self, seconds):
        deadline = time.monotonic() + seconds
        self.check()
        while time.monotonic() < deadline:
            time.sleep(min(0.5, max(0, deadline - time.monotonic())))
            self.check()


def now():
    return datetime.now(timezone.utc).isoformat()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def sandbox_environment(sandbox):
    """Do not inherit the real profile, telemetry, credentials or engine settings."""
    sandbox = sandbox.resolve()
    for name in ("tmp", ".cache", ".config", ".local/share"):
        (sandbox / name).mkdir(parents=True, exist_ok=True)
    return dict(HOME=str(sandbox), CFFIXED_USER_HOME=str(sandbox),
                TMPDIR=str(sandbox / "tmp") + "/", XDG_CACHE_HOME=str(sandbox / ".cache"),
                XDG_CONFIG_HOME=str(sandbox / ".config"),
                XDG_DATA_HOME=str(sandbox / ".local/share"),
                PATH="/usr/bin:/bin:/usr/sbin:/sbin", LANG="en_US.UTF-8",
                PYTHONDONTWRITEBYTECODE="1")


def prepare_sandbox(output, template, links, real_home):
    for name in links:
        if (not name.startswith(".") or name in (".", "..", ".cache", ".config", ".local")
                or Path(name).name != name):
            raise ValueError("engine links must be named dot-directories, not paths: " + name)
        if not (real_home / name).is_dir():
            raise ValueError("engine link is not an installed directory: " + name)
    if template:
        if output.resolve().is_relative_to(template.resolve()):
            raise ValueError("output must not be inside its own sandbox template")
        # No inherited symlink can redirect state writes back into a real home.
        for path in [template] + list(template.rglob("*")):
            if path.is_symlink() or not (path.is_dir() or path.is_file()):
                raise ValueError("sandbox template must contain only regular files/directories")
    output.mkdir(parents=True, exist_ok=True)
    sandbox = Path(tempfile.mkdtemp(prefix="home-", dir=output)).resolve()
    if template:
        shutil.copytree(template, sandbox, dirs_exist_ok=True)
    for name in links:
        target = sandbox / name
        if target.exists():
            raise ValueError("engine link collides with sandbox template: " + name)
        target.symlink_to((real_home / name).resolve(), target_is_directory=True)
    return sandbox


def command_evidence(command, timeout=15):
    """Read-only metadata probes, without shell expansion or screenshot capture."""
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout,
                                check=False)
        return dict(command=command, exit_status=result.returncode,
                    stdout=result.stdout, stderr=result.stderr)
    except (OSError, subprocess.TimeoutExpired) as error:
        return dict(command=command, exit_status=None, error=type(error).__name__)


def hash_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def bundle_manifest(bundle, output):
    """Canonical content/mode inventory; symlinks are content, never traversed."""
    entries = []

    def visit(path):
        info = path.lstat()
        entry = dict(path=path.relative_to(bundle).as_posix(), mode=stat.S_IMODE(info.st_mode))
        if stat.S_ISLNK(info.st_mode):
            target = os.readlink(path)
            entry.update(type="symlink", target=target,
                         sha256=hashlib.sha256(os.fsencode(target)).hexdigest())
        elif stat.S_ISREG(info.st_mode):
            entry.update(type="file", sha256=hash_file(path))
        elif stat.S_ISDIR(info.st_mode):
            entry["type"] = "directory"
        else:
            raise ValueError("unsupported bundle entry: " + str(path))
        entries.append(entry)
        if entry["type"] == "directory":
            for child in sorted(path.iterdir()):
                visit(child)

    visit(bundle)
    entries.sort(key=lambda entry: entry["path"])
    write_json(output, entries)
    return hash_file(output)


def launcher_snapshot(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return dict(state="absent")
    result = dict(state="present", lstat={key: getattr(info, key) for key in
        ("st_dev", "st_ino", "st_mode", "st_nlink", "st_uid", "st_gid", "st_size",
         "st_mtime_ns", "st_ctime_ns")})
    if stat.S_ISLNK(info.st_mode):
        result["target"] = os.readlink(path)
    try:
        # Include the referent's contents, not just a symlink's target string.
        if path.is_file():
            result["sha256"] = hash_file(path)
        elif path.is_symlink() and not path.exists():
            result["content_state"] = "absent"
        else:
            raise ValueError("system launcher is not a regular file or symlink")
    except FileNotFoundError:
        raise RuntimeError("system launcher changed while being snapshotted")
    return result


def app_metadata(path, bundle_output=None, archive=None):
    path = path.resolve()
    binary = path / "Contents/MacOS/PAIR"
    manifest = path / "Contents/Resources/cli-bin/manifest.json"
    result = dict(path=str(path), binary=str(binary), manifest_path=str(manifest))
    try:
        if bundle_output is not None:
            result["bundle_manifest_path"] = str(bundle_output)
            result["bundle_manifest_sha256"] = bundle_manifest(path, bundle_output)
        if archive is not None:
            result["archive_path"] = str(archive.resolve())
            result["archive_sha256"] = hash_file(archive)
        result["binary_sha256"] = hash_file(binary)
        result["manifest_sha256"] = hash_file(manifest)
        result["service_manifest"] = json.loads(manifest.read_text(encoding="utf-8"))
        with (path / "Contents/Info.plist").open("rb") as stream:
            info = plistlib.load(stream)
        result["version"] = info.get("CFBundleShortVersionString")
        result["build"] = info.get("CFBundleVersion")
        if not os.access(binary, os.X_OK):
            raise ValueError("app executable is not executable")
    except (OSError, ValueError, plistlib.InvalidFileException) as error:
        result["error"] = str(error)
    return result


def parse_low_power_modes(text):
    """Parse pmset's source-specific settings; do not invent a value if absent."""
    result = {}
    source = None
    for line in text.splitlines():
        if line and not line[0].isspace() and line.rstrip().endswith(":"):
            source = line.strip().rstrip(":")
        fields = line.split()
        if source and len(fields) == 2 and fields[0] == "lowpowermode" and fields[1] in ("0", "1"):
            result[source] = fields[1] == "1"
    return result


def power_display_metadata():
    probes = dict(power_source=["/usr/bin/pmset", "-g", "batt"],
                  power_modes=["/usr/bin/pmset", "-g", "custom"],
                  display_state=["/usr/sbin/ioreg", "-r", "-n", "IODisplayWrangler", "-d", "1"])
    evidence = {name: command_evidence(command) for name, command in probes.items()}
    evidence["timestamp"] = now()
    evidence["low_power_mode_by_source"] = parse_low_power_modes(evidence["power_modes"].get("stdout", ""))
    return evidence


def environment_metadata(args, argv):
    probes = dict(
        macos=["/usr/bin/sw_vers"], hardware_model=["/usr/sbin/sysctl", "-n", "hw.model"],
        hardware=["/usr/sbin/sysctl", "hw.machine", "hw.memsize", "hw.ncpu", "machdep.cpu.brand_string"],
    )
    evidence = {name: command_evidence(command) for name, command in probes.items()}
    evidence.update(power_display_metadata())
    return dict(timestamp=now(), argv=argv, platform=platform.platform(),
                machine=platform.machine(), python=sys.version, probes=evidence,
                low_power_mode_by_source=parse_low_power_modes(evidence["power_modes"].get("stdout", "")),
                low_power_mode_note="See lowpowermode in power_modes for each source; missing means unavailable.",
                display_note="IODisplayWrangler only; absence means unavailable, no screenshots.",
                source_commit=command_evidence(["git", "-C", str(ROOT), "rev-parse", "HEAD"]),
                apps={variant: app_metadata(getattr(args, variant),
                      args.output.resolve() / (variant + "-bundle-manifest.json"),
                      getattr(args, variant + "_archive")) for variant in ("baseline", "candidate")},
                protocol=vars_for_json(args), energy_label=ENERGY_LABEL, limits=LIMITS)


def vars_for_json(args):
    return {key: str(value) if isinstance(value, Path) else value for key, value in vars(args).items()}


def hook_environment(env, run_dir, sandbox, app_pid, scenario):
    return dict(env, PAIR_PERF_OUTPUT=str(run_dir), PAIR_PERF_SANDBOX=str(sandbox),
                PAIR_PERF_APP_PID=str(app_pid), PAIR_PERF_SCENARIO=scenario)


def start_hook(command, name, run_dir, env, handles, proc):
    stdout = (run_dir / (name + ".stdout.txt")).open("w", encoding="utf-8")
    handles.append(stdout)
    stderr = (run_dir / (name + ".stderr.txt")).open("w", encoding="utf-8")
    handles.append(stderr)
    process = subprocess.Popen(shlex.split(command), cwd=env["HOME"], env=env,
                               stdout=stdout, stderr=stderr, start_new_session=True)
    return ProcessGroup(process, proc), dict(command=command, pid=process.pid, started=now(),
                         stdout=stdout.name, stderr=stderr.name)


class ProcessGroup:
    """A new-session child whose unreaped leader reserves the numeric PGID.

    This is the sole owner of reaping. Never call Popen.poll/wait/send_signal
    elsewhere: even poll and send_signal can reap an exited child. Identities
    are collected only while the leader is alive; after loss of the reservation
    only those individually revalidated identities may be signalled.
    """
    def __init__(self, process, proc):
        self.process, self.proc = process, proc
        self.pid = process.pid
        self.known = {}
        self.pinned = True
        self.errors = {}
        self.observe()

    def exited(self):
        try:
            if self.process.returncode is not None:
                raise ChildProcessError("leader already reaped")
            return child_exited(self.pid)
        except OSError as error:
            self.pinned = False
            self.errors["identity_unverified"] = str(error) or "leader is no longer a waitable child"
            return True

    def observe(self):
        exited = self.exited()
        try:
            # The system sampling table is not a membership oracle: unrelated
            # root-owned tasks routinely deny libproc reads. Query this group
            # directly, then read/revalidate only its kernel-listed identities.
            membership = self.proc.group_members(self.pid)
            exited = self.exited() or exited
            group, inaccessible = {}, []
            for pid, start in membership.items():
                if pid == self.pid and exited:
                    continue  # waitid positively confirms the reserved leader exited.
                try:
                    process = self.proc.info(pid)
                    if (process.get("pgid") != self.pid or
                            tuple(process.get("start_time", ())) != tuple(start) or
                            not process.get("raw", {}).get("ri_proc_start_abstime")):
                        raise ValueError("incomplete or changed member identity")
                    group[pid] = process
                except (OSError, ValueError, TypeError) as error:
                    inaccessible.append(dict(pid=pid, start_time=start, error=str(error)))
            exited = self.exited() or exited  # Full reads may outlive the leader.
            if self.pinned and not exited:
                self.known.update({identity(p): p for p in group.values()})
            unknown = [pid for pid, p in group.items()
                       if pid != self.pid and identity(p) not in self.known]
            if unknown or inaccessible:
                self.errors["unverified_members"] = dict(unknown=unknown, inaccessible=inaccessible)
            else:
                # A later authoritative empty query resolves a raced/failed
                # member read. It does not erase signalling/reservation errors.
                self.errors.pop("unverified_members", None)
            # Only the waitid-confirmed exited leader is exempt. Every other
            # enumerated member, including a zombie, must disappear.
            members = [p for pid, p in group.items() if pid != self.pid]
            return exited and not members and not inaccessible
        except Exception as error:
            self.errors["collector_error"] = type(error).__name__ + ": " + str(error)
            return None

    def send(self, signum, leader_only=False):
        self.exited()
        # Darwin can return EPERM for a zombie-only group even though its
        # unreaped leader still reserves the number. Avoid the signal only
        # when kernel membership + waitid establish no remaining members.
        if self.pinned and not leader_only and self.observe() and self.pinned:
            return
        self.exited()  # Revalidate AFTER collection, immediately before signalling.
        if self.pinned:
            try:
                if leader_only:
                    os.kill(self.pid, signum)
                else:
                    os.killpg(self.pid, signum)
            except ProcessLookupError:
                pass
            except OSError as error:
                self.errors.setdefault("group_errors", []).append(str(error))
            return
        # ECHILD/previous reap: never even probe the numeric PGID with signal 0.
        for key, previous in self.known.items():
            try:
                current = self.proc.info(previous["pid"])
                if identity(current) != key or current["pgid"] != self.pid:
                    self.errors["identity_unverified"] = "recorded member identity changed"
                    continue
                os.kill(current["pid"], signum)
            except ProcessLookupError:
                pass
            except (OSError, KeyError) as error:
                self.errors["identity_unverified"] = str(error)

    def reap(self, evidence):
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            evidence["reap_timeout"] = True
        finally:
            self.pinned = False
            evidence.update(self.errors)


def wait_group(group, deadline):
    while True:
        gone = group.observe()
        if gone:
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(min(0.1, max(0, deadline - time.monotonic())))


def finish_hook(group, evidence, timeout, stop=False):
    # Failed verification is still finalized by reap below. The run's finally
    # block must not retry it with a now-unreserved numeric PGID.
    if "cleanup_ok" in evidence:
        return
    if not stop:
        deadline = time.monotonic() + timeout
        while not group.exited():
            group.observe()
            if time.monotonic() >= deadline:
                evidence["timeout"] = True
                break
            time.sleep(min(0.1, max(0, deadline - time.monotonic())))
    evidence["termination_requested"] = True
    group.send(signal.SIGTERM)
    gone = wait_group(group, time.monotonic() + timeout)
    if not gone:
        evidence["forced_kill"] = True
        group.send(signal.SIGKILL)
        gone = wait_group(group, time.monotonic() + 5)
    # Only now, after verification or escalation, release the PID/PGID number.
    group.reap(evidence)
    evidence["cleanup_ok"] = gone and not group.errors and not evidence.get("reap_timeout")
    evidence.update(exit_status=group.process.returncode, ended=now())


def clean_quit(proc, app, root, known, timeout=30):
    """Shutdown a pinned child even when collection fails; fail verification closed."""
    evidence = dict(signal="SIGTERM", requested=now(), timeout_seconds=timeout)
    deadline = time.monotonic() + timeout
    if not app.exited():
        app.send(signal.SIGTERM, leader_only=True)
    gone = wait_group(app, deadline)
    if not gone:
        evidence["forced_kill"] = True
        app.send(signal.SIGKILL)
        if not app.exited():
            app.send(signal.SIGKILL, leader_only=True)
        gone = wait_group(app, time.monotonic() + 5)
    evidence["group_gone"] = gone
    app.reap(evidence)
    try:
        table = proc.snapshot()
        evidence["orphans"] = find_orphans(table, root, known)
        evidence["collection_errors"] = proc.errors
        evidence["unverified_pids"] = [error for error in proc.errors
            if any(key[0] == error["pid"] for key in known) and error["errno"] != errno.ESRCH]
    except Exception as error:
        evidence["collector_error"] = type(error).__name__ + ": " + str(error)
    evidence.update(exit_status=app.process.returncode, ended=now())
    evidence["ok"] = (app.process.returncode is not None and gone and not app.errors and
        not any(evidence.get(key) for key in ("orphans", "unverified_pids", "collector_error",
            "reap_timeout", "forced_kill")))
    return evidence


def run_once(args, proc, variant, scenario, run_dir, index, artifact, cancellation=None):
    if cancellation is not None:
        return measure_run(args, proc, variant, scenario, run_dir, index, artifact, cancellation)
    with Cancellation() as cancellation:
        return measure_run(args, proc, variant, scenario, run_dir, index, artifact, cancellation)


def measure_run(args, proc, variant, scenario, run_dir, index, artifact, cancellation):
    run_dir.mkdir()
    (run_dir / "samples.jsonl").write_text("", encoding="utf-8")
    result = dict(id=run_dir.name, variant=variant, scenario=scenario, status="failed",
                  started=now(), metrics={}, failures=[], hooks={}, repetition=index,
                  interval_seconds=args.interval or (10 if scenario == "tray" else 5),
                  launch_window_note="PAIR opens Overview on launch. Tray requires an external visibility hook.",
                  artifact=artifact, raw_samples=str(run_dir / "samples.jsonl"))
    app = None
    pre = None
    post = None
    known = {}
    root = None
    handles = []
    ledger = None
    try:
        result["system_launcher"] = dict(path=str(SYSTEM_LAUNCHER),
            allow_write=args.allow_system_launcher_write)
        result["system_launcher"]["before"] = launcher_snapshot(SYSTEM_LAUNCHER)
        if os.access(SYSTEM_LAUNCHER.parent, os.W_OK) and not args.allow_system_launcher_write:
            raise RuntimeError("no app launched: /usr/local/bin is writable; "
                               "--allow-system-launcher-write is required to accept this risk")
        cancellation.check()
        result["environment_before"] = power_display_metadata()
        sandbox = prepare_sandbox(run_dir, args.template, args.link_dotdir, Path.home())
        result["sandbox"] = str(sandbox)
        env = sandbox_environment(sandbox)
        cancellation.check()
        # Do not persist app prompt/message bodies via stdout/stderr capture.
        app = subprocess.Popen([artifact["binary"]], cwd=sandbox, env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               start_new_session=True)
        app = ProcessGroup(app, proc)
        cancellation.check()
        # Capture the root identity immediately; failure still requires cleanup.
        root = identity(proc.info(app.pid))
        known[root] = proc.info(app.pid)
        env = hook_environment(env, run_dir, sandbox, app.pid, scenario)
        if args.pre_sample_hook:
            pre, result["hooks"]["pre_sample"] = start_hook(
                args.pre_sample_hook, "pre-sample", run_dir, env, handles, proc)
        warmup_end = time.monotonic() + args.warmup
        while time.monotonic() < warmup_end:
            cancellation.check()
            if app.exited():
                raise RuntimeError("app exited during warm-up")
            table = proc.snapshot()
            app.observe()
            if pre:
                pre.observe()
            owned = owned_processes(table, root, known)
            known.update({identity(p): p for p in owned.values()})
            cancellation.sleep(min(result["interval_seconds"], max(0, warmup_end - time.monotonic())))
        steady_start = proc.absolute_time()
        ledger = Accounting(steady_start, proc.numer, proc.denom)
        deadline = None
        due = None
        with (run_dir / "samples.jsonl").open("w", encoding="utf-8") as stream:
            while True:
                cancellation.check()
                if app.exited():
                    raise RuntimeError("app exited during steady sampling")
                table = proc.snapshot()
                app.observe()
                if pre:
                    pre.observe()
                owned = owned_processes(table, root, known)
                known.update({identity(p): p for p in owned.values()})
                timestamp, ticks = now(), proc.absolute_time()
                elapsed = proc.seconds(ticks - steady_start)
                for process in owned.values():
                    process["counter_timestamp"] = timestamp
                    process["counter_elapsed_seconds"] = elapsed
                exited = ledger.observe(elapsed, list(owned.values()))
                if deadline is None:
                    # Measure the full requested span after the first completed
                    # reading, not including the cost of initial discovery.
                    due = time.monotonic()
                    deadline = due + args.duration
                # A frame exists even if the process tree is empty.
                stream.write(json.dumps(dict(event="frame", timestamp=timestamp,
                    elapsed_seconds=elapsed, mach_abstime=ticks,
                    collection_errors=proc.errors, owned_count=len(owned))) + "\n")
                for event, processes in (("sample", owned.values()), ("exit_final_known", exited)):
                    for process in processes:
                        stream.write(json.dumps(dict(process, timestamp=timestamp,
                            elapsed_seconds=elapsed, event=event,
                            counter_timestamp_note="last observation" if event == "exit_final_known" else "current frame")) + "\n")
                stream.flush()
                if not any(identity(p) == root for p in owned.values()):
                    raise RuntimeError("app root could not be sampled")
                failed_owned = [e for e in proc.errors if any(k[0] == e["pid"] for k in known)
                                and e["errno"] != errno.ESRCH]
                if failed_owned:
                    result["failures"].append(dict(owned_collection_errors=failed_owned))
                if time.monotonic() >= deadline:
                    break
                due = min(due + result["interval_seconds"], deadline)
                cancellation.sleep(max(0, due - time.monotonic()))
        if ledger.warnings:
            result["failures"].append("Accounting warnings: inspect raw samples")
        cancellation.check()
        if pre:
            finish_hook(pre, result["hooks"]["pre_sample"], args.hook_timeout, stop=True)
            if result["hooks"]["pre_sample"]["cleanup_ok"]:
                pre = None
            else:
                result["hook_cleanup_unverified"] = True
                raise RuntimeError("Pre-sample hook cleanup unverified")
        cancellation.check()
        if args.post_run_check:
            post, evidence = start_hook(args.post_run_check, "post-run-check", run_dir, env, handles, proc)
            result["hooks"]["post_run_check"] = evidence
            evidence["phase"] = "after steady sampling, before app quit"
            finish_hook(post, evidence, args.hook_timeout)
            if evidence["cleanup_ok"]:
                post = None
            else:
                result["hook_cleanup_unverified"] = True
        else:
            result["hooks"]["post_run_check"] = dict(status="not configured",
                note="Routing eligibility, freshness and alias failover require external evidence.")
    except (Exception, KeyboardInterrupt) as error:
        result["failures"].append(type(error).__name__ + ": " + str(error))
        if isinstance(error, (KeyboardInterrupt, Cancelled)):
            result["interrupted"] = True
    finally:
        if ledger and ledger.times:
            result.update(metrics=ledger.summary(), accounting_warnings=ledger.warnings,
                          measured_seconds=ledger.times[-1] - ledger.times[0],
                          snapshots=len(ledger.times))
        for hook, key in ((pre, "pre_sample"), (post, "post_run_check")):
            if hook:
                try:
                    finish_hook(hook, result["hooks"][key], args.hook_timeout, stop=True)
                except Exception as error:
                    result["failures"].append("Hook cleanup failed: " + str(error))
                    result["hook_cleanup_unverified"] = True
        for key, evidence in result["hooks"].items():
            if "pid" not in evidence and key == "post_run_check" and "status" in evidence:
                continue
            if not evidence.get("cleanup_ok"):
                result["hook_cleanup_unverified"] = True
                result["failures"].append(key + " cleanup unverified; stop experiment")
            if evidence.get("exit_status") != 0 or evidence.get("forced_kill") or evidence.get("timeout"):
                result["failures"].append(key + " hook failed or required forced termination")
        if app:
            try:
                identity_unavailable = root is None
                if identity_unavailable:
                    root = (app.pid, -1)
                    result["failures"].append("Root identity unavailable; descendant cleanup unverified")
                result["quit"] = clean_quit(proc, app, root, known)
                if identity_unavailable:
                    result["quit"]["ok"] = False
            except Exception as error:
                result["quit"] = dict(ok=False, error=str(error))
            if not result["quit"]["ok"]:
                result["failures"].append("Clean quit/orphan verification failed; stop experiment")
        for handle in handles:
            handle.close()
        try:
            launcher = result["system_launcher"]
            launcher["after"] = launcher_snapshot(SYSTEM_LAUNCHER)
            result["launcher_changed"] = launcher["before"] != launcher["after"]
            if result["launcher_changed"]:
                result["failures"].append("System launcher changed; stop experiment (not restored)")
        except Exception as error:
            result["launcher_changed"] = True  # Fail closed if comparison is impossible.
            result["failures"].append("System launcher verification failed: " + str(error))
        result["environment_after"] = power_display_metadata()
        if cancellation.signum is not None:
            result["interrupted"] = True
            result["cancellation_signal"] = signal.Signals(cancellation.signum).name
            result["failures"].append("Cancelled by " + result["cancellation_signal"])
        result["status"] = "failed" if result["failures"] else "ok"
        result["ended"] = now()
        write_json(run_dir / "summary.json", result)
    return result


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="new evidence directory (must not exist)")
    parser.add_argument("--baseline", type=Path, default=Path("/Applications/PAIR.app"))
    parser.add_argument("--candidate", type=Path,
                        default=ROOT / "desktop/release/0.7.0-rc.2/mac/mac-arm64/PAIR.app")
    parser.add_argument("--baseline-archive", type=Path, help="optional original archive to hash")
    parser.add_argument("--candidate-archive", type=Path, help="optional original archive to hash")
    parser.add_argument("--allow-system-launcher-write", action="store_true",
                        help="accept packaged startup's /usr/local/bin launcher write risk")
    parser.add_argument("--scenario", nargs="+", choices=("tray", "overview", "inference"),
                        default=["tray", "overview", "inference"])
    parser.add_argument("--repetitions", type=int, default=5, help="runs per variant and scenario")
    parser.add_argument("--seed", type=int, default=20260421)
    parser.add_argument("--warmup", type=float, default=60)
    parser.add_argument("--duration", type=float, default=120)
    parser.add_argument("--interval", type=float, help="default: tray 10 s, overview/inference 5 s")
    parser.add_argument("--template", type=Path, help="copy a trusted state template into each fresh home")
    parser.add_argument("--link-dotdir", action="append", default=[], metavar=".NAME")
    parser.add_argument("--pre-sample-hook", help="argv command (no implicit shell); starts before warm-up")
    parser.add_argument("--post-run-check", help="argv command at end of steady sampling, before quit")
    parser.add_argument("--hook-timeout", type=float, default=30)
    parser.add_argument("--context", help="operator description: source commits, topology, engines/models, tracing, workload")
    parser.add_argument("--authorize-launch", action="store_true", help="explicitly authorize GUI app launch")
    return parser


def save_report(output, report):
    report["groups"] = aggregate_runs(report["runs"])
    write_json(output / "report.json", report)
    (output / "report.md").write_text(render_markdown(report), encoding="utf-8")


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    parser = build_parser()
    args = parser.parse_args(argv)
    durations = [args.warmup, args.duration, args.hook_timeout]
    if args.interval is not None:
        durations.append(args.interval)
    if not all(math.isfinite(value) for value in durations):
        parser.error("durations and intervals must be finite")
    if len(set(args.scenario)) != len(args.scenario):
        parser.error("scenario labels must be unique")
    if args.repetitions < 1 or args.warmup < 0 or args.duration <= 0 or args.hook_timeout <= 0:
        parser.error("positive repetitions/duration/hook-timeout and nonnegative warmup required")
    if args.interval is not None and not 0 < args.interval <= args.duration:
        parser.error("interval must be positive and no longer than duration")
    if not args.authorize_launch:
        parser.error("no app launched: --authorize-launch is required on an idle, authorized test machine")
    with Cancellation() as cancellation:
        return experiment(args, argv, cancellation)


def experiment(args, argv, cancellation):
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    report = dict(metadata=environment_metadata(args, argv), runs=[], groups={})
    report["metadata"]["diagnostic_only"] = args.repetitions < 5 or args.duration < 120
    order = alternating_order(args.repetitions, args.seed)
    report["metadata"]["variant_order_per_scenario"] = order
    supported = platform.system() == "Darwin" and platform.machine() == "arm64"
    proc = None
    collector_error = None
    if supported:
        try:
            proc = Libproc()
        except (OSError, RuntimeError) as error:
            collector_error = str(error)
            report["metadata"]["collector_error"] = collector_error
    if proc:
        report["metadata"]["mach_timebase"] = dict(numer=proc.numer, denom=proc.denom)
        report["metadata"]["rusage_flavor"] = "RUSAGE_INFO_V4"
    abort = False
    save_report(output, report)
    for scenario in args.scenario:
        for index, variant in enumerate(order):
            artifact = report["metadata"]["apps"][variant]
            run_id = "%s-%02d-%s" % (scenario, index + 1, variant)
            reason = ("Previous run failed confinement/cleanup or experiment was interrupted"
                      if abort or cancellation.signum is not None else
                      collector_error if collector_error else
                      "Requires macOS arm64" if not proc else artifact.get("error"))
            if reason:
                run_dir = output / run_id
                run_dir.mkdir()
                run = dict(id=run_id, variant=variant, scenario=scenario, status="skipped",
                           failures=[reason], metrics={}, raw_samples=str(run_dir / "samples.jsonl"))
                (run_dir / "samples.jsonl").write_text("", encoding="utf-8")
                write_json(run_dir / "summary.json", run)
            else:
                run = run_once(args, proc, variant, scenario, output / run_id,
                               index // 2 + 1, artifact, cancellation)
                abort = (run.get("interrupted", False) or run.get("hook_cleanup_unverified", False)
                         or run.get("launcher_changed", False)
                         or not run.get("quit", {}).get("ok", False))
            report["runs"].append(run)
            save_report(output, report)
    return 1 if any(run["status"] != "ok" for run in report["runs"]) else 0


if __name__ == "__main__":
    raise SystemExit(main())
