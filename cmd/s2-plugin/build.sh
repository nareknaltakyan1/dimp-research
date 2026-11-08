#!/usr/bin/env bash
# Builds the S2 host binary and its plugins with ONE consistent set of flags
# (host and plugins must come from the same toolchain, flags and dependency
# versions, otherwise plugin.Open fails).
#
# Harness convention: `build.sh <out_dir>` leaves an executable
# <out_dir>/server (scripts/run_experiment.sh). Nothing is written into the
# source tree; generated sources go under <out_dir>/_gen.
#
#   cmd/s2-plugin/build.sh /tmp/s2         # /tmp/s2/{server,s2-plugin,memgrowth,plugins/{v1,v2,v2-broken}.so}
#   cmd/s2-plugin/build.sh                 # default out dir: cmd/s2-plugin/bin (git-ignored)
#   GENS=20 cmd/s2-plugin/build.sh /tmp/s2 # also plugins/v2.{1..20}.so (E5, memgrowth)
#   FLAGS=-race cmd/s2-plugin/build.sh /tmp/s2
# OUT=<dir> is still accepted when no argument is given.
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=${1:-${OUT:-cmd/s2-plugin/bin}}
FLAGS=${FLAGS:-}
GENS=${GENS:-0}
export CGO_ENABLED=1
mkdir -p "$OUT/plugins"
OUT=$(cd "$OUT" && pwd)
# macOS: the Go 1.24/1.25 external link of a plugin produces chained fixups
# that recent dyld rejects ("chained fixups, seg_count does not match number
# of segments"). Linking without fixup chains works around it.
PLDFLAGS=""
if [ "$(uname -s)" = "Darwin" ]; then PLDFLAGS="-extldflags=-Wl,-no_fixup_chains"; fi
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/server" ./cmd/s2-plugin
ln -sf server "$OUT/s2-plugin"
# shellcheck disable=SC2086
go build $FLAGS -o "$OUT/memgrowth" ./cmd/s2-plugin/memgrowth
for v in v1 v2; do
  # shellcheck disable=SC2086
  go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/$v.so" ./cmd/s2-plugin/plugins/$v
done
# E6 failure target: a well-formed plugin without the New symbol.
# shellcheck disable=SC2086
go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/v2-broken.so" ./bench/fixtures/plugin-nonew
# Plugin generations. The Go runtime refuses to load two plugins with the
# same package path ("plugin already loaded"), and -ldflags=-pluginpath alone
# does NOT help (symbols keep the compile-time package path). Every
# generation therefore gets a unique *import path*: its source is copied into
# its own throw-away module under $OUT/_gen (the v2 plugin imports only the
# standard library, so it does not need to live inside this module).
# The throw-away module must use the SAME toolchain as the host: copy the go
# directive of this module and forbid toolchain switching (a newer "go" line
# would make the go command download/select another toolchain, and plugin.Open
# then fails with "built with a different version of package ...").
GOLINE=$(awk '$1 == "go" {print $2; exit}' go.mod)
for ((i = 1; i <= GENS; i++)); do
  src="$OUT/_gen/v2gen$i"
  mkdir -p "$src"
  sed "s/^var Version = \"v2\"/var Version = \"v2.$i\"/" cmd/s2-plugin/plugins/v2/main.go > "$src/main.go"
  printf 'module dimp.local/s2gen/v2gen%d\n\ngo %s\n' "$i" "$GOLINE" > "$src/go.mod"
  # shellcheck disable=SC2086
  (cd "$src" && GOWORK=off GOTOOLCHAIN=local go build $FLAGS -buildmode=plugin -ldflags "$PLDFLAGS" -o "$OUT/plugins/v2.$i.so" .)
done
