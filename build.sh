#!/usr/bin/env bash
# Build the sidecar for each target (darwin/amd64 is for an x86_64 dev
# daemon on a Mac) and assemble the installable package
# under dist/. Install: Hearthloom dashboard → ☰ → Integrations → embers
# → "install one you built yourself" → this dist/ folder (or zip it).
set -euo pipefail
cd "$(dirname "$0")"

VERSION=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' hearthember.json | head -1)
LDFLAGS="-s -w -X main.Version=${VERSION}"

rm -rf dist
mkdir -p dist/bin
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
  os=${target%/*}; arch=${target#*/}
  echo "building ${os}/${arch}…"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/bin/htesla-${os}-${arch}" ./cmd/htesla
done
cp hearthember.json dist/
cp -R ui dist/ui
mkdir -p dist/licenses
cp LICENSE dist/licenses/htesla-LICENSE
cp third_party/fleettelemetry/LICENSE dist/licenses/fleet-telemetry-LICENSE
cp third_party/fleettelemetry/NOTICE dist/licenses/fleet-telemetry-NOTICE

echo "package ready: $(pwd)/dist (htesla ${VERSION})"
