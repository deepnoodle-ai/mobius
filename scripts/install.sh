#!/bin/sh
#
# Mobius CLI installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/deepnoodle-ai/mobius/main/scripts/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/deepnoodle-ai/mobius/main/scripts/install.sh | sh -s -- --version 0.1.0
#
# Installs to ~/.mobius/bin/mobius (no sudo required).
#
set -e

REPO="deepnoodle-ai/mobius"
RELEASES_URL="https://github.com/${REPO}/releases/download"
RELEASES_API_URL="https://api.github.com/repos/${REPO}/releases?per_page=30"
INSTALL_DIR="${HOME}/.mobius/bin"
INSTALL_VERSION=""

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            if [ -z "${2:-}" ] || case "$2" in -*) true;; *) false;; esac; then
                printf "Error: --version requires a value (e.g. --version 0.1.0)\n" >&2
                exit 1
            fi
            INSTALL_VERSION="$2"
            shift 2
            ;;
        *)
            printf "Error: unknown flag: %s\n" "$1" >&2
            exit 1
            ;;
    esac
done

info()  { printf "  %s\n" "$*"; }
error() { printf "Error: %s\n" "$*" >&2; exit 1; }

detect_downloader() {
    if command -v curl >/dev/null 2>&1; then
        DOWNLOADER="curl"
    elif command -v wget >/dev/null 2>&1; then
        DOWNLOADER="wget"
    else
        error "curl or wget is required but neither was found."
    fi
}

download() {
    url="$1"
    dest="$2"
    if [ "$DOWNLOADER" = "curl" ]; then
        curl -fsSL "$url" -o "$dest"
    else
        wget -qO "$dest" "$url"
    fi
}

download_stdout() {
    url="$1"
    if [ "$DOWNLOADER" = "curl" ]; then
        curl -fsSL "$url"
    else
        wget -qO- "$url"
    fi
}

detect_platform() {
    OS=$(uname -s)
    case "$OS" in
        Darwin) OS="darwin" ;;
        Linux)  OS="linux" ;;
        *)      error "Unsupported operating system: $OS. Mobius CLI supports macOS and Linux." ;;
    esac

    ARCH=$(uname -m)
    case "$ARCH" in
        arm64|aarch64) ARCH="arm64" ;;
        x86_64|amd64)  ARCH="amd64" ;;
        *)             error "Unsupported architecture: $ARCH. Mobius CLI supports arm64 and x86_64." ;;
    esac

    PLATFORM="${OS}-${ARCH}"
}

detect_profile() {
    SHELL_NAME=$(basename "${SHELL:-/bin/sh}")
    case "$SHELL_NAME" in
        zsh)  PROFILE_FILE="$HOME/.zshrc" ;;
        bash)
            if [ -f "$HOME/.bash_profile" ] || [ "$(uname -s)" = "Darwin" ]; then
                PROFILE_FILE="$HOME/.bash_profile"
            else
                PROFILE_FILE="$HOME/.bashrc"
            fi
            ;;
        *) PROFILE_FILE="$HOME/.profile" ;;
    esac
}

expected_checksum() {
    checksums="$1"
    file="$2"
    result=$(printf '%s\n' "$checksums" | awk -v target="$file" '$2 == target {print $1}')
    if [ -z "$result" ]; then
        error "No checksum found for ${file}."
    fi
    printf '%s' "$result"
}

verify_checksum() {
    file="$1"
    expected="$2"

    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$file" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(LC_ALL=C shasum -a 256 "$file" | awk '{print $1}')
    else
        error "sha256sum or shasum is required to verify the download."
    fi

    if [ "$actual" != "$expected" ]; then
        error "Checksum verification failed.\n  Expected: $expected\n  Got:      $actual"
    fi
}

resolve_version() {
    if [ -n "$INSTALL_VERSION" ]; then
        VERSION="$INSTALL_VERSION"
        return
    fi

    releases_json=$(download_stdout "$RELEASES_API_URL") || error "Failed to fetch Mobius release metadata."

    if command -v jq >/dev/null 2>&1; then
        tag=$(printf '%s' "$releases_json" | jq -r '
            [.[] | select(.draft | not) | select(.tag_name | test("^v[0-9]+(\\.[0-9]+)*$"))][0].tag_name // empty
        ')
    else
        tag=$(
            printf '%s' "$releases_json" |
            grep -o "\"tag_name\"[[:space:]]*:[[:space:]]*\"v[^\"]*\"" |
            sed 's/.*:[[:space:]]*"//;s/"$//' |
            while IFS= read -r candidate; do
                if [ -z "$candidate" ]; then
                    continue
                fi
                printf '%s\n' "$candidate" | grep -Eq '^v[0-9]+(\.[0-9]+)*$' || continue
                printf '%s\n' "$candidate"
                break
            done
        )
    fi

    if [ -z "$tag" ] || [ "$tag" = "null" ]; then
        error "Could not determine the latest stable Mobius CLI version."
    fi

    VERSION="${tag#v}"
}

main() {
    echo "Installing Mobius CLI..."

    detect_downloader
    detect_platform
    resolve_version

    info "Platform: ${PLATFORM}"
    info "Version: ${VERSION}"

    TAG="v${VERSION}"
    BINARY_NAME="mobius-${PLATFORM}"
    BINARY_URL="${RELEASES_URL}/${TAG}/${BINARY_NAME}"
    CHECKSUMS_URL="${RELEASES_URL}/${TAG}/checksums.txt"

    INSTALL_PATH="${INSTALL_DIR}/mobius"
    if [ -x "$INSTALL_PATH" ]; then
        CURRENT_VERSION=$("$INSTALL_PATH" --version 2>/dev/null | head -1 || echo "unknown")
        info "Current version: ${CURRENT_VERSION}"
        info "Upgrading to: ${VERSION}"
    fi

    TMP_DIR=$(mktemp -d)
    TMP_BIN="${TMP_DIR}/mobius"
    trap 'rm -rf "$TMP_DIR"' EXIT

    printf "  Downloading binary... "
    download "$BINARY_URL" "$TMP_BIN"
    echo "done"

    printf "  Downloading checksums... "
    CHECKSUMS=$(download_stdout "$CHECKSUMS_URL") || error "Failed to fetch checksums from ${CHECKSUMS_URL}"
    echo "done"

    EXPECTED_SHA=$(expected_checksum "$CHECKSUMS" "$BINARY_NAME")

    printf "  Verifying checksum... "
    verify_checksum "$TMP_BIN" "$EXPECTED_SHA"
    echo "ok"

    mkdir -p "$INSTALL_DIR"

    printf "  Installing to %s... " "$INSTALL_PATH"
    chmod +x "$TMP_BIN"
    mv "$TMP_BIN" "$INSTALL_PATH"
    echo "done"

    echo ""
    echo "Mobius CLI v${VERSION} installed successfully!"

    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            detect_profile
            echo ""
            echo "Add Mobius to your PATH by running:"
            echo ""
            echo "  echo 'export PATH=\"\$HOME/.mobius/bin:\$PATH\"' >> ${PROFILE_FILE}"
            echo "  source ${PROFILE_FILE}"
            echo ""
            ;;
    esac

    echo "Run 'mobius --help' to get started."
}

main
