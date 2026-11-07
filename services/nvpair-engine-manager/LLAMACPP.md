<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# llama.cpp

PAIR manages the official **b12171 macOS arm64** build. Other platforms are
unsupported. PAIR installs, starts, stops and configures the engine and serves
it through the dedicated `llamacpp-proxy` worker, default port **1237**. Routing
uses exact model IDs from the engine's `/v1/models` inventory, with separate
`llamacpp` attribution. The Go model hub supports catalog, downloads, cancel
and delete over JSON-RPC; desktop/TUI hub screens and load profiles are deferred.

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
Without a model, readiness and health require `/props` to return the top-level
`"role":"router"` identity, present in b12171 `tools/server/server-models.cpp`
(`get_router_props`). With `--model`, owned-process probes instead match the
inherited descriptor's `model_path` from `tools/server/server-context.cpp`
(`get_res_props`); settings and previews retain the user's model selection.
A different service returning HTTP 200 is not an engine match. Foreground-process
shutdown uses the existing process lifecycle, not command-mode `wait_for_exit`.
The model cache is `LLAMA_CACHE={install_dir}-models`, a PAIR-managed sibling
that survives engine uninstall. PAIR downloads models itself; llama-server is
kept offline and none of its download launch options are enabled.

## Go model hub and owned cache

`engine:model-catalog {query?: string}` returns `{models: [{id, file, size?,
sha256?}]}`. Empty query reads ggml-org, bartowski and unsloth, up to 50 repos
each. Public search admits at most 50 repos and 100 query characters; at most
20 search queries are cached for six hours. The aggregate metadata deadline is
20 seconds (broker 25 seconds). Only public, ungated text-generation GGUF repos
with exactly one single-file Q4_K_M candidate are listed. IDs are
`owner/repo:Q4_K_M`. All path components use bounded strict ASCII validation.

Existing `engine:action` supports llama.cpp `pull_model {model: id}` with
`opId`, `list_downloaded` returning `{models: [{id, file, size, sha256,
finalHost}]}`, and `delete_model {model: id}`. Inventory combines the ownership
manifest with a bounded (4096 entries) no-follow regular-GGUF scan; manually
supplied GGUFs have their filename as ID and empty checksum/host and are never
deletable by PAIR. Recorded files must match their descriptor-derived file identity, size and valid
single-file GGUF metadata to appear. The live `list_models` remains the
llama-server served inventory, not this installed list. Installed scans check a
five-second context budget between bounded per-file metadata reads (broker ten
seconds; future clients fifteen seconds). Filesystem syscall latency itself
cannot be preempted by a Go context.

PAIR resolves metadata again before transfer, requiring LFS SHA-256 and size
and a pinned repository commit. Limit: 64 GiB per file plus 256 MiB free-space
reserve. During streaming, fstatfs on the open transfer descriptor rechecks
space every 256 MiB or five seconds, whichever occurs first at a streaming
boundary (a blocked read writes no additional bytes). Remaining free space must
cover the unreceived file bytes plus the 256 MiB safety margin. A drop below that
threshold aborts with "not enough disk space" through the existing progress/error
state and identity-checked cleanup of this operation's temp. Transfers have a
30-minute operation bound, 10-second dial/TLS/header
bounds, a 64 KiB response-header cap and a 30-second idle-read bound. Redirects
must stay HTTPS, cannot contain credentials and are limited to five; the final
host is recorded. Bodies and transfer URLs are never logged. An exclusive
random temporary file is created inside the pinned cache descriptor; exact
size, SHA-256 and single-file GGUF metadata are checked before fsync and an
atomic no-replace rename. The small ownership manifest is fsynced and atomically
replaced. Existing destinations are never overwritten. A crash between file
commit and manifest commit can leave an unrecorded file, never a deletable
owned entry; abandoned crash temp files are not automatically reclaimed.

The cache is in the user's own app data: entry swaps require another process
running as the same user, not a different unprivileged user. Staging uses
`openat(cacheFd, randomName, O_CREAT|O_EXCL|O_NOFOLLOW|O_RDWR|O_CLOEXEC, 0600)`
through the no-follow directory walk and records dev/ino immediately with fstat.
After hashing and fsync through that descriptor, commit uses renameatx_np with
RENAME_EXCL on macOS (renameat2 RENAME_NOREPLACE on Linux), then fstatat with
AT_SYMLINK_NOFOLLOW compares the final entry's dev/ino and size to the verified
file. A mismatch fails with no ownership record and leaves the unrelated entry
untouched. Only a match permits directory fsync and manifest commit; the manifest
records dev/ino (`fileIdentity`), size and SHA-256. Generated 64-hex `.gguf` names
are reserved case-insensitively, matching default macOS filesystem semantics:
unrecorded commits cannot become selectable manual models, and case aliases
cannot bypass recorded-model identity checks.
Selection and the actual opened launch descriptor must match a recorded model's
dev/ino/size; replaced files are refused without re-hashing up to 64 GiB.

macOS provides no rename-by-descriptor or unlink-by-descriptor. A same-user
process can still change an entry after the last identity check; the residual
check-to-use window is the minimum the OS allows, not an atomicity guarantee.
Launch validates the inherited descriptor, so a later pathname replacement
cannot redirect llama-server. These checks detect entry replacement, not writes
in place by a process already authorized as the same user.

`engine:cancel-pull` accepts asynchronously; only the terminal progress update
confirms transfer release and removal of that operation's own temp file.
The shared pull tracker reports `cancelled` only when that operation's user
cancel was requested and the returned error wraps `context.Canceled`. Metadata
requests and streaming reads preserve that cause, including transports whose
body read errors lose it when cancellation tears down the connection. The pull
then returns `{"status":"cancelled"}` without a `pull-failed` engine error.
Unrelated failures (including checksum/size mismatch, HTTP status, disk space,
identity checks and manifest writes) remain errors regardless of cancel timing,
even when cancellation arrives during teardown. Independent cleanup failures
also remain errors.
Completion is fenced against cancel acceptance after the post-rename identity
check and successful manifest write/fsync. A cancel accepted before that fence
rolls back any newly written manifest entry and removes the operation's
identity-matched published file as well as its temp. After the fence, cancellation
is no longer accepted: the pull reports success and the model remains installed.
No filesystem or network I/O is performed while holding the cancellation lock.
Before temp cleanup or completed-model deletion, fstatat with
AT_SYMLINK_NOFOLLOW checks dev/ino (and the recorded size for a completed model)
immediately before unlinkat. A changed entry is left untouched and reports
"cleanup skipped" through the terminal error state, not successful cancellation.
The original temp descriptor stays open until cleanup finishes. The same check
protects manifest staging cleanup and rollback of an unrecorded model. The
remaining check-to-unlink window cannot be eliminated on macOS; no unlink-by-fd
primitive exists. Unlike Ollama/LM Studio in #50, only this operation's
identity-matched temp is removed; shared or substituted files are not knowingly
removed. Concurrent
cache mutations are refused (including duplicate pulls), not queued. #50's
operation-ID fencing, broker worker-loss interruption and epoch recovery apply
unchanged. Verified-peer downloads/cancel/delete use the existing authenticated
remote methods and streams. Start is refused during a cache mutation; deletion
conservatively requires the engine to be stopped and no lifecycle op in flight.
Every cache ancestor is opened with openat/no-follow from `/`; mutations stay
relative to that descriptor even after a parent swap. Delete requires a matching
ownership manifest record, file identity and regular file, and never recursively removes data.

This intentionally differs from NVIDIA/Personal-AI-Router@6d2d67f: PAIR does not
ask llama-server's router-mode API to download. All checks and #50 tracking stay
inside Go. No real model was downloaded during development.

## Launch allowlist (b12171)

Unlike #48's open Ollama/LM Studio launch text, llama.cpp's real parser exposes
host tools, agents, tunnels, MCP, filesystem paths and model downloads. As with
#49's LM Studio load options, only explicitly typed inference/resource tuning is
accepted. Unknown options, positional arguments, `--`, repeated options (including
aliases or opposing switches), non-ASCII and **all** environment assignments are
invalid. Saved settings use the same checks; invalid settings are reported, not
dropped or applied. No download or template-file option is permitted.

`--model/-m` selects one existing regular `.gguf` file in the PAIR-owned
`{install_dir}-models` cache. Absolute and cache-relative paths are accepted;
both cache and file are resolved with `EvalSymlinks` before containment checks.
Symlink escapes, URLs, directories, missing files and non-GGUF names are rejected
in previews, saved settings and before launch. Previews retain the canonical
cache path. Immediately before spawning, PAIR resolves the cache directory once,
then opens its canonical path from `/` one directory component at a time using
`openat` with `O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC`. It walks the model's
cache-relative subdirectories from that descriptor the same way, rejecting
absolute paths and parent traversal in the relative path. The final file opens
read-only with `O_NOFOLLOW` and is checked by descriptor for regular-file type
and single-file GGUF metadata. A parent or subdirectory replaced with a symlink
is never followed; no fresh pathname resolution can bless an outside file.
Only the child copy of this descriptor is inherited, through
`exec.Cmd.ExtraFiles`; llama-server
receives `--model /dev/fd/3`. The parent's copy closes after spawn, including on
failure. Replacing the original pathname cannot redirect the child's later opens.

The b12171 loader opens metadata with `gguf_init_from_file`/`fopen("rb")`, then
reopens the path with `llama_file` for reads or `mmap` on that descriptor. macOS
`/dev/fd/3` duplicates the inherited descriptor on every open. Split GGUFs derive
sibling paths, so split filenames and `split.*` metadata (including renamed
shards) are explicitly rejected at spawn. Metadata inspection is bounded to
64 MiB; malformed or unsupported GGUF metadata fails closed. Multi-file models
and mmproj are not supported. The hub fills the cache with verified single-file
models. `--model` accepts the installed catalog ID, cache filename or contained
path. Omitting `--model` retains router mode.

Other allowed value options (documented short aliases included): `--ctx-size/-c`,
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
No arbitrary template programs, other paths, URLs or non-finite numeric values are allowed.

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
