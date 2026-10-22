#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Automated installer and updater for Personal AI Router (PAIR) on macOS.
#
# Downloads tagged release artifacts and SHA256SUMS, verifies the selected DMG,
# and installs/updates PAIR.app into /Applications. GitHub regular/latest is used
# by default, even when that release's tag contains "rc".
#
# The fork's release candidates are unsigned, unnotarized and macOS arm64 only.
# curl downloads do not set com.apple.quarantine. Neither that nor a matching
# checksum provides signing, notarization, or assurance of app/helper trust.
# SHA256SUMS detects corruption, not compromise of the release publisher.
#
# Usage:
#   # Install or update to latest release:
#   curl -fsSL https://raw.githubusercontent.com/JaredP94/Personal-AI-Router/main/scripts/install-mac.sh | bash
#
#   # Install a specific version:
#   ./scripts/install-mac.sh --version 0.7.0-rc.4
#
#   # Launch after install:
#   ./scripts/install-mac.sh --launch
#
#   # Install from a local DMG file (offline / dev testing):
#   ./scripts/install-mac.sh --dmg /path/to/NVPAIR-Setup-0.7.0-rc.4-arm64.dmg

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
  -v, --version <version>  Install a specific version (e.g. 0.7.0-rc.4 or v0.7.0-rc.4).
      --repo <owner/repo>  GitHub repository to download from (default: $PAIR_REPO).
      --dmg <path>         Install a local DMG; verify it independently first.
                          Cannot be combined with --version.
      --launch             Launch Personal AI Router after installation.
  -h, --help               Show this help message.

Defaults to GitHub's regular/latest release, even if its tag contains rc.
The fork's release candidates are unsigned, unnotarized and arm64 only.
Downloaded DMGs require SHA256SUMS. Checksums and curl's lack of quarantine
provide no signing, notarization, or assurance of app/helper trust.

Environment Variables:
  PAIR_REPO                Override GitHub repository (default: $PAIR_REPO).
USG
    exit 0
}

fail() {
    echo "Error: $*" >&2
    exit 1
}

require_value() {
    if [ -z "${2:-}" ] || [[ "$2" == -* ]]; then
        fail "$1 requires a non-empty value. Run '$0 --help' for usage."
    fi
}

valid_version() {
    # A version must remain a single safe URL path segment and asset basename.
    local version="${1#v}"
    [[ "$version" =~ ^[0-9][A-Za-z0-9._+-]*$ ]]
}

# Parse and validate user input before network access or process changes.
while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--version)
            require_value "$@"
            REQUESTED_VERSION="$2"
            shift 2
            ;;
        --repo)
            require_value "$@"
            PAIR_REPO="$2"
            shift 2
            ;;
        --dmg)
            require_value "$@"
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
            echo "Error: Unknown option: $1" >&2
            echo "Run '$(basename "$0") --help' for usage." >&2
            exit 1
            ;;
    esac
done

REPO_PATTERN='^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9_.-]+$'
if ! [[ "$PAIR_REPO" =~ $REPO_PATTERN ]] || [ "${PAIR_REPO#*/}" = "." ] || [ "${PAIR_REPO#*/}" = ".." ]; then
    fail "Invalid repository '$PAIR_REPO'; use GitHub owner/name (no URL or extra path)."
fi
if [ -n "$REQUESTED_VERSION" ] && ! valid_version "$REQUESTED_VERSION"; then
    fail "Invalid version '$REQUESTED_VERSION'; use a filename-safe version such as 0.7.0-rc.4."
fi
if [ -n "$LOCAL_DMG" ]; then
    [ -z "$REQUESTED_VERSION" ] || fail "--dmg and --version cannot be combined; choose one source."
    [ -f "$LOCAL_DMG" ] && [ -r "$LOCAL_DMG" ] || fail "Local DMG is not a readable file: $LOCAL_DMG. Check its path and permissions."
fi

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
QUIT_PID=""

finish_quit_request() {
    if [ -n "$QUIT_PID" ]; then
        # Only our own osascript child may be killed, never PAIR or its workers.
        if kill -0 "$QUIT_PID" 2>/dev/null; then
            kill -KILL "$QUIT_PID" 2>/dev/null || true
        fi
        wait "$QUIT_PID" 2>/dev/null || true
        QUIT_PID=""
    fi
}

cleanup() {
    finish_quit_request
    if [ -n "$MOUNT_POINT" ] && [ -d "$MOUNT_POINT" ]; then
        hdiutil detach "$MOUNT_POINT" -quiet 2>/dev/null || hdiutil detach "$MOUNT_POINT" -force -quiet 2>/dev/null || true
    fi
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

DMG_FILE=""

if [ -n "$LOCAL_DMG" ]; then
    DMG_FILE="$LOCAL_DMG"
    echo "Using local disk image: $DMG_FILE"
    echo "Warning: Local DMGs are not checksum-verified here; verify this file independently before installing." >&2
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
    valid_version "$TAG" || fail "Unsafe release tag '$TAG'; specify a filename-safe --version."

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

    SUMS_FILE="$TMP_DIR/SHA256SUMS"
    if ! curl -fL --progress-bar "https://github.com/$PAIR_REPO/releases/download/${TAG}/SHA256SUMS" -o "$SUMS_FILE"; then
        fail "Required SHA256SUMS could not be downloaded for $TAG; nothing has been installed."
    fi
    SUM_PATTERN='^([[:xdigit:]]{64})  ([^/[:space:]]+)$'
    SUM_COUNT=0
    EXPECTED_SUM=""
    while IFS= read -r line || [ -n "$line" ]; do
        [[ "$line" =~ $SUM_PATTERN ]] || fail "Invalid SHA256SUMS row; expected SHA256, two spaces, and a basename."
        if [ "${BASH_REMATCH[2]}" = "$DMG_NAME" ]; then
            SUM_COUNT=$((SUM_COUNT + 1))
            EXPECTED_SUM="${BASH_REMATCH[1]}"
        fi
    done < "$SUMS_FILE"
    [ "$SUM_COUNT" -eq 1 ] || fail "SHA256SUMS must contain exactly one entry for $DMG_NAME (found $SUM_COUNT)."
    # Verify only the selected basename, never arbitrary paths from the manifest.
    printf '%s  %s\n' "$EXPECTED_SUM" "$DMG_NAME" > "$TMP_DIR/selected.sha256"
    if ! (cd "$TMP_DIR" && shasum -a 256 -c selected.sha256); then
        fail "SHA256SUMS verification failed for $DMG_NAME; do not install this download."
    fi
fi

# Mount disk image
echo "Mounting disk image..."
MOUNT_POINT="$TMP_DIR/mount"
mkdir -p "$MOUNT_POINT"
if ! hdiutil attach "$DMG_FILE" -mountpoint "$MOUNT_POINT" -nobrowse -readonly -quiet; then
    fail "Could not mount $DMG_FILE; the installed app has not been stopped or replaced."
fi

# Require the exact bundle and executable before asking the installed app to quit.
APP_SRC="$MOUNT_POINT/PAIR.app"
[ -d "$APP_SRC" ] || fail "Required PAIR.app was not found at the root of $DMG_FILE."
[ -f "$APP_SRC/Contents/MacOS/PAIR" ] && [ -x "$APP_SRC/Contents/MacOS/PAIR" ] || fail "PAIR.app is missing an executable Contents/MacOS/PAIR; obtain a correctly packaged DMG."

DEST_DIR="/Applications"
DEST_APP="$DEST_DIR/PAIR.app"

app_owned_running() {
    # Anchor the executable path, not global names shared with TUI/dev services.
    # The literal dot and trailing slash exclude similarly named app bundles.
    local status
    if pgrep -f '^/Applications/PAIR[.]app/Contents/' >/dev/null; then
        return 0
    else
        status=$?
        [ "$status" -eq 1 ] || fail "Unable to check installed PAIR processes (pgrep status $status); quit PAIR and retry."
        return 1
    fi
}

if app_owned_running; then
    echo "Asking installed PAIR to quit (up to 20 seconds)..."
    osascript -e 'with timeout of 20 seconds' \
        -e 'if application "/Applications/PAIR.app" is running then' \
        -e 'tell application "/Applications/PAIR.app" to quit' \
        -e 'end if' -e 'end timeout' >/dev/null 2>&1 &
    QUIT_PID=$!
    # AppleScript timeouts alone do not bound a stuck osascript process.
    for ((elapsed=0; elapsed<20; elapsed++)); do
        if ! app_owned_running; then
            break
        fi
        sleep 1
    done
    finish_quit_request
    if app_owned_running; then
        fail "PAIR-owned processes remain after the 20-second quit window. Quit PAIR and its app-owned helpers, then retry; nothing was replaced."
    fi
fi

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
    sudo ditto "$APP_SRC" "$DEST_APP"
else
    rm -rf "$DEST_APP"
    ditto "$APP_SRC" "$DEST_APP"
fi

# ditto preserves packaged executable modes in both normal and sudo installs.
# Do not recursively mark resource files executable or hide permission failures.

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
