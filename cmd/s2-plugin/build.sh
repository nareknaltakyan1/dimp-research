#!/usr/bin/env bash
# Builds the S2 host binary and its plugins with ONE consistent set of flags
# (host and plugins must come from the same toolchain, flags and dependency
# versions, otherwise plugin.Open fails).
#
#   cmd/s2-plugin/build.sh                 # -> cmd/s2-plugin/bin/{s2-plugin,plugins/v1.so,plugins/v2.so}
#   OUT=/tmp/s2 cmd/s2-plugin/build.sh
#   GENS=20 cmd/s2-plugin/build.sh         # also builds plugins/v2_gen{1..20}.so for memgrowth
#   FLAGS=-race cmd/s2-plugin/build.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${OUT:-cmd/s2-plugin/bin}
FLAGS=${FLAGS:-}
GENS=${GENS:-0}
export CGO_ENABLED=1
mkdir -p "$OUT/plugins"
# macOS: the Go 1.24/1.25 external link of a plugin produces chained fixups
# that recent dyld rejects ("chained fixups, seg_count does not match number
# of segments"). Linking without fixup chains works around it.
PLDFLAGS=""
if [ "$(uname -s)" = "Darwin" ]; then PLDFLAGS="-extldflags=-Wl,-no_fixup_chains"; fi
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/s2-plugin" ./cmd/s2-plugin
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/memgrowth" ./cmd/s2-plugin/memgrowth
for v in v1 v2; do
  # shellcheck disable=SC2086
  go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/$v.so" ./cmd/s2-plugin/plugins/$v
done
# "Symbol versioning trick": the Go runtime refuses to load two plugins with
# the same package path ("plugin already loaded"), and -ldflags=-pluginpath
# alone does NOT help (symbols keep the compile-time package path, so Open
# fails with "could not find symbol"). The working trick is to give every
# generation a unique *import path* by copying its source into its own
# directory inside the module (a leading "_" hides it from ./... patterns).
for ((i = 1; i <= GENS; i++)); do
  src="cmd/s2-plugin/bin/_gen/v2gen$i"
  mkdir -p "$src"
  sed "s/^var Version = \"v2\"/var Version = \"v2.$i\"/" cmd/s2-plugin/plugins/v2/main.go > "$src/main.go"
  # shellcheck disable=SC2086
  go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/v2_gen$i.so" "./$src"
done
ls -la "$OUT" "$OUT/plugins"
