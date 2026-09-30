#!/usr/bin/env bash
# The development fabric (docs/development.md): three boxes and a vault in Docker, running the same
# install script as real boxes.
#
#   hack/dev/fabric.sh up | test | down | shell <box>
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$PWD
NET=wcl-net SUBNET=198.18.0.0/24            # a stand-in internet: public-looking, not private
ZONE=dev.wecolab.test
VAULT_IP=198.18.0.10
CACHE=$ROOT/hack/dev/cache
ip_of() { case $1 in pub) echo 198.18.0.2 ;; home) echo 198.18.0.3 ;; mac) echo 198.18.0.4 ;; esac; }
mkdir -p "$CACHE"
say() { printf '\n### %s\n' "$*"; }

box() { # box NAME: a privileged container running systemd
  local n=$1
  docker inspect "wcl-$n" >/dev/null 2>&1 && return 0
  docker run -d --name "wcl-$n" --hostname "$n" --network $NET --ip "$(ip_of "$n")" \
    --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
    -v "wcl-$n-var:/var" -v /lib/modules:/lib/modules:ro -v "$ROOT:/src:ro" -v "$CACHE:/cache" \
    --add-host "console.$ZONE:$(ip_of pub)" --add-host "mesh.$ZONE:$(ip_of pub)" \
    wcl-box >/dev/null
  for _ in $(seq 30); do docker exec "wcl-$n" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done
  docker exec -i "wcl-$n" bash < hack/dev/snap.sh > "$CACHE/new-$n.txt"   # the box before WeCoLab (test step 8)
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
  WECOLAB_LAPTOP=1 run wcl-mac "$inv"
}

console() { # console METHOD PATH [JSON]: the Console's API, through the Door at pub
  docker exec wcl-pub curl -fsSk -X "$1" -H "Host: console.$ZONE" -H 'Content-Type: application/json' ${3:+-d "$3"} "https://127.0.0.1$2"
}

up() {
  say "images and binaries"
  docker build -q -t wcl-box hack/dev >/dev/null
  make -s dist
  docker network inspect $NET >/dev/null 2>&1 || docker network create --subnet $SUBNET $NET >/dev/null
  say "the vault: S3 with Object Lock at http://$VAULT_IP"
  if ! docker inspect wcl-vault >/dev/null 2>&1; then
    docker run -d --name wcl-vault --network $NET --ip $VAULT_IP -v wcl-vault:/data -v "$ROOT/hack/dev/s3.json:/etc/s3.json:ro" \
      chrislusf/seaweedfs:4.48 server -dir=/data -s3 -s3.port=80 -s3.config=/etc/s3.json -master.volumeSizeLimitMB=256 -volume.max=64 >/dev/null
    for _ in $(seq 30); do s3 s3api list-buckets >/dev/null 2>&1 && break; sleep 2; done
    s3 s3api create-bucket --bucket wecolab-dev --object-lock-enabled-for-bucket >/dev/null
    s3 s3api put-object-lock-configuration --bucket wecolab-dev \
      --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"GOVERNANCE","Days":1}}}' >/dev/null
  fi
  say "pub: a new fabric"
  box pub
  WECOLAB_ZONE=$ZONE WECOLAB_EMAIL=owner@$ZONE WECOLAB_SITE=pub WECOLAB_PROJECT=dev run_env wcl-pub
  say "home: a second site, a steward"
  local inv
  inv=$(console POST /api/sites '{"Name":"home","Owner":"dev","Steward":true}' | jq -r .invite)
  box home
  run wcl-home "$inv"
  say "mac: a laptop node of home"
  mac
  say "up"; console GET /api/state | jq -c '.sites[] | {Name, Ready, Steward, Writer, boxes: [.Boxes[] | {Name, IP, Ready}]}'
}

reload() { # new code into the running fabric: images rebuilt and imported, WeCoLab's pods restarted
  make -s dist
  local a=arm64 w=/tmp/wcl-warden-host
  [ "$(docker info -f '{{.Architecture}}')" = x86_64 ] && a=amd64
  go build -o "$w" ./cmd/warden
  "$w" image --name ghcr.io/wecolabhq/warden:dev --arch $a --out /tmp/warden-dev-$a.tar "dist/bin/warden-$a=/warden" "$CACHE/sops-v3.13.3.linux.$a=/usr/local/bin/sops"
  "$w" image --name ghcr.io/wecolabhq/console:dev --arch $a --out /tmp/console-dev-$a.tar "dist/bin/console-$a=/console" "$CACHE/sops-v3.13.3.linux.$a=/usr/local/bin/sops"
  for n in pub home mac; do
    docker inspect "wcl-$n" >/dev/null 2>&1 || continue
    for i in warden console; do
      docker cp "/tmp/$i-dev-$a.tar" "wcl-$n:/var/lib/wecolab/dist/$i-dev-$a.tar"
      docker exec "wcl-$n" sh -c "cp /var/lib/wecolab/dist/$i-dev-$a.tar /var/lib/rancher/k3s/agent/images/ && k3s ctr images import /var/lib/wecolab/dist/$i-dev-$a.tar >/dev/null && k3s ctr images label ghcr.io/wecolabhq/$i:dev io.cri-containerd.pinned=pinned >/dev/null"
    done
    docker exec "wcl-$n" sh -c 'export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; for r in deployment/wecolab-warden deployment/wecolab-console deployment/door daemonset/wecolab-node-agent; do k3s kubectl -n wecolab-system rollout restart $r >/dev/null 2>&1 || true; done' 2>/dev/null || true
  done
  echo reloaded
}

down() {
  for n in pub home mac vault; do docker rm -f "wcl-$n" >/dev/null 2>&1 || true; done
  for n in pub home mac vault; do docker volume rm "wcl-$n-var" "wcl-$n" >/dev/null 2>&1 || true; done
  docker network rm $NET >/dev/null 2>&1 || true
}

case "${1:-}" in
  up) up ;;
  mac) mac ;;
  reload) reload ;;
  down) down ;;
  test) exec bash hack/dev/test.sh ;;
  shell) exec docker exec -it "wcl-${2:?box}" bash ;;
  console) shift; console "$@" ;;
  *) echo "usage: $0 up|test|down|shell <box>|console METHOD PATH [JSON]" >&2; exit 2 ;;
esac
