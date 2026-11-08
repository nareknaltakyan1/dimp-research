#!/usr/bin/env bash
# Builds S7: host, CLI tools, guest modules, a DEV signing key and pre-staged
# signed bundles (for the contract harness /admin/upgrade?to=vN).
#   cmd/s7-inplace/build.sh          # -> cmd/s7-inplace/bin/{s7-inplace,s7-patchctl,s7-load,modules/,bundles/,keys/}
#   OUT=/tmp/s7 cmd/s7-inplace/build.sh
# The dev private key stays in $OUT/dev-signing.key and is NOT trusted by
# the Kubernetes deployment (which mounts its own public key).
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${OUT:-cmd/s7-inplace/bin}
mkdir -p "$OUT/modules" "$OUT/bundles" "$OUT/keys"
go build -o "$OUT/s7-inplace" ./cmd/s7-inplace
go build -o "$OUT/s7-patchctl" ./cmd/s7-patchctl
go build -o "$OUT/s7-load" ./cmd/s7-load
for v in v1 v2 v3; do
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -ldflags="-s -w" -o "$OUT/modules/$v.wasm" ./cmd/s7-inplace/guest/$v
done
if [ ! -f "$OUT/dev-signing.key" ]; then
  "$OUT/s7-patchctl" keygen -out "$OUT/dev-signing" >/dev/null
fi
cp "$OUT/dev-signing.pub" "$OUT/keys/dev.pub"
"$OUT/s7-patchctl" sign -key "$OUT/dev-signing.key" -module "$OUT/modules/v2.wasm" -version v2 -base v1 -out "$OUT/bundles/v2" >/dev/null
"$OUT/s7-patchctl" sign -key "$OUT/dev-signing.key" -module "$OUT/modules/v3.wasm" -version v3 -base v2 -out "$OUT/bundles/v3" >/dev/null
ls -la "$OUT" "$OUT/modules" "$OUT/bundles"/*
