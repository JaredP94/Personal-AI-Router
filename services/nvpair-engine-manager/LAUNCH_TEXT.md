<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Engine launch text

Ollama and LM Studio accept editable arguments and leading environment
assignments in `pair-arguments-v1` notation. The executable and startup
subcommand remain manifest-owned. oMLX is launched unchanged through `omlx start`;
its app manages server settings. Only its PAIR proxy port is editable.

## Notation

- Spaces, tabs and line breaks separate tokens. Single quotes preserve literal
  text. Double quotes recognize only `\"` and `\\` escapes; other backslashes
  stay literal, including Windows paths.
- Quoted and unquoted segments may form one token, such as `--name="two words"`.
  `""` represents an empty argument.
- Environment assignments precede arguments, for example
  `MY_SETTING="two words" --future-option 3`. Names start with a letter or
  underscore and contain only letters, digits and underscores. Duplicate names
  are rejected (case-insensitive on Windows).
- This is not a shell command. Variables, paths and placeholders such as
  `$HOME`, `%USERPROFILE%`, `~` and `{port}` are not expanded in user values.
  Unquoted shell operators and command substitution are rejected. Quote them
  when they are literal argument values.
- Input is valid UTF-8, at most 16 KiB and 256 tokens. Unsupported control
  characters and unterminated quotes are rejected without echoing input.

Unknown vendor arguments and environment assignments pass through literally.
PAIR has no catalog of their types, ranges or compatibility. The engine may
reject an otherwise valid PAIR settings preview when it starts.

## PAIR-owned settings

Bind addresses, server ports and proxy wiring cannot be supplied through launch
text, even when a supplied value equals the current managed value. Use the
numeric server-port and proxy-port fields. Managed flag aliases, equals forms,
attached short values and ambiguous bundles containing a managed short option
are rejected, including after `--`. The bundled protected controls include
Ollama's `OLLAMA_HOST` and LM Studio's `--port`/`-p`, `--bind`/`--host`,
`LMS_SERVER_PORT` and `LMS_SERVER_HOST`.

Saved overrides undergo the same checks before execution. Launch settings do
not change the existing bind policy: default loopback and the explicit live
`engine_allow_lan_bind` permission still govern bind overrides.

## Persistence and diagnostics

The service saves literal host-platform `runtime.launch_args` and
`runtime.launch_env` arrays separately from trusted manifest templates.
Assignments replace prior editable assignments, so removing one does not leave
it behind. Explicit values override defaults; omitted and unchanged manifest
values continue following the manifest. Other platform and unrelated override
fields are preserved. Files use mode `0600`, with private override directories.

Custom launch output is excluded from exported engine logs. Startup failures
return a stable failure rather than vendor output that could echo secrets.
Desktop subprocess logs redact `launchText`, `launch_args` and `launch_env`,
including malformed/truncated frames. Settings snapshots themselves contain
these values: protect local configuration and pair only with trusted devices.

See [manifest bindings](MANIFEST.md) and the
[broker settings protocol](../nvpair-ui-broker/ENGINE_SETTINGS.md).
