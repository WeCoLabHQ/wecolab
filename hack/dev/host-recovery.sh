#!/usr/bin/env bash
# Invoked only inside an owner-labelled disposable installed systemd guest.
set -euo pipefail
[ "${WECOLAB_DISPOSABLE_SYSTEMD_GUEST:-}" = 1 ] && [ "$(id -u)" = 0 ] && [ -d /run/systemd/system ] || exit 2
[ -f /var/lib/wecolab/done ] && [ -f /etc/nebula/config.d/20-fabric.yml ] || exit 2
systemctl is-active -q nebula
unit=/run/systemd/system/nebula.service.d/99-wecolab-recovery-failure.conf
[ ! -e "$unit" ] || { echo 'foreign runtime override' >&2; exit 2; }
original=$(mktemp /run/wecolab-nebula-recovery.XXXXXX)
cp -p /etc/nebula/config.d/20-fabric.yml "$original"
mkdir -p "${unit%/*}"
cleanup() {
  rm -f "$unit"
  cp -p "$original" /etc/nebula/config.d/20-fabric.yml
  rm -f "$original"
  systemctl daemon-reload
  systemctl reload nebula || :
}
trap cleanup EXIT
# the first transaction must still try systemd reload and retain a retry marker.
printf '\n' >> /etc/nebula/config.d/20-fabric.yml
printf '[Service]\nExecReload=\nExecReload=/usr/bin/false\n' > "$unit"
systemctl daemon-reload
if /usr/local/sbin/wecolab-nebula-sync; then
  echo 'sync incorrectly accepted failed real systemd reload' >&2; exit 1
fi
[ -e /etc/nebula/.wecolab-reload-pending ] || { echo 'pending reload lost' >&2; exit 1; }
systemctl is-active -q nebula
rm -f "$unit"
systemctl daemon-reload
/usr/local/sbin/wecolab-nebula-sync
[ ! -e /etc/nebula/.wecolab-reload-pending ] && [ ! -e /etc/nebula/.wecolab-sync-rollback ] || {
  echo 'identical steward bundle was not retried and committed' >&2; exit 1;
}
systemctl is-active -q nebula
