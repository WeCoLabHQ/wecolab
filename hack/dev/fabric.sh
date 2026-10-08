#!/usr/bin/env bash
# The development fabric (docs/development.md): three boxes and a vault in Docker, running the same
# install script as real boxes.
#
#   hack/dev/fabric.sh up | test | down | shell <box>
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$PWD
PREFIX=${WECOLAB_DEV_PREFIX:-wcl}
NET=${WECOLAB_DEV_NETWORK:-$PREFIX-net}
SUBNET=${WECOLAB_DEV_SUBNET:-198.18.0.0/24}
ZONE=${WECOLAB_DEV_ZONE:-dev.wecolab.test}
BASE=${SUBNET%.*}
VAULT_IP=$BASE.10
CACHE=${WECOLAB_DEV_CACHE:-$ROOT/hack/dev/cache/$PREFIX}
OWNER=${WECOLAB_DEV_OWNER:-}
[[ $PREFIX =~ ^[a-z][a-z0-9-]{0,30}$ && $NET =~ ^[a-z][a-z0-9-]{0,50}$ && $BASE =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && $SUBNET == "$BASE.0/24" ]] || { echo "invalid isolated fabric prefix/network/subnet" >&2; exit 2; }
[[ $ZONE =~ ^[a-z0-9.-]+$ ]] || { echo "invalid fabric zone" >&2; exit 2; }
[[ $SUBNET != 198.18.0.0/24 ]] || { echo "choose a subnet distinct from the existing wcl-net lab" >&2; exit 2; }
[[ -z $OWNER || $OWNER =~ ^[a-zA-Z0-9_-]{12,80}$ ]] || { echo "invalid fabric owner" >&2; exit 2; }
ip_of() { case $1 in pub) echo "$BASE.2" ;; home) echo "$BASE.3" ;; mac) echo "$BASE.4" ;; *) return 2 ;; esac; }
container() { printf '%s-%s' "$PREFIX" "$1"; }
owned() {
  local kind=$1 name=$2 label
  if [[ $kind == network ]]; then
    docker network inspect "$name" >/dev/null 2>&1 || return 1
    label=$(docker network inspect -f '{{index .Labels "wecolab.dev.owner"}}' "$name")
  elif [[ $kind == volume ]]; then
    docker volume inspect "$name" >/dev/null 2>&1 || return 1
    label=$(docker volume inspect -f '{{index .Labels "wecolab.dev.owner"}}' "$name")
  else
    docker inspect "$name" >/dev/null 2>&1 || return 1
    label=$(docker inspect -f '{{index .Config.Labels "wecolab.dev.owner"}}' "$name")
  fi
  [[ -n $OWNER && $label == "$OWNER" ]] || { echo "foreign or unlabelled $kind $name; refusing mutation" >&2; exit 1; }
}
guard() {
  [[ -n $OWNER ]] || { echo "WECOLAB_DEV_OWNER is required for fabric mutation (existing unlabelled wcl-* are not owned)" >&2; exit 2; }
  [[ $PREFIX != wcl ]] || { echo "default wcl-* lab is not disposable; choose a unique WECOLAB_DEV_PREFIX" >&2; exit 2; }
  [[ ! -L $CACHE ]] || { echo "symlink cache refused" >&2; exit 2; }
  if [[ -e $CACHE ]]; then
    [[ -f $CACHE/.wecolab-owner && $(<"$CACHE/.wecolab-owner") == "$OWNER" ]] || { echo "foreign or unlabelled cache $CACHE" >&2; exit 2; }
  fi
  local n
  for n in pub home mac vault; do owned container "$(container "$n")" || [[ $? == 1 ]] || return 1; done
  for n in pub home mac vault; do owned volume "$(container "$n")" || [[ $? == 1 ]] || return 1; owned volume "$(container "$n")-var" || [[ $? == 1 ]] || return 1; done
  owned network "$NET" || [[ $? == 1 ]] || return 1
}
[[ $CACHE == "$ROOT/hack/dev/cache/$PREFIX" ]] || { echo "cache must be this run's isolated hack/dev/cache/$PREFIX directory" >&2; exit 2; }
say() { printf '\n### %s\n' "$*"; }

box() { # box NAME: a privileged container running systemd
  local n=$1 b; b=$(container "$n")
  owned container "$b" && return 0
  docker volume create --label "wecolab.dev.owner=$OWNER" "$b-var" >/dev/null
  docker run -d --name "$b" --label "wecolab.dev.owner=$OWNER" --hostname "$n" --network "$NET" --ip "$(ip_of "$n")" \
    --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
    -v "$b-var:/var" -v /lib/modules:/lib/modules:ro -v "$ROOT:/src:ro" -v "$CACHE:/cache" \
    --add-host "console.$ZONE:$(ip_of pub)" --add-host "mesh.$ZONE:$(ip_of pub)" \
    "$PREFIX-box" >/dev/null
  for _ in $(seq 30); do docker exec "$b" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done
  docker exec -i "$b" bash < hack/dev/snap.sh > "$CACHE/new-$n.txt"
}

s3() { # the AWS CLI against the vault
  docker run --rm --network $NET -e AWS_ACCESS_KEY_ID=wecolab -e AWS_SECRET_ACCESS_KEY=wecolab-dev-vault -e AWS_DEFAULT_REGION=us-east-1 \
    amazon/aws-cli:latest --endpoint-url "http://$VAULT_IP" "$@"
}

run_env() { # the first box, with its answers in the environment
  docker cp install.sh "$1:/root/install.sh"
  docker exec -e WECOLAB_DEV=1 -e WECOLAB_CACHE=/cache -e WECOLAB_BIN=/src/dist/bin -e WECOLAB_ZONE -e WECOLAB_EMAIL -e WECOLAB_SITE -e WECOLAB_PROJECT \
    "$1" bash /root/install.sh
}

# run BOX ARGS...: install.sh on a box, from a copy (bash reads a script as it runs; the source may change).
# WECOLAB_LAPTOP=1 joins it as a laptop, as WeCoLab for Mac does.
run() {
  local b=$1; shift
  docker cp install.sh "$b:/root/install.sh"
  docker exec -e WECOLAB_DEV=1 -e WECOLAB_CACHE=/cache -e WECOLAB_BIN=/src/dist/bin ${WECOLAB_LAPTOP:+-e WECOLAB_LAPTOP=1} "$b" bash /root/install.sh "$@"
}

mac() { # the laptop node of home
  local inv
  inv=$(console POST /api/sites/home/boxes '{"Laptop":true}' | jq -r .invite)
  box mac
  WECOLAB_LAPTOP=1 run "$(container mac)" "$inv"
}

console() { # console METHOD PATH [JSON]: the Console's API, through the Door at pub
  docker exec "$(container pub)" curl -fsSk -X "$1" -H "Host: console.$ZONE" -H 'Content-Type: application/json' ${3:+-d "$3"} "https://127.0.0.1$2"
}

# The Door can start before Flux has created the Console. Wait on a read-only
# authority check before the first non-idempotent site/invite creation.
wait_console() {
  local _
  for _ in $(seq 120); do
    if console GET /api/settings 2>/dev/null | jq -e '.isWriter == true' >/dev/null; then return 0; fi
    sleep 5
  done
  echo "writer Console did not become ready" >&2
  return 1
}

up() {
  guard
  mkdir -p "$CACHE"
  printf '%s\n' "$OWNER" > "$CACHE/.wecolab-owner"
  local f
  for f in "$ROOT"/hack/dev/cache/{k3s-*,sops-*,nebula-*,SHASUM256.txt,sums}; do
    [[ ! -f $f || -e "$CACHE/${f##*/}" ]] || cp "$f" "$CACHE/"
  done
  say "images and binaries"
  docker build -q -t "$PREFIX-box" hack/dev >/dev/null
  make -s dist
  owned network "$NET" || docker network create --label "wecolab.dev.owner=$OWNER" --subnet "$SUBNET" "$NET" >/dev/null
  say "the vault: S3 with Object Lock at http://$VAULT_IP"
  if ! owned container "$(container vault)"; then
    docker volume create --label "wecolab.dev.owner=$OWNER" "$(container vault)" >/dev/null
    docker run -d --name "$(container vault)" --label "wecolab.dev.owner=$OWNER" --network "$NET" --ip "$VAULT_IP" -v "$(container vault):/data" -v "$ROOT/hack/dev/s3.json:/etc/s3.json:ro" \
      chrislusf/seaweedfs:4.48 server -dir=/data -s3 -s3.port=80 -s3.config=/etc/s3.json -master.volumeSizeLimitMB=256 -volume.max=64 >/dev/null
    for _ in $(seq 30); do s3 s3api list-buckets >/dev/null 2>&1 && break; sleep 2; done
    s3 s3api create-bucket --bucket wecolab-dev --object-lock-enabled-for-bucket >/dev/null
    s3 s3api put-object-lock-configuration --bucket wecolab-dev \
      --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"GOVERNANCE","Days":1}}}' >/dev/null
  fi
  say "pub: a new fabric"
  box pub
  WECOLAB_ZONE=$ZONE WECOLAB_EMAIL=owner@$ZONE WECOLAB_SITE=pub WECOLAB_PROJECT=dev run_env "$(container pub)"
  wait_console
  say "home: a second site, a steward"
  local inv
  inv=$(console POST /api/sites '{"Name":"home","Owner":"dev","Steward":true}' | jq -r .invite)
  box home
  run "$(container home)" "$inv"
  say "mac: a laptop node of home"
  mac
  say "up"; console GET /api/state | jq -c '.sites[] | {Name, Ready, Steward, Writer, boxes: [.Boxes[] | {Name, IP, Ready}]}'
}

reload() { # new code into the running fabric: images rebuilt and imported, WeCoLab's pods restarted
  make -s dist
  local a=arm64 w="/tmp/$PREFIX-warden-host"
  [ "$(docker info -f '{{.Architecture}}')" = x86_64 ] && a=amd64
  go build -o "$w" ./cmd/warden
  "$w" image --name ghcr.io/wecolabhq/warden:dev --arch "$a" --out "/tmp/$PREFIX-warden-dev-$a.tar" "dist/bin/warden-$a=/warden" "$CACHE/sops-v3.13.3.linux.$a=/usr/local/bin/sops"
  "$w" image --name ghcr.io/wecolabhq/console:dev --arch "$a" --out "/tmp/$PREFIX-console-dev-$a.tar" "dist/bin/console-$a=/console" "$CACHE/sops-v3.13.3.linux.$a=/usr/local/bin/sops"
  for n in pub home mac; do
    owned container "$(container "$n")" || continue
    for i in warden console; do
      docker cp "/tmp/$PREFIX-$i-dev-$a.tar" "$(container "$n"):/var/lib/wecolab/dist/$i-dev-$a.tar"
      docker exec "$(container "$n")" sh -c "cp /var/lib/wecolab/dist/$i-dev-$a.tar /var/lib/rancher/k3s/agent/images/ && k3s ctr images import /var/lib/wecolab/dist/$i-dev-$a.tar >/dev/null && k3s ctr images label ghcr.io/wecolabhq/$i:dev io.cri-containerd.pinned=pinned >/dev/null"
    done
    docker exec "$(container "$n")" sh -c 'export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; for r in deployment/wecolab-warden deployment/wecolab-console deployment/door daemonset/wecolab-node-agent; do k3s kubectl -n wecolab-system rollout restart $r >/dev/null 2>&1 || true; done' 2>/dev/null || true
  done
  echo reloaded
}

down() {
  guard
  local n
  for n in pub home mac vault; do
    if owned container "$(container "$n")"; then docker rm -f "$(container "$n")" >/dev/null; fi
  done
  for n in pub home mac vault; do
    if owned volume "$(container "$n")-var"; then docker volume rm "$(container "$n")-var" >/dev/null; fi
    if owned volume "$(container "$n")"; then docker volume rm "$(container "$n")" >/dev/null; fi
  done
  if owned network "$NET"; then docker network rm "$NET" >/dev/null; fi
}

case "${1:-}" in
  up) up ;;
  mac) guard; mac ;;
  reload) guard; reload ;;
  down) down ;;
  test) guard; exec bash hack/dev/test.sh ;;
  shell) guard; owned container "$(container "${2:?box}")"; exec docker exec -it "$(container "$2")" bash ;;
  console) guard; shift; console "$@" ;;
  *) echo "usage: $0 up|test|down|shell <box>|console METHOD PATH [JSON]" >&2; exit 2 ;;
esac
