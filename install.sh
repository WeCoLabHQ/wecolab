#!/usr/bin/env bash
# WeCoLab: one script for every box (docs/install.md).
#
#   install.sh              make a new fabric on this box: the first site, public, steward and writer. On a
#                           box that belongs to a fabric already, converge its host steps (packages,
#                           binaries, units, firewall), never its fabric's keys, tokens or certificates
#   install.sh wcl2.…       join this box to a fabric with a one-time invite from the Console
#   install.sh people       on the first box: set the owner's password and start the people mesh, when the
#                           install ran without a terminal to ask in
#   install.sh takeover     on a steward's manager: become the writer when no Console can be reached
#   install.sh uninstall    remove everything WeCoLab added to this box, and nothing else
#
# It asks only for what the fabric cannot make itself. Unattended, set WECOLAB_ZONE, WECOLAB_EMAIL,
# WECOLAB_PASSWORD, WECOLAB_SITE and WECOLAB_PROJECT. WECOLAB_LAPTOP=1 joins a laptop's VM (WeCoLab for Mac
# sets it). WECOLAB_DEV=1 is the development fabric (docs/development.md). Re-runnable: every step checks
# before it acts. Secrets travel in files, stdin and the environment, never on a command line.
set -euo pipefail

NEBULA_VERSION=v1.11.2
K3S_VERSION=v1.36.4+k3s1
SOPS_VERSION=v3.13.3
GO_VERSION=1.26.3
NETBIRD_VERSION=v0.80.0
# Release payload hashes are independent of remote checksum files and are checked even for cached bytes.
NEBULA_SHA256_amd64=6140d33f2ec21ce7f6b655b5bc820e93a684d97e51d0ddcf907324b5b28aac1e
NEBULA_SHA256_arm64=85d10e7bc2d121193c1392a1a919172ded7c413f46e602138281cfa9fa1b0231
SOPS_SHA256_amd64=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b
SOPS_SHA256_arm64=53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77
K3S_SHA256_amd64=835873f37245fc615f547a2fe2af9402a347875f13fa64a1f136de644955ea3f
K3S_SHA256_arm64=c920706346d5ad4e5cd3c7bf1bb09ce71ebe07fec829e513e40f1caf98aed8bb
K3S_INSTALL_COMMIT=4dedb15be78017a8ddd5b9e81acd44f3481078ed
K3S_INSTALL_SHA256=46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad
NETBIRD_SHA256_amd64=47ffaba4fc3929f31795bd6c5232d6c29744d3169e2c93e6d9c84624f0ef6405
NETBIRD_SHA256_arm64=8cbd99fa068a7b0f3968b2d31dc341acc17dd1e97c3053e61ed5905dbdae7341
GO_SHA256_amd64=2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556  # go.dev/dl sums for GO_VERSION: change them with it
GO_SHA256_arm64=9d89a3ea57d141c2b22d70083f2c8459ba3890f2d9e818e7e933b75614936565
NETWORK=10.77.0.0/16
PEOPLE_NET=100.96.0.0/16
CERTS_PORT=8094                            # a steward's certificate service, over Nebula (nebula.PortCerts)
DEV=${WECOLAB_DEV:-}
STATE=/var/lib/wecolab
DIST=$STATE/dist
BUNDLE=$STATE/bundle.json                  # the join response, until the box is up
CACHE=${WECOLAB_CACHE:-}                   # development: downloads kept between runs
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export PATH=$PATH:/usr/local/bin

say() { printf '\n==> %s\n' "$*"; }
die() { printf 'wecolab: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "unsupported machine $(uname -m)" ;; esac
mkdir -p "$STATE" "$DIST" && chmod 700 "$STATE"

# $STATE/installed lists what WeCoLab added to this box, so uninstall removes exactly that and leaves
# whatever the box had before (docs/operations.md, "Remove WeCoLab from a box").
mark() { grep -qxF "$1" "$STATE/installed" 2>/dev/null || echo "$1" >> "$STATE/installed"; }
has() { grep -qxF "$1" "$STATE/installed" 2>/dev/null; }

# valid KIND VALUE: the fabric's rules for names (internal/validate), checked before anything is made of
# them; Warden and the Console check them again.
valid() {
  case $1 in
    name) [[ $2 =~ ^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$ ]] ;;
    label) [[ $2 =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] ;;
    dns) [ ${#2} -le 253 ] && [[ $2 =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+$ ]] ;;
    email) [[ $2 =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$ ]] ;;
    ipv4) [[ $2 =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] ;;
    token) [[ $2 =~ ^[A-Za-z0-9._:-]+$ ]] ;;
    *) return 1 ;;
  esac
}
# as_label: text in a name's shape (lowercase letters, digits, inner dashes), e.g. from a host name.
as_label() { tr 'A-Z' 'a-z' | sed 's/[_.]/-/g; s/[^a-z0-9-]//g; s/^-*//; s/-*$//'; }

# fabric_made: this box belongs to a fabric already, joined or made here (a first box from before the done
# marker has an install.json without its secrets).
fabric_made() { [ -f "$STATE/done" ] || { [ -f "$STATE/install.json" ] && ! jq -e .siteKey "$STATE/install.json" >/dev/null 2>&1; }; }

# A box that already runs its own k3s or Nebula keeps them: WeCoLab needs its own and will not take them over.
preflight() {
  if ! has k3s && { command -v k3s >/dev/null || systemctl cat k3s.service k3s-agent.service >/dev/null 2>&1; }; then
    die "this box already runs k3s; WeCoLab needs a box without one"
  fi
  if ! has nebula-host && { [ -e /etc/nebula ] || systemctl cat nebula.service >/dev/null 2>&1; }; then
    die "this box already has Nebula (/etc/nebula or nebula.service); WeCoLab needs a box without one"
  fi
}

# The first box's NetBird client is the Door's: one that WeCoLab did not install is someone else's.
netbird_ours() { ! command -v netbird >/dev/null && [ ! -e /usr/local/bin/netbird ] && [ ! -L /usr/local/bin/netbird ] || has netbird || has netbird-direct || die "this box already has a NetBird client; the fabric's first box needs its own"; }
remove_netbird_binary() { ! has netbird-direct || rm -f /usr/local/bin/netbird; }

# ports_free: a public box serves the Door (80, 443, 53), NetBird (3478/udp, 127.0.0.1:8081) and a Nebula
# lighthouse (4242/udp). DNS on loopback (the box's own resolver) is no obstacle.
ports_free() {
  local busy
  busy=$(ss -Hlntu | awk '{n = split($5, a, ":"); p = a[n]; h = substr($5, 1, length($5) - length(p) - 1)}
    ($1 == "tcp" && (p == 80 || p == 443 || p == 8081)) || ($1 == "udp" && (p == 3478 || p == 4242)) || (p == 53 && h !~ /^(127\.|\[::1\])/) {print $1 " " $5}')
  [ -z "$busy" ] || die "this box already listens where a public site must: $(echo $busy)"
}

# ufw_allow RULE...: open a port in ufw, remembered only when it was not open already.
ufw_allow() {
  ufw show added 2>/dev/null | grep -qE "^ufw allow $*( comment .*)?\$" && return 0
  ufw allow "$@" >/dev/null && mark "ufw $*"
}

# ---------------------------------------------------------------------------------------------------
# Pieces every box needs

packages() {
  local want="curl jq iptables iproute2 ca-certificates $*" missing=""
  for p in $want; do dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"; done
  [ -z "$missing" ] && return 0
  export DEBIAN_FRONTEND=noninteractive
  dpkg --configure -a >/dev/null 2>&1 || true   # an apt run cut short (a reboot, a lost session) blocks every later one
  local apt=(apt-get -qq -o DPkg::Lock::Timeout=300)
  "${apt[@]}" update >/dev/null || die "apt-get update failed: fix this box's package sources, then re-run"
  "${apt[@]}" install -y $missing >/dev/null || die "apt-get could not install$missing"
  for p in $missing; do mark "pkg $p"; done
}

# fetch URL FILE: cache is only a transport, never a trust anchor; callers verify bytes before using them.
fetch() {
  local url=$1 dest=$2 tmp key=${3:-$(basename "$1")}
  if [ -n "$CACHE" ] && [ -f "$CACHE/$key" ]; then
    cp "$CACHE/$key" "$dest"
    return
  fi
  tmp=$(mktemp "${dest}.XXXXXXXX") || return 1
  if ! curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 ${DEV:+-k} -o "$tmp" "$url"; then
    rm -f "$tmp"
    return 1
  fi
  mv "$tmp" "$dest"
}

verify_payload() {
  local file=$1 expected=$2 actual
  actual=$(sha256sum "$file") || return 1
  [ "${actual%% *}" = "$expected" ] || die "pinned SHA-256 mismatch for $(basename "$file")"
}

# Only verified bytes are promoted to the offline cache. An interrupted download never replaces them.
cache_verified() {
  [ -z "$CACHE" ] || { mkdir -p "$CACHE" && cp "$1" "$CACHE/$(basename "$2")"; }
}

install_nebula() {
  if command -v nebula >/dev/null && nebula -version 2>/dev/null | grep -q "${NEBULA_VERSION#v}"; then
    has nebula || die "Nebula was installed outside WeCoLab"
    return 0
  fi
  local t; t=$(mktemp -d)
  local b=https://github.com/slackhq/nebula/releases/download/$NEBULA_VERSION
  fetch "$b/nebula-linux-$ARCH.tar.gz" "$t/nebula-linux-$ARCH.tar.gz" || die "could not fetch Nebula"
  local expected=NEBULA_SHA256_$ARCH
  verify_payload "$t/nebula-linux-$ARCH.tar.gz" "${!expected}"
  cache_verified "$t/nebula-linux-$ARCH.tar.gz" "$b/nebula-linux-$ARCH.tar.gz"
  tar -xzf "$t/nebula-linux-$ARCH.tar.gz" -C "$t" || die "invalid Nebula archive"
  mark nebula
  install -m 0755 "$t/nebula" "$t/nebula-cert" /usr/local/bin/ && rm -rf "$t"
  NEBULA_NEW=1
}

# sops for both machines in $STATE/bin, whatever this box has (the Console's and Warden's images carry their
# own), and on this box's PATH unless it has one already.
install_sops() {
  local b=https://github.com/getsops/sops/releases/download/$SOPS_VERSION t
  mkdir -p "$STATE/bin"
  local a expected
  for a in amd64 arm64; do
    local name=sops-$SOPS_VERSION.linux.$a
    t=$(mktemp -d)
    fetch "$b/$name" "$t/$name" || die "could not fetch SOPS for $a"
    expected=SOPS_SHA256_$a
    verify_payload "$t/$name" "${!expected}"
    cache_verified "$t/$name" "$b/$name"
    install -m 0755 "$t/$name" "$STATE/bin/$name" && rm -rf "$t"
  done
  command -v sops >/dev/null && return 0
  mark sops
  install -m 0755 "$STATE/bin/sops-$SOPS_VERSION.linux.$ARCH" /usr/local/bin/sops
}

# The box's Nebula key is born here and never leaves; only its public half is sent.
nebula_key() {
  mark nebula-host
  mkdir -p /etc/nebula/config.d
  [ -f /etc/nebula/host.key ] || nebula-cert keygen -out-key /etc/nebula/host.key -out-pub /etc/nebula/host.pub
  chmod 600 /etc/nebula/host.key
}

# bundle FIELD: a field of the join response ($BUNDLE), an array's items one per line, "" when absent.
# Scripts read the response only through this: cmd/console's tests check that every field named in a
# call is one the Console sends.
bundle() { jq -r --arg f "$1" '.[$f] | if type == "array" then .[] elif . == null then empty else . end' "$BUNDLE"; }

# ssh_keys JSON: the SSH keys the fabric says this box should have (its site owners' and the admins'), in a
# marked block of root's authorized_keys that is rewritten each time, so a person removed from the fabric
# loses access within the hour. Lines outside the block are the box's own. JSON without sshKeys (a steward
# from before they were sent) changes nothing.
ssh_keys() {
  local f=/root/.ssh/authorized_keys keys
  jq -e 'has("sshKeys")' "$1" >/dev/null 2>&1 || return 0
  keys=$(jq -r '.sshKeys[]? | select(type == "string" and test("\\A(ssh|ecdsa|sk)-[a-z0-9@.-]+ [A-Za-z0-9+/]+=*( [ -~]*)?\\z"))' "$1") || return 1
  [ -n "$keys" ] || [ -f "$f" ] || return 0
  # Each step checked here, not by set -e (off in a function called with ||): a half-written file must
  # never replace the box's own keys.
  mkdir -p -m 700 /root/.ssh || return 1
  { if [ -f "$f" ]; then sed '/^# BEGIN WeCoLab/,/^# END WeCoLab/d' "$f" || return 1; fi
    [ -z "$keys" ] || printf '# BEGIN WeCoLab: the fabric rewrites this block every hour; edits here are lost\n%s\n# END WeCoLab\n' "$keys"
  } > "$f.wecolab" && chmod 600 "$f.wecolab" && mv "$f.wecolab" "$f"
}

# ssh_unappend: the keys an install.sh from before the marked block appended one at a time ("ssh KEY" in
# $STATE/installed) move into the block, which every sync then keeps to the fabric's keys, and their
# marks go. Lines outside the block that are not such keys are the box's own and stay.
ssh_unappend() {
  local f=/root/.ssh/authorized_keys old=$STATE/ssh-appended
  sed -n 's/^ssh //p' "$STATE/installed" > "$old"
  if [ -s "$old" ] && [ -f "$f" ]; then
    awk 'NR == FNR {old[$0]; next} /^# BEGIN WeCoLab/ {b = 1} b || !($0 in old) {print} /^# END WeCoLab/ {b = 0}' "$old" "$f" > "$f.wecolab" \
      && chmod 600 "$f.wecolab" && mv "$f.wecolab" "$f" || return 1
    grep -q '^# BEGIN WeCoLab' "$f" || { jq -Rn '{sshKeys: [inputs]}' "$old" > "$old.json" && ssh_keys "$old.json"; } || return 1
  fi
  grep -vE '^ssh( .*|-dir|-file)$' "$STATE/installed" > "$STATE/installed.new" || true
  mv "$STATE/installed.new" "$STATE/installed" && rm -f "$old" "$old.json"
}

# nebula_files PUBLIC: the certificate, the fabric's settings and this box's own, from the join response.
nebula_files() {
  local port=0 lan
  bundle ca > /etc/nebula/ca.crt
  bundle cert > /etc/nebula/host.crt
  bundle config > /etc/nebula/config.d/20-fabric.yml
  bundle stewards > /etc/nebula/stewards
  bundle box > /etc/nebula/name
  bundle life > /etc/nebula/life
  [ "$1" != true ] || port=4242
  lan=$(ip -o -f inet addr show "$(ip route show default | awk '{print $5; exit}')" | awk '{print $4; exit}')
  cat > /etc/nebula/config.d/10-local.yml <<YAML
# this box's own Nebula settings, written when it joined
listen: { host: "::", port: $port }
tun: { dev: nebula1, mtu: 1300 }
preferred_ranges: ["$lan"]
logging: { level: info, format: json }
YAML
}

nebula_unit() {
  cat > /etc/systemd/system/nebula.service <<'UNIT'
[Unit]
Description=Nebula: the fabric's mesh between boxes
Wants=basic.target network-online.target nss-lookup.target time-sync.target
After=basic.target network.target network-online.target
[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/bin/nebula -config /etc/nebula/config.d
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload && systemctl enable nebula >/dev/null 2>&1
}

# start_nebula PUBLIC: the files from the join response, then the service.
start_nebula() {
  nebula_files "$1"
  nebula_unit
  nebula -test -config /etc/nebula/config.d >/dev/null || die "nebula refuses its configuration"
  systemctl restart nebula
  for _ in $(seq 30); do ip -o addr show dev nebula1 2>/dev/null | grep -q inet && break; sleep 1; done
  ip -o addr show dev nebula1 | grep -q inet || die "nebula did not come up"
  if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
    if [ "$1" = true ]; then for p in 80/tcp 443/tcp 53/tcp 53/udp 3478/udp 4242/udp; do ufw_allow "$p"; done; fi
    ufw_allow in on nebula1
  fi
  sync_timer
}

# Every hour the box asks a steward for its CA bundle, the fabric's blocklist and lighthouses, its SSH
# keys, and a new certificate when a third of its life is left (docs/operations.md, "Certificates"). An
# answer replaces the files only once Nebula accepts it on a staging copy; the files it replaced are kept
# as .bak. A missing field or a bad certificate leaves the box as it was, with its mesh up.
sync_script() {
  printf '#!/bin/bash\n# written by install.sh: this box'"'"'s Nebula files and SSH keys from a steward\nset -eu\nN=/etc/nebula PORT=%s\n' "$CERTS_PORT"
  declare -f ssh_keys
  cat <<'SH'
name=$(cat $N/name)
# stage STEWARD DIR: the steward's answer, complete, in DIR/new, and whether Nebula accepts it
stage() {
  local t=$2
  mkdir -p "$t/new" "$t/test"
  curl -fsS -m 20 -X POST --data-binary @$N/host.crt -o "$t/out" "http://$1:$PORT/nebula/$name" || return 1
  jq -er .ca "$t/out" > "$t/new/ca.crt" || return 1
  jq -er .config "$t/out" > "$t/new/20-fabric.yml" || return 1
  jq -r '.stewards[]? | select(test("^[0-9]{1,3}([.][0-9]{1,3}){3}$"))' "$t/out" > "$t/new/stewards" && [ -s "$t/new/stewards" ] || return 1
  if jq -e .cert "$t/out" >/dev/null; then jq -r .cert "$t/out" > "$t/new/host.crt"; else cp $N/host.crt "$t/new/host.crt"; fi
  cp $N/config.d/10-local.yml "$t/test/"
  sed -e "s#$N/ca.crt#$t/new/ca.crt#" -e "s#$N/host.crt#$t/new/host.crt#" "$t/new/20-fabric.yml" > "$t/test/20-fabric.yml"
  nebula -test -config "$t/test" >/dev/null 2>&1
}
pending=$N/.wecolab-reload-pending
rollback=$N/.wecolab-sync-rollback
t=""
trap '[ -z "$t" ] || rm -rf "$t"' EXIT
restore_old() {
  local f
  for f in ca.crt host.crt config.d/20-fabric.yml stewards; do
    [ -f "$rollback/$f" ] && cp -p "$rollback/$f" "$N/$f" ||
      { echo "could not restore $f; recover from $rollback manually" >&2; return 1; }
  done
  if ! systemctl reload nebula; then
    echo "old Nebula settings could not be reloaded; recover from $rollback manually" >&2
    return 1
  fi
  rm -rf "$rollback"
  rm -f "$pending"
}
# A killed sync can leave changed files on disk; restore its private snapshot before fetching again.
if [ -d "$rollback" ]; then
  restore_old || exit 1
fi
for s in $(cat "$N/stewards"); do
  t=$(mktemp -d)
  if stage "$s" "$t"; then
    changed=""
    for f in ca.crt host.crt config.d/20-fabric.yml stewards; do
      new=$t/new/${f#config.d/}
      cmp -s "$new" "$N/$f" || changed=1
    done
    if [ -n "$changed" ]; then
      mkdir -p "$t/old/config.d"
      for f in ca.crt host.crt config.d/20-fabric.yml stewards; do
        cp -p "$N/$f" "$t/old/$f" || exit 1
      done
      mv "$t/old" "$rollback" || exit 1
      touch "$pending" || exit 1
      for f in ca.crt host.crt config.d/20-fabric.yml stewards; do
        new=$t/new/${f#config.d/}
        cmp -s "$new" "$N/$f" && continue
        if ! cp -p "$N/$f" "$N/${f#config.d/}.bak" ||
           ! install -m 0644 "$new" "$N/$f"; then
          echo "could not install Nebula $f; restoring old settings" >&2
          restore_old || exit 1
          exit 1
        fi
      done
    fi
    if [ -n "$changed" ] || [ -e "$pending" ]; then
      if ! systemctl reload nebula; then
        echo "Nebula reload failed; restoring old settings" >&2
        [ ! -d "$rollback" ] || restore_old
        exit 1
      fi
      rm -rf "$rollback"
      rm -f "$pending"
    fi
    ssh_keys "$t/out" || { echo "SSH keys not updated" >&2; exit 1; }
    exit 0
  fi
  rm -rf "$t"; t=""
done
echo "no steward answered with settings Nebula accepts" >&2; exit 1
SH
}

sync_timer() {
  sync_script > /usr/local/sbin/wecolab-nebula-sync
  chmod 755 /usr/local/sbin/wecolab-nebula-sync
  local every=1h
  [ "$(cat /etc/nebula/life 2>/dev/null)" -le 3600 ] 2>/dev/null && every=5m
  printf '[Unit]\nDescription=WeCoLab: Nebula certificate, fabric settings and SSH keys\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/wecolab-nebula-sync\n' > /etc/systemd/system/wecolab-nebula-sync.service
  # The first sync waits: at a join the stewards learn of the box from the Fabric a minute or two later.
  printf '[Unit]\nDescription=WeCoLab: sync Nebula every %s\n[Timer]\nOnActiveSec=10m\nOnUnitActiveSec=%s\nRandomizedDelaySec=60\n[Install]\nWantedBy=timers.target\n' "$every" "$every" > /etc/systemd/system/wecolab-nebula-sync.timer
  systemctl daemon-reload && systemctl enable --now wecolab-nebula-sync.timer >/dev/null 2>&1
}

# k3s_config: this box's k3s settings, from the join response. A site's manager is a k3s server; any other
# box an agent of its site's manager. Nodes hold only the agent token, which cannot fetch the cluster's
# bootstrap data or keys (the server token is an administrator's). k3s listens on the box's Nebula address
# and loopback.
k3s_config() {
  local role ip name token agent server laptop
  role=$(bundle role) ip=$(bundle ip) name=$(bundle box) token=$(bundle k3sToken) agent=$(bundle k3sAgentToken)
  server=$(bundle k3sServer) laptop=$(bundle laptop)
  valid label "$name" && valid ipv4 "$ip" && valid token "$token" && { [ -z "$agent" ] || valid token "$agent"; } \
    && { [ "$role" = manager ] || [[ $server =~ ^https://[0-9.]+:6443$ ]]; } || die "the join response is malformed"
  printf 'node-name: %s\nnode-ip: %s\nbind-address: %s\nflannel-iface: nebula1\ntoken: %s\nkube-proxy-arg: ["nodeport-addresses=%s"]\n' \
    "$name" "$ip" "$ip" "$token" "$NETWORK"
  if [ "$role" = manager ]; then
    [ -z "$agent" ] || printf 'agent-token: %s\n' "$agent"
    cat <<YAML
advertise-address: $ip
tls-san: [$ip]
flannel-backend: vxlan
disable: [traefik, servicelb]
secrets-encryption: true
write-kubeconfig-mode: "0600"
kube-apiserver-arg: ["admission-control-config-file=/etc/rancher/k3s/psa.yaml"]
YAML
  else
    printf 'server: %s\n' "$server"
    # A laptop registers as in use: no work lands on it before its node agent has read the mode file.
    [ "$laptop" != true ] || printf 'node-label: ["wecolab.io/laptop=true"]\nnode-taint: ["wecolab.io/laptop=true:NoSchedule", "wecolab.io/idle=true:NoSchedule", "wecolab.io/idle=true:NoExecute"]\n'
  fi
  # The development fabric shares a laptop's disk: evict only when it is really full.
  [ -z "$DEV" ] || printf 'kubelet-arg: ["eviction-hard=imagefs.available<1Gi,nodefs.available<1Gi", "image-gc-high-threshold=99", "image-gc-low-threshold=98"]\n'
}

# apparmor: containerd's AppArmor profile for containers, loaded before k3s starts: containerd makes
# its own only when none of that name is loaded. It is containerd's template (contrib/apparmor/template.go)
# plus peers stacked on the profile. From kernel 6.17 (Ubuntu 26.04) a container's processes can carry
# the label cri-containerd.apparmor.d//&unconfined, which containerd's rules do not name yet, so a process
# could not signal or trace its own children: Prisma could not stop its engine, which kept a lock
# (k3s-io/k3s#13625; canonical/k8s-snap#2750 adds the same rules). On older kernels they match nothing.
# Replacing the loaded profile reaches running containers at once.
apparmor() {
  [ -r /sys/kernel/security/apparmor/profiles ] && command -v apparmor_parser >/dev/null || return 0
  local p=/etc/apparmor.d/cri-containerd.apparmor.d s=$STATE/apparmor tmp digest
  [ ! -L "$p" ] && [ ! -L "$s" ] || die "AppArmor profile or recovery state is a symlink; refusing replacement"
  [ ! -d "$s" ] || [ -f "$s/digest" ] || die "incomplete AppArmor recovery state at $s; restore it manually"
  ! has apparmor || [ -d "$s" ] || die "AppArmor ownership predates recovery state; refusing replacement"
  tmp=$(mktemp /etc/apparmor.d/.wecolab-profile.XXXXXX)
  { [ ! -f /etc/apparmor.d/abi/3.0 ] || echo 'abi <abi/3.0>,'
    cat <<'AA'
#include <tunables/global>

# WeCoLab (install.sh): containerd's profile for containers, with peers stacked on it.
profile cri-containerd.apparmor.d flags=(attach_disconnected,mediate_deleted) {
  #include <abstractions/base>

  network,
  capability,
  file,
  umount,
  signal (receive) peer=unconfined,
  signal (receive) peer=runc,
  signal (receive) peer=crun,
  signal (send,receive) peer=cri-containerd.apparmor.d,
  signal (send,receive) peer=cri-containerd.apparmor.d//&*,

  deny @{PROC}/* w,
  deny @{PROC}/{[^1-9/],[^1-9/][^0-9/],[^1-9s/][^0-9y/][^0-9s/],[^1-9/][^0-9/][^0-9/][^0-9/]*}/** w,
  deny @{PROC}/sys/[^k]** w,
  deny @{PROC}/sys/kernel/{?,??,[^s][^h][^m]**} w,
  deny @{PROC}/sysrq-trigger rwklx,
  deny @{PROC}/kcore rwklx,

  deny mount,

  deny /sys/[^f]*/** wklx,
  deny /sys/f[^s]*/** wklx,
  deny /sys/fs/[^c]*/** wklx,
  deny /sys/fs/c[^g]*/** wklx,
  deny /sys/fs/cg[^r]*/** wklx,
  deny /sys/firmware/** rwklx,
  deny /sys/devices/virtual/powercap/** rwklx,
  deny /sys/kernel/security/** rwklx,

  ptrace (trace,tracedby,read,readby) peer=cri-containerd.apparmor.d,
  ptrace (trace,tracedby,read,readby) peer=cri-containerd.apparmor.d//&*,
}
AA
  } > "$tmp"
  chmod 644 "$tmp"
  digest=$(sha256sum "$tmp"); digest=${digest%% *}
  if [ -d "$s" ]; then
    [ ! -L "$s/digest" ] && [ ! -L "$s/original" ] && [ ! -L "$s/next-digest" ] ||
      { rm -f "$tmp"; die "AppArmor recovery state contains a symlink"; }
    local current; current=$(sha256sum "$p" 2>/dev/null); current=${current%% *}
    [ -f "$p" ] && { [ "$current" = "$(cat "$s/digest")" ] ||
      { [ -f "$s/next-digest" ] && [ "$current" = "$(cat "$s/next-digest")" ]; }; } ||
      { rm -f "$tmp"; die "AppArmor profile changed outside WeCoLab; original retained at $s"; }
  else
    local snapshot
    snapshot=$(mktemp -d "$STATE/.apparmor.XXXXXX")
    if [ -e "$p" ]; then cp -p "$p" "$snapshot/original"; else touch "$snapshot/original-absent"; fi
    if grep -q '^cri-containerd.apparmor.d ' /sys/kernel/security/apparmor/profiles; then
      touch "$snapshot/original-loaded"
    else
      touch "$snapshot/original-unloaded"
    fi
    printf '%s\n' "$digest" > "$snapshot/digest"
    mv "$snapshot" "$s"
  fi
  printf '%s\n' "$digest" > "$s/next-digest"
  if ! apparmor_parser -r "$tmp" || ! mv "$tmp" "$p"; then
    rm -f "$tmp"
    apparmor_restore || die "AppArmor refused profile; original retained at $s"
    die "AppArmor refused containerd's profile"
  fi
  touch "$s/committed"
  mv "$s/next-digest" "$s/digest"
  mark apparmor
}

apparmor_restore() {
  local p=/etc/apparmor.d/cri-containerd.apparmor.d s=$STATE/apparmor
  [ ! -L "$p" ] && [ ! -L "$s" ] || { echo "AppArmor symlink conflict: $p or $s" >&2; return 1; }
  [ -d "$s" ] && [ -f "$s/digest" ] || { echo "missing AppArmor recovery state at $s" >&2; return 1; }
  for part in original original-absent original-loaded original-unloaded digest next-digest committed; do
    [ ! -L "$s/$part" ] || { echo "AppArmor recovery symlink conflict: $s/$part" >&2; return 1; }
  done
  [ -f "$p" ] || [ ! -f "$s/committed" ] ||
    { echo "AppArmor profile removed externally: $p; original retained at $s" >&2; return 1; }
  if [ -f "$p" ]; then
    local digest; digest=$(sha256sum "$p"); digest=${digest%% *}
    if [ "$digest" != "$(cat "$s/digest")" ] &&
       { [ ! -f "$s/next-digest" ] || [ "$digest" != "$(cat "$s/next-digest")" ]; }; then
      # A parser rejection leaves the original file on disk, not our generated one.
      [ -f "$s/original" ] && cmp -s "$s/original" "$p" ||
        { echo "AppArmor profile modified externally: $p; original retained at $s" >&2; return 1; }
    fi
  fi
  if [ -f "$s/original" ]; then
    [ ! -L "$s/original" ] || return 1
    cp -p "$s/original" "$p" || return 1
    if [ -f "$s/original-loaded" ]; then
      apparmor_parser -r "$p" || return 1
    else
      apparmor_parser -R "$p" || return 1
    fi
  elif [ -f "$s/original-absent" ]; then
    [ ! -f "$p" ] || { apparmor_parser -R "$p" && rm -f "$p"; } || return 1
  else
    echo "missing original AppArmor profile metadata at $s" >&2; return 1
  fi
  rm -rf "$s"
}

k3s_dependencies() {
  mkdir -p /etc/systemd/system/k3s.service.d /etc/systemd/system/k3s-agent.service.d
  printf '[Unit]\nWants=nebula.service\nAfter=nebula.service wecolab-pod-isolation.service\nRequires=wecolab-pod-isolation.service\n' |
    tee /etc/systemd/system/k3s.service.d/10-nebula.conf > /etc/systemd/system/k3s-agent.service.d/10-nebula.conf
  systemctl daemon-reload
}

# An interrupted install can have its binary but not its unit. Never replace an existing identity.
k3s_identity() {
  local config=/etc/rancher/k3s/config.yaml role=$1 name=$2 ip=$3 key value expected
  [ -f "$config" ] || return 0
  [ ! -L "$config" ] || die "k3s configuration is a symlink; refusing to replace its identity"
  for key in node-name node-ip bind-address; do
    case "$key" in node-name) expected=$name ;; *) expected=$ip ;; esac
    value=$(sed -n "s/^$key: //p" "$config")
    [ "$value" = "$expected" ] || die "existing k3s $key differs from the join identity; refusing to overwrite it"
  done
  if [ "$role" = manager ]; then
    ! grep -q '^server:' "$config" || die "existing k3s config is an agent, not a manager"
    [ "$(sed -n 's/^agent-token: //p' "$config")" = "$(bundle k3sAgentToken)" ] ||
      die "existing k3s agent token differs from the join identity"
  else
    [ "$(sed -n 's/^server: //p' "$config")" = "$(bundle k3sServer)" ] ||
      die "existing k3s server differs from the join identity"
  fi
  [ "$(sed -n 's/^token: //p' "$config")" = "$(bundle k3sToken)" ] ||
    die "existing k3s token differs from the join identity"
}

# install_k3s: k3s as k3s_config says, with the firewall up first.
install_k3s() {
  local role name unit skip="" t
  role=$(bundle role) name=$(bundle box)
  unit=k3s-agent.service; [ "$role" != manager ] || unit=k3s.service
  has k3s || { [ ! -e /etc/rancher/k3s/config.yaml ] &&
    ! command -v k3s >/dev/null && ! systemctl cat k3s.service k3s-agent.service >/dev/null 2>&1; } ||
    die "existing k3s is not owned by WeCoLab"
  local other=k3s.service
  [ "$role" != manager ] || other=k3s-agent.service
  ! systemctl cat "$other" >/dev/null 2>&1 || die "existing k3s unit has a different role"
  k3s_identity "$role" "$name" "$(bundle ip)"
  mkdir -p /etc/rancher/k3s
  k3s_dependencies
  # Own the incomplete setup before writing its config or copying its cached binary.
  mark k3s
  if [ "$role" = manager ]; then
    # Pod Security is "restricted" everywhere but the site's own infrastructure namespaces.
    cat > /etc/rancher/k3s/psa.yaml <<YAML
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
  - name: PodSecurity
    configuration:
      apiVersion: pod-security.admission.config.k8s.io/v1
      kind: PodSecurityConfiguration
      defaults: { enforce: restricted, enforce-version: latest, audit: restricted, audit-version: latest, warn: restricted, warn-version: latest }
      exemptions: { usernames: [], runtimeClasses: [], namespaces: [kube-system, kube-public, kube-node-lease, flux-system, cert-manager, cnpg-system, wecolab-system] }
YAML
  fi
  if [ ! -f /etc/rancher/k3s/config.yaml ]; then
    local config_tmp
    config_tmp=$(mktemp /etc/rancher/k3s/.config.XXXXXX)
    (umask 077; k3s_config > "$config_tmp") || { rm -f "$config_tmp"; return 1; }
    chmod 600 "$config_tmp" && mv "$config_tmp" /etc/rancher/k3s/config.yaml
  fi
  firewall "$(bundle public)"
  apparmor
  if ! systemctl cat "$unit" >/dev/null 2>&1; then
    t=$(mktemp -d)
    local expected=K3S_SHA256_$ARCH binary=k3s
    [ "$ARCH" != arm64 ] || binary=k3s-arm64
    if [ -n "$CACHE" ] && [ -f "$CACHE/k3s-$ARCH" ]; then
      verify_payload "$CACHE/k3s-$ARCH" "${!expected}"
      install -m 0755 "$CACHE/k3s-$ARCH" /usr/local/bin/k3s
    fi
    if command -v k3s >/dev/null && [ -n "$CACHE" ] && [ -f "$CACHE/k3s-$ARCH" ]; then
      skip=true
    elif command -v k3s >/dev/null && has k3s && verify_payload "$(command -v k3s)" "${!expected}"; then
      skip=true
    else
      fetch "https://github.com/k3s-io/k3s/releases/download/$K3S_VERSION/$binary" "$t/k3s-$ARCH" "k3s-$ARCH" ||
        die "could not fetch pinned k3s binary"
      verify_payload "$t/k3s-$ARCH" "${!expected}"
      cache_verified "$t/k3s-$ARCH" "k3s-$ARCH"
      install -m 0755 "$t/k3s-$ARCH" /usr/local/bin/k3s
      skip=true
    fi
    fetch "https://raw.githubusercontent.com/k3s-io/k3s/$K3S_INSTALL_COMMIT/install.sh" "$t/k3s-install.sh" k3s-install.sh ||
      die "could not fetch pinned k3s installer"
    verify_payload "$t/k3s-install.sh" "$K3S_INSTALL_SHA256"
    cache_verified "$t/k3s-install.sh" "k3s-install.sh"
    INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_SKIP_DOWNLOAD="$skip" INSTALL_K3S_EXEC="$([ "$role" = manager ] && echo server || echo agent)" sh "$t/k3s-install.sh" >/dev/null ||
      { rm -rf "$t"; return 1; }
    rm -rf "$t"
  fi
  systemctl enable "$unit" >/dev/null 2>&1
  systemctl start "$unit"
  if [ "$role" = manager ]; then
    for _ in $(seq 60); do kubectl get node "$name" >/dev/null 2>&1 && break; sleep 2; done
    kubectl wait --for=condition=Ready "node/$name" --timeout=300s >/dev/null
  fi
  kvm_label
}

# kvm_label: a box with KVM says so on its node, and Kata Containers goes only there (decision 26). It is
# the kubelet that labels, as it may its own node; k3s's node-label would apply only at registration, and
# this runs at every converge too.
kvm_label() {
  [ -c /dev/kvm ] || return 0
  [ -e /opt/kata ] || has kata || mark kata   # where kata-deploy installs; uninstall removes it if it is ours
  local node; node=$(sed -n 's/^node-name: //p' /etc/rancher/k3s/config.yaml)
  for _ in $(seq 60); do
    k3s kubectl --kubeconfig /var/lib/rancher/k3s/agent/kubelet.kubeconfig label node "$node" wecolab.io/kvm=true --overwrite >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "note: could not label $node wecolab.io/kvm=true; workspaces will not run on it until a converge does" >&2
}

# firewall_script PUBLIC: WeCoLab's own iptables chains, applied at every boot before k3s starts (pods ran
# unfiltered until they were, before).
#
# WECOLAB-HOST (filter INPUT, IPv4 and IPv6): only Nebula reaches k3s. The API server and supervisor
# (6443), the kubelet (10250), flannel's VXLAN (8472/udp: the kernel binds it on every address, whatever
# k3s is told) and NetBird's metrics (9091: its configuration takes a port, not an address, and it listens
# on both families) are dropped from anywhere but loopback, Nebula and the pod network, whether or not ufw
# is active. A box without IPv6 gets the IPv4 chain only.
#
# WECOLAB-POD (mangle PREROUTING from cni0): pods may reach the internet, their own cluster and DNS (the
# box's own resolver too), never this box's other services, its LAN or the meshes, except the fabric's own
# ports that its system pods use over Nebula. Replies to connections the box admitted (the Door reaching an
# app over Nebula) pass: the chain runs after connection tracking. At a public box the Door's ports are
# the internet's, so pods reach them there as from anywhere (the Console signs people in through
# mesh.<zone> on the same box).
firewall_script() {
  local door="" limit=""
  [ "${1:-}" != true ] || door="\$ipt -A WECOLAB-POD -m addrtype --dst-type LOCAL -p tcp -m multiport --dports 80,443 -j RETURN"
  # The Door waits on 443 without a read timeout (door.yaml), so one source may hold only so many
  # connections. ponytail: 200 per address (IPv6 too: per /128); a /64 mask or hashlimit if abuse needs it.
  [ "${1:-}" != true ] || limit="  \$ipt -A WECOLAB-HOST -p tcp --syn --dport 443 -m connlimit --connlimit-above 200 -j REJECT --reject-with tcp-reset"
  cat <<SH
#!/bin/sh
set -e
for ipt in iptables ip6tables; do
  [ \$ipt = iptables ] || \$ipt -nL INPUT >/dev/null 2>&1 || continue
  \$ipt -N WECOLAB-HOST 2>/dev/null || true
  \$ipt -F WECOLAB-HOST
  for i in lo nebula1 cni0 flannel.1; do \$ipt -A WECOLAB-HOST -i \$i -j RETURN; done
$limit
  \$ipt -A WECOLAB-HOST -p tcp -m multiport --dports 6443,10250,9091 -j DROP
  \$ipt -A WECOLAB-HOST -p udp --dport 8472 -j DROP
  \$ipt -C INPUT -j WECOLAB-HOST 2>/dev/null || \$ipt -I INPUT 1 -j WECOLAB-HOST
done

iptables -t raw -D PREROUTING -i cni0 -j WECOLAB-POD 2>/dev/null || true   # where earlier versions put it
ipt="iptables -t mangle"
\$ipt -N WECOLAB-POD 2>/dev/null || true
\$ipt -F WECOLAB-POD
\$ipt -A WECOLAB-POD -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
\$ipt -A WECOLAB-POD -d 10.42.0.0/16 -j RETURN
\$ipt -A WECOLAB-POD -d 10.43.0.0/16 -j RETURN
\$ipt -A WECOLAB-POD -p udp --dport 53 -j RETURN
\$ipt -A WECOLAB-POD -p tcp --dport 53 -j RETURN
\$ipt -A WECOLAB-POD -m addrtype --dst-type LOCAL -p tcp -m multiport --dports 6443,10250 -j RETURN
\$ipt -A WECOLAB-POD -d $NETWORK -p tcp -m multiport --dports 8093,8094,30300 -j RETURN
$door
\$ipt -A WECOLAB-POD -m addrtype --dst-type LOCAL -j DROP
for n in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16; do \$ipt -A WECOLAB-POD -d \$n -j DROP; done
\$ipt -C PREROUTING -i cni0 -j WECOLAB-POD 2>/dev/null || \$ipt -I PREROUTING 1 -i cni0 -j WECOLAB-POD
SH
}

# firewall PUBLIC: firewall_script as a unit that runs before k3s.
firewall() {
  firewall_script "${1:-}" > /usr/local/sbin/wecolab-pod-isolation
  chmod +x /usr/local/sbin/wecolab-pod-isolation
  printf '[Unit]\nDescription=WeCoLab: only Nebula reaches k3s; pods reach neither this box nor its networks\nBefore=k3s.service k3s-agent.service\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/local/sbin/wecolab-pod-isolation\n[Install]\nWantedBy=multi-user.target\n' > /etc/systemd/system/wecolab-pod-isolation.service
  systemctl daemon-reload && systemctl enable wecolab-pod-isolation >/dev/null 2>&1 && systemctl restart wecolab-pod-isolation
}

# import_tar FILE REF: into k3s's containerd, pinned so the kubelet's image cleanup never takes it (it
# cannot be pulled again from anywhere), and kept where k3s imports it on every start.
import_tar() {
  mkdir -p /var/lib/rancher/k3s/agent/images && cp "$1" /var/lib/rancher/k3s/agent/images/
  for _ in $(seq 30); do
    k3s ctr images import "$1" >/dev/null 2>&1 || true
    if k3s ctr images ls -q 2>/dev/null | grep -qxF "$2"; then
      k3s ctr images label "$2" io.cri-containerd.pinned=pinned >/dev/null 2>&1 || true
      return 0
    fi
    sleep 3
  done
  die "could not import $2"
}

dl() { # dl VERSION SOURCE IMAGE ARCH: one of WeCoLab's image tarballs into $DIST, whole or not at all
  local f="$DIST/$3-$1-$4.tar"
  [ -f "$f" ] || { fetch "$2/dl/$3-$1-$4.tar" "$f.part" && mv "$f.part" "$f"; }
}

import_images() { # import_images VERSION SOURCE IMAGES...: WeCoLab's images for this machine
  local v=$1 src=$2; shift 2
  for img in "$@"; do
    dl "$v" "$src" "$img" "$ARCH"
    import_tar "$DIST/$img-$v-$ARCH.tar" "ghcr.io/wecolabhq/$img:$v"
  done
}

# ---------------------------------------------------------------------------------------------------
# A site's manager: its copy of the Fabric, and Flux on it

# api ARGS...: Forgejo's API as the fabric account; the token reaches curl through a file descriptor.
api() { curl -fsS -H @<(printf 'Authorization: token %s\n' "$TOKEN") -H 'Content-Type: application/json' "$@"; }
# gitfj ARGS...: git with the token in its environment (GIT_CONFIG_*).
gitfj() { GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" git "$@"; }

# put_secret NS NAME KEY VALUE...: a Secret applied through stdin (server-side, so no copy of it is kept
# in an annotation).
put_secret() {
  local ns=$1 name=$2 data='{}'; shift 2
  while [ $# -gt 1 ]; do data=$(K=$1 V=$2 jq -c '. + {(env.K): (env.V | @base64)}' <<<"$data"); shift 2; done
  jq -c --arg ns "$ns" --arg n "$name" '{apiVersion: "v1", kind: "Secret", type: "Opaque", metadata: {name: $n, namespace: $ns}, data: .}' <<<"$data" \
    | kubectl apply --server-side --force-conflicts --field-manager=wecolab-install -f - >/dev/null
}

forgejo_up() { # forgejo_up MANIFEST MIRROR_PASSWORD PUSHERS...
  local manifest=$1 mirror=$2; shift 2
  kubectl get ns wecolab-system >/dev/null 2>&1 || kubectl create ns wecolab-system >/dev/null
  kubectl label ns wecolab-system pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
  kubectl -n wecolab-system get secret forgejo >/dev/null 2>&1 || put_secret wecolab-system forgejo secret-key "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  kubectl apply --server-side --field-manager=wecolab-install -f "$manifest" >/dev/null
  kubectl -n wecolab-system rollout status deploy/forgejo --timeout=600s >/dev/null
  fj() { kubectl -n wecolab-system exec deploy/forgejo -- forgejo "$@"; }
  if ! kubectl -n wecolab-system get secret git >/dev/null 2>&1; then
    fj admin user create --admin --username fabric --email fabric@wecolab.invalid --random-password --must-change-password=false >/dev/null 2>&1 || true   # an earlier run's
    local tok; tok=$(fj admin user generate-access-token -u fabric -t "wecolab-$(date +%s)" --scopes all --raw)
    put_secret wecolab-system git token "$tok"
  fi
  TOKEN=$(kubectl -n wecolab-system get secret git -o jsonpath='{.data.token}' | base64 -d)
  FJ="http://$(kubectl -n wecolab-system get svc forgejo -o jsonpath='{.spec.clusterIP}'):3000/api/v1"
  api "$FJ/users/mirror" >/dev/null 2>&1 || MIRROR=$mirror jq -n '{username: "mirror", email: "mirror@wecolab.invalid", password: env.MIRROR, must_change_password: false}' \
    | api -X POST "$FJ/admin/users" -d @- >/dev/null
  api "$FJ/repos/fabric/fabric" >/dev/null 2>&1 || api -X POST "$FJ/user/repos" -d '{"name":"fabric","private":true,"default_branch":"main"}' >/dev/null
  api -X PUT "$FJ/repos/fabric/fabric/collaborators/mirror" -d '{"permission":"write"}' >/dev/null
  jq -nc '{rule_name: "main", enable_push: true, enable_push_whitelist: true, push_whitelist_usernames: $ARGS.positional}' --args "$@" \
    | api -X POST "$FJ/repos/fabric/fabric/branch_protections" -d @- >/dev/null 2>&1 || true   # there already
}

# flux_up FLUX_MANIFEST AGE_KEY SITE: Flux on this site's own copy; everything else comes from it. Flux
# applies a Kustomization without a service account as nobody (the manifest's lockdown flags), so the
# platform's own name kustomize-controller's.
flux_up() {
  local manifest=$1 key=$2 site=$3 sa=kustomize-controller
  kubectl apply --server-side --field-manager=wecolab-install -f "$manifest" >/dev/null
  kubectl -n flux-system rollout status deploy/source-controller deploy/kustomize-controller --timeout=600s >/dev/null
  for d in helm-controller notification-controller image-reflector-controller image-automation-controller source-watcher; do
    kubectl -n flux-system delete deploy "$d" --ignore-not-found >/dev/null   # a fabric made before they were dropped
  done
  put_secret flux-system fabric-git username fabric password "$TOKEN"
  put_secret flux-system sops-age "$site.agekey" "$(cat "$key")"
  local sub; sub="SITE: \"$site\", ZONE: \"$ZONE\", NETWORK: \"$NETWORK\", PEOPLE_NET: \"$PEOPLE_NET\""
  kubectl apply -f - >/dev/null <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: { name: fabric, namespace: flux-system }
spec:
  url: http://forgejo.wecolab-system.svc:3000/fabric/fabric.git
  ref: { branch: main }
  interval: 1m
  secretRef: { name: fabric-git }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: crds, namespace: flux-system }
spec: { interval: 10m, path: ./crds, prune: false, serviceAccountName: $sa, sourceRef: { kind: GitRepository, name: fabric } }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: flux, namespace: flux-system }
spec: { interval: 1h, path: ./system/vendor/flux, prune: false, serviceAccountName: $sa, sourceRef: { kind: GitRepository, name: fabric } }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: cert-manager, namespace: flux-system }
spec: { interval: 1h, path: ./system/vendor/cert-manager, prune: false, wait: true, timeout: 10m, serviceAccountName: $sa, sourceRef: { kind: GitRepository, name: fabric } }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: cnpg, namespace: flux-system }
spec: { interval: 1h, path: ./system/vendor/cnpg, prune: false, wait: true, timeout: 10m, serviceAccountName: $sa, sourceRef: { kind: GitRepository, name: fabric } }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: barman, namespace: flux-system }
spec:
  interval: 1h
  path: ./system/vendor/barman
  prune: false
  wait: true
  timeout: 10m
  serviceAccountName: $sa
  dependsOn: [{ name: cert-manager }, { name: cnpg }]
  sourceRef: { kind: GitRepository, name: fabric }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: system, namespace: flux-system }
spec:
  interval: 10m
  path: ./system/wecolab
  prune: true
  serviceAccountName: $sa
  dependsOn: [{ name: crds }]
  sourceRef: { kind: GitRepository, name: fabric }
  postBuild: { substitute: { $sub } }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: fabric, namespace: flux-system }
spec:
  interval: 1m
  path: ./fabric
  prune: true
  serviceAccountName: $sa
  dependsOn: [{ name: crds }, { name: system }]
  sourceRef: { kind: GitRepository, name: fabric }
YAML
}

wait_ready() { # wait_ready KUSTOMIZATION SECONDS
  for _ in $(seq "$(( $2 / 5 ))"); do
    [ "$(kubectl -n flux-system get kustomization "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ] && return 0
    sleep 5
  done
  kubectl -n flux-system get kustomization "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' >&2; echo >&2
  return 1
}

# ---------------------------------------------------------------------------------------------------
# Joining with an invite

join() {
  local code=$1 inv url token host pub="" laptop=false
  [ -z "${WECOLAB_LAPTOP:-}" ] || laptop=true
  ! fabric_made || die "this box already belongs to a fabric: install.sh uninstall first"
  packages age
  inv=$(printf '%s' "${code#wcl2.}" | tr '_-' '/+' | awk '{l=length($0)%4; if(l) $0=$0 substr("===",1,4-l); print}' | base64 -d 2>/dev/null) || die "the invite does not decode"
  url=$(printf '%s' "$inv" | jq -r .c) token=$(printf '%s' "$inv" | jq -r .t)
  [[ $url =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "the invite names no Console"
  CURL=(curl -fsS --retry 3); [ -z "$DEV" ] || CURL+=(-k)
  install_nebula
  nebula_key
  host=$(hostname -s | as_label)
  valid label "$host" || die "this box's host name ($(hostname -s)) makes no name: give it one of letters, digits and dashes"
  # A new site's age key is born here; only its public half leaves.
  [ -f "$STATE/site.agekey" ] || (umask 077; age-keygen -o "$STATE/site.agekey" 2>/dev/null)
  local agepub; agepub=$(age-keygen -y "$STATE/site.agekey")
  pub=${WECOLAB_PUBLIC:-$(curl -4fsS -m 5 https://api.ipify.org 2>/dev/null || true)}
  [ -n "$DEV" ] && pub=$(ip -o -f inet addr show "$(ip route show default | awk '{print $5; exit}')" | awk '{print $4}' | cut -d/ -f1)
  say "joining through $url"
  if [ ! -f "$BUNDLE" ]; then
    local body; body=$(CODE=$token jq -n --arg host "$host" --arg key "$(cat /etc/nebula/host.pub)" --arg age "$agepub" --arg pub "$pub" --arg arch "$ARCH" \
      --argjson laptop "$laptop" '{code: env.CODE, host: $host, key: $key, age: $age, public: $pub, arch: $arch, laptop: $laptop}')
    (umask 077; printf '%s' "$body" | "${CURL[@]}" -X POST -H 'Content-Type: application/json' --data-binary @- -o "$BUNDLE.part" "$url/join") \
      || { rm -f "$BUNDLE.part"; die "the Console refused the invite (used, expired, or unknown)"; }
    mv "$BUNDLE.part" "$BUNDLE"
  fi
  local role site public v
  role=$(bundle role) site=$(bundle site) public=$(bundle public) v=$(bundle version) ZONE=$(bundle zone)
  { [ "$role" = manager ] || [ "$role" = node ]; } && valid label "$site" && valid dns "$ZONE" && valid token "$v" || die "the join response is malformed"
  [ "$public" = true ] || public=false
  printf 'ROLE=%s\nPUBLIC=%s\n' "$role" "$public" > "$STATE/host.env"
  say "box $(bundle box) at $(bundle ip): $role of site $site"
  [ "$public" = false ] || systemctl is-active -q nebula || ports_free
  ssh_keys "$BUNDLE"
  start_nebula "$public"
  install_k3s
  if [ "$role" = manager ]; then
    say "this site's copy of the Fabric"
    import_images "$v" "$url" warden console
    # Both machines' images, so this site's Console, at a steward, serves /dl/ to any box that joins. Only
    # for later boxes: a Console without one does not stop this join.
    for a in amd64 arm64; do
      for img in warden console; do dl "$v" "$url" $img $a || echo "note: the $a $img image was not fetched; this site cannot serve it" >&2; done
    done
    "${CURL[@]}" "$url/fabric/system/wecolab/forgejo.yaml" -o "$STATE/forgejo.yaml"
    "${CURL[@]}" "$url/fabric/system/vendor/flux/manifest.yaml" -o "$STATE/flux.yaml"
    forgejo_up "$STATE/forgejo.yaml" "$(bundle mirror)" mirror
    say "waiting for the writer to push the Fabric here"
    for _ in $(seq 120); do api "$FJ/repos/fabric/fabric/branches/main" >/dev/null 2>&1 && break; sleep 5; done
    api "$FJ/repos/fabric/fabric/branches/main" >/dev/null 2>&1 || die "the writer has not pushed the Fabric here yet; re-run to keep waiting"
    flux_up "$STATE/flux.yaml" "$STATE/site.agekey" "$site"
    wait_ready fabric 900 || die "Flux has not applied the Fabric yet; re-run to keep waiting"
  else
    import_images "$v" "$url" warden   # the laptop agent
  fi
  touch "$STATE/done"
  shred -u "$STATE/site.agekey" "$BUNDLE" 2>/dev/null || rm -f "$STATE/site.agekey" "$BUNDLE"
  say "joined: box $(hostname -s) is part of site $site"
}

# ---------------------------------------------------------------------------------------------------
# A new fabric on this box

# delegated PUBLIC: the zone's parent delegates it to ns1.<zone> at this box. Asked of the parent's own
# servers: nothing answers for the zone itself until this install starts Names.
delegated() {
  local p=${ZONE#*.} ns=""
  while [ -z "$ns" ] && [ "$p" != "${p#*.}" ]; do ns=$(dig +short NS "$p" @1.1.1.1 2>/dev/null | head -1); [ -n "$ns" ] || p=${p#*.}; done
  [ -n "$ns" ] && dig +norec A "ns1.$ZONE" @"$ns" 2>/dev/null | awk -v n="ns1.$ZONE." -v ip="$1" '$1==n && $4=="A" && $5==ip {f=1} END {exit !f}'
}

# ask VAR PROMPT DEFAULT KIND: from the environment, else from the terminal (stdin is often this script,
# piped into bash); the answer must be a valid KIND.
ask() {
  local v=${!1:-}
  if [ -z "$v" ]; then
    read -rp "$2${3:+ [$3]}: " v </dev/tty || die "$2: there is no terminal to ask in; set $1"
    v=${v:-${3:-}}
  fi
  valid "$4" "$v" || die "$2: \"$v\" will not do (docs/install.md)"
  printf -v "$1" '%s' "$v"
}

build() { # WeCoLab's binaries and images, before a registry holds them (decision 13)
  local src=${WECOLAB_SRC:-} bin=${WECOLAB_BIN:-}
  if [ -z "$bin" ]; then
    [ -n "$src" ] || die "no verified release binaries: supply WECOLAB_BIN from an authenticated release (developer source builds require explicit WECOLAB_SRC)"
    # Go for the build lives under $STATE and goes with it: never /usr/local/go, which may be the box's own.
    if ! command -v go >/dev/null; then
      if [ ! -x "$STATE/go/bin/go" ]; then
        local t sum; t=$(mktemp -d)
        fetch "https://go.dev/dl/go$GO_VERSION.linux-$ARCH.tar.gz" "$t/go.tgz" || die "could not fetch Go"
        case $ARCH in amd64) sum=$GO_SHA256_amd64 ;; arm64) sum=$GO_SHA256_arm64 ;; esac
        verify_payload "$t/go.tgz" "$sum"
        cache_verified "$t/go.tgz" "go$GO_VERSION.linux-$ARCH.tar.gz"
        rm -rf "$STATE/go" && tar -C "$STATE" -xzf "$t/go.tgz" && rm -rf "$t"
      fi
      export PATH=$PATH:$STATE/go/bin
    fi
    bin=$STATE/bin && mkdir -p "$bin"
    VERSION=$(cd "$src" && git describe --tags --always --dirty) || die "developer source must be a named Git checkout"
    for a in amd64 arm64; do
      for c in warden console; do
        (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o "$bin/$c-$a" "./cmd/$c")
      done
    done
    echo "$VERSION" > "$bin/VERSION"
  fi
  VERSION=$(cat "$bin/VERSION")
  mark warden
  install -m 0755 "$bin/warden-$ARCH" /usr/local/bin/warden
  for a in amd64 arm64; do
    [ -f "$DIST/warden-$VERSION-$a.tar" ] || warden image --name "ghcr.io/wecolabhq/warden:$VERSION" --arch $a --out "$DIST/warden-$VERSION-$a.tar" \
      "$bin/warden-$a=/warden" "$STATE/bin/sops-$SOPS_VERSION.linux.$a=/usr/local/bin/sops"
    [ -f "$DIST/console-$VERSION-$a.tar" ] || warden image --name "ghcr.io/wecolabhq/console:$VERSION" --arch $a --out "$DIST/console-$VERSION-$a.tar" \
      "$bin/console-$a=/console" "$STATE/bin/sops-$SOPS_VERSION.linux.$a=/usr/local/bin/sops"
  done
}

create() {
  [ -f "$STATE/install.json" ] || [ ! -f /etc/nebula/host.crt ] || die "this box already belongs to a fabric: install.sh uninstall first to make a new one here"
  [ -f "$STATE/install.json" ] || [ ! -f "$BUNDLE" ] || die "this box is joining a fabric: run install.sh again with its invite"
  say "a new fabric"
  if [ -f "$STATE/install.json" ]; then
    . "$STATE/fabric.env"   # a first run that stopped partway: its fabric is made, with these names
  else
    ask WECOLAB_ZONE "The fabric's DNS zone, delegated to this box (e.g. fab.example.org)" "" dns
    ask WECOLAB_EMAIL "Your email (the owner's sign-in, and Let's Encrypt's contact)" "" email
    ask WECOLAB_SITE "This site's name" "$(hostname -s | as_label)" name
    ask WECOLAB_PROJECT "Your project's name" "$(printf '%s' "${WECOLAB_EMAIL%@*}" | as_label)" name
    ZONE=$WECOLAB_ZONE SITE=$WECOLAB_SITE
    printf 'ZONE=%q\nWECOLAB_EMAIL=%q\nSITE=%q\n' "$ZONE" "$WECOLAB_EMAIL" "$SITE" > "$STATE/fabric.env"
  fi
  local host; host=$(hostname -s | as_label)
  valid label "$host" || die "this box's host name ($(hostname -s)) makes no name: give it one of letters, digits and dashes"
  printf 'ROLE=manager\nPUBLIC=true\n' > "$STATE/host.env"
  [ -n "$DEV" ] || netbird_ours
  packages git dnsutils
  local pub; pub=${WECOLAB_PUBLIC:-$(curl -4fsS -m 5 https://api.ipify.org 2>/dev/null || true)}
  [ -n "$DEV" ] && pub=$(ip -o -f inet addr show "$(ip route show default | awk '{print $5; exit}')" | awk '{print $4}' | cut -d/ -f1)
  [ -n "$pub" ] || die "cannot tell this box's public address; set WECOLAB_PUBLIC"
  systemctl is-active -q nebula || ports_free
  if [ -z "$DEV" ]; then
    say "waiting for $ZONE to be delegated to $pub"
    local ok=""
    for _ in $(seq 120); do
      delegated "$pub" && ok=1 && break
      echo "   create at your DNS host:  ns1.$ZONE A $pub   and   $ZONE NS ns1.$ZONE"; sleep 30
    done
    [ -n "$ok" ] || die "the delegation of $ZONE to $pub is not visible yet; re-run once it is"
  fi
  install_nebula
  install_sops
  build
  nebula_key

  say "the Fabric's first commit: keys, the Nebula CA, secrets"
  local repo=$STATE/fabric-repo st=$STATE/install.json
  if [ ! -f "$st" ]; then
    rm -rf "$repo"
    warden bootstrap --zone "$ZONE" --email "$WECOLAB_EMAIL" --site "$SITE" --project "$WECOLAB_PROJECT" --public "$pub" --host "$host" \
      --version "$VERSION" --network "$NETWORK" --box-key /etc/nebula/host.pub ${DEV:+--dev} --out "$repo" --state "$st"
  fi
  # The key and the response a joining box would get, made from the bootstrap.
  (umask 077; jq -r .siteKey "$st" > "$STATE/site.agekey"
   jq --arg site "$SITE" --arg zone "$ZONE" '{site: $site, box: .box.name, role: "manager", ip: .box.ip, ca, cert, config,
     stewards: [.box.ip], life, public: true, laptop: false, k3sToken, k3sAgentToken, zone: $zone}' "$st" > "$BUNDLE")
  start_nebula true
  install_k3s
  for img in warden console; do import_tar "$DIST/$img-$VERSION-$ARCH.tar" "ghcr.io/wecolabhq/$img:$VERSION"; done

  say "Forgejo with the Fabric, then Flux"
  forgejo_up "$repo/system/wecolab/forgejo.yaml" "$(jq -r .mirrorPassword "$st")" mirror fabric
  if ! api "$FJ/repos/fabric/fabric/branches/main" >/dev/null 2>&1; then
    # A re-run after a failed push finds its commit made, and only pushes.
    (cd "$repo" && { [ -d .git ] || git init -q -b main; } && git add -A \
      && { git rev-parse -q --verify HEAD >/dev/null || git -c user.name=WeCoLab -c user.email="fabric@$ZONE" commit -qm "A new fabric: $ZONE"; } \
      && gitfj push -q "${FJ%/api/v1}/fabric/fabric.git" main)
  fi
  flux_up "$repo/system/vendor/flux/manifest.yaml" "$STATE/site.agekey" "$SITE"
  wait_ready fabric 1200 || die "Flux has not applied the Fabric yet; re-run to keep waiting"
  say "Warden, the Console and the Door, from the Fabric"
  for _ in $(seq 120); do kubectl -n wecolab-system get deploy wecolab-console door >/dev/null 2>&1 && break; sleep 5; done
  kubectl -n wecolab-system rollout status deploy/wecolab-console deploy/door --timeout=600s >/dev/null || die "the Console or the Door did not start"

  # The owner's password is typed, never passed around: without a terminal, the people step waits for one.
  local later=""
  if [ -z "$DEV" ]; then
    if [ -n "${WECOLAB_PASSWORD:-}" ] || (: </dev/tty) 2>/dev/null; then people; else later=1; fi
  fi
  local card=/root/wecolab-recovery-card.txt
  if [ ! -f "$STATE/card-printed" ]; then
    (umask 077
     { echo "WeCoLab recovery card for $ZONE, $(date -u +%F)"
       echo "With this key and any copy of the Fabric, the whole fabric can be rebuilt (docs/operations.md)."
       echo "Keep it offline. Anyone holding it can read every secret of the fabric."
       echo; jq -r .recoveryKey "$st"; } > "$card")
    touch "$STATE/card-printed"
  fi
  shred -u "$STATE/site.agekey" "$BUNDLE" 2>/dev/null || rm -f "$STATE/site.agekey" "$BUNDLE"
  (umask 077; jq 'del(.siteKey, .recoveryKey, .k3sToken, .k3sAgentToken, .mirrorPassword)' "$st" > "$st.tmp") && mv "$st.tmp" "$st"
  touch "$STATE/done"
  cat <<MSG

The fabric $ZONE is up.
  Console:        https://console.$ZONE   (sign in as $WECOLAB_EMAIL)
  Recovery card:  $card
                  Copy it somewhere offline, then:  shred -u $card
Next: in the Console, add your object storage key (Settings), then add a second site (Add a site).
MSG
  local me; me=$(readlink -f "$0" 2>/dev/null) && [ -f "$me" ] && me="sudo bash $me people" || me="run the verified local release installer with people (docs/install.md)"
  [ -z "$later" ] || printf '\nNo terminal to ask for your password in. In a terminal on this box, run\n  %s\nto set it and start the people mesh; the Console signs in through it.\n' "$me"
}

# people: the people mesh, by warden people: NetBird's owner, the fabric's service token, the people group
# admitted to the Door, and this box on the mesh as the Door. What NetBird shows only once waits in
# $STATE/people.json (0600) until the service token is in the Fabric; a re-run picks up where it stopped.
people() {
  say "the people mesh: NetBird at https://mesh.$ZONE"
  netbird_ours
  wait_ready people 900 || die "NetBird has not started; re-run to keep waiting"
  if ! command -v netbird >/dev/null; then
    local t expected=NETBIRD_SHA256_$ARCH netbird_tmp="/usr/local/bin/.netbird-wecolab.$$"
    t=$(mktemp -d)
    fetch "https://github.com/netbirdio/netbird/releases/download/$NETBIRD_VERSION/netbird_${NETBIRD_VERSION#v}_linux_$ARCH.tar.gz" "$t/netbird-$ARCH.tar.gz" "netbird-$ARCH.tar.gz" ||
      die "could not fetch pinned NetBird"
    verify_payload "$t/netbird-$ARCH.tar.gz" "${!expected}"
    cache_verified "$t/netbird-$ARCH.tar.gz" "netbird-$ARCH.tar.gz"
    tar -xzf "$t/netbird-$ARCH.tar.gz" -C "$t" || die "invalid NetBird archive"
    install -m 0755 "$t/netbird" "$netbird_tmp" || { rm -f "$netbird_tmp"; die "could not install NetBird binary"; }
    mv "$netbird_tmp" /usr/local/bin/netbird || { rm -f "$netbird_tmp"; die "could not activate NetBird binary"; }
    mark netbird-direct
    mark netbird
    rm -rf "$t"
  fi
  if has netbird-direct; then mark netbird; fi
  if has netbird-direct && ! has netbird-service; then
    netbird service install || { netbird service uninstall >/dev/null 2>&1 || true; die "could not install NetBird service"; }
    mark netbird-service
  fi
  WECOLAB_GIT_URL=${FJ%/api/v1} WECOLAB_GIT_TOKEN=$TOKEN warden people --zone "$ZONE" --email "$WECOLAB_EMAIL" --people-net "$PEOPLE_NET" \
    --age-key "$STATE/site.agekey" --state "$STATE/people.json"
}

# install.sh people: the people step on its own, for an install that had no terminal to ask in.
people_later() {
  [ -f "$STATE/fabric.env" ] || die "run this on the fabric's first box, after install.sh"
  . "$STATE/fabric.env"
  TOKEN=$(kubectl -n wecolab-system get secret git -o jsonpath='{.data.token}' | base64 -d)
  FJ="http://$(kubectl -n wecolab-system get svc forgejo -o jsonpath='{.spec.clusterIP}'):3000/api/v1"
  (umask 077; kubectl -n flux-system get secret sops-age -o jsonpath="{.data.${SITE}\.agekey}" | base64 -d > "$STATE/site.agekey")
  people
  shred -u "$STATE/site.agekey" 2>/dev/null || rm -f "$STATE/site.agekey"
  say "the people mesh is up: sign in at https://console.$ZONE"
}

# install.sh takeover: this steward becomes the writer, from its own box, for when the writer's site is gone
# and with it the Console people reach (docs/operations.md, "The writer"). Root on a steward's manager is
# trusted with the fabric already; the site's Warden does it, with its copy of the Fabric.
takeover_here() {
  local node site
  node=$(kubectl get node -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -n "$node" ] || die "this box is not a site's manager"
  site=$(kubectl get sites.wecolab.io -o json | jq -r --arg n "$node" '.items[] | select(any(.spec.boxes[]?; .name == $n)) | .metadata.name')
  [ -n "$site" ] || die "no site in the Fabric has this box ($node)"
  say "$site takes over as the fabric's writer"
  kubectl -n wecolab-system exec deploy/wecolab-warden -- /warden takeover --site "$site"
}

# converge: a box that belongs to a fabric takes this script's host steps again (packages, binaries, units,
# the firewall). Its keys, tokens and certificates are the fabric's, and stay as they are.
converge() {
  [ -f "$STATE/host.env" ] || [ ! -f "$STATE/install.json" ] || printf 'ROLE=manager\nPUBLIC=true\n' > "$STATE/host.env"
  [ -f "$STATE/host.env" ] || die "$STATE/host.env is missing: this box was installed by an older install.sh; uninstall it and join again"
  . "$STATE/host.env"
  say "this box belongs to a fabric: converging its host steps"
  packages
  NEBULA_NEW=""
  install_nebula
  nebula_unit
  [ -z "$NEBULA_NEW" ] || systemctl try-restart nebula
  sync_timer
  if has k3s; then k3s_dependencies; fi
  firewall "$PUBLIC"
  apparmor
  if has k3s; then
    local unit=k3s-agent.service
    [ "$ROLE" != manager ] || unit=k3s.service
    systemctl cat "$unit" >/dev/null 2>&1 && systemctl start "$unit"
  fi
  kvm_label
  [ ! -f "$STATE/install.json" ] || install_sops
  ssh_unappend
  # Not migrated: k3s from before bind-address and the agent token. Its tokens are the fabric's, which a
  # re-run never changes; only a new join sets them.
  grep -q '^bind-address:' /etc/rancher/k3s/config.yaml 2>/dev/null \
    || echo "note: this box's k3s predates bind-address and the agent token; WECOLAB-HOST guards its ports meanwhile, and a box that leaves (install.sh uninstall) and joins again gets them" >&2
  say "done"
}

# ---------------------------------------------------------------------------------------------------
# Removing WeCoLab from this box: what $STATE/installed lists, the fabric's own files, and nothing else.
# The box's data in the fabric (its databases, volumes, its copy of the Fabric) goes with k3s. Every step
# is best-effort: a step that finds nothing to remove must not stop the others.

uninstall() {
  local apparmor_conflict="" recovery=""
  [ -f "$STATE/installed" ] || die "WeCoLab did not install anything here ($STATE/installed is missing)"
  say "removing WeCoLab from $(hostname -s)"
  systemctl disable --now wecolab-nebula-sync.timer wecolab-pod-isolation >/dev/null 2>&1 || true
  iptables -t mangle -D PREROUTING -i cni0 -j WECOLAB-POD 2>/dev/null || true
  iptables -t mangle -F WECOLAB-POD 2>/dev/null || true
  iptables -t mangle -X WECOLAB-POD 2>/dev/null || true
  for ipt in iptables ip6tables; do
    $ipt -D INPUT -j WECOLAB-HOST 2>/dev/null || true
    $ipt -F WECOLAB-HOST 2>/dev/null || true
    $ipt -X WECOLAB-HOST 2>/dev/null || true
  done
  if has k3s; then
    ! has kata || pkill -f '^/opt/kata/' || true   # workspaces' VMs and their shims: k3s's killall knows only its own
    for u in k3s-uninstall.sh k3s-agent-uninstall.sh; do [ ! -x "/usr/local/bin/$u" ] || "/usr/local/bin/$u" >/dev/null 2>&1 || true; done
    rm -rf /etc/rancher/node   # the node password; k3s's own uninstall leaves it
    systemctl stop kubepods.slice >/dev/null 2>&1 || true   # the kubelet's cgroups outlive it
    ! mountpoint -q /var/lib/kubelet || umount /var/lib/kubelet || true
    rmdir /var/lib/kubelet 2>/dev/null || true
  fi
  rm -f /etc/systemd/system/k3s.service.d/10-nebula.conf /etc/systemd/system/k3s-agent.service.d/10-nebula.conf
  ! has kata || rm -rf /opt/kata   # Kata's files; its containerd settings went with k3s
  if has apparmor || [ -d "$STATE/apparmor" ]; then   # after k3s: nothing runs under it now
    if ! apparmor_restore; then
      recovery=$(mktemp -d /var/lib/wecolab-apparmor-recovery.XXXXXX) ||
        die "cannot preserve AppArmor recovery state; $STATE has been left intact"
      cp -a "$STATE/apparmor" "$STATE/installed" "$recovery/" ||
        die "cannot preserve AppArmor recovery state; $STATE has been left intact"
      apparmor_conflict="AppArmor requires manual recovery: $recovery"
      echo "$apparmor_conflict" >&2
    fi
  fi
  rmdir /etc/systemd/system/k3s.service.d /etc/systemd/system/k3s-agent.service.d /etc/rancher /var/lib/rancher /wecolab 2>/dev/null || true
  if has netbird; then
    if command -v netbird >/dev/null; then
      netbird down >/dev/null 2>&1 || true
      netbird service stop >/dev/null 2>&1 || true
      netbird service uninstall >/dev/null 2>&1 || true
    fi
    DEBIAN_FRONTEND=noninteractive apt-get purge -y -qq netbird >/dev/null 2>&1 || true
    rm -rf /etc/apt/sources.list.d/netbird.list /usr/share/keyrings/netbird-archive-keyring.gpg /etc/netbird /var/lib/netbird
  fi
  remove_netbird_binary
  if has nebula-host; then
    systemctl disable --now nebula >/dev/null 2>&1 || true
    rm -rf /etc/nebula /etc/systemd/system/nebula.service
  fi
  rm -f /etc/systemd/system/wecolab-nebula-sync.service /etc/systemd/system/wecolab-nebula-sync.timer /usr/local/sbin/wecolab-nebula-sync \
    /etc/systemd/system/wecolab-pod-isolation.service /usr/local/sbin/wecolab-pod-isolation
  ! has nebula || rm -f /usr/local/bin/nebula /usr/local/bin/nebula-cert
  ! has sops || rm -f /usr/local/bin/sops
  ! has warden || rm -f /usr/local/bin/warden
  sed -n 's/^ufw //p' "$STATE/installed" | while IFS= read -r r; do ufw delete allow $r >/dev/null 2>&1 || true; done
  local keys=/root/.ssh/authorized_keys
  ssh_unappend || true   # an older install.sh's keys join the block, which goes
  if grep -q '^# BEGIN WeCoLab' "$keys" 2>/dev/null; then
    sed -i '/^# BEGIN WeCoLab/,/^# END WeCoLab/d' "$keys" || true
    [ -s "$keys" ] || rm -f "$keys"
    rmdir /root/.ssh 2>/dev/null || true
  fi
  systemctl daemon-reload || true
  local pkgs; pkgs=$(sed -n 's/^pkg //p' "$STATE/installed" | tr '\n' ' ')
  rm -rf "$STATE"
  say "WeCoLab is gone from $(hostname -s)"
  [ -z "$pkgs" ] || echo "   Left in place, since other software may use them now: $pkgs(installed by WeCoLab; apt-get purge them if unused)"
  [ ! -f /root/wecolab-recovery-card.txt ] || echo "   Left in place: /root/wecolab-recovery-card.txt. Once it is copied offline: shred -u /root/wecolab-recovery-card.txt"
  echo "   Remove the box from the fabric too, in the Console: Sites, the site, the box, Remove."
  [ -z "$apparmor_conflict" ] || die "$apparmor_conflict"
}

# ---------------------------------------------------------------------------------------------------

case "${1:-}" in
  wcl2.*) preflight; join "$1" ;;
  people) people_later ;;
  takeover) takeover_here ;;
  uninstall) uninstall ;;
  "") if fabric_made; then converge; else preflight; create; fi ;;
  *) die "usage: install.sh [invite | people | takeover | uninstall]" ;;
esac
