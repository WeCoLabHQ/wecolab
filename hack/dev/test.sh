#!/usr/bin/env bash
# End-to-end checks on the development fabric (docs/development.md). Each step waits for the fabric to
# settle and fails with what it saw.
set -euo pipefail
cd "$(dirname "$0")/../.."
export ZONE=dev.wecolab.test
console() { docker exec wcl-pub curl -fsSk -X "$1" -H "Host: console.$ZONE" -H 'Content-Type: application/json' ${3:+-d "$3"} "https://127.0.0.1$2"; }
door() { docker exec wcl-pub curl -sk -o /dev/null -w '%{http_code}' -H "Host: $1" https://127.0.0.1/; }
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

if [ "$FROM" -le 1 ]; then
step "1. every site and box is Ready"
until_ 600 "sites and boxes Ready" sh -c "$(declare -f console); console GET /api/state | jq -e '[.sites[] | .Ready and ([.Boxes[] | .Ready] | all)] | all and length == 2'"
fi

if [ "$FROM" -le 2 ]; then
step "2. a database app at pub, with a standby at home"
console POST /api/deploy '{"Name":"notes","Project":"dev","Image":"nginxinc/nginx-unprivileged:1.27-alpine","Port":8080,"Sites":["pub","home"],"Primary":"pub","Database":true,"RPO":"5m",
  "Vault":{"KeyID":"wecolab","Key":"wecolab-dev-vault","Bucket":"wecolab-dev","Endpoint":"http://198.18.0.10"}}' | jq -c .
until_ 900 "notes is Ready at pub" state_is notes pub
until_ 120 "notes answers through the Door" sh -c "[ \$(docker exec wcl-pub curl -sk -o /dev/null -w '%{http_code}' -H 'Host: notes.$ZONE' https://127.0.0.1/) = 200 ]"
fi

if [ "$FROM" -le 3 ]; then
step "3. planned switchover to home and back"
console POST /api/apps/dev/notes/primary '{"To":"home"}' | jq -c .
# While the move is in flight pub replays its own archive (RoleAt), which only CloudNativePG can say it
# accepts, and stays demoted at the point its token names until the handover clears.
until_ 120 "the handover reaches pub" sh -c "$(declare -f console app); app notes | jq -e '.Handover != null'"
until_ 180 "pub replays its own archive toward home" sh -c "docker exec wcl-pub k3s kubectl -n dev get cluster.postgresql.cnpg.io notes-db -o json | jq -e '.spec.replica | .self==\"pub\" and .source==\"pub\" and .primary==\"home\"'"
until_ 120 "pub's CloudNativePG accepted it" sh -c "docker exec wcl-pub k3s kubectl -n flux-system get kustomization app-dev-notes -o json | jq -e '[.status.conditions[] | select(.type==\"Ready\") | .status] == [\"True\"]'"
tok=""
for _ in $(seq 120); do
  app notes | jq -e '.Handover == null' >/dev/null 2>&1 && break
  db=$(docker exec wcl-pub k3s kubectl -n dev get cluster.postgresql.cnpg.io notes-db -o json) || { sleep 5; continue; }
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
until_ 120 "the Door follows" sh -c "[ \$(docker exec wcl-pub curl -sk -o /dev/null -w '%{http_code}' -H 'Host: notes.$ZONE' https://127.0.0.1/) = 200 ]"
console POST /api/apps/dev/notes/primary '{"To":"pub"}' | jq -c .
until_ 600 "notes is primary at pub again" state_is notes pub
fi

if [ "$FROM" -le 4 ]; then
step "4. a forced move, made during a planned one, rebuilds the old primary from the vault"
console POST /api/apps/dev/notes/primary '{"To":"home"}' | jq -c .   # planned: pub starts demoting
console POST /api/apps/dev/notes/primary '{"To":"home","Force":true}' | jq -c .   # then forced, whatever the handover reached
until_ 600 "notes is primary at home, forced" state_is notes home
until_ 900 "pub's database rebuilt as a standby under generation 2" sh -c "$(declare -f console app); app notes | jq -e '.Archive.pub == 2 and ([.Conditions[] | select(.type==\"StandbyStaged\") | .status] == [\"True\"])'"
console POST /api/apps/dev/notes/primary '{"To":"pub"}' | jq -c .
until_ 600 "planned back to pub" state_is notes pub
fi

if [ "$FROM" -le 5 ]; then
step "5. deleting the app removes it and its data at both sites"
console DELETE /api/apps/dev/notes | jq -c .
for b in pub home; do
  until_ 300 "nothing of notes left at $b" sh -c "! docker exec wcl-$b k3s kubectl -n dev get deploy,cluster.postgresql.cnpg.io,pvc 2>/dev/null | grep -q notes"
done
until_ 300 "notes leaves the Fabric once both sites removed their part" sh -c "$(declare -f console); ! console GET /api/state | jq -e '.apps[] | select(.Name==\"notes\")' >/dev/null"
fi

if [ "$FROM" -le 6 ]; then
step "6. every box renews its certificate"
for b in pub home mac; do
  docker exec wcl-$b systemctl start wecolab-nebula-sync.service
  until_ 60 "$b synced with a steward" docker exec wcl-$b sh -c "systemctl show wecolab-nebula-sync.service -p Result | grep -q success"
done
fi

if [ "$FROM" -le 7 ]; then
step "7. home takes over as writer; pub follows"
home_ip=$(docker exec wcl-home ip -o -4 addr show nebula1 | awk '{print $4}' | cut -d/ -f1)
docker exec wcl-pub curl -fsS -X POST "http://$home_ip:30800/api/settings/takeover" | jq -c .
until_ 300 "every site follows home at epoch 2" sh -c "for b in pub home; do docker exec wcl-\$b curl -fsS http://\$(docker exec wcl-\$b ip -o -4 addr show nebula1 | awk '{print \$4}' | cut -d/ -f1):8093/status | jq -e '.writer==\"home\" and .epoch==2' >/dev/null || exit 1; done"
until_ 120 "the Console at console.$ZONE is home's" sh -c "$(declare -f console); console GET /api/settings | jq -e '.site==\"home\" and .isWriter'"
console POST /api/projects '{"Name":"after-takeover"}' | jq -c .
until_ 180 "a commit at home reaches pub" docker exec wcl-pub k3s kubectl get ns after-takeover
# From the box, as when no Console can be reached: pub takes the writer back.
docker cp install.sh wcl-pub:/root/install.sh
docker exec wcl-pub bash /root/install.sh takeover
until_ 300 "every site follows pub at epoch 3 after install.sh takeover" sh -c "for b in pub home; do docker exec wcl-\$b curl -fsS http://\$(docker exec wcl-\$b ip -o -4 addr show nebula1 | awk '{print \$4}' | cut -d/ -f1):8093/status | jq -e '.writer==\"pub\" and .epoch==3' >/dev/null || exit 1; done"
fi

if [ "$FROM" -le 7 ]; then
step "7b. removing a box blocks its certificate"
mac_fp=$(docker exec wcl-mac nebula-cert print -json -path /etc/nebula/host.crt | jq -r '.[0].fingerprint')
console DELETE /api/sites/home/boxes/home-mac | jq -c .
until_ 180 "mac's certificate is in the blocklist at pub and home" sh -c "for b in pub home; do docker exec wcl-\$b k3s kubectl -n wecolab-system get cm fabric -o jsonpath='{.data.blocklist}' | grep -q $mac_fp || exit 1; done"
until_ 120 "the Fabric no longer has mac" sh -c "! docker exec wcl-pub k3s kubectl get site home -o jsonpath='{.spec.boxes[*].name}' | grep -q home-mac"
fi

if [ "$FROM" -le 8 ]; then
step "8. uninstall leaves every box as it was before WeCoLab"
for b in mac home pub; do
  docker cp install.sh wcl-$b:/root/install.sh
  docker exec wcl-$b bash /root/install.sh uninstall >/dev/null
  # The harness's own mounts and Docker's DNS rules for its network are not WeCoLab's.
  left=$(docker exec -i wcl-$b bash < hack/dev/snap.sh | diff hack/dev/cache/new-$b.txt - | grep -E '^[<>]' | grep -v -E 'file /root/install.sh$|DOCKER_|mount /(src|cache)$' || true)
  [ -z "$left" ] || { echo "    FAILED: $b differs from before WeCoLab:" >&2; echo "$left" >&2; exit 1; }
  ok "$b is as it was"
done
fi

printf '\nall checks passed\n'
