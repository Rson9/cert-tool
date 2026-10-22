#!/usr/bin/env bash
#
# cert-tool uninstaller — restores the server to its pre-installation state.
#
# One-line uninstall:
#
#   curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/uninstall.sh | sudo bash
#
# Options (pass after "bash -s --" when piping):
#
#   --keep-sites       Keep nginx site configs and certificates (only remove the tool)
#   --keep-packages    Keep packages that 'cert init' installed
#   --dry-run          Show the uninstall plan without changing anything
#   -h, --help         Show this help
#
# The actual work is done by "cert uninstall" so there is a single source of
# truth. If the binary is missing, a matching release binary is downloaded to
# a temporary directory just for this run.
#
# Environment overrides: CERT_TOOL_REPO, CERT_TOOL_BASE_URL, CERT_TOOL_VERSION.

set -euo pipefail

REPO="${CERT_TOOL_REPO:-Rson9/cert-tool}"
BASE_URL="${CERT_TOOL_BASE_URL:-https://github.com/${REPO}/releases}"
VERSION="${CERT_TOOL_VERSION:-latest}"

info() { printf '  %s\n' "$*"; }
die()  { printf 'Error: %s\n' "$*" >&2; exit 1; }

usage() { sed -n '3,17p' "$0" | sed 's/^# \{0,1\}//'; }

case "${1:-}" in
    -h|--help) usage; exit 0 ;;
esac

echo "cert-tool uninstaller"

# ---------------------------------------------------------------------------
# Locate the installed binary (or download one to run the uninstall logic)
# ---------------------------------------------------------------------------
CERT_BIN=""
for candidate in "$(command -v cert 2>/dev/null || true)" /usr/local/bin/cert; do
    if [ -n "$candidate" ] && [ -x "$candidate" ]; then
        CERT_BIN="$candidate"
        break
    fi
done

TMP_DIR=""
cleanup() { [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR"; }
trap cleanup EXIT

if [ -z "$CERT_BIN" ]; then
    info "cert binary not found — downloading one to run the uninstall logic"

    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "unsupported architecture: $ARCH" ;;
    esac
    case "$OS" in
        linux|darwin) ;;
        *) die "unsupported operating system: $OS" ;;
    esac

    ASSET="cert-${OS}-${ARCH}"
    if [ "$VERSION" = "latest" ]; then
        URL="${BASE_URL}/latest/download/${ASSET}"
    else
        URL="${BASE_URL}/download/${VERSION}/${ASSET}"
    fi

    TMP_DIR="$(mktemp -d)"
    info "Downloading ${URL}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "${TMP_DIR}/${ASSET}" "$URL" || die "download failed: ${URL}"
    else
        wget -q -O "${TMP_DIR}/${ASSET}" "$URL" || die "download failed: ${URL}"
    fi
    chmod 755 "${TMP_DIR}/${ASSET}"
    CERT_BIN="${TMP_DIR}/${ASSET}"
fi

info "Using ${CERT_BIN}"
echo

# ---------------------------------------------------------------------------
# Run the real uninstall (it shows a plan and removes itself at the end)
# ---------------------------------------------------------------------------
exec "$CERT_BIN" uninstall --yes "$@"
