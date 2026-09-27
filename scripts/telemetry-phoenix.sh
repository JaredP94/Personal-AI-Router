#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
COMPOSE_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docker-compose.telemetry.yml"

case "${1:-status}" in
  up|start)
    docker compose -f "$COMPOSE_FILE" up -d
    echo "Arize Phoenix started on http://localhost:6006 (OTLP gRPC: 4317)"
    ;;
  down|stop)
    docker compose -f "$COMPOSE_FILE" stop
    echo "Arize Phoenix stopped"
    ;;
  status)
    docker compose -f "$COMPOSE_FILE" ps
    ;;
  open)
    if command -v open >/dev/null; then
      open "http://localhost:6006"
    elif command -v xdg-open >/dev/null; then
      xdg-open "http://localhost:6006"
    else
      echo "Open http://localhost:6006 in your browser"
    fi
    ;;
  *)
    echo "Usage: $0 {up|down|status|open}"
    exit 1
    ;;
esac
