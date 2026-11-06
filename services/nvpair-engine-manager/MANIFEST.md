<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine manifest capabilities

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
