# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Example pre-sample hook: sequential OpenAI-compatible streaming requests.

No response text or request content is logged. Does not start an engine.
Run by absolute script path because hook cwd is the isolated home.
"""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import signal
from statistics import median
import threading
import time
import urllib.error
import urllib.request


PROMPT = "Explain briefly why leaves are green."


def stream_request(base_url, model, max_tokens, timeout):
    request = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions",
        data=json.dumps(dict(model=model, stream=True, max_tokens=max_tokens,
                             messages=[dict(role="user", content=PROMPT)])).encode("utf-8"),
        headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    started = time.monotonic()
    result = dict(timestamp=datetime.now(timezone.utc).isoformat(), status="failed")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            for line in response:
                if line.strip() == b"data: [DONE]":
                    result["status"] = "ok"
                    break
            else:
                result["error"] = "StreamEndedWithoutDONE"
    except (OSError, urllib.error.URLError, ValueError) as error:
        # Exception text can contain response bodies or credentials: type only.
        result["error"] = type(error).__name__
    result["latency_seconds"] = time.monotonic() - started
    return result


def run_load(output, base_url, model, max_tokens, timeout, duration=3600, delay=1,
             max_requests=None, stop=None):
    stop = stop if stop is not None else threading.Event()
    output.mkdir(parents=True, exist_ok=True)
    deadline = time.monotonic() + duration
    requests, succeeded, latencies = 0, 0, []
    with (output / "requests.jsonl").open("w", encoding="utf-8") as stream:
        while not stop.is_set() and time.monotonic() < deadline:
            result = stream_request(base_url, model, max_tokens, timeout)
            requests += 1
            succeeded += result["status"] == "ok"
            latencies.append(result["latency_seconds"])
            stream.write(json.dumps(dict(result, request=requests)) + "\n")
            stream.flush()
            if max_requests is not None and requests >= max_requests:
                break
            stop.wait(delay)
    summary = dict(requests=requests, succeeded=succeeded, failed=requests - succeeded,
                   model=model, max_tokens=max_tokens, concurrency=1, delay_seconds=delay,
                   latency_seconds=dict(median=median(latencies) if latencies else None,
                                        min=min(latencies, default=None),
                                        max=max(latencies, default=None)),
                   latency_note="End-to-end streaming latency, including failed requests; not token throughput.")
    (output / "inference-summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True, help="OpenAI API root, including /v1 if needed")
    parser.add_argument("--model", required=True)
    parser.add_argument("--output", type=Path, default=os.environ.get("PAIR_PERF_OUTPUT"))
    parser.add_argument("--max-tokens", type=int, default=64)
    parser.add_argument("--timeout", type=float, default=15)
    parser.add_argument("--duration", type=float, default=3600)
    parser.add_argument("--delay", type=float, default=1)
    args = parser.parse_args()
    if not args.output or min(args.max_tokens, args.timeout, args.duration) <= 0 or args.delay < 0:
        parser.error("output and positive max-tokens/timeout/duration required; delay must be nonnegative")
    stop = threading.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, lambda *_: stop.set())
    summary = run_load(args.output, args.base_url, args.model, args.max_tokens,
                       args.timeout, args.duration, args.delay, stop=stop)
    return 0 if summary["succeeded"] > 0 and summary["failed"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
