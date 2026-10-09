#!/usr/bin/env bash
# End-to-end checks on the development fabric (docs/development.md). Each step waits for the fabric to
# settle and fails with what it saw.
set -euo pipefail
cd "$(dirname "$0")/../.."
export ZONE=${WECOLAB_DEV_ZONE:-dev.wecolab.test}
PREFIX=${WECOLAB_DEV_PREFIX:?choose an isolated WECOLAB_DEV_PREFIX}
[[ $PREFIX != wcl && -n ${WECOLAB_DEV_OWNER:-} ]] || { echo "disposable owner/prefix required" >&2; exit 2; }
for n in pub home mac; do
  [[ $(docker inspect -f '{{index .Config.Labels "wecolab.dev.owner"}}' "$PREFIX-$n") == "$WECOLAB_DEV_OWNER" ]] || { echo "foreign $PREFIX-$n" >&2; exit 2; }
done
net=${WECOLAB_DEV_NETWORK:-$PREFIX-net}
[[ $(docker network inspect -f '{{index .Labels "wecolab.dev.owner"}}' "$net") == "$WECOLAB_DEV_OWNER" ]] || { echo "foreign network $net" >&2; exit 2; }
[[ -f hack/dev/cache/$PREFIX/.wecolab-owner && $(<hack/dev/cache/$PREFIX/.wecolab-owner) == "$WECOLAB_DEV_OWNER" ]] || { echo "foreign cache $PREFIX" >&2; exit 2; }
console() {
  local box=${CONSOLE_BOX:-pub} endpoint=https://127.0.0.1 address
  if [ "$box" != pub ]; then
    address=$(docker exec "$PREFIX-$box" ip -j -4 addr show dev nebula1 | jq -er '[.[].addr_info[] | select(.family=="inet") | .local] | if length==1 then .[0] else error("private Console requires one site mesh address") end') || return
    endpoint="http://$address:30800"
  fi
  docker exec "$PREFIX-$box" curl -fsSk -X "$1" -H "Host: console.$ZONE" -H 'Content-Type: application/json' ${3:+-d "$3"} "$endpoint$2"
}
door() { docker exec "$PREFIX-pub" curl -sk -o /dev/null -w '%{http_code}' -H "Host: $1" https://127.0.0.1/; }
FROM=${FROM:-1}  # FROM=4 hack/dev/test.sh starts at step 4
step() { printf '\n### %s\n' "$*"; }
ok() { printf '    ok: %s\n' "$*"; }
# until SECONDS DESCRIPTION COMMAND...: retry until the command succeeds.
until_() {
  local n=$1 what=$2; shift 2
  for _ in $(seq "$(( n / 5 ))"); do "$@" >/dev/null 2>&1 && { ok "$what"; return 0; }; sleep 5; done
  echo "    FAILED: $what" >&2; "$@" >&2 || true; return 1
}
app() { console GET /api/state | jq -e --arg n "$1" '.apps[] | select(.Name==$n)'; }
state_is() { console GET /api/state | jq -e --arg n "$1" --arg a "$2" '.apps[] | select(.Name==$n and .Active==$a and .Ready=="True" and .Handover==null)'; }
writer_is() {
  console GET /api/settings | jq -e --arg site "$1" --argjson epoch "$2" '.site==$site and .writer==$site and .isWriter and .epoch==$epoch'
}
writers_follow() {
  local writer=$1 epoch=$2 box address
  for box in pub home; do
    address=$(docker exec "$PREFIX-$box" ip -j -4 addr show dev nebula1 | jq -er '[.[].addr_info[] | select(.family=="inet") | .local] | if length==1 then .[0] else error("writer status requires one site mesh address") end') || return
    docker exec "$PREFIX-$box" curl -fsS "http://$address:8093/status" |
      jq -e --arg writer "$writer" --argjson epoch "$epoch" '.writer==$writer and .epoch==$epoch' || return
  done
  writer_is "$writer" "$epoch"
}
propagation_boundary() {
  local b address
  for b in pub home; do
    echo "    propagation boundary at $b:"
    docker exec "$PREFIX-$b" k3s kubectl --request-timeout=15s -n flux-system get gitrepositories,kustomizations -o json |
      jq '[.items[] | select(.metadata.name=="fabric" or .metadata.name=="system" or .metadata.name=="crds") |
        {kind, name:.metadata.name, generation:.metadata.generation,
         revision:(.status.artifact.revision // .status.lastAppliedRevision),
         observedGeneration:.status.observedGeneration, conditions:.status.conditions}]' || true
    docker exec "$PREFIX-$b" k3s kubectl --request-timeout=15s -n wecolab-system get configmap fabric -o json |
      jq '.data | {writer,epoch,blocklist}' || true
    docker exec "$PREFIX-$b" k3s kubectl --request-timeout=15s get site home -o json |
      jq '{site:.metadata.name, boxes:[.spec.boxes[] | {name,certs}]}' || true
    address=$(docker exec "$PREFIX-$b" ip -j -4 addr show dev nebula1 |
      jq -er '[.[].addr_info[] | select(.family=="inet") | .local] | if length==1 then .[0] else error("one mesh address required") end') || continue
    docker exec "$PREFIX-$b" curl -fsS --max-time 15 "http://$address:8093/fabric/commits/0000000000000000000000000000000000000000" |
      jq '{head,onMain,claim}' || true
  done
}
notes_move_ready() {
  local state
  state=$(app notes) || return
  jq -e '.Handover == null and ([.Conditions[] | select(.type=="PrimaryHealthy" or .type=="StandbyStaged" or .type=="WithinRPO") | .status] | length==3 and all(.=="True"))' <<<"$state"
}
notes_gone_at() {
  local resources
  resources=$(docker exec "$PREFIX-$1" k3s kubectl -n dev get deploy,cluster.postgresql.cnpg.io,pvc -o name) || return
  [[ $resources != *notes* ]]
}
notes_gone() {
  local state
  state=$(console GET /api/state) || return
  jq -e '[.apps[] | select(.Namespace=="dev" and .Name=="notes")] | length==0' <<<"$state"
}
mac_gone() {
  local boxes
  boxes=$(docker exec "$PREFIX-pub" k3s kubectl get site home -o jsonpath='{.spec.boxes[*].name}') || return
  [[ " $boxes " != *" home-mac "* ]]
}
snapshot_delta() {
  local difference
  difference=$(diff "$1" -) || [[ $? -eq 1 ]] || return
  printf '%s\n' "$difference" | grep -E '^[<>]' | grep -v -E 'file /root/install.sh$|DOCKER_|mount /(src|cache)$' || true
}

if [ "$FROM" -le 1 ]; then
step "1. every site and box is Ready"
until_ 600 "sites and boxes Ready" sh -c "$(declare -f console); PREFIX=$PREFIX ZONE=$ZONE; console GET /api/state | jq -e '[.sites[] | .Ready and ([.Boxes[] | .Ready] | all)] | all and length == 2'"
fi

if [ "$FROM" -le 2 ]; then
step "2. a database app at pub, with a standby at home"
vault_ip=${WECOLAB_DEV_SUBNET:-198.18.0.0/24}; vault_ip=${vault_ip%.*}.10
console POST /api/deploy "$(jq -cn --arg endpoint "http://$vault_ip" '{Name:"notes",Project:"dev",Image:"nginxinc/nginx-unprivileged:1.27-alpine",Port:8080,Sites:["pub","home"],Primary:"pub",Database:true,RPO:"5m",Vault:{KeyID:"wecolab",Key:"wecolab-dev-vault",Bucket:"wecolab-dev",Endpoint:$endpoint}}')" | jq -c .
until_ 900 "notes is Ready at pub" state_is notes pub
until_ 120 "notes answers through the Door" sh -c "[ \$(docker exec $PREFIX-pub curl -sk -o /dev/null -w '%{http_code}' -H 'Host: notes.$ZONE' https://127.0.0.1/) = 200 ]"
fi

if [ "$FROM" -le 3 ]; then
step "3. planned switchover to home and back"
until_ 900 "notes meets planned-move recovery gates toward home" notes_move_ready
console POST /api/apps/dev/notes/primary '{"To":"home"}' | jq -c .
# While the move is in flight pub replays its own archive (RoleAt), which only CloudNativePG can say it
# accepts, and stays demoted at the point its token names until the handover clears.
until_ 120 "the handover reaches pub" sh -c "$(declare -f console app); PREFIX=$PREFIX ZONE=$ZONE; app notes | jq -e '.Handover != null'"
until_ 180 "pub replays its own archive toward home" sh -c "docker exec $PREFIX-pub k3s kubectl -n dev get cluster.postgresql.cnpg.io notes-db -o json | jq -e '.spec.replica | .self==\"pub\" and .source==\"pub\" and .primary==\"home\"'"
until_ 120 "pub's CloudNativePG accepted it" sh -c "docker exec $PREFIX-pub k3s kubectl -n flux-system get kustomization app-dev-notes -o json | jq -e '[.status.conditions[] | select(.type==\"Ready\") | .status] == [\"True\"]'"
tok=""
for _ in $(seq 120); do
  app notes | jq -e '.Handover == null' >/dev/null 2>&1 && break
  db=$(docker exec "$PREFIX-pub" k3s kubectl -n dev get cluster.postgresql.cnpg.io notes-db -o json) || { sleep 5; continue; }
  t=$(jq -r '.status.demotionToken // ""' <<<"$db")
  w=$(jq -r '.status as $s | ($s.instancesReportedState // {})[$s.currentPrimary // ""].isPrimary // false' <<<"$db")
  if [ -n "$tok" ] && { [ "$t" != "$tok" ] || [ "$w" != false ]; }; then
    echo "    FAILED: pub left its demotion point before the handover cleared" >&2; jq .status <<<"$db" >&2; exit 1
  fi
  [ -n "$tok" ] || [ -z "$t" ] || [ "$w" != false ] || { tok=$t; ok "pub demoted with a token"; }
  sleep 5
done
[ -n "$tok" ] || { echo "    FAILED: pub never showed a demotion token while it handed over" >&2; exit 1; }
until_ 600 "notes is primary at home" state_is notes home
until_ 120 "the Door follows" sh -c "[ \$(docker exec $PREFIX-pub curl -sk -o /dev/null -w '%{http_code}' -H 'Host: notes.$ZONE' https://127.0.0.1/) = 200 ]"
until_ 900 "notes meets planned-move recovery gates toward pub" notes_move_ready
console POST /api/apps/dev/notes/primary '{"To":"pub"}' | jq -c .
until_ 600 "notes is primary at pub again" state_is notes pub
fi

if [ "$FROM" -le 4 ]; then
step "4. forced move after independently powering off pub"
preview=$(console GET /api/apps/dev/notes/force-preview)
[[ $(jq -r .PreviousPrimary <<<"$preview") == pub ]] || { echo "unexpected force preview" >&2; exit 1; }
writer_epoch=$(console GET /api/settings | jq -er .epoch)
old_cluster=$(docker exec "$PREFIX-pub" k3s kubectl -n dev get cluster notes-db -o jsonpath='{.metadata.uid}')
old_pod=$(docker exec "$PREFIX-pub" k3s kubectl -n dev get cluster notes-db -o jsonpath='{.status.currentPrimary}')
docker exec "$PREFIX-pub" k3s kubectl -n dev annotate cluster notes-db 'cnpg.io/fencedInstances=["*"]' --overwrite --field-manager=flux-client-side-apply
until_ 180 "old postmaster fenced across future container startup" docker exec "$PREFIX-pub" k3s kubectl -n dev exec "$old_pod" -c postgres -- sh -c 'pg_ctl status; status=$?; test "$status" -eq 3'
docker stop "$PREFIX-pub" >/dev/null
docker cp install.sh "$PREFIX-home:/root/install.sh"
docker exec "$PREFIX-home" bash /root/install.sh takeover
CONSOLE_BOX=home
export CONSOLE_BOX
until_ 120 "home's Console admits the new writer epoch" writer_is home "$((writer_epoch+1))"
fencing=$(jq -cn --argjson p "$preview" '{To:"home",Force:true,Fencing:($p + {Method:"power-off",Evidence:"Disposable pub container stopped; docker inspect reports not running"})}')
[[ $(docker inspect -f '{{.State.Running}}' "$PREFIX-pub") == false ]] || exit 1
console POST /api/apps/dev/notes/primary "$fencing" | jq -c .
until_ 600 "notes is primary at home, forced" state_is notes home
docker start "$PREFIX-pub" >/dev/null
unset CONSOLE_BOX
until_ 900 "pub's database rebuilt as a standby under generation 2" sh -c "$(declare -f console app); PREFIX=$PREFIX ZONE=$ZONE; app notes | jq -e '.Archive.pub == 2 and ([.Conditions[] | select(.type==\"StandbyStaged\") | .status] == [\"True\"])'"
[[ $(docker exec "$PREFIX-pub" k3s kubectl -n dev get cluster notes-db -o jsonpath='{.metadata.uid}') != "$old_cluster" ]] ||
  { echo "    FAILED: old fenced database was not replaced" >&2; exit 1; }
until_ 900 "rebuilt standby meets planned-move recovery gates toward pub" notes_move_ready
console POST /api/apps/dev/notes/primary '{"To":"pub"}' | jq -c .
until_ 600 "planned back to pub" state_is notes pub
docker cp install.sh "$PREFIX-pub:/root/install.sh"
docker exec "$PREFIX-pub" bash /root/install.sh takeover
until_ 300 "every site and the public Console follow pub before deletion" writers_follow pub "$((writer_epoch+2))"
fi

if [ "$FROM" -le 5 ]; then
step "5. deleting the app removes it and its data at both sites"
console DELETE /api/apps/dev/notes | jq -c .
for b in pub home; do
  until_ 300 "nothing of notes left at $b" notes_gone_at "$b"
done
until_ 300 "notes leaves the Fabric once both sites removed their part" notes_gone
fi

if [ "$FROM" -le 6 ]; then
step "6. every box renews its certificate"
for b in pub home mac; do
  docker exec "$PREFIX-$b" systemctl start wecolab-nebula-sync.service
  until_ 60 "$b synced with a steward" docker exec "$PREFIX-$b" sh -c "systemctl show wecolab-nebula-sync.service -p Result | grep -q success"
done
fi

if [ "$FROM" -le 7 ]; then
step "7. home takes over as writer; pub follows"
home_ip=$(docker exec "$PREFIX-home" ip -o -4 addr show nebula1 | awk '{print $4}' | cut -d/ -f1)
epoch=$(docker exec "$PREFIX-pub" curl -fsS "http://$(docker exec "$PREFIX-pub" ip -o -4 addr show nebula1 | awk '{print $4}' | cut -d/ -f1):8093/status" | jq -r .epoch)
docker exec "$PREFIX-pub" curl -fsS -X POST "http://$home_ip:30800/api/settings/takeover" | jq -c .
until_ 300 "every site and the public Console follow home at next epoch" writers_follow home "$((epoch+1))"
console POST /api/projects '{"Name":"after-takeover"}' | jq -c .
if ! until_ 180 "a commit at home reaches pub" docker exec "$PREFIX-pub" k3s kubectl get ns after-takeover; then
  propagation_boundary
  exit 1
fi
docker cp install.sh "$PREFIX-pub:/root/install.sh"
docker exec "$PREFIX-pub" bash /root/install.sh takeover
until_ 300 "every site and the public Console follow pub after install.sh takeover" writers_follow pub "$((epoch+2))"
fi

if [ "$FROM" -le 7 ]; then
step "7b. removing a box blocks its certificate"
mac_fp=$(docker exec "$PREFIX-mac" nebula-cert print -json -path /etc/nebula/host.crt | jq -r '.[0].fingerprint')
console DELETE /api/sites/home/boxes/home-mac | jq -c .
if ! until_ 180 "mac's certificate is in the blocklist at pub and home" sh -c "for b in pub home; do docker exec $PREFIX-\$b k3s kubectl -n wecolab-system get cm fabric -o jsonpath='{.data.blocklist}' | grep -q $mac_fp || exit 1; done"; then
  printf '    expected certificate fingerprint: %s\n' "$mac_fp"
  propagation_boundary
  exit 1
fi
until_ 120 "the Fabric no longer has mac" mac_gone
fi

if [ "$FROM" -le 8 ]; then
step "8. uninstall leaves every box as it was before WeCoLab"
for b in mac home pub; do
  baseline="${WECOLAB_DEV_CACHE:-hack/dev/cache/$PREFIX}/new-$b.txt"
  [[ -s $baseline && -r $baseline ]] || { echo "missing or unreadable uninstall baseline: $baseline" >&2; exit 1; }
done
for b in mac home pub; do
  docker cp install.sh "$PREFIX-$b:/root/install.sh"
  docker exec "$PREFIX-$b" bash /root/install.sh uninstall >/dev/null
  # The harness's own mounts and Docker's DNS rules for its network are not WeCoLab's.
  left=$(docker exec -i "$PREFIX-$b" bash < hack/dev/snap.sh | snapshot_delta "${WECOLAB_DEV_CACHE:-hack/dev/cache/$PREFIX}/new-$b.txt") ||
    { echo "    FAILED: could not capture or compare $b's uninstall inventory" >&2; exit 1; }
  [ -z "$left" ] || { echo "    FAILED: $b differs from before WeCoLab:" >&2; echo "$left" >&2; exit 1; }
  ok "$b is as it was"
done
fi

printf '\nall checks passed\n'
