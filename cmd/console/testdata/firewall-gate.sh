#!/usr/bin/env bash
# Run ONLY as root inside an expendable systemd guest; inputs are installer-generated units.
set -euo pipefail
[ "${WECOLAB_DISPOSABLE_SYSTEMD_GUEST:-}" = 1 ] || { echo 'refusing non-disposable host' >&2; exit 1; }
[ "$(id -u)" = 0 ] && [ -d /run/systemd/system ] || { echo 'requires root and a running systemd guest' >&2; exit 1; }
[ "$#" = 3 ] || { echo 'usage: firewall-gate.sh k3s-dropin k3s-agent-dropin firewall-unit' >&2; exit 1; }
for unit in k3s.service k3s-agent.service nebula.service wecolab-pod-isolation.service; do
  if systemctl cat "$unit" >/dev/null 2>&1 || [ -e "/etc/systemd/system/$unit" ]; then
    echo "refusing guest with existing $unit" >&2; exit 1
  fi
done
scratch=$(mktemp -d /run/wecolab-isolation-smoke.XXXXXX)
cleanup() {
  systemctl stop k3s.service k3s-agent.service nebula.service wecolab-pod-isolation.service >/dev/null 2>&1 || :
  rm -f /etc/systemd/system/k3s.service /etc/systemd/system/k3s-agent.service /etc/systemd/system/nebula.service /etc/systemd/system/wecolab-pod-isolation.service
  rm -rf /etc/systemd/system/k3s.service.d /etc/systemd/system/k3s-agent.service.d /etc/systemd/system/wecolab-pod-isolation.service.d "$scratch"
  systemctl daemon-reload
}
trap cleanup EXIT
mkdir -p /etc/systemd/system/k3s.service.d /etc/systemd/system/k3s-agent.service.d /etc/systemd/system/wecolab-pod-isolation.service.d
cp "$1" /etc/systemd/system/k3s.service.d/10-nebula.conf
cp "$2" /etc/systemd/system/k3s-agent.service.d/10-nebula.conf
cp "$3" /etc/systemd/system/wecolab-pod-isolation.service
printf '[Unit]\nDescription=Disposable mesh fixture\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/true\n' > /etc/systemd/system/nebula.service
for role in k3s k3s-agent; do
  printf '[Unit]\nDescription=Disposable %s fixture\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/touch %s/%s-workload\n' "$role" "$scratch" "$role" > "/etc/systemd/system/$role.service"
done
printf '[Service]\nExecStart=\nExecStart=/bin/sh %s/firewall\n' "$scratch" > /etc/systemd/system/wecolab-pod-isolation.service.d/10-smoke.conf
printf '#!/bin/sh\nexit 42\n' > "$scratch/firewall"
chmod 700 "$scratch/firewall"
systemctl daemon-reload
for role in k3s k3s-agent; do
  if systemctl start "$role"; then echo "$role started despite failed firewall" >&2; exit 1; fi
  [ "$(systemctl show -p ExecMainStatus --value wecolab-pod-isolation.service)" = 42 ] || { echo "firewall did not reach its injected failure" >&2; exit 1; }
  [ ! -e "$scratch/$role-workload" ] || { echo "$role ran workload despite failed firewall" >&2; exit 1; }
  systemctl reset-failed "$role" wecolab-pod-isolation.service
 done
printf '#!/bin/sh\nexit 0\n' > "$scratch/firewall"
for role in k3s k3s-agent; do
  systemctl start "$role"
  [ -e "$scratch/$role-workload" ] || { echo "$role did not run after firewall succeeded" >&2; exit 1; }
done
printf 'both k3s roles are gated by successful isolation\n'
