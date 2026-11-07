#!/usr/bin/env bash
# Run a command inside the Linux benchmark container (deploy/Dockerfile.bench)
# with the repo mounted at /work and pinned CPU/memory resources.
#
#   scripts/in_docker.sh scripts/run_experiment.sh mock E1
#   REPS=3 scripts/in_docker.sh scripts/run_matrix.sh s1-epoch-swap
#   scripts/in_docker.sh bash            # interactive shell
#
# Resources (recorded in every run's metadata.json):
#   DOCKER_CPUSET (default 0-5): host CPUs given to the container.
#     Inside, SERVER_CPUS (0-1) runs the server, LOADGEN_CPUS (2-5) runs
#     loadgen+procmon, so the client never steals server CPU.
#   DOCKER_CPUS (default = size of cpuset), DOCKER_MEMORY (default 4g).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${BENCH_IMAGE:-dimp-bench}"
: "${DOCKER_CPUSET:=0-5}"
: "${DOCKER_CPUS:=6}"
: "${DOCKER_MEMORY:=4g}"

docker build -q -f "$ROOT/deploy/Dockerfile.bench" -t "$IMAGE" "$ROOT/deploy" >/dev/null

PASS=(REPS RATE WARMUP_S UPGRADE_AT_S UPGRADES UPGRADE_INTERVAL_S TAIL_S CONCURRENCY WORK_MIX KEEPALIVE TIMEOUT
      UPGRADE_TARGETS TRIGGER SERVER_ARGS SERVER_CPUS LOADGEN_CPUS PORT RSS_INTERVAL SEED FAIL_TARGET RUN_TAG
      EXPERIMENTS SKIP_ANALYSIS SOL_LABEL)
ENVARGS=()
for v in "${PASS[@]}"; do [[ -n "${!v+x}" ]] && ENVARGS+=(-e "$v=${!v}"); done
HOSTINFO="$(docker info --format '{{.OperatingSystem}}; {{.NCPU}} cpus; {{.MemTotal}} B; kernel {{.KernelVersion}}') ; host $(uname -sm)"

# Other containers on the same Docker VM compete for CPU and invalidate timing.
OTHERS=$(docker ps -q | wc -l | tr -d ' ')
if (( OTHERS > 0 )); then
  echo "WARNING: $OTHERS other container(s) running on this Docker host; measurements may be perturbed (recorded in metadata)." >&2
fi
TTY=(); [[ -t 0 && -t 1 ]] && TTY=(-it)
exec docker run --rm ${TTY[@]+"${TTY[@]}"} \
  --cpuset-cpus "$DOCKER_CPUSET" --cpus "$DOCKER_CPUS" --memory "$DOCKER_MEMORY" \
  --cap-add SYS_PTRACE --security-opt seccomp=unconfined \
  --sysctl net.ipv4.ip_local_port_range="1024 65000" --sysctl net.ipv4.tcp_tw_reuse=1 \
  --sysctl net.core.somaxconn=4096 \
  -v "$ROOT:/work" -v dimp-gomod:/go/pkg/mod -v dimp-gocache:/root/.cache/go-build \
  -e DOCKER_CPUSET="$DOCKER_CPUSET" -e DOCKER_CPUS="$DOCKER_CPUS" -e DOCKER_MEMORY="$DOCKER_MEMORY" \
  -e DOCKER_HOST_INFO="$HOSTINFO" -e DOCKER_OTHER_CONTAINERS="$OTHERS" ${ENVARGS[@]+"${ENVARGS[@]}"} \
  -w /work "$IMAGE" "$@"
