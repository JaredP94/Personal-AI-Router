# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the actual Bash installer with isolated destinations and command doubles.

Only /Applications is relocated in a temporary script copy. PATH is a whitelist;
network, mounts, signals, privilege escalation and launching are always mocked.
The kill shell builtin is overridden too. No real app is stopped or installed.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "install-mac.sh"
DMG_BYTES = b"fixture release disk image\n"

# These are the only real subprocesses the doubles can invoke. Filesystem
# operations are implemented below and confined to the temporary test root.
READ_ONLY_TOOLS = {
    name: shutil.which(name)
    for name in ("basename", "dirname", "grep", "sed", "awk", "head", "shasum", "cat")
}

DISPATCH = r'''
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
root = Path(os.environ["HARNESS_ROOT"])
with (root / "events").open("a") as stream:
    stream.write(json.dumps([name] + args) + "\n")

def safe(value):
    path = Path(value).absolute()
    if not path.is_relative_to(root):
        raise SystemExit("Blocked filesystem access: " + str(path))
    return path

real = json.loads(os.environ["READ_ONLY_TOOLS"])
if name in real:
    raise SystemExit(subprocess.call([real[name]] + args))
if name == "uname":
    print("Darwin" if args == ["-s"] else os.environ.get("MOCK_ARCH", "arm64"))
elif name == "mktemp":
    path = root / "work"
    path.mkdir()
    print(path)
elif name == "mkdir":
    for value in args:
        if not value.startswith("-"):
            safe(value).mkdir(parents=True, exist_ok=True)
elif name == "rm":
    for value in args:
        if not value.startswith("-"):
            path = safe(value)
            if path.is_dir():
                shutil.rmtree(path)
            elif path.exists():
                path.unlink()
elif name == "curl":
    url = next(value for value in args if value.startswith("https://"))
    if url.endswith("/releases/latest"):
        print(json.dumps({"tag_name": os.environ.get("MOCK_TAG", "v0.7.0-rc.1")}))
    else:
        if os.environ.get("DOWNLOAD_FAIL") == "1":
            raise SystemExit(22)
        target = safe(args[args.index("-o") + 1])
        if url.endswith("/SHA256SUMS"):
            mode = os.environ.get("SUMS", "valid")
            if mode == "missing":
                raise SystemExit(22)
            digest = hashlib.sha256(b"fixture release disk image\n").hexdigest()
            asset = os.environ.get("SUM_ASSET", "NVPAIR-Setup-0.7.0-rc.1-arm64.dmg")
            row = digest + "  " + asset + "\n"
            options = {
                "valid": row + "a" * 64 + "  latest-mac.yml\n",
                "duplicate": row + row,
                "invalid": "not-a-digest  " + asset + "\n",
                "mismatch": "0" * 64 + "  " + asset + "\n",
                "absent": digest + "  other.dmg\n",
                "path": digest + "  ./" + asset + "\n",
                "single-space": digest + " " + asset + "\n",
                "malformed-extra": row + "garbage\n",
            }
            target.write_text(options[mode])
        else:
            target.write_bytes(b"fixture release disk image\n")
elif name == "hdiutil":
    if args[0] == "attach":
        if os.environ.get("MOUNT_FAIL") == "1":
            raise SystemExit(1)
        mount = safe(args[args.index("-mountpoint") + 1])
        app = mount / os.environ.get("BUNDLE", "PAIR.app")
        (app / "Contents/MacOS").mkdir(parents=True)
        binary = app / "Contents/MacOS/PAIR"
        binary.write_text("fixture executable")
        binary.chmod(0o644 if os.environ.get("NONEXEC") else 0o755)
        workers = app / "Contents/Resources/cli-bin"
        workers.mkdir(parents=True)
        worker = workers / "nvpair-ui-broker"
        worker.write_text("fixture worker")
        worker.chmod(0o755)
        (workers / "data.json").write_text("{}")
elif name == "pgrep":
    if os.environ.get("PGREP_ERROR"):
        raise SystemExit(2)
    # Include an independent TUI and an app with a similar prefix. A broad
    # process-name search must not be allowed to treat these as app-owned.
    import re
    pattern = args[-1]
    candidates = [
        "/tmp/dev/nvpair-tui", "/tmp/dev/nvpair-ui-broker",
        str(root / "Applications/PAIR.app.other/Contents/MacOS/PAIR"),
        str(root / "Applications/PAIRXapp/Contents/MacOS/PAIR"),
    ]
    if (root / "running").exists():
        candidates.append(str(root / "Applications/PAIR.app/Contents/MacOS/PAIR"))
        candidates.append(str(root / "Applications/PAIR.app/Contents/Resources/cli-bin/nvpair-ui-broker"))
    matches = [i + 300 for i, value in enumerate(candidates) if re.search(pattern, value)]
    if matches:
        print("\n".join(map(str, matches)))
    raise SystemExit(0 if matches else 1)
elif name == "osascript":
    (root / "quit-pid").write_text(str(os.getpid()))
    installed = str(root / "Applications/PAIR.app")
    if ('tell application "' + installed + '" to quit') not in args:
        raise SystemExit("Quit request did not target the installed app path")
    if ('if application "' + installed + '" is running then') not in args:
        raise SystemExit("Quit request could launch an app instead of quitting it")
    if not os.environ.get("REFUSE_QUIT") and not os.environ.get("HANG_QUIT"):
        (root / "running").unlink(missing_ok=True)
    if os.environ.get("QUIT_FAIL"):
        raise SystemExit(1)
elif name == "kill":
    # Simulated osascript liveness, never forward a signal to a real PID.
    raise SystemExit(0 if os.environ.get("HANG_QUIT") else 1)
elif name == "sleep":
    # Yield to the concurrently launched osascript without 20 real seconds.
    import time
    time.sleep(0.02)
elif name in ("cp", "ditto"):
    source, dest = map(safe, args[-2:])
    if dest.name != "PAIR.app":
        dest = dest / source.name
    shutil.copytree(source, dest)
elif name == "find":
    print("\n".join(str(path) for path in sorted(safe(args[0]).glob("*.app"))))
elif name in ("sudo", "pkill", "chmod", "open"):
    # Never escalate, signal or launch, including against the old installer.
    # sudo re-dispatches only our own confined filesystem doubles.
    if name == "sudo":
        raise SystemExit(subprocess.call([str(root / "bin" / args[0])] + args[1:]))
else:
    raise SystemExit("Unexpected command: " + name)
'''


class InstallMacTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="install_mac_test_")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.dest = self.root / "Applications/PAIR.app"
        self.dest.mkdir(parents=True)
        (self.dest / "old-install").write_text("keep me")
        self.local = self.root / "local.dmg"
        self.local.write_bytes(DMG_BYTES)
        self.script = self.root / "install.sh"
        self.script.write_text(SCRIPT.read_text().replace("/Applications", str(self.root / "Applications")))
        dispatch = self.bin / "dispatch"
        dispatch.write_text("#!" + sys.executable + "\n" + DISPATCH)
        dispatch.chmod(0o755)
        commands = list(READ_ONLY_TOOLS) + [
            "uname", "mktemp", "mkdir", "rm", "curl", "hdiutil", "pgrep",
            "osascript", "kill", "sleep", "cp", "ditto", "sudo", "pkill",
            "chmod", "open", "find",
        ]
        for command in commands:
            (self.bin / command).symlink_to(dispatch)
        startup = self.root / "bash_env"
        startup.write_text('''
kill() { "$HARNESS_ROOT/bin/kill" "$@"; }
# Simulate denied permission bits regardless of the account running the suite.
[() {
    if builtin [ "${FORCE_SUDO:-}" = 1 ] &&
       builtin [ "$#" -eq 4 ] && builtin [ "$1" = '!' ] && builtin [ "$2" = -w ]; then
        return 0
    fi
    if builtin [ "${UNREADABLE_DMG:-}" = 1 ] &&
       builtin [ "$#" -eq 3 ] && builtin [ "$1" = -r ]; then
        return 1
    fi
    builtin [ "$@"
}
''')
        self.env = {
            "PATH": str(self.bin), "HOME": str(self.root),
            "HARNESS_ROOT": str(self.root), "BASH_ENV": str(startup),
            "READ_ONLY_TOOLS": json.dumps(READ_ONLY_TOOLS),
            "LC_ALL": "C", "SUMS": "valid",
        }

    def run_installer(self, *args, **env):
        (self.root / "events").write_text("")
        # Each table row starts with the same independent installed-app fixture.
        shutil.rmtree(self.dest)
        self.dest.mkdir()
        (self.dest / "old-install").write_text("keep me")
        result = subprocess.run(
            ["/bin/bash", str(self.script), *args], env={**self.env, **env},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            timeout=15,
        )
        events = self.root / "events"
        self.events = [json.loads(line) for line in events.read_text().splitlines()] if events.exists() else []
        return result

    def assert_untouched(self):
        self.assertTrue((self.dest / "old-install").is_file(), self.events)
        self.assertEqual((self.dest / "old-install").read_text(), "keep me")
        self.assertFalse(any(event[0] in ("osascript", "pkill", "ditto", "cp", "sudo") for event in self.events), self.events)

    def assert_no_mount(self):
        self.assertFalse(any(event[:2] == ["hdiutil", "attach"] for event in self.events), self.events)

    def test_latest_rc_download_verified_before_mount_and_quit(self):
        # Break caught: stale asset naming, no checksum, or premature shutdown.
        (self.root / "running").touch()
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stdout)
        urls = [next(arg for arg in event if arg.startswith("https://")) for event in self.events if event[0] == "curl"]
        self.assertEqual(urls, [
            "https://api.github.com/repos/JaredP94/Personal-AI-Router/releases/latest",
            "https://github.com/JaredP94/Personal-AI-Router/releases/download/v0.7.0-rc.1/NVPAIR-Setup-0.7.0-rc.1-arm64.dmg",
            "https://github.com/JaredP94/Personal-AI-Router/releases/download/v0.7.0-rc.1/SHA256SUMS",
        ])
        names = [event[0] for event in self.events]
        self.assertLess(names.index("shasum"), names.index("hdiutil"))
        self.assertLess(names.index("hdiutil"), names.index("osascript"))
        self.assertLess(names.index("osascript"), names.index("ditto"))
        self.assertFalse((self.dest / "old-install").exists())
        self.assertTrue(os.access(self.dest / "Contents/MacOS/PAIR", os.X_OK))
        self.assertFalse(os.access(self.dest / "Contents/Resources/cli-bin/data.json", os.X_OK))
        self.assertNotIn("pkill", names)
        self.assertNotIn("chmod", names)

    def test_pinned_version_accepts_optional_v_prefix(self):
        # Break caught: doubled v or v in asset filenames; unnecessary latest lookup.
        for version in ("0.7.0-rc.1", "v0.7.0-rc.1"):
            with self.subTest(version=version):
                result = self.run_installer("--version", version)
                self.assertEqual(result.returncode, 0, result.stdout)
                curls = [event for event in self.events if event[0] == "curl"]
                self.assertFalse(any("/latest" in arg for event in curls for arg in event))
                self.assertTrue(any(arg.endswith("/v0.7.0-rc.1/NVPAIR-Setup-0.7.0-rc.1-arm64.dmg") for event in curls for arg in event))

    def test_unavailable_x64_leaves_installed_app_untouched(self):
        result = self.run_installer(MOCK_ARCH="x86_64", DOWNLOAD_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("x64.dmg", result.stdout)
        self.assert_untouched()
        self.assert_no_mount()

    def test_bad_checksums_fail_closed_before_mount_or_quit(self):
        # Break caught: accepting missing, ambiguous, malformed or wrong digest rows.
        for mode in ("missing", "duplicate", "invalid", "mismatch", "absent", "path", "single-space", "malformed-extra"):
            with self.subTest(mode=mode):
                result = self.run_installer(SUMS=mode)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("SHA256", result.stdout)
                self.assert_untouched()
                self.assert_no_mount()

    def test_bad_image_or_non_pair_bundle_never_stops_app(self):
        for env in ({"MOUNT_FAIL": "1"}, {"BUNDLE": "Other.app"}):
            with self.subTest(env=env):
                result = self.run_installer(**env)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assert_untouched()

    def test_local_dmg_requires_independent_verification_without_network(self):
        result = self.run_installer("--dmg", str(self.local))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("independently", result.stdout.lower())
        self.assertFalse(any(event[0] in ("curl", "shasum") for event in self.events))
        self.assertTrue((self.dest / "Contents/MacOS/PAIR").exists())
        # Independent TUI/dev processes and similarly named apps stay running.
        self.assertFalse(any(event[0] in ("osascript", "pkill") for event in self.events))

    def test_sudo_copy_preserves_executable_modes_and_nonexecutable_resources(self):
        result = self.run_installer("--dmg", str(self.local), FORCE_SUDO="1")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual([event[1] for event in self.events if event[0] == "sudo"], ["rm", "ditto"])
        self.assertTrue(os.access(self.dest / "Contents/MacOS/PAIR", os.X_OK))
        self.assertTrue(os.access(self.dest / "Contents/Resources/cli-bin/nvpair-ui-broker", os.X_OK))
        self.assertFalse(os.access(self.dest / "Contents/Resources/cli-bin/data.json", os.X_OK))
        self.assertFalse(any(event[0] == "chmod" for event in self.events))

    def test_unreadable_local_dmg_rejected_before_any_external_command(self):
        result = self.run_installer("--dmg", str(self.local), UNREADABLE_DMG="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("readable", result.stdout)
        self.assertEqual(self.events, [])
        self.assert_untouched()

    def test_repo_override_and_launch_keep_public_cli_working(self):
        result = self.run_installer("-v", "v0.7.0-rc.1", "--repo", "Example/PAIR-fork", "--launch")
        self.assertEqual(result.returncode, 0, result.stdout)
        urls = [arg for event in self.events if event[0] == "curl" for arg in event if arg.startswith("https://")]
        self.assertEqual(urls, [
            "https://github.com/Example/PAIR-fork/releases/download/v0.7.0-rc.1/NVPAIR-Setup-0.7.0-rc.1-arm64.dmg",
            "https://github.com/Example/PAIR-fork/releases/download/v0.7.0-rc.1/SHA256SUMS",
        ])
        self.assertEqual([event for event in self.events if event[0] == "open"], [["open", str(self.dest)]])
        names = [event[0] for event in self.events]
        self.assertLess(names.index("ditto"), names.index("open"))

    def test_invalid_repository_environment_rejected_before_network(self):
        result = self.run_installer(PAIR_REPO="https://github.com/Example/PAIR")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.events, [])
        self.assert_untouched()

    def test_invalid_arguments_fail_before_network_mount_or_process_changes(self):
        cases = [
            ["--version"], ["-v"], ["--repo"], ["--dmg"],
            ["--version", ""], ["--repo", ""], ["--dmg", ""],
            ["--repo", "--launch"], ["--version", "../bad"],
            ["--version", "v"], ["--version", "1?bad"],
            ["--repo", "owner"], ["--repo", "a/b/c"],
            ["--repo", "../repo"], ["--repo", "owner/repo?x"],
            ["--dmg", str(self.local), "--version", "0.7.0-rc.1"],
            ["--dmg", str(self.root / "missing.dmg")],
            ["--dmg", str(self.root)], ["--bogus"], ["--skip-checksum"],
        ]
        for args in cases:
            with self.subTest(args=args):
                result = self.run_installer(*args)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("unbound variable", result.stdout)
                self.assertIn("Error:", result.stdout)
                self.assertFalse(any(event[0] in ("curl", "hdiutil", "pgrep") for event in self.events), self.events)
                self.assert_untouched()

    def test_unsafe_resolved_tag_never_downloads_or_stops(self):
        result = self.run_installer(MOCK_TAG="v0.7/../../bad")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(len([event for event in self.events if event[0] == "curl"]), 1)
        self.assert_untouched()
        self.assert_no_mount()

    def test_refuses_replacement_when_owned_processes_remain(self):
        (self.root / "running").touch()
        result = self.run_installer(REFUSE_QUIT="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("20", result.stdout)
        self.assertEqual((self.dest / "old-install").read_text(), "keep me")
        self.assertFalse(any(event[0] in ("pkill", "ditto", "cp") for event in self.events))
        self.assertEqual(len([event for event in self.events if event[0] == "osascript"]), 1)
        self.assertEqual(len([event for event in self.events if event[0] == "sleep"]), 20)

    def test_hung_quit_is_bounded_and_only_its_own_process_is_signalled(self):
        (self.root / "running").touch()
        result = self.run_installer(HANG_QUIT="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        signals = [event for event in self.events if event[0] == "kill" and "-0" not in event]
        self.assertEqual(len(signals), 1, self.events)
        self.assertIn("-KILL", signals[0])
        self.assertEqual(signals[0][-1], (self.root / "quit-pid").read_text())
        self.assertEqual((self.dest / "old-install").read_text(), "keep me")
        self.assertFalse(any(event[0] in ("pkill", "ditto", "cp") for event in self.events))

    def test_process_query_errors_refuse_replacement(self):
        result = self.run_installer(PGREP_ERROR="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_untouched()

    def test_nonexecutable_source_refused_instead_of_silent_chmod(self):
        result = self.run_installer(NONEXEC="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_untouched()


if __name__ == "__main__":
    unittest.main()
