#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Automated installer and updater for Personal AI Router (PAIR) on macOS.
#
# Downloads tagged release artifacts via curl (which does not set the
# com.apple.quarantine attribute), installs/updates PAIR.app into /Applications,
# and eliminates the need for manual "xattr -cr" on unsigned builds.
#
# Usage:
#   # Install or update to latest release:
#   curl -fsSL https://raw.githubusercontent.com/JaredP94/Personal-AI-Router/main/scripts/install-mac.sh | bash
#
#   # Install a specific version:
#   ./scripts/install-mac.sh --version 0.3.4
#
#   # Launch after install:
#   ./scripts/install-mac.sh --launch
#
#   # Install from a local DMG file (offline / dev testing):
#   ./scripts/install-mac.sh --dmg /path/to/NVPAIR-Setup-0.3.4-arm64.dmg

set -euo pipefail

DEFAULT_REPO="JaredP94/Personal-AI-Router"
PAIR_REPO="${PAIR_REPO:-$DEFAULT_REPO}"
REQUESTED_VERSION=""
LOCAL_DMG=""
AUTO_LAUNCH=0

usage() {
    cat <<USG
Personal AI Router (PAIR) macOS Installer / Updater

Usage:
  $(basename "$0") [options]

Options:
  -v, --version <version>  Install a specific version (e.g. 0.3.4 or v0.3.4). Defaults to latest.
      --repo <owner/repo>  GitHub repository to download from (default: $PAIR_REPO).
      --dmg <path>         Install from a local .dmg file instead of downloading.
      --launch             Launch Personal AI Router after installation.
  -h, --help               Show this help message.

Environment Variables:
  PAIR_REPO                Override GitHub repository (default: $PAIR_REPO).
USG
    exit 0
}

# Parse CLI flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--version)
            REQUESTED_VERSION="$2"
            shift 2
            ;;
        --repo)
            PAIR_REPO="$2"
            shift 2
            ;;
        --dmg)
            LOCAL_DMG="$2"
            shift 2
            ;;
        --launch)
            AUTO_LAUNCH=1
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "Unknown option: $1" >&2
            echo "Run '$(basename "$0") --help' for usage." >&2
            exit 1
            ;;
    esac
done

# Verify macOS host
OS_TYPE="$(uname -s)"
if [ "$OS_TYPE" != "Darwin" ]; then
    echo "Error: This script is intended for macOS only (detected: $OS_TYPE)." >&2
    exit 1
fi

# Detect architecture
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
    arm64|aarch64) ARCH="arm64" ;;
    x86_64|amd64)  ARCH="x64" ;;
    *)
        echo "Error: Unsupported architecture '$ARCH_RAW'. PAIR requires arm64 or x64." >&2
        exit 1
        ;;
esac

TMP_DIR="$(mktemp -d /tmp/nvpair-install.XXXXXX)"
MOUNT_POINT=""

cleanup() {
    if [ -n "$MOUNT_POINT" ] && [ -d "$MOUNT_POINT" ]; then
        hdiutil detach "$MOUNT_POINT" -quiet 2>/dev/null || hdiutil detach "$MOUNT_POINT" -force -quiet 2>/dev/null || true
    fi
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

DMG_FILE=""

if [ -n "$LOCAL_DMG" ]; then
    if [ ! -f "$LOCAL_DMG" ]; then
        echo "Error: Specified DMG file does not exist: $LOCAL_DMG" >&2
        exit 1
    fi
    DMG_FILE="$LOCAL_DMG"
    echo "Using local disk image: $DMG_FILE"
else
    if [ -n "$REQUESTED_VERSION" ]; then
        TAG="v${REQUESTED_VERSION#v}"
        VERSION="${TAG#v}"
    else
        echo "Querying latest release from $PAIR_REPO..."
        TAG=""
        # Try GitHub REST API
        API_RESP=$(curl -s "https://api.github.com/repos/$PAIR_REPO/releases/latest" 2>/dev/null || true)
        if [ -n "$API_RESP" ]; then
            TAG=$(echo "$API_RESP" | grep -m1 '"tag_name":' | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' || true)
        fi

        # Fallback to redirect location header if API is rate-limited or unauthenticated
        if [ -z "$TAG" ] || [ "$TAG" = "null" ]; then
            REDIRECT_URL=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$PAIR_REPO/releases/latest" 2>/dev/null || true)
            TAG=$(echo "$REDIRECT_URL" | awk -F'/' '{print $NF}')
        fi

        if [ -z "$TAG" ] || [ "$TAG" = "null" ] || [ "$TAG" = "latest" ]; then
            echo "Error: Unable to resolve latest release for $PAIR_REPO." >&2
            echo "Please specify a version explicitly: $0 --version <version>" >&2
            exit 1
        fi
        VERSION="${TAG#v}"
    fi

    DMG_NAME="NVPAIR-Setup-${VERSION}-${ARCH}.dmg"
    DOWNLOAD_URL="https://github.com/$PAIR_REPO/releases/download/${TAG}/${DMG_NAME}"

    echo "Target release: $TAG ($ARCH)"
    echo "Downloading $DMG_NAME..."

    DMG_FILE="$TMP_DIR/$DMG_NAME"
    if ! curl -fL --progress-bar "$DOWNLOAD_URL" -o "$DMG_FILE"; then
        echo "Error: Failed to download $DOWNLOAD_URL" >&2
        echo "Please verify that release $TAG exists and contains $DMG_NAME." >&2
        exit 1
    fi
fi

# Stop any running Personal AI Router instances and helper processes
echo "Stopping any active Personal AI Router processes..."
for proc in \
    "PAIR" \
    "nvpair-tui" \
    "ollama-proxy" \
    "lmstudio-proxy" \
    "omlx-proxy" \
    "nvpair-node-info" \
    "nvpair-node-scanner" \
    "nvpair-manual-nodes" \
    "nvpair-node-settings" \
    "nvpair-engine-manager" \
    "nvpair-workload-manager" \
    "nvpair-cluster-manager" \
    "nvpair-job-scheduler" \
    "nvpair-errors" \
    "nvpair-ui-broker"; do
    pkill -TERM -x "$proc" 2>/dev/null || true
done
sleep 1

# Mount disk image
echo "Mounting disk image..."
MOUNT_POINT="$TMP_DIR/mount"
mkdir -p "$MOUNT_POINT"
hdiutil attach "$DMG_FILE" -mountpoint "$MOUNT_POINT" -nobrowse -readonly -quiet

# Locate .app bundle inside DMG
APP_SRC="$MOUNT_POINT/PAIR.app"
if [ ! -d "$APP_SRC" ]; then
    APP_SRC="$(find "$MOUNT_POINT" -maxdepth 1 -name "*.app" | head -n1)"
fi

if [ -z "$APP_SRC" ] || [ ! -d "$APP_SRC" ]; then
    echo "Error: Could not find application bundle in $DMG_FILE" >&2
    exit 1
fi

DEST_DIR="/Applications"
DEST_APP="$DEST_DIR/PAIR.app"

# Check write permissions for /Applications
NEED_SUDO=0
if [ -d "$DEST_APP" ]; then
    if [ ! -w "$DEST_APP" ]; then
        NEED_SUDO=1
    fi
fi
if [ ! -w "$DEST_DIR" ]; then
    NEED_SUDO=1
fi

echo "Installing to $DEST_APP..."
if [ "$NEED_SUDO" -eq 1 ]; then
    echo "Elevated permissions required to write to $DEST_DIR. Prompting for sudo..."
    sudo rm -rf "$DEST_APP"
    sudo cp -R "$APP_SRC" "$DEST_DIR/"
else
    rm -rf "$DEST_APP"
    cp -R "$APP_SRC" "$DEST_DIR/"
fi

# Ensure executable bits on Go workers and helper executables
if [ -d "$DEST_APP/Contents/Resources/cli-bin" ]; then
    chmod -R +x "$DEST_APP/Contents/Resources/cli-bin" 2>/dev/null || true
fi
if [ -d "$DEST_APP/Contents/MacOS" ]; then
    chmod -R +x "$DEST_APP/Contents/MacOS" 2>/dev/null || true
fi

# Unmount disk image
hdiutil detach "$MOUNT_POINT" -quiet || hdiutil detach "$MOUNT_POINT" -force -quiet || true
MOUNT_POINT=""

echo ""
echo "================================================================"
echo " Personal AI Router installed successfully!"
echo " Location: $DEST_APP"
echo "================================================================"
echo ""

if [ "$AUTO_LAUNCH" -eq 1 ]; then
    echo "Launching Personal AI Router..."
    open "$DEST_APP"
else
    echo "You can launch the app from Applications or run:"
    echo "  open /Applications/PAIR.app"
fi
