#!/usr/bin/env bash
# Builds S6 (hotpatch-ready compilation): the host, the patch plugins, keys
# and signed manifests. Host and plugins MUST come from the same toolchain,
# flags and sources (they share gen/biz and internal/patchable), so build
# them in one place -- on the measurement platform, inside Docker:
#
#   docker run --rm -v "$PWD":/src -w /src golang:1.25 cmd/s6-hotpatch/build.sh
#   OUT=/tmp/s6 FLAGS=-race cmd/s6-hotpatch/build.sh
#
# Output ($OUT, default cmd/s6-hotpatch/bin):
#   s6-hotpatch, signtool, hotpatchgen
#   keys/signing.key, keys/trusted.pub         (generated once, reused)
#   plugins/v2.so + v2.manifest.json           good patch (3 functions, one transaction)
#   plugins/badfp.so + badfp.manifest.json     wrong signature, honest manifest -> rejected before dlopen
#   plugins/badfp-liar.manifest.json           same .so, manifest lies -> rejected after dlopen
#   plugins/badsig.*                           v2 signed by an untrusted key
#   plugins/tampered.*                         v2 manifest, .so modified after signing
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${OUT:-cmd/s6-hotpatch/bin}
FLAGS=${FLAGS:-}
export CGO_ENABLED=1
PLDFLAGS=""
if [ "$(uname -s)" = "Darwin" ]; then PLDFLAGS="-extldflags=-Wl,-no_fixup_chains"; fi
mkdir -p "$OUT/plugins" "$OUT/keys" "$OUT/work"

# 0. the generated patchable package must match its source
go run ./tools/hotpatchgen check -dir cmd/s6-hotpatch/biz -out cmd/s6-hotpatch/gen/biz

# 1. host and tools
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/s6-hotpatch" ./cmd/s6-hotpatch
go build -o "$OUT/signtool" ./cmd/s6-hotpatch/signtool
go build -o "$OUT/hotpatchgen" ./tools/hotpatchgen

# 2. plugins + static fingerprints of their Patches maps
for p in v2 badfp; do
  # shellcheck disable=SC2086
  go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/$p.so" "./cmd/s6-hotpatch/plugins/$p"
  "$OUT/hotpatchgen" fingerprints -dir "cmd/s6-hotpatch/plugins/$p" > "$OUT/work/$p.fp.json"
done

# 3. keys
[ -f "$OUT/keys/signing.key" ] || "$OUT/signtool" keygen -priv "$OUT/keys/signing.key" -pub "$OUT/keys/trusted.pub"
"$OUT/signtool" keygen -priv "$OUT/work/attacker.key" -pub "$OUT/work/attacker.pub" >/dev/null

# 4. manifests
S="$OUT/signtool"; P="$OUT/plugins"; W="$OUT/work"; K="$OUT/keys/signing.key"
$S sign -key "$K" -so "$P/v2.so" -label v2 -fingerprints "$W/v2.fp.json" -out "$P/v2.manifest.json"
$S sign -key "$K" -so "$P/badfp.so" -label badfp -fingerprints "$W/badfp.fp.json" -out "$P/badfp.manifest.json"
# liar: real badfp.so, but the manifest claims ApplyDiscount has the expected fingerprint
$S sign -key "$K" -so "$P/badfp.so" -label badfp-liar -fingerprints "$W/v2.fp.json" -only biz.ApplyDiscount \
   -plugin-name badfp.so -out "$P/badfp-liar.manifest.json"
cp "$P/v2.so" "$P/badsig.so"
$S sign -key "$W/attacker.key" -so "$P/badsig.so" -label v2 -fingerprints "$W/v2.fp.json" -out "$P/badsig.manifest.json"
cp "$P/v2.so" "$P/tampered.so"
$S sign -key "$K" -so "$P/tampered.so" -label v2 -fingerprints "$W/v2.fp.json" -out "$P/tampered.manifest.json"
printf 'X' >> "$P/tampered.so"   # modified after signing

ls -la "$OUT" "$P"
