#!/usr/bin/env bash
#
# cert-tool installer.
#
# One-line install:
#
#   curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/install.sh | sudo bash
#
# Options (pass after "bash -s --" when piping):
#
#   --version <tag>        Install a specific release, e.g. v1.2.3 (default: latest)
#   --install-dir <dir>    Installation directory (default: /usr/local/bin)
#   -h, --help             Show this help
#
# Environment overrides:
#
#   CERT_TOOL_REPO         GitHub repository (default: Rson9/cert-tool)
#   CERT_TOOL_BASE_URL     Release download base URL (default: https://github.com/<repo>/releases)
#   CERT_TOOL_VERSION      Same as --version
#   CERT_TOOL_INSTALL_DIR  Same as --install-dir

set -euo pipefail

REPO="${CERT_TOOL_REPO:-Rson9/cert-tool}"
BASE_URL="${CERT_TOOL_BASE_URL:-https://github.com/${REPO}/releases}"
INSTALL_DIR="${CERT_TOOL_INSTALL_DIR:-/usr/local/bin}"
VERSION="${CERT_TOOL_VERSION:-latest}"
BIN_NAME="cert"

info() { printf '  %s\n' "$*"; }
die()  { printf 'Error: %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
}

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
    case "$1" in
        --version)      VERSION="$2"; shift 2 ;;
        --version=*)    VERSION="${1#*=}"; shift ;;
        --install-dir)  INSTALL_DIR="$2"; shift 2 ;;
        --install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
        -h|--help)      usage; exit 0 ;;
        *)              die "unknown option: $1 (use --help)" ;;
    esac
done

# ---------------------------------------------------------------------------
# Detect platform
# ---------------------------------------------------------------------------
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
    linux|darwin) ;;
    *) die "unsupported operating system: $OS (supported: linux, darwin)" ;;
esac

case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $ARCH (supported: amd64, arm64)" ;;
esac

ASSET="${BIN_NAME}-${OS}-${ARCH}"

if [ "$VERSION" = "latest" ]; then
    URL="${BASE_URL}/latest/download/${ASSET}"
else
    URL="${BASE_URL}/download/${VERSION}/${ASSET}"
fi
SUM_URL="${URL%/*}/checksums.txt"

echo "cert-tool installer"
info "repository : ${REPO}"
info "version    : ${VERSION}"
info "platform   : ${OS}/${ARCH}"
info "asset      : ${ASSET}"
info "target     : ${INSTALL_DIR}/${BIN_NAME}"
echo

# ---------------------------------------------------------------------------
# Download
# ---------------------------------------------------------------------------
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

download() {
    # download <url> <output>
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "neither curl nor wget is available"
    fi
}

info "Downloading ${URL}"
download "$URL" "${TMP_DIR}/${ASSET}" || die "download failed: ${URL}

If this is not a GitHub release, set CERT_TOOL_BASE_URL to your mirror, e.g.:
  curl -fsSL .../install.sh | sudo CERT_TOOL_BASE_URL=https://your.mirror/releases bash"

# ---------------------------------------------------------------------------
# Verify checksum (best effort, when checksums.txt is published)
# ---------------------------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
    SHASUM="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    SHASUM="shasum -a 256"
else
    SHASUM=""
fi

if [ -n "$SHASUM" ] && download "$SUM_URL" "${TMP_DIR}/checksums.txt" 2>/dev/null; then
    expected="$(grep -E "[[:space:]]${ASSET}\$" "${TMP_DIR}/checksums.txt" | awk '{print $1}' | head -n1 || true)"
    if [ -n "$expected" ]; then
        actual="$($SHASUM "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
        if [ "$expected" != "$actual" ]; then
            die "checksum mismatch for ${ASSET}
  expected: ${expected}
  actual  : ${actual}"
        fi
        info "Checksum verified"
    else
        info "No checksum entry for ${ASSET}, skipping verification"
    fi
else
    info "Checksum file unavailable, skipping verification"
fi

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
# Escalate privileges only when the target directory actually requires it.
needs_privileges() {
    local dir="$INSTALL_DIR"
    while [ ! -d "$dir" ]; do dir="$(dirname "$dir")"; done
    [ ! -w "$dir" ]
}

run_privileged() {
    if [ "$(id -u)" -eq 0 ] || ! needs_privileges; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        die "root privileges are required to install into ${INSTALL_DIR} (install sudo or re-run as root)"
    fi
}

chmod 755 "${TMP_DIR}/${ASSET}"
"${TMP_DIR}/${ASSET}" version >/dev/null 2>&1 || die "the downloaded binary does not run on this platform"

run_privileged install -d "$INSTALL_DIR"
run_privileged install -m 755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/${BIN_NAME}"

echo
echo "✅ cert installed: ${INSTALL_DIR}/${BIN_NAME}"
"${INSTALL_DIR}/${BIN_NAME}" version 2>/dev/null || true
echo
echo "Next steps:"
echo "  cert check                                        # verify dependencies"
echo "  sudo cert init -d example.com -e you@example.com  # initialize this server"
echo "  sudo cert add api                                 # issue a certificate and proxy"
