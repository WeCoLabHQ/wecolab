#!/usr/bin/env bash
# What a box looks like wherever WeCoLab could leave traces: files, firewall rules, links, units, mounts.
# The development fabric takes one when a box starts and compares after uninstall (test step 8).
find /usr/local /etc/systemd/system /etc/apparmor.d/cri-containerd.apparmor.d /etc/nebula /etc/rancher /var/lib/rancher /var/lib/wecolab /var/lib/kubelet /var/lib/cni /etc/cni /run/k3s /run/flannel /wecolab /root/.ssh /etc/apt/sources.list.d -xdev 2>/dev/null | grep -v -E '^/usr/local/(share|lib|include|games|src|etc|man)$' | sort | sed 's/^/file /'
for t in filter nat mangle raw; do iptables-save -t $t 2>/dev/null | grep -E '^(:|-A)' | sed -E 's/ \[[0-9]+:[0-9]+\]//; s/^/ipt '$t' /'; done | sort
ip -o link | awk -F': ' '{print "link " $2}' | sed 's/@.*//' | sort
systemctl list-unit-files --no-legend 2>/dev/null | awk '{print "unit " $1}' | sort
mount | awk '{print "mount " $3}' | sort
