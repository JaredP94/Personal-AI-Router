# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for Windows NSIS installer inventory and firewall rules.

Verifies services/installer/nvpair-setup.nsi covers every component declared in
services/versions.json, includes licenses and notices, and pairs every firewall
add rule with a corresponding delete rule.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import unittest

REPO_ROOT = Path(__file__).resolve().parents[2]
NSI_PATH = REPO_ROOT / "services" / "installer" / "nvpair-setup.nsi"
VERSIONS_PATH = REPO_ROOT / "services" / "versions.json"


def load_nsi_text() -> str:
    return NSI_PATH.read_text(encoding="utf-8")


def load_component_keys() -> set[str]:
    data = json.loads(VERSIONS_PATH.read_text(encoding="utf-8"))
    return set(data.get("components", {}).keys())


def parse_add_rules(text: str) -> list[dict[str, str | None]]:
    rules = []
    for line in text.splitlines():
        if "advfirewall" in line and "add rule" in line:
            name_m = re.search(r'name="([^"]+)"', line)
            prog_m = re.search(r'program="([^"]+)"', line)
            proto_m = re.search(r'protocol=([^\s\']+)', line)
            port_m = re.search(r'localport=([^\s\']+)', line)
            if name_m and prog_m:
                rules.append({
                    "name": name_m.group(1),
                    "program": prog_m.group(1),
                    "protocol": proto_m.group(1) if proto_m else None,
                    "localport": port_m.group(1) if port_m else None,
                })
    return rules


class NsisInventoryTests(unittest.TestCase):
    def test_install_and_uninstall_cover_every_component(self):
        text = load_nsi_text()
        components = load_component_keys()

        installed = set(re.findall(r'File\s+"(?:\.\.\\build\\bin\\|\.\./build/bin/)([^"]+)\.exe"', text))
        uninstalled = set(re.findall(r'Delete\s+"\$INSTDIR\\bin\\([^"]+)\.exe"', text))

        self.assertEqual(
            installed,
            components,
            f"Installed binaries do not match versions.json components. Missing: {components - installed}, Extra: {installed - components}",
        )
        self.assertEqual(
            uninstalled,
            components,
            f"Uninstalled binaries do not match versions.json components. Missing: {components - uninstalled}, Extra: {uninstalled - components}",
        )

    def test_every_added_firewall_rule_is_deleted(self):
        text = load_nsi_text()
        added_rules = set(re.findall(r'netsh\s+advfirewall\s+firewall\s+add\s+rule\s+name="([^"]+)"', text))
        deleted_rules = set(re.findall(r'netsh\s+advfirewall\s+firewall\s+delete\s+rule\s+name="([^"]+)"', text))

        self.assertTrue(len(added_rules) > 0, "Expected at least one firewall add rule")
        self.assertEqual(
            added_rules,
            deleted_rules,
            f"Firewall add/delete mismatch. Added but not deleted: {added_rules - deleted_rules}, Deleted but not added: {deleted_rules - added_rules}",
        )

    def test_omlx_proxy_mirrors_lmstudio_rules(self):
        text = load_nsi_text()
        add_rules = parse_add_rules(text)

        lmstudio_rules = [
            r for r in add_rules
            if r["program"] in (r"$INSTDIR\bin\lmstudio-proxy.exe", "$INSTDIR/bin/lmstudio-proxy.exe")
        ]
        self.assertTrue(len(lmstudio_rules) > 0, "Expected at least one LM Studio proxy rule")

        omlx_rules = [
            r for r in add_rules
            if r["program"] in (r"$INSTDIR\bin\omlx-proxy.exe", "$INSTDIR/bin/omlx-proxy.exe")
        ]
        self.assertTrue(len(omlx_rules) > 0, "Expected at least one oMLX proxy rule")

        for lm_rule in lmstudio_rules:
            expected_proto = lm_rule["protocol"]
            expected_port = lm_rule["localport"]
            matching = [
                r for r in omlx_rules
                if r["protocol"] == expected_proto and r["localport"] == expected_port
            ]
            self.assertTrue(
                len(matching) == 1,
                f"Expected exactly one oMLX rule mirroring LM Studio rule {lm_rule['name']} (protocol={expected_proto}, port={expected_port}), found {matching}",
            )

    def test_notices_installed_and_removed(self):
        text = load_nsi_text()
        required_notices = ["LICENSE", "THIRD_PARTY_NOTICES.md"]
        for notice in required_notices:
            with self.subTest(notice=notice):
                file_pattern = rf'File\s+"(?:\.\.[\\/]){{2}}{re.escape(notice)}"'
                self.assertTrue(
                    re.search(file_pattern, text),
                    f'Missing File line for {notice} (expected File "..\\..\\{notice}")',
                )
                delete_pattern = rf'Delete\s+"\$INSTDIR\\{re.escape(notice)}"'
                self.assertTrue(
                    re.search(delete_pattern, text),
                    f'Missing Delete line for {notice} (expected Delete "$INSTDIR\\{notice}")',
                )


if __name__ == "__main__":
    unittest.main()
