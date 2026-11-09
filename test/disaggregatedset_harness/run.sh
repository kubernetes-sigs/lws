#!/usr/bin/env bash
# Test the checked-out commit.
# Bash 3.2 compatible; no changes to the user's Kubernetes context.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPOSITORY="$(cd "$HARNESS_DIR/../.." && pwd)"
suite() {
  (cd "$REPOSITORY" && go run ./test/disaggregatedset_harness/cmd "$@")
}
SUITE_ARGS=("$@")
fail() { printf '\033[31;1mERROR: %s\033[0m\n' "$*" >&2; exit 1; }
section() { printf '\n\033[36;1m━━ %s ━━\033[0m\n' "$*"; }
# Use the driver's parser for validation and launch mode before runtime setup.
MODE="$(suite launcher-mode "$@")"
case "$MODE" in
  readonly) suite "$@"; exit 0 ;;
  automatic) ;;
  interactive) [[ -t 0 ]] || fail 'Interactive mode needs a terminal. Use --auto for unattended runs.' ;;
  *) fail "Unexpected launcher mode: $MODE" ;;
esac
HARNESS_MAX_PODS="${HARNESS_MAX_PODS:-160}"
[[ "$HARNESS_MAX_PODS" =~ ^[1-9][0-9]{0,3}$ ]] && (( HARNESS_MAX_PODS <= 1000 )) || fail 'HARNESS_MAX_PODS must be an integer between 1 and 1000.'
TOOLS=(docker kubectl git go jq tar)
for tool in "${TOOLS[@]}"; do
  command -v "$tool" >/dev/null || fail "Missing $tool. Install it before running the harness."
done
KIND="${KIND:-$(command -v kind || true)}"
[[ -n "$KIND" ]] || fail 'Install kind on PATH, or set KIND=/absolute/path/to/kind.'
command -v "$KIND" >/dev/null || fail "Cannot execute kind: $KIND"
docker info >/dev/null || fail 'Docker is not available.'
select_commit() {
  COMMIT="$(git -C "$REPOSITORY" rev-parse --verify 'HEAD^{commit}')" || fail 'Cannot resolve the checked-out controller commit.'
}
select_commit
[[ -d "$HARNESS_DIR/observer" ]] || fail 'Missing shared-observer helper source.'

mkdir -p "$HARNESS_DIR/.state"
RUN_DIR="$(mktemp -d "$HARNESS_DIR/.state/run.XXXXXX")"
TOKEN="$(basename "$RUN_DIR" | tr '[:upper:].' '[:lower:]-')"
CLUSTER="lws-v1-$TOKEN"
CONTEXT="kind-$CLUSTER"
NODE="$CLUSTER-control-plane"
KCFG="$RUN_DIR/kubeconfig"
IMAGE="lws-v1-harness:$COMMIT"
NODE_ID=''
CLUSTER_OWNED=0
DRIVER_PID=''
RUN_PASSED=0
printf '%s\n' "$TOKEN" > "$RUN_DIR/owner"
printf '%s\n' "$RUN_DIR" > "$HARNESS_DIR/.state/last-run"
k() { kubectl --kubeconfig "$KCFG" --context "$CONTEXT" --request-timeout=10s "$@"; }

owns_node() {
  local id="$1" label="$2" mount="$3"
  [[ "$label" == "$CLUSTER" && ( -z "$NODE_ID" || "$id" == "$NODE_ID" ) ]] || return 1
  [[ "$mount" == "$RUN_DIR/owner" ]] && return 0
  # Docker Desktop reports macOS bind sources through its VM's /host_mnt.
  [[ "$(uname -s)" == Darwin && "$mount" == "/host_mnt$RUN_DIR/owner" ]]
}

cleanup() {
  local status=$? owner='' actual_id='' cluster_label='' owner_mount='' matching_nodes='' deadline
  trap - EXIT
  trap '' INT TERM HUP PIPE
  set +e
  if [[ -n "$DRIVER_PID" ]]; then
    kill -TERM "$DRIVER_PID" 2>/dev/null
    # Allow the suite's deferred cleanup to restore temporary local settings.
    # Its isolated process group also owns every observer/kubectl subprocess.
    deadline=$((SECONDS + 180))
    while kill -0 -- "-$DRIVER_PID" 2>/dev/null && (( SECONDS < deadline )); do sleep 0.2; done
    kill -KILL -- "-$DRIVER_PID" 2>/dev/null
    wait "$DRIVER_PID" 2>/dev/null
  fi
  read -r owner < "$RUN_DIR/owner"
  if (( CLUSTER_OWNED )) && [[ "$owner" == "$TOKEN" ]]; then
    if ! matching_nodes=$(docker ps -a --no-trunc --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.ID}}' 2>/dev/null); then
      printf 'REFUSING cleanup: cannot list nodes for %s.\n' "$CLUSTER" >&2
      status=1
    elif [[ -z "$matching_nodes" ]]; then
      section "Owned kind cluster $CLUSTER is already absent"
    elif ! actual_id=$(docker inspect --type container --format '{{.Id}}' "$NODE" 2>/dev/null) ||
         ! cluster_label=$(docker inspect --type container --format '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$NODE" 2>/dev/null) ||
         ! owner_mount=$(docker inspect --type container --format '{{range .Mounts}}{{if eq .Destination "/var/run/lws-v1-harness-owner"}}{{.Source}}{{end}}{{end}}' "$NODE" 2>/dev/null); then
      printf 'REFUSING cleanup: cannot inspect ownership of %s.\n' "$NODE" >&2
      status=1
    elif [[ "$matching_nodes" != "$actual_id" ]] || ! owns_node "$actual_id" "$cluster_label" "$owner_mount"; then
      printf 'REFUSING cleanup: %s is no longer this run\047s node.\n' "$NODE" >&2
      status=1
    else
      section "Remove owned kind cluster $CLUSTER"
      if [[ -s "$KCFG" ]]; then
        k -n v1-harness get disaggregatedsets,lws,pods,dsrs -o yaml > "$RUN_DIR/final-resources.yaml" 2>&1
        k -n v1-harness get events --sort-by=.lastTimestamp > "$RUN_DIR/events.log" 2>&1
        k -n lws-system logs deployment/lws-controller-manager --all-containers --tail=3000 > "$RUN_DIR/controller.log" 2>&1
      fi
      "$KIND" delete cluster --name "$CLUSTER" --kubeconfig "$KCFG" > "$RUN_DIR/cleanup.log" 2>&1 || status=1
      if ! matching_nodes=$(docker ps -a --no-trunc --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.ID}}' 2>/dev/null); then
        printf 'Cannot confirm cleanup of %s; Docker node listing failed.\n' "$CLUSTER" >&2
        status=1
      elif [[ -n "$matching_nodes" ]]; then
        printf 'Cleanup did not remove %s; inspect %s/cleanup.log.\n' "$NODE" "$RUN_DIR" >&2
        status=1
      fi
    fi
  elif (( CLUSTER_OWNED )); then
    printf 'REFUSING cleanup: run ownership token changed.\n' >&2
    status=1
  fi
  if [[ "$status" == 0 && "$RUN_PASSED" != 1 ]]; then status=1; fi
  jq -n --argjson exitCode "$status" --argjson suitePassed "$RUN_PASSED" \
    --arg cluster "$CLUSTER" '{exitCode:$exitCode,suitePassed:($suitePassed==1),cluster:$cluster}' > "$RUN_DIR/runtime-result.json"
  printf '\nArtifacts retained: %s\n' "$RUN_DIR"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
exec > >(tee "$RUN_DIR/transcript.log") 2>&1
export CLICOLOR_FORCE=1
section "DisaggregatedSet regression lab · controller ${COMMIT:0:12}"
printf 'Testing committed HEAD; uncommitted controller changes are not included.\n'
printf 'Artifacts: %s\nPrivate context: %s\n' "$RUN_DIR" "$CONTEXT"

case "$(docker info --format '{{.Architecture}}')" in
  arm64|aarch64) ARCH=arm64 ;;
  amd64|x86_64) ARCH=amd64 ;;
  *) fail 'Docker must use arm64 or amd64.' ;;
esac
mkdir "$RUN_DIR/source" "$RUN_DIR/bin" "$RUN_DIR/build"
section 'Build native Go scenario driver'
(cd "$REPOSITORY" && go build -mod=readonly -trimpath -o "$RUN_DIR/bin/harness" ./test/disaggregatedset_harness/cmd)
git -C "$REPOSITORY" archive "$COMMIT" | tar -x -C "$RUN_DIR/source"
build_observer() {
  local overlay="$RUN_DIR/observer-overlay.json"
  bash "$HARNESS_DIR/observer/prepare.sh" "$RUN_DIR/source" "$overlay"
  section 'Build the read-only observer with a temporary Go overlay'
  (cd "$RUN_DIR/source"; GOMAXPROCS=2 go build -overlay="$overlay" -tags=harness -mod=readonly -p=2 -trimpath \
    -o "$RUN_DIR/bin/observe" ./test/disaggregatedset_harness/observer)
}
build_observer
prepare_image() {
if [[ "$(docker image inspect "$IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null || true)" != "$COMMIT" ||
      "$(docker image inspect "$IMAGE" --format '{{.Architecture}}' 2>/dev/null || true)" != "$ARCH" ]]; then
  section 'Build the pinned controller'
  (cd "$RUN_DIR/source"
    CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOMAXPROCS=2 go build -mod=readonly -p=2 -trimpath \
      -ldflags "-X sigs.k8s.io/lws/pkg/version.GitCommit=$COMMIT" -o "$RUN_DIR/build/manager" ./cmd)
  local build_flags=(--platform "linux/$ARCH")
  # The legacy Docker builder has no provenance flag. Keep the same source
  # label and image contents with either builder, without changing global config.
  if docker build --help 2>/dev/null | grep -F -- '--provenance' >/dev/null; then
    build_flags+=(--provenance=false)
  fi
  docker build "${build_flags[@]}" --build-arg "REVISION=$COMMIT" \
    -t "$IMAGE" -f - "$RUN_DIR/build" <<'DOCKERFILE'
FROM gcr.io/distroless/static:nonroot
ARG REVISION
LABEL org.opencontainers.image.revision=$REVISION
COPY manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
DOCKERFILE
fi
}
prepare_image

section 'Create a fresh isolated kind cluster'
clusters="$("$KIND" get clusters)" || fail 'Cannot list kind clusters.'
if printf '%s\n' "$clusters" | grep -Fxq "$CLUSTER" || docker inspect --type container "$NODE" >/dev/null 2>&1; then
  fail "The generated cluster name $CLUSTER already exists; refusing to reuse it."
fi
# The large suite must not stall at kubelet's unrelated default 110-Pod limit.
kind_config() {
jq -n --arg owner "$RUN_DIR/owner" --argjson maxPods "$HARNESS_MAX_PODS" '{
  kind:"Cluster",apiVersion:"kind.x-k8s.io/v1alpha4",
  kubeadmConfigPatches:["kind: KubeletConfiguration\nmaxPods: \($maxPods)\n"],
  nodes:[{role:"control-plane",extraMounts:[{
    hostPath:$owner,containerPath:"/var/run/lws-v1-harness-owner",readOnly:true
  }]}]
}'
}
# Claim the unique name before creation, so interrupted creation is cleaned too.
kind_config > "$RUN_DIR/kind-config.json"
CLUSTER_OWNED=1
"$KIND" create cluster --name "$CLUSTER" --config "$RUN_DIR/kind-config.json" \
  --image "${KIND_NODE_IMAGE:-kindest/node:v1.36.1}" --kubeconfig "$KCFG" --wait 2m
NODE_ID="$(docker inspect --type container --format '{{.Id}}' "$NODE")"
chmod 600 "$KCFG"
[[ "$(k config current-context)" == "$CONTEXT" && "$(k config get-contexts -o name)" == "$CONTEXT" ]] || fail 'Private kubeconfig has an unexpected context.'
server="$(k config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
[[ "$server" =~ ^https://127\.0\.0\.1:[0-9]+$ ]] || fail 'The harness API endpoint is not localhost.'
k label node "$NODE" "harness.lws.x-k8s.io/run=$TOKEN" >/dev/null
[[ "$(k get node "$NODE" -o jsonpath='{.status.allocatable.pods}')" == "$HARNESS_MAX_PODS" ]] || fail 'Node Pod capacity does not match the requested harness limit.'
jq -n --arg commit "$COMMIT" --arg cluster "$CLUSTER" --arg node "$NODE" --arg nodeID "$NODE_ID" --arg token "$TOKEN" \
  --arg image "$IMAGE" --argjson maxPods "$HARNESS_MAX_PODS" \
  '{controllerCommit:$commit,controllerImage:$image,
    cluster:$cluster,node:$node,nodeID:$nodeID,owner:$token,maxPods:$maxPods}' > "$RUN_DIR/runtime.json"
docker image inspect busybox:1.36 >/dev/null 2>&1 || docker pull busybox:1.36
docker image save "$IMAGE" busybox:1.36 | docker exec --privileged -i "$NODE" \
  ctr --namespace=k8s.io images import --platform "linux/$ARCH" --digests --snapshotter=overlayfs -

section 'Install the pinned CRDs, admission webhooks, and controller'
jq -n --arg tag "$COMMIT" '{
  apiVersion:"kustomize.config.k8s.io/v1beta1", kind:"Kustomization",
  resources:["source/config/default"],
  images:[{name:"us-central1-docker.pkg.dev/k8s-staging-images/lws/lws",newName:"lws-v1-harness",newTag:$tag}],
  replicas:[{name:"lws-controller-manager",count:1}],
  patches:[{target:{kind:"Deployment",name:"lws-controller-manager"},
    patch:"- op: replace\n  path: /spec/template/spec/containers/0/resources/requests\n  value: {cpu: 100m, memory: 128Mi}\n"}]
}' > "$RUN_DIR/kustomization.yaml"
k apply --server-side -k "$RUN_DIR" > "$RUN_DIR/install.log"
k wait --for=condition=Established --timeout=90s \
  crd/disaggregatedsets.disaggregatedset.x-k8s.io crd/leaderworkersets.leaderworkerset.x-k8s.io
k -n lws-system rollout status deployment/lws-controller-manager --timeout=180s
[[ "$(k -n lws-system get deployment lws-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}')" == "$IMAGE" ]] || fail 'Controller Deployment image does not match the checked-out commit.'
k -n lws-system get pods -l control-plane=controller-manager -o json > "$RUN_DIR/controller-pods.json"
jq --slurpfile pods "$RUN_DIR/controller-pods.json" '. + {controllerImageIDs:[$pods[0].items[].status.containerStatuses[]? | select(.name=="manager") | .imageID]}' \
  "$RUN_DIR/runtime.json" > "$RUN_DIR/runtime-image.json"
mv "$RUN_DIR/runtime-image.json" "$RUN_DIR/runtime.json"
k -n kube-system wait pods -l tier=control-plane --for=condition=Ready --timeout=90s
k create namespace v1-harness

section 'Run scenarios'
# Isolate cancellation without taking stdin from interactive checkpoints.
"$RUN_DIR/bin/harness" "${SUITE_ARGS[@]}" --process-group \
  --kubeconfig "$KCFG" --context "$CONTEXT" --run-dir "$RUN_DIR" --node "$NODE" \
  --controller-commit "$COMMIT" --observer "$RUN_DIR/bin/observe" <&0 &
DRIVER_PID=$!
wait "$DRIVER_PID"
DRIVER_PID=''
RUN_PASSED=1
