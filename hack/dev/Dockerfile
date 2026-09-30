# A box for the development fabric: Ubuntu 24.04 running systemd, like a fresh VPS.
FROM ubuntu:24.04
ENV container=docker DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y -qq systemd systemd-sysv dbus curl jq iptables ca-certificates age dnsutils git iproute2 kmod \
      procps less vim-tiny && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-logind getty.target console-getty.service systemd-udevd.service systemd-networkd-wait-online.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
