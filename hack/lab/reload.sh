#!/usr/bin/env bash
# Try a fix on real machines before it is released: rebuild WeCoLab's components from this checkout under
# the version the lab already runs, and load them on every site's manager. The images keep their tag, so
# nothing else changes; the Fabric's system/ still changes only by `warden upgrade`.
#
#   LAB_WRITER=root@203.0.113.7 LAB_SITES="me@192.0.2.10 me@192.0.2.11" hack/lab/reload.sh warden console
#
# LAB_WRITER is the writer's manager: it makes the images (and keeps them in /var/lib/wecolab/dist, which
# its Console serves to joining boxes). LAB_SITES are the other sites' managers. Each login needs sudo.
# Pods are deleted rather than rolled out: a rollout's annotation is not in the Fabric, so Flux would
# take it back and restart everything a second time.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${LAB_WRITER:?the manager of the writer site, as user@host}" "${1:?components: warden, console or both}"
cmp -s install.sh cmd/console/join.sh || cp install.sh cmd/console/join.sh # as make does

remote() { local h=$1; shift; ssh -n -o BatchMode=yes "$h" "$@"; }
img=$(remote "$LAB_WRITER" "sudo k3s kubectl -n wecolab-system get deploy wecolab-warden -o jsonpath='{.spec.template.spec.containers[0].image}'")
V=${img##*:} R=${img%/warden:*} # the version and the image repository the lab runs
mine=$(grep -o -m1 'ghcr\.io/[a-z0-9-]*/warden' install.sh); mine=${mine%/warden}
case " $* " in *" console "*) [ "$R" = "$mine" ] || {
  echo "the lab names its images $R/…, this checkout $mine/…: the Console's join.sh would import images under" >&2
  echo "names the lab's manifests do not use. Move the lab to the new names first (docs/operations.md, Upgrades)." >&2
  exit 1; } ;; esac
arch_for() {
  local host=$1 machine
  machine=$(remote "$host" 'uname -m') || { echo "$host: cannot determine architecture" >&2; return 1; }
  case "$machine" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "$host: unsupported architecture $machine" >&2; return 1 ;;
  esac
}
# Discover every manager before building or changing images anywhere.
hosts=("$LAB_WRITER")
for h in ${LAB_SITES:-}; do hosts+=("$h"); done
arches=()
for h in "${hosts[@]}"; do arch=$(arch_for "$h") || exit 1; arches+=("$arch"); done
out=$(mktemp -d)
for a in amd64 arm64; do # warden always: it makes the images
  for c in $(printf '%s\n' warden "$@" | sort -u); do CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags "-s -w -X main.Version=$V" -o "$out/$c-$a" "./cmd/$c"; done
done
remote "$LAB_WRITER" 'rm -rf /tmp/wcl-reload && mkdir -p /tmp/wcl-reload'
scp -q "$out"/* "$LAB_WRITER":/tmp/wcl-reload/
rm -rf "$out"

# The images, made where the Console's sops binaries are, with the new warden.
ssh -o BatchMode=yes "$LAB_WRITER" "sudo V=$V R=$R COMPONENTS='$*' bash -s" <<'SH' # stdin carries the script: no -n
set -e
D=/var/lib/wecolab/dist B=/tmp/wcl-reload
case $(uname -m) in aarch64|arm64) a=arm64 ;; x86_64|amd64) a=amd64 ;; *) echo "writer: unsupported architecture $(uname -m)" >&2; exit 1 ;; esac
install -m 0755 $B/warden-$a $B/warden
for c in $COMPONENTS; do
  for a in amd64 arm64; do
    extra="/var/lib/wecolab/bin/sops-v3.13.3.linux.$a=/usr/local/bin/sops" # both carry SOPS, as install.sh builds them
    rm -f $D/$c-$V-$a.tar
    $B/warden image --name $R/$c:$V --arch $a --out $D/$c-$V-$a.tar "$B/$c-$a=/$c" $extra
    [ -s "$D/$c-$V-$a.tar" ] || { echo "no new image for $c ($a)" >&2; exit 1; }
  done
done
echo built
SH

# Every site: import, pin, and restart what runs the components.
for i in "${!hosts[@]}"; do
  h=${hosts[$i]} arch=${arches[$i]}
  for c in "$@"; do
    if [ "$h" != "$LAB_WRITER" ]; then
      scp -q "$LAB_WRITER:/var/lib/wecolab/dist/$c-$V-$arch.tar" "$out.tar" || exit 1
      scp -q "$out.tar" "$h:/tmp/$c-$V-$arch.tar" || exit 1
      rm -f "$out.tar"
    fi
    src=/tmp/$c-$V-$arch.tar; [ "$h" = "$LAB_WRITER" ] && src=/var/lib/wecolab/dist/$c-$V-$arch.tar
    remote "$h" "sudo sh -c 'cp $src /var/lib/rancher/k3s/agent/images/$c-$V-$arch.tar && k3s ctr images import $src >/dev/null && k3s ctr images label $R/$c:$V io.cri-containerd.pinned=pinned >/dev/null'"
    [ "$h" = "$LAB_WRITER" ] || remote "$h" "rm -f /tmp/$c-$V-$arch.tar"
  done
  apps="" # the Door runs the entrance Warden; the Console runs only at stewards
  case " $* " in *" warden "*) apps="wecolab-warden door" ;; esac
  case " $* " in *" console "*) apps="$apps wecolab-console" ;; esac
  for a in $apps; do
    remote "$h" "sudo k3s kubectl -n wecolab-system get deploy $a >/dev/null 2>&1 && sudo k3s kubectl -n wecolab-system delete pod -l app=$a --wait=false >/dev/null && sudo k3s kubectl -n wecolab-system rollout status deploy/$a --timeout=180s >/dev/null" || true
  done
  echo "$h: $* at $V"
done
remote "$LAB_WRITER" 'rm -rf /tmp/wcl-reload'
