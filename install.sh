#!/bin/sh
# One-line installer for the omnibuf CLI (https://github.com/omnibuf/omnibuf-cli)
# Usage:
#   curl -fsSL https://omnibuf.com/install.sh | sh
#   curl -fsSL https://omnibuf.com/install.sh | VERSION=v0.1.0 BIN_DIR=/usr/local/bin sh
set -eu

REPO="omnibuf/omnibuf-cli"
BIN_NAME="omnibuf"

detect_os() {
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$os" in
    darwin) echo "darwin" ;;
    linux) echo "linux" ;;
    *)
      echo "error: unsupported OS '$os' (use Windows zip from https://github.com/${REPO}/releases)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "error: unsupported architecture '$arch'" >&2
      exit 1
      ;;
  esac
}

resolve_version() {
  if [ -n "${VERSION:-}" ]; then
    echo "$VERSION"
    return
  fi
  latest="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  if [ -z "$latest" ]; then
    echo "error: failed to resolve latest release tag from GitHub" >&2
    exit 1
  fi
  echo "$latest"
}

verify_sha256() {
  file="$1"
  expected="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"
  else
    return 0
  fi
  if [ "$actual" != "$expected" ]; then
    echo "error: SHA-256 mismatch for $file (expected $expected, got $actual)" >&2
    exit 1
  fi
}

main() {
  os="$(detect_os)"
  arch="$(detect_arch)"
  version="$(resolve_version)"
  archive="${BIN_NAME}-${os}-${arch}.tar.gz"
  base_url="https://github.com/${REPO}/releases/download/${version}"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT INT TERM

  echo "Downloading ${BIN_NAME} ${version} (${os}/${arch})..."
  curl -fsSL "${base_url}/${archive}" -o "${tmpdir}/${archive}"
  curl -fsSL "${base_url}/checksums.txt" -o "${tmpdir}/checksums.txt"

  expected_sha="$(grep "  ${archive}\$" "${tmpdir}/checksums.txt" | awk '{print $1}')"
  if [ -n "$expected_sha" ]; then
    verify_sha256 "${tmpdir}/${archive}" "$expected_sha"
  fi

  tar -xzf "${tmpdir}/${archive}" -C "${tmpdir}"
  bin_path="$(find "${tmpdir}" -name "${BIN_NAME}" -type f | head -n 1)"
  if [ -z "$bin_path" ]; then
    echo "error: ${BIN_NAME} binary not found in archive ${archive}" >&2
    exit 1
  fi

  bin_dir="${BIN_DIR:-}"
  if [ -z "$bin_dir" ]; then
    if [ -w "/usr/local/bin" ]; then
      bin_dir="/usr/local/bin"
    else
      bin_dir="${HOME}/.local/bin"
      mkdir -p "$bin_dir"
    fi
  fi

  install -m 0755 "$bin_path" "${bin_dir}/${BIN_NAME}"
  echo "Installed ${BIN_NAME} to ${bin_dir}/${BIN_NAME}"
  case ":${PATH}:" in
    *":${bin_dir}:"*) ;;
    *) echo "note: add ${bin_dir} to your PATH to invoke '${BIN_NAME}' directly." ;;
  esac
}

main "$@"
