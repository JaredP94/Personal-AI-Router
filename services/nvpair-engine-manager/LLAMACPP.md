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
`codesign --verify --strict --all-architectures` on **every Mach-O file**,
identified by thin/fat magic bytes rather than extension or execute bits. This
includes adjacent dylibs, extensionless code and every universal-binary slice.
Missing codesign or a broken signature fails installation. Quarantine/signature
checks run again for the same-pin already-installed return and before every
spawn, with required layout and link containment revalidated. Quarantined files fail installation naming the file; PAIR never
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

## Launch allowlist (b12171)

Unlike #48's open Ollama/LM Studio launch text, llama.cpp's real parser exposes
host tools, agents, tunnels, MCP, filesystem paths and model downloads. As with
#49's LM Studio load options, only explicitly typed inference/resource tuning is
accepted. Unknown options, positional arguments, `--`, repeated options (including
aliases or opposing switches), non-ASCII and **all** environment assignments are
invalid. Saved settings use the same checks; invalid settings are reported, not
dropped or applied. No model/download or template-file option is permitted.

Allowed value options (documented short aliases included): `--ctx-size/-c`,
`--batch-size/-b`, `--ubatch-size/-ub` (context 0–1048576, batches 1–1048576),
`--threads/-t`, `--threads-batch/-tb` (1–4096),
`--gpu-layers/--n-gpu-layers/-ngl` (0–1048576, `auto`, `all`),
`--flash-attn/-fa` (`on/off/auto`), `--cache-type-k/-ctk`, `--cache-type-v/-ctv`
(`f32/f16/bf16/q8_0/q4_0/q4_1/q5_0/q5_1/iq4_nl`), `--parallel/-np` (1–1024),
`--predict/--n-predict/-n` (-1–1048576), `--temp/--temperature` (0–10),
`--top-k` (0–1048576), `--top-p`, `--min-p` (0–1),
`--chat-template` (only named built-ins from b12171 `src/llama-chat.cpp`),
`--reasoning-format` (`none/auto/deepseek/deepseek-legacy`),
`--reasoning/-rea` (`on/off/auto`), `--reasoning-effort`
(`default/minimal/low/medium/high/xhigh/max`), `--reasoning-budget` (-1–1048576),
`--load-mode/-lm` (`auto/none/mmap/mlock/mmap+mlock/dio`).
`--flag value` and `--flag=value` are normalized to separate argv values because
b12171's parser requires them. Presence-only boolean switches: `--cont-batching/-cb`,
`--no-cont-batching/-nocb`, `--kv-offload/-kvo`, `--no-kv-offload/-nkvo`,
`--metrics`, `--slots`, `--no-slots`. Every allowlisted name is checked against
`testdata/llamacpp-b12171-options.txt`, the option names b12171 defines; regenerate
it from `common/arg.cpp` when the pinned release changes.
No arbitrary template programs, paths, URLs or non-finite numeric values are allowed.

## Binding audit (b12171)

Reviewed the real parser at
[common/arg.cpp](https://github.com/ggml-org/llama.cpp/blob/b12171/common/arg.cpp),
including every assignment to `hostnames`, `port` and `reuse_port`, its environment
handling and long-option normalization:

- `--host` / `LLAMA_ARG_HOST`: IP lists **or Unix socket paths ending in `.sock`**.
  There is no separate Unix-socket option or short host alias in this parser.
- `--port` / `LLAMA_ARG_PORT`: TCP port; no short port alias.
- `--reuse-port` / `LLAMA_ARG_REUSE_PORT`: enables multiple sockets on the port.
  PAIR sets `0` only after excluding configuration files; `0` cannot undo a
  config-enabled reuse-port value in this tag.
- Port-changing presets (no environment aliases): `--embd-gemma-default`,
  `--fim-qwen-1.5b-default`, `--fim-qwen-3b-default`, `--fim-qwen-7b-default`,
  `--fim-qwen-7b-spec`, `--fim-qwen-14b-spec`, `--fim-qwen-30b-default`,
  `--gpt-oss-20b-default`, `--gpt-oss-120b-default`, `--vision-gemma-4b-default`,
  `--vision-gemma-12b-default`.

All are forbidden in editable launch input, including matching values,
duplicates, `--flag=value`, underscore spellings (`--reuse_port`, etc.) and tokens
after `--`. Protected environment assignments are rejected even in argument
positions. `LLAMA_CACHE` is also protected to keep the managed cache identity.
The child inherits **no** parent environment. This removes all `LLAMA_ARG_*`,
`LLAMA_CACHE`, `HF_TOKEN`, `HF_ENDPOINT`, `MODEL_ENDPOINT`, `MTMD_BACKEND_DEVICE`,
`LLAMA_APP_CMD` and dynamic-loader overrides, rather than maintaining a removal
denylist. PAIR supplies only manifest-owned values: cache, listener, reuse-port=0,
empty API prefix, offline mode, fixed system PATH and child-only HOME/XDG paths.
The configuration audit uses b12171 `common/arg.cpp`:
`common_params_apply_system_config` always reads `/etc/llama.cpp/config.ini` and
user `config.ini` **before** environment/CLI; there is no disable-config option.
`common/common.cpp:fs_get_config_directory` uses `XDG_CONFIG_HOME`, then
`HOME/.config` (not macOS Application Support). PAIR points HOME/XDG_CONFIG_HOME
at `{install_dir}/.pair-home` and `{install_dir}/.pair-config`; the actual user
configuration cannot be found. Before every spawn it refuses any file (including
symlinks or inspection failures) at `/etc/llama.cpp/config.ini` or
`{install_dir}/.pair-config/llama.cpp/config.ini`, naming the path. It never
modifies ambient files. Thus a config enabling reuse-port or a tool runtime is
never applied. `common/common.cpp:fs_get_cache_directory` and
`common_get_model_endpoint`, and `common/arg.cpp` environment handlers are the audited
cache/download configuration sources. Additional launch options stay literal,
without shell evaluation. Non-loopback binding is available only through
the existing validated start path with `engine_allow_lan_bind`, not launch text.

Selected lifecycle/manifest concepts are adapted from
NVIDIA/Personal-AI-Router@6d2d67f; its unified-proxy plumbing is not included.
NVIDIA/Personal-AI-Router#153 informs identity-bound containment/removal;
NVIDIA/Personal-AI-Router#186's optional ModelScope work remains deferred.

Packaged install, update, inference and stop/restart smoke testing is still a
maintainer pre-merge task. Development tests use generated fixtures and never
execute a downloaded release binary.
