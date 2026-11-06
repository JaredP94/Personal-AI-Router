<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# llama.cpp: lifecycle-only preview

PAIR manages the official **b12171 macOS arm64** build. Other platforms are
unsupported. It can install, start, stop and configure the engine, but **llama.cpp
does not serve requests through PAIR yet**. There is no proxy worker, routing,
model hub, download/delete adapter or load profile in this change.

The macOS build is **not notarized**, like this fork's PAIR application. The
release is checksum-pinned, not publisher-trusted by that checksum. Its existing
ad-hoc signature is preserved and checked for integrity with the system
`codesign --verify --strict`; missing codesign or a broken signature fails
installation. Quarantined files fail installation naming the file; PAIR never
clears quarantine, re-signs code or bypasses the operating system's checks.

Artifact: `llama-b12171-bin-macos-arm64.tar.gz` from ggml-org/llama.cpp, SHA-256
`870ef967970fa18bbd31460bc5f7929bbb5f091359d8dc0f58e20133aa4b5fcd`.
The archive wraps files in `llama-b12171/`; installation strips that component.
The required layout was derived from that release archive and `otool -L`:
`llama-server`, `LICENSE`, `libllama-server-impl.dylib`, all versioned and alias
names for `libllama`, `libllama-common`, `libmtmd` (0.9.0), and `libggml`,
`libggml-base`, `libggml-blas`, `libggml-cpu`, `libggml-metal`, `libggml-rpc`
(0.29.0). Metal resources are bundled in the Metal library, not a separate
`.metallib` or `.metal` file in this archive. Internal library symlinks stay intact.
See [archive safeguards](MANIFEST.md#pinned-macos-archive-installs) for limits,
atomic replacement/rollback and identity-bound uninstall. Updating requires the
engine to be stopped; repeated `engine:install` applies a changed manifest pin.

Default listener: **127.0.0.1:8082**, distinct from the fork's other engine and
service ports. Configured-port reservations and occupied-port checks still apply.
Readiness and health require `/props` to return the top-level `"role":"router"`
identity, present in b12171 `tools/server/server-models.cpp` (`get_router_props`).
A different service returning HTTP 200 is not an engine match. Foreground-process
shutdown uses the existing process lifecycle, not command-mode `wait_for_exit`.
The model cache is `LLAMA_CACHE={install_dir}-models`, a PAIR-managed sibling
that survives engine uninstall. Nothing in this PR downloads models.

## Binding audit (b12171)

Reviewed the real parser at
[common/arg.cpp](https://github.com/ggml-org/llama.cpp/blob/b12171/common/arg.cpp),
including every assignment to `hostnames`, `port` and `reuse_port`, its environment
handling and long-option normalization:

- `--host` / `LLAMA_ARG_HOST`: IP lists **or Unix socket paths ending in `.sock`**.
  There is no separate Unix-socket option or short host alias in this parser.
- `--port` / `LLAMA_ARG_PORT`: TCP port; no short port alias.
- `--reuse-port` / `LLAMA_ARG_REUSE_PORT`: enables multiple sockets on the port.
  PAIR forces the environment value to `0` even when inherited.
- Port-changing presets (no environment aliases): `--embd-gemma-default`,
  `--fim-qwen-1.5b-default`, `--fim-qwen-3b-default`, `--fim-qwen-7b-default`,
  `--fim-qwen-7b-spec`, `--fim-qwen-14b-spec`, `--fim-qwen-30b-default`,
  `--gpt-oss-20b-default`, `--gpt-oss-120b-default`, `--vision-gemma-4b-default`,
  `--vision-gemma-12b-default`.

All are forbidden in editable launch input, including matching values,
duplicates, `--flag=value`, underscore spellings (`--reuse_port`, etc.) and tokens
after `--`. Protected environment assignments are rejected even in argument
positions. `LLAMA_CACHE` is also protected to keep the managed cache identity.
Host/port environment defaults are overwritten by PAIR. System configuration
loads before environment and CLI in this tag; PAIR's managed host/port and
reuse-port environment therefore take precedence. Additional launch options stay
literal, without shell evaluation. Non-loopback binding is available only through
the existing validated start path with `engine_allow_lan_bind`, not launch text.

Selected lifecycle/manifest concepts are adapted from
NVIDIA/Personal-AI-Router@6d2d67f; its unified-proxy plumbing is not included.
NVIDIA/Personal-AI-Router#153 informs identity-bound containment/removal;
NVIDIA/Personal-AI-Router#186's optional ModelScope work remains deferred.

Packaged install, update, inference and stop/restart smoke testing is still a
maintainer pre-merge task. Development tests use generated fixtures and never
execute a downloaded release binary.
