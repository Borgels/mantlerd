#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
VERSION="${1:-}"

if [ -z "$VERSION" ]; then
  VERSION="dev"
fi

mkdir -p "$DIST_DIR"
rm -f "${DIST_DIR}/mantlerd-"*

build_target() {
  os="$1"
  arch="$2"
  out="${DIST_DIR}/mantlerd-${os}-${arch}"
  echo "Building ${out}"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
    go build -buildvcs=false -trimpath -ldflags "-s -w -X main.agentVersion=${VERSION}" \
    -o "$out" "${ROOT_DIR}/cmd/mantler"

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$out" | awk '{print $1}' > "${out}.sha256"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$out" | awk '{print $1}' > "${out}.sha256"
  else
    openssl dgst -sha256 "$out" | awk '{print $NF}' > "${out}.sha256"
  fi
}

build_target linux amd64
build_target linux arm64
build_target darwin amd64
build_target darwin arm64

# Combined checksums file with bare filenames (fallback for installers that can't find per-binary .sha256 files).
CHECKSUMS_FILE="${DIST_DIR}/checksums.txt"
rm -f "$CHECKSUMS_FILE"
for f in "${DIST_DIR}/mantlerd-"*; do
  case "$f" in *.sha256) continue ;; esac
  hash="$(cat "${f}.sha256")"
  printf '%s  %s\n' "$hash" "$(basename "$f")" >> "$CHECKSUMS_FILE"
done
