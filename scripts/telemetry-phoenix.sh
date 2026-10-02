#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
COMPOSE_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docker-compose.telemetry.yml"

PROJECT_ARGS=()
EXISTING_PROJECT="$(docker inspect nvpair-phoenix --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null || true)"
if [ -n "$EXISTING_PROJECT" ]; then
  PROJECT_ARGS=(-p "$EXISTING_PROJECT")
fi

case "${1:-status}" in
  up|start)
    docker compose "${PROJECT_ARGS[@]}" -f "$COMPOSE_FILE" up -d
    echo "Arize Phoenix started on http://localhost:6006 (OTLP gRPC: 4317)"
    ;;
  down|stop)
    docker compose "${PROJECT_ARGS[@]}" -f "$COMPOSE_FILE" stop
    echo "Arize Phoenix stopped"
    ;;
  update|pull)
    echo "Pulling latest Arize Phoenix image..."
    docker compose "${PROJECT_ARGS[@]}" -f "$COMPOSE_FILE" pull
    docker compose "${PROJECT_ARGS[@]}" -f "$COMPOSE_FILE" up -d
    echo "Arize Phoenix updated and started on http://localhost:6006 (OTLP gRPC: 4317)"
    ;;
  status)
    docker compose "${PROJECT_ARGS[@]}" -f "$COMPOSE_FILE" ps
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
    echo "Usage: $0 {up|down|update|status|open}"
    exit 1
    ;;
esac

