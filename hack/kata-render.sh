#!/usr/bin/env bash
# Render Kata Containers' kata-deploy chart into the Fabric's system/ (decision 26). Sites run no Helm, so
# the chart is rendered here, pinned, and applied by Flux with the rest of system/wecolab. Needs helm.
#
#   hack/kata-render.sh
#
# Without Helm's hooks: they would run as plain Jobs at every apply, and the post-delete one uninstalls.
# The DaemonSet undoes its node's install itself when its pod stops. Only nodes labelled wecolab.io/kvm
# (install.sh, on boxes with /dev/kvm) get Kata: in this mode kata-deploy does not check for KVM itself.
# Cloud Hypervisor (kata-clh) only, which the chart offers on amd64 only.
set -euo pipefail
cd "$(dirname "$0")/.."
V=4.2.0
out=internal/bootstrap/template/system/wecolab/kata.yaml
{ printf '# Rendered by hack/kata-render.sh from the kata-deploy chart %s. Do not edit; render again.\n' "$V"
  helm template kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --version "$V" \
    --namespace kube-system --no-hooks -f - <<'YAML'
k8sDistribution: k3s
imagePullPolicy: IfNotPresent
snapshotter: { setup: [] }
nodeFeatureRules: { create: false }
nodeSelector: { wecolab.io/kvm: "true", kubernetes.io/arch: amd64 }
shims:
  disableAll: true
  clh: { enabled: true }
defaultShim: { amd64: clh } # the chart's, qemu-runtime-rs, is not among the shims: kata-deploy would refuse
YAML
} > "$out"
# Without nodes/proxy, which reaches the kubelet API of every node, for an advisory read of /configz that
# kata-deploy skips with a warning. Its node patches are bounded by system/wecolab/kata-policy.yaml.
python3 -c '
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(?:#[^\n]*\n)*- apiGroups: \[\"\"\]\n  resources: \[\"nodes/proxy\"\]\n  verbs: \[\"get\"\]\n", "", s)
assert n == 1, "the nodes/proxy rule moved: read the chart again"
s, n = re.subn(r"image: quay.io/kata-containers/kata-deploy:4\.2\.0\b",
               "image: quay.io/kata-containers/kata-deploy:4.2.0@sha256:8878e275eeb611f5ed1613009db195f4034b25fea461aef61d407fead58872d3", s)
assert n == 1, "the Kata image tag moved: inspect and pin the new multiarch digest before rendering"
open(p, "w").write(s)' "$out"
! grep -q -e '{{' -e '${' "$out" || { echo "$out has {{ or \${: bootstrap and Flux would both read it as a template" >&2; exit 1; }
echo "wrote $out"
