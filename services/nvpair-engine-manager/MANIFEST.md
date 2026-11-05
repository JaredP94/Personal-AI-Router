<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Editable launch manifest bindings

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
