# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for services/installer_build.sh.

Confines all build and staging operations to temporary directories without
touching real services/build or services/dist directories. Uses the real
jq, tar, uname, and bash on PATH.
"""

import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[2] / "services" / "installer_build.sh"


class ServicesInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="services_installer_test_")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

        # Root notices and license
        (self.root / "LICENSE").write_text("fixture license\n")
        (self.root / "THIRD_PARTY_NOTICES.md").write_text("fixture notices\n")

        # Services tree
        self.services_dir = self.root / "services"
        self.services_dir.mkdir()

        self.installer_script = self.services_dir / "installer_build.sh"
        shutil.copy2(SCRIPT, self.installer_script)
        self.installer_script.chmod(0o755)

        self.components = ["omlx-proxy", "nvpair-alpha", "nvpair-beta"]
        self.versions_file = self.services_dir / "versions.json"
        versions_data = {
            "product": "1.2.3",
            "installer": "1.2.3",
            "components": {name: "1.0.0" for name in self.components},
        }
        self.versions_file.write_text(json.dumps(versions_data, indent=2))

        # Platform install guides
        (self.services_dir / "installer" / "macos").mkdir(parents=True)
        (self.services_dir / "installer" / "macos" / "INSTALL.md").write_text("macos install guide\n")
        (self.services_dir / "installer" / "linux").mkdir(parents=True)
        (self.services_dir / "installer" / "linux" / "INSTALL.md").write_text("linux install guide\n")

        # Fake built executables
        self.bin_dir = self.services_dir / "build" / "bin"
        self.bin_dir.mkdir(parents=True)
        for name in self.components:
            binary = self.bin_dir / name
            binary.write_text(f"#!/bin/sh\necho {name}\n")
            binary.chmod(0o755)

    def run_installer(self, *args, **env_overrides):
        env = {
            **os.environ,
            "NVPAIR_SKIP_BUILD": "1",
            "COPYFILE_DISABLE": "1",
            **env_overrides,
        }
        return subprocess.run(
            ["/bin/bash", str(self.installer_script), *args],
            cwd=str(self.services_dir),
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

    def test_stages_every_component_and_notices(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}")

        dist_dir = self.services_dir / "dist"
        archives = list(dist_dir.glob("NVIDIA-Personal-AI-Router-*.tar.gz"))
        self.assertEqual(len(archives), 1, f"Expected 1 archive in dist, found {archives}")
        archive_path = archives[0]

        stage_prefix = "NVIDIA-Personal-AI-Router-1.2.3/"
        with tarfile.open(archive_path, "r:gz") as tar:
            members = [
                m for m in tar.getmembers()
                if not m.isdir() and not Path(m.name).name.startswith("._")
            ]
            relative_paths = {
                m.name[len(stage_prefix):] if m.name.startswith(stage_prefix) else m.name
                for m in members
            }
            expected_paths = {
                "bin/omlx-proxy",
                "bin/nvpair-alpha",
                "bin/nvpair-beta",
                "INSTALL.md",
                "LICENSE",
                "THIRD_PARTY_NOTICES.md",
            }
            self.assertEqual(relative_paths, expected_paths)

            bin_members = [
                m for m in members
                if m.name.startswith(stage_prefix + "bin/") or m.name.startswith("bin/")
            ]
            self.assertEqual(len(bin_members), 3)
            for m in bin_members:
                self.assertEqual(
                    stat.S_IMODE(m.mode),
                    0o755,
                    f"Expected mode 0755 for {m.name}, got {oct(m.mode)}",
                )

    def test_missing_component_fails_before_archive(self):
        (self.bin_dir / "omlx-proxy").unlink()

        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("omlx-proxy", result.stderr)

        dist_dir = self.services_dir / "dist"
        archives = list(dist_dir.glob("*.tar.gz")) if dist_dir.exists() else []
        self.assertEqual(archives, [])

    def test_nonexecutable_component_fails_before_archive(self):
        (self.bin_dir / "omlx-proxy").chmod(0o644)

        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("omlx-proxy", result.stderr)

        dist_dir = self.services_dir / "dist"
        archives = list(dist_dir.glob("*.tar.gz")) if dist_dir.exists() else []
        self.assertEqual(archives, [])

    def test_missing_notice_fails(self):
        (self.root / "THIRD_PARTY_NOTICES.md").unlink()

        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("THIRD_PARTY_NOTICES.md", result.stderr)

        dist_dir = self.services_dir / "dist"
        archives = list(dist_dir.glob("*.tar.gz")) if dist_dir.exists() else []
        self.assertEqual(archives, [])

    def test_missing_license_fails(self):
        (self.root / "LICENSE").unlink()

        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("LICENSE", result.stderr)

        dist_dir = self.services_dir / "dist"
        archives = list(dist_dir.glob("*.tar.gz")) if dist_dir.exists() else []
        self.assertEqual(archives, [])


if __name__ == "__main__":
    unittest.main()
