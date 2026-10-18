<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Release Hygiene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the non-runtime release concerns: unsigned macOS builds stop re-prompting for a helper they cannot register, standalone installers ship every service plus the license notices, dependency attribution is reviewed and gated, and a private vulnerability-reporting channel exists.

**Architecture:** The macOS helper control tool (Swift, single source of the signing policy) gains a `trust` command; Electron derives a firewall-setup mode from it and shows it in Settings → Service. Installer file lists are derived from (shell) or checked against (NSIS) `services/versions.json`. The canonical notice generator consumes a reviewed attribution manifest and gains a non-writing `licenses:check` gate.

**Tech Stack:** Swift (Security framework), TypeScript/Electron/React, Vitest, Bash, NSIS, Python 3.9+ `unittest`, `gh` CLI.

**Spec:** [`docs/superpowers/specs/2026-10-17-release-concerns-remediation-design.md`](../specs/2026-10-17-release-concerns-remediation-design.md) §6, §7, §8 (unsigned path).

## Global Constraints

- Work on a new branch `release/hygiene` from `release/performance-and-docs`; signed-off commits (`git commit -s`).
- Every new file carries the two-line SPDX header; `node scripts/spdx-headers.mjs` passes.
- TypeScript: no `as` casts, no `any`, no `unknown` in signatures, `@/` imports across directories, static imports only, no renderer imports from `src/electron/`.
- New exports must be imported somewhere or `npm run dead-code:check` fails.
- No legacy fallbacks: delete what is replaced.
- Do not weaken `HelperConstants` requirements, bypass macOS trust, or change repository settings without the owner's explicit go-ahead (Task 9).
- Do not invent license grants or copyright holders; unresolvable entries are recorded as blocked, not filled in.
- User-facing copy says "engine", not "backend".

## Review Focus

1. **A genuinely NVIDIA-signed build must still run automatic firewall setup.** Test: the `trusted` path still shows the first-run dialog and calls `ensureConfigured(true)` (Task 2).
2. **A missing or failing `trust` answer must not loop prompts either.** Test: `trust` → `null` yields mode `unknown`, no dialog, no install (Task 2).
3. **Staging must fail loudly when any component binary is missing**, not ship a partial bundle. Test in Task 4.
4. **Every Windows firewall rule the installer adds is deleted on uninstall.** Test in Task 5.
5. **`licenses:check` must fail on any README-fallback or missing text without a resolved manifest entry**, and on a stale `THIRD_PARTY_NOTICES.md`. Tests in Task 6.

---

### Task 1: Helper control tool reports build trust

**Files:**
- Create: `desktop/native/Shared/SignatureVerification.swift`
- Modify: `desktop/native/PrivilegedHelper/main.swift` (remove `verifySignature`, now shared)
- Modify: `desktop/native/HelperControl/main.swift` (add `trust` command)
- Modify (only if needed): `desktop/scripts/build/macos/build-helper.ts` (ensure the ctl links `Security`)

**Interfaces:**
- Produces: `func verifySignature(path: String, requirement: String) -> Bool` in Shared (moved verbatim).
- Produces: `nvpair-helper-ctl trust` prints one JSON object `{"trusted": Bool, "reason": String}` and exits 0. `trusted` is true only when **both** the host bundle (`Bundle.main.bundlePath`) satisfies `HelperConstants.appCodeSigningRequirement` **and** the tool's own executable satisfies `HelperConstants.ctlCodeSigningRequirement`. `reason` is `"trusted"`, `"host-unsigned"` or `"ctl-unsigned"` (first failure).

- [ ] **Step 1:** Move `verifySignature` from `PrivilegedHelper/main.swift` into `Shared/SignatureVerification.swift` unchanged; the daemon keeps calling it.
- [ ] **Step 2:** Add `cmdTrust()` and the `case "trust":` branch in `HelperControl/main.swift`, using `emit(_:)`.
- [ ] **Step 3: Build and verify against an unsigned build**

Run: `cd desktop && npm run build:helper:mac && ./native/build/nvpair-helper-ctl trust`
Expected: exit 0 and `{"reason":"host-unsigned","trusted":false}` (or `ctl-unsigned`); JSON key order may vary.

- [ ] **Step 4: Commit** — `feat(helper): report whether this build satisfies the helper signing policy`

### Task 2: Firewall-setup decision module (Electron main)

**Files:**
- Create: `desktop/src/electron/services/mac-helper-setup.ts`
- Modify: `desktop/src/electron/services/mac-privilege-service.ts` (add `trust`)
- Modify: `desktop/src/electron/main.ts` (replace the body of `runMacPrivilegedSetup` with a call into the new module)
- Test: `desktop/tests/modular/mac-helper-setup.test.ts`

**Interfaces:**
- Produces in `mac-privilege-service.ts`: `trust(): Promise<boolean | null>` — `null` when the ctl gives no parseable answer. Add to the exported `macPrivilege` object.
- Produces in `desktop/src/shared/types/ipc-channels.ts` (shared so Task 3's renderer can import it): `export type FirewallSetupMode = 'automatic' | 'approval-required' | 'manual-unsigned-build' | 'unknown' | 'not-applicable'`.
- Produces in `mac-helper-setup.ts`:
  - `interface MacHelperSetupDeps { isSupported(): boolean; trust(): Promise<boolean | null>; isSetupComplete(): boolean; setSetupComplete(value: boolean): void; ensureConfigured(forceFirewall: boolean): Promise<EnsureConfiguredResult>; explainFirstRun(): Promise<void>; explainApproval(): Promise<void> }` (export `EnsureConfiguredResult` from `mac-privilege-service.ts`).
  - `runMacHelperSetup(deps: MacHelperSetupDeps): Promise<FirewallSetupMode>`
  - `getFirewallSetupMode(): FirewallSetupMode` — last result, initially `'not-applicable'`.
- Behavior: not supported → `'not-applicable'`. `trust` false → `'manual-unsigned-build'`, log once at info, **no** dialogs, **no** `ensureConfigured`. `trust` null → `'unknown'`, log at error, no dialogs, no install. Trusted → today's flow unchanged (first-run explanation, `ensureConfigured(firstTime)`, approval explanation on first run, `setSetupComplete(true)` only when enabled and firewall configured); returns `'automatic'`, `'approval-required'`, or `'unknown'` on incomplete setup. Never throws.

- [ ] **Step 1: Write the failing tests** — real module, fake deps recording calls:
  - `untrusted build skips dialogs and helper install`: `trust→false` ⇒ mode `'manual-unsigned-build'`; `explainFirstRun`, `ensureConfigured`, `setSetupComplete` never called.
  - `unknown trust does not prompt`: `trust→null` ⇒ `'unknown'`; no dialog/install calls.
  - `trusted first run keeps the existing flow`: setup incomplete, ensure → `{supported:true, registration:'enabled', firewallConfigured:true}` ⇒ `explainFirstRun` once, `ensureConfigured(true)`, `setSetupComplete(true)`, mode `'automatic'`.
  - `approval pending explains only on first run`: registration `'requiresApproval'` ⇒ mode `'approval-required'`, `explainApproval` called when first run, not when `isSetupComplete()` is true; `setSetupComplete` not called.
  - `unsupported platform`: `isSupported→false` ⇒ `'not-applicable'`, `trust` never called.
- [ ] **Step 2:** Run `cd desktop && npx vitest run --project unit tests/modular/mac-helper-setup.test.ts` — expect FAIL (module missing).
- [ ] **Step 3:** Implement `trust()` (runs `['trust']`, 8000 ms timeout, reads boolean `trusted`) and the module; wire `main.ts` with the real deps (dialogs keep their current copy).
- [ ] **Step 4:** Re-run the test file — PASS. Then `npm run typecheck && npm run lint`.
- [ ] **Step 5: Commit** — `fix(desktop): stop helper prompts on builds that cannot satisfy its signing policy`

### Task 3: Show firewall-setup mode in Settings → Service

**Files:**
- Modify: `desktop/src/shared/types/ipc-channels.ts` (add the channel; `FirewallSetupMode` already lives here from Task 2)
- Modify: `desktop/src/electron/ipc/service.ipc.ts`, `desktop/src/preload/api/service.api.ts`
- Create: `desktop/src/ui/components/ServiceSettings/FirewallSetupCard.tsx`, `desktop/src/ui/utils/firewall-setup-copy.ts`
- Modify: `desktop/src/ui/components/ServiceSettings/ServiceSettings.tsx` (render card next to `VersionsCard`, macOS only)
- Test: `desktop/tests/modular/firewall-setup-copy.test.ts`

**Interfaces:**
- Channel `'service:get-firewall-setup': { request: void; response: FirewallSetupMode }` via `safeHandle`, returning `getFirewallSetupMode()`.
- Preload: `getFirewallSetup(): Promise<FirewallSetupMode>` on `IServiceApi`.
- `firewallSetupCopy(mode: FirewallSetupMode): { title: string; detail: string; tone: 'green' | 'yellow' | 'gray' } | null` — `null` for `'not-applicable'`.
- Required copy (exact):
  - `manual-unsigned-build`: title `Firewall setup: manual`, detail `This unsigned build can't install the background helper that configures the macOS firewall. If macOS asks whether PAIR's services may accept incoming connections, choose Allow on networks you trust. Other nodes can't reach this one until you do.`
  - `approval-required`: title `Firewall setup: waiting for approval`, detail `Open System Settings > General > Login Items & Extensions, enable the PAIR background item, then relaunch PAIR.`
  - `automatic`: title `Firewall setup: automatic`, detail `The PAIR helper configured the macOS firewall.`
  - `unknown`: title `Firewall setup: unknown`, detail `PAIR couldn't check the helper. Collect logs from this page if other nodes can't reach this machine.`

- [ ] **Step 1:** Failing test asserting the four literal title/detail pairs, tones (`automatic`→green, `unknown`→gray, others→yellow) and `null` for `'not-applicable'`.
- [ ] **Step 2:** Run it — FAIL.
- [ ] **Step 3:** Implement copy function, channel, preload method and card (renders nothing when copy is `null`; fetch once on mount; uses existing card/Badge components like `VersionsCard`).
- [ ] **Step 4:** Run the test, `npm run typecheck`, `npm run lint`, `npm run dead-code:check`, `npm run service-contracts:check` — all pass.
- [ ] **Step 5: Commit** — `feat(desktop): show macOS firewall setup mode in service settings`

### Task 4: Shell installer stages every component and the notices

**Files:**
- Modify: `services/installer_build.sh`
- Test: `scripts/tests/test_services_installer.py`

**Interfaces:**
- Staging copies `"$BIN_SRC/$name"` for each `name` from `jq -r '.components | keys[]' "$VERSIONS_FILE"`; a missing or non-executable binary prints `ERROR: missing staged binary <name>` and exits non-zero before `tar`.
- Also copies `"$ROOT/../LICENSE"` and `"$ROOT/../THIRD_PARTY_NOTICES.md"` to the stage root; missing either is an error.

- [ ] **Step 1: Failing tests** — copy `installer_build.sh` into a temp tree (`<tmp>/services/`), write a fixture `versions.json` with three components including `omlx-proxy`, fake executables in `services/build/bin/`, root `LICENSE` and `THIRD_PARTY_NOTICES.md`, minimal `installer/{macos,linux}/INSTALL.md`; run with `NVPAIR_SKIP_BUILD=1` and a real `jq`:
  - `stages_every_component_and_notices`: archive lists exactly `bin/<three names>`, `INSTALL.md`, `LICENSE`, `THIRD_PARTY_NOTICES.md`; staged binaries are mode 0755.
  - `missing_component_fails_before_archive`: delete `omlx-proxy` ⇒ non-zero exit, stderr names `omlx-proxy`, no archive in `dist/`.
  - `missing_notice_fails`: remove `THIRD_PARTY_NOTICES.md` ⇒ non-zero exit.
- [ ] **Step 2:** `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest scripts.tests.test_services_installer -v` (or `discover -s scripts/tests`) — FAIL.
- [ ] **Step 3:** Replace the 13 hardcoded `cp` lines with the `jq` loop and add the notice copies.
- [ ] **Step 4:** Tests pass; `shellcheck services/installer_build.sh` clean.
- [ ] **Step 5: Commit** — `fix(installer): stage every service component with the license notices`

### Task 5: Windows NSIS script covers oMLX and the notices

**Files:**
- Modify: `services/installer/nvpair-setup.nsi`
- Test: `scripts/tests/test_nsis_inventory.py`

**Interfaces:**
- Install section: `File "..\build\bin\omlx-proxy.exe"`; `SetOutPath "$INSTDIR"` gains `File "..\..\LICENSE"` and `File "..\..\THIRD_PARTY_NOTICES.md"`.
- Firewall: add `NVPAIR oMLX Proxy` and `NVPAIR mDNS oMLX Proxy (UDP 5353)` rules mirroring the LM Studio proxy rules; delete both on uninstall.
- Uninstall: `Delete` for `omlx-proxy.exe`, `LICENSE`, `THIRD_PARTY_NOTICES.md`.

- [ ] **Step 1: Failing tests** reading the real NSIS file and `services/versions.json`:
  - `install_and_uninstall_cover_every_component`: set of `File "..\build\bin\<n>.exe"` names == set of `Delete "$INSTDIR\bin\<n>.exe"` names == `versions.json` component keys.
  - `every_added_firewall_rule_is_deleted`: set of `add rule name="..."` == set of `delete rule name="..."`.
  - `omlx_proxy_mirrors_lmstudio_rules`: for each LM Studio proxy rule, an oMLX rule exists with the same `protocol`/`localport` arguments and `program="$INSTDIR\bin\omlx-proxy.exe"`.
  - `notices_installed_and_removed`: `LICENSE` and `THIRD_PARTY_NOTICES.md` have both `File` and `Delete` lines.
- [ ] **Step 2:** Run — FAIL (and record any pre-existing add/delete mismatch the test reveals; fix it in Step 3).
- [ ] **Step 3:** Edit the NSIS script.
- [ ] **Step 4:** Tests pass. (`makensis` is not available on macOS; note in the commit body that the script was not compiled.)
- [ ] **Step 5: Commit** — `fix(installer): include oMLX proxy and license notices in the Windows script`

### Task 6: Reviewed attribution manifest and `licenses:check`

**Files:**
- Create: `desktop/licenses/attribution-manifest.json`, `desktop/licenses/texts/` (directory with `.gitkeep` until Task 7)
- Create: `desktop/scripts/attribution-manifest.ts`
- Modify: `desktop/scripts/generate-licenses.ts`, `desktop/scripts/collect-go-licenses.ts` (comment only), `desktop/package.json` (`"licenses:check": "tsx scripts/generate-licenses.ts --check"`)
- Modify: `Makefile` (`check` target adds `licenses-check` → `cd desktop && npm run licenses:check`)
- Test: `desktop/tests/modular/attribution-manifest.test.ts`

**Interfaces:**
- Manifest entries (JSON array under `"entries"`): `{ "name": string, "version": string, "kind": "npm" | "go" | "asset", "license": string, "status": "resolved" | "blocked", "sourceUrl": string, "sourceRef": string, "textFile": string | null, "textSha256": string | null, "notes": string }`. `textFile` is relative to `desktop/licenses/texts/`.
- `loadAttributionManifest(path: string): AttributionManifest` — validates shape, rejects duplicates (`name@version`), and verifies each resolved entry's text file exists and its SHA-256 equals `textSha256`; throws with the offending key.
- `applyAttribution(entries: LicenseEntry[], manifest: AttributionManifest, readText: (file: string) => string): { entries: LicenseEntry[]; unresolved: string[] }` — resolved manifest entries replace the text/license of the matching `name@version`; `kind: "asset"` entries are appended; entries still containing the README-fallback marker or `(no license file found` and without a resolved manifest entry, plus blocked manifest entries, are reported in `unresolved` as `name@version`.
- `--check`: render to memory; exit 1 if output differs from `THIRD_PARTY_NOTICES.md` or `unresolved` is non-empty (print each key). Without `--check`, write the file and print unresolved keys as warnings (local builds keep working). In `--check` mode run `go list` with `GOPROXY=off` and `GOFLAGS=-mod=readonly` so the gate never downloads modules; a missing module cache fails with a message to run `make deps-go` first.
- `collectGoModuleEntries(moduleDirs: string[])` takes module directories instead of `(servicesDir, componentNames)`; the generator passes every `services/<component>` from `versions.json` plus `scripts/collectlogs` and `scripts/inference-dispatcher` (both Go modules with no external requirements today, so a future dependency cannot ship unattributed).

- [ ] **Step 1: Failing tests** (fixture manifest and temp text files; literal expectations):
  - `resolved entry replaces fallback text` (text equals fixture file content; license equals manifest license).
  - `fallback without manifest entry is unresolved` → `['pkg@1.0.0']`.
  - `blocked entry stays unresolved`.
  - `asset entry is appended once`.
  - `hash mismatch is rejected` → throws naming the key.
  - `duplicate key is rejected`.
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Implement; empty manifest (`{"entries": []}`) is valid.
- [ ] **Step 4:** Tests pass. `npm run licenses` regenerates identically (no manifest entries yet); `npm run licenses:check` **fails** listing the 23 known keys — expected until Task 7.
- [ ] **Step 5: Commit** — `feat(notices): gate releases on a reviewed attribution manifest`

### Task 7: Resolve the unresolved attributions

**Files:**
- Modify: `desktop/licenses/attribution-manifest.json`; create text files under `desktop/licenses/texts/`
- Regenerate: `THIRD_PARTY_NOTICES.md`

**Scope (from the current notice):** 13 `@radix-ui/*` packages, `@electron-internal/extract-zip@1.0.4`, `agent-base@6.0.2`, `https-proxy-agent@5.0.1`, `flip-toolkit@7.2.4`, `react-flip-toolkit@7.2.4`, `lazy-val@1.0.5`, `overlayscrollbars@2.15.1`, `overlayscrollbars-react@0.5.6`, `react-compiler-runtime@1.0.0`, `react-remove-scroll-bar@2.3.8`, `saxes@6.0.0`, `github.com/mattn/go-localereader@v0.0.1`; assets `lucide` (from the license block in `desktop/src/ui/lib/kaizen-ui-foundations/base-external.css`) and `kaizen-ui-foundations@1.0.0`.

- [ ] **Step 1:** For each package, fetch the upstream license file at the tag/commit matching the exact version (`gh api repos/<o>/<r>/contents/LICENSE?ref=<tag>` or the published tarball via `npm pack <name>@<version>` in a fresh empty temp directory, outside the repo). Store verbatim text, record `sourceUrl`, `sourceRef` (tag or commit SHA), `textSha256`, `status: "resolved"`.
- [ ] **Step 2:** Where no authoritative text exists at that version (expected for `go-localereader`, possibly others), set `status: "blocked"` with `notes` stating what was checked. Do not synthesize a license.
- [ ] **Step 3:** `lucide`: extract the license block verbatim from the vendored CSS into a text file; `status: "resolved"`. `kaizen-ui-foundations`: `status: "blocked"`, notes `NVIDIA redistribution/modification terms needed from the owner`.
- [ ] **Step 4:** `npm run licenses`, then `npm run licenses:check`. Expected: failure lists only blocked keys. Record that list in `docs/release-readiness.mdx` as owner decisions.
- [ ] **Step 5: Commit** — `fix(notices): add reviewed upstream license texts for fallback entries`

### Task 8: Pin vendored Kaizen CSS bytes

**Files:**
- Modify: `desktop/scripts/vendor-kaizen-ui-foundations-css.ts`

**Interfaces:** each asset gains `sha256: string` (lowercase hex of the **downloaded** bytes before font stripping); the script fails with the URL and both digests on mismatch, before writing.

- [ ] **Step 1:** Compute current digests by fetching each URL once (`curl -fsSL <url> | shasum -a 256`) and record them.
- [ ] **Step 2:** Add the check; keep the generated-file header noting font-face removal.
- [ ] **Step 3:** `npm run vendor:kaizen-css` then `git diff --exit-code desktop/src/ui/lib/kaizen-ui-foundations` — no change. Temporarily alter one digest locally and confirm the script fails without writing; restore it.
- [ ] **Step 4: Commit** — `fix(build): verify vendored Kaizen CSS against pinned digests`

### Task 9: Private vulnerability reporting (owner action)

**Files:** `SECURITY.md`, `docs/release-readiness.mdx`

- [ ] **Step 1:** Ask the owner to either enable it in GitHub (Settings → Code security → Private vulnerability reporting) or explicitly authorize running `gh api -X PUT repos/JaredP94/Personal-AI-Router/private-vulnerability-reporting`. Do not run it without that authorization.
- [ ] **Step 2:** Verify: `gh api repos/JaredP94/Personal-AI-Router/private-vulnerability-reporting` returns `{"enabled":true}`.
- [ ] **Step 3:** Update `SECURITY.md` "This fork" section to state it is enabled (with the check date) and remove the release-blocking sentence; mark the item done in `docs/release-readiness.mdx`. The owner sends one benign test report and confirms receipt.
- [ ] **Step 4: Commit** — `docs(security): document the fork's private vulnerability reporting channel`

### Task 10: Docs, changelog and full verification

**Files:** `CHANGELOG.md` (Unreleased), `docs/release-readiness.mdx`, `docs/known-issues.mdx` (unsigned firewall note points to the new card), `docs/developing.mdx` and `AGENTS.md` (mention `licenses:check` in checks)

- [ ] **Step 1:** Update the docs to match Tasks 1–9; changelog entries under Unreleased Fixed/Added.
- [ ] **Step 2: Full verification**

```bash
node scripts/spdx-headers.mjs
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/tests -v
shellcheck scripts/install-mac.sh services/installer_build.sh
cd desktop && npm run lint && npm run typecheck && npm run test:unit \
  && npm run dead-code:check && npm run service-contracts:check && npm run licenses:check
```

Expected: all pass except `licenses:check`, which may fail **only** on owner-blocked keys recorded in Task 7.

- [ ] **Step 3: Commit** — `docs(release): record release hygiene changes and remaining owner decisions`
