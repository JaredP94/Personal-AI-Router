<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine manifest capabilities

## Pinned macOS archive installs

`install.archive` is an alternative to `install.disk_image`, scripts and commands.
It requires a single checksum-pinned `fetch`, a nonempty `required` relative-file
list and a `codesign` target in that list. Files are unpacked with one wrapper
component stripped into a private sibling staging directory, never by a shell.
The runner rejects absolute/traversing paths, escaping or invalid links, duplicate
case-alias paths, special files, setuid/setgid modes, quarantine metadata, more
than 512 entries or 2 GiB of expanded data. Internal library links are preserved.
Required files must be nonempty regular files (or contained links to them).

On macOS every extracted entry is checked for `com.apple.quarantine`; any such
attribute fails installation naming the file, without changing the attribute.
The system `/usr/bin/codesign --verify --strict <executable>` must succeed; this
checks existing signature integrity, not notarization or publisher trust. PAIR
never re-signs the files. Other platforms fail closed.

The completed staging directory includes an identity-bound PAIR install claim
and artifact digest. First installation uses exclusive rename; updates atomically
exchange the complete directories using macOS `RENAME_SWAP`. Extraction,
validation, claim-write or exchange failure leaves the previous install intact.
Repeated `engine:install` is a no-op for the same claimed digest; a changed pinned
digest replaces a stopped, claimed installation. Stop the engine before updating.
Unclaimed installations are never replaced or uninstalled. Archive installs may
use the special `models_dir: "{install_dir}-models"` sibling cache; uninstall
removes only the engine tree, never this cache. There is no separate update RPC.

## Command-mode shutdown confirmation

`runtime.stop.wait_for_exit` is an optional boolean (default `false`). It
requires `runtime.mode: "command"`, a nonempty `runtime.stop.cmd`, and a
`runtime.ready` probe; manifests enabling it without these are rejected.

By default, command-mode shutdown confirms three consecutive connection
refusals on the readiness listener. A persistent host process, such as LM
Studio after `lms server stop`, need not exit; its server can be restarted.
Set `wait_for_exit: true` only when the app must fully exit before restarting.
Only bundled oMLX enables it:

```json
"stop": { "cmd": ["{cli}", "stop"], "wait_for_exit": true }
```

With this capability, PAIR also waits for the serving PID observed before the
stop command to exit, when the host can resolve it. If PID lookup is unavailable,
listener closure remains the available evidence. The stop command is bounded by
`max(grace_s, 5s)` and confirmation by `grace_s` (default 5s), without extending
the deadline. Timeout retains running state and reports an error. The state
mutex is never held across lookup, command execution, or confirmation; the
lifecycle operation lock still serializes stop/start. Failed-start cleanup uses
the same confirmation policy.

## Editable launch manifest bindings

The engine-manager runner remains manifest-driven. `runtime.editable_launch`
identifies the fixed startup arguments and PAIR-owned networking controls; it
is not a catalog of vendor options. Other user arguments and environment
assignments pass through literally after trusted manifest expansion.

```json
{
  "editable_launch": {
    "fixed_args": ["server", "start"],
    "controls": [
      { "flags": ["--port", "-p"], "env": ["LMS_SERVER_PORT"], "value": "{server.port}" },
      { "flags": ["--bind", "--host"], "env": ["LMS_SERVER_HOST"], "value": "{server.host}" }
    ]
  }
}
```

- `fixed_args` are never editable. Command-mode `start_index` selects the
  editable start step (default zero).
- `flags` lists canonical spelling first, followed by aliases. `env` lists
  protected environment aliases. Every alias receives the managed value.
- `value` binds `server.host` and/or `server.port` with literal separators,
  for example Ollama's `{server.host}:{server.port}` binding to `OLLAMA_HOST`.
- The fields within one binding must be unique and separated unambiguously.
  Controls collectively bind both server fields. Unknown fields, misspelled
  properties and duplicate sources fail loading.
- The numeric server-port field supplies `server.port`. Existing runtime bind
  policy supplies `server.host`; no user launch setting can replace either.
  These controls are rejected in editable input, including their aliases.
- The proxy listener belongs to the broker and is never a launch binding.

[manifest.schema.json](manifest.schema.json) supplies editor validation; Go
also validates cross-control constraints. Bundled oMLX intentionally has no
editable launch definition: its `omlx start` command is not changed.

The private per-engine override deep-merges onto the bundled manifest.
Host-platform `launch_args` and `launch_env` are literal replacement arrays,
not templates. Port saves preserve unrelated fields and other platforms;
restoring a bundled default removes only port pins, not launch values. Invalid
existing files are preserved rather than overwritten.

See [LAUNCH_TEXT.md](LAUNCH_TEXT.md) for the notation and security boundary.
