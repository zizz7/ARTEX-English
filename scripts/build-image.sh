#!/usr/bin/env bash
# Build the ARTEX Docker image from THIS repository (the published autumn27/artex image is the
# upstream Chinese build, not this fork).
#
#   scripts/build-image.sh          # slim image  -> artex:local (Dockerfile)
#   scripts/build-image.sh kali     # full toolset -> artex:kali (Dockerfile.kali)
#
# Compiles the web UI and the Go binary on the host when Node 20+ and Go 1.26+ are installed,
# otherwise inside the official node/golang containers, so Docker alone is enough.
set -euo pipefail

cd "$(dirname "$0")/.."
flavor="${1:-slim}"
case "$flavor" in
  slim) dockerfile=Dockerfile;      tag="${ARTEX_IMAGE:-artex:local}" ;;
  kali) dockerfile=Dockerfile.kali; tag="artex:kali" ;;
  *) echo "usage: $0 [slim|kali]" >&2; exit 2 ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

info(){ printf '\033[36m==>\033[0m %s\n' "$*"; }
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }

info "Building web UI"
if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  (cd web && npm ci && NEXT_EXPORT=1 npx next build)
else
  docker run --rm -v "$PWD/web:/web" -w /web node:20-bookworm \
    sh -c 'npm ci && NEXT_EXPORT=1 npx next build'
fi
rm -rf server/webui/dist && cp -r web/out server/webui/dist

info "Building linux/${arch} binary"
version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
mkdir -p "dist/${arch}"
if command -v go >/dev/null 2>&1; then
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -tags embedui \
    -ldflags "-s -w -X main.version=${version}" -o "dist/${arch}/artex" ./cmd/artex
else
  docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$arch" golang:1.26-bookworm \
    go build -tags embedui -ldflags "-s -w -X main.version=${version}" -o "dist/${arch}/artex" ./cmd/artex
fi

info "Building image ${tag} from ${dockerfile}"
docker build -f "$dockerfile" -t "$tag" .
info "Done: ${tag}"
