#!/usr/bin/env bash
# Builds the S3 host and the WASM guest modules (pure Go, no cgo).
#   cmd/s3-wasm/build.sh               # -> cmd/s3-wasm/bin/{s3-wasm,memgrowth,modules/v1.wasm,modules/v2.wasm}
#   OUT=/tmp/s3 cmd/s3-wasm/build.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${OUT:-cmd/s3-wasm/bin}
FLAGS=${FLAGS:-}
mkdir -p "$OUT/modules"
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/s3-wasm" ./cmd/s3-wasm
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/memgrowth" ./cmd/s3-wasm/memgrowth
for v in v1 v2; do
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o "$OUT/modules/$v.wasm" ./cmd/s3-wasm/guest/$v
done
ls -la "$OUT" "$OUT/modules"
