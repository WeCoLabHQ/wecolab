# What a workload can and cannot do to a box

The honest version. "Holds" means enforced by the box's own kernel or its site's API server,
independent of anything the Fabric or a project writes. Rows marked *measured* were verified on the
first WeCoLab lab; the rest are this design's intent and are verified by the development fabric
(docs/development.md) before a release. WeCoLab's own words (site, box, steward, project, Door, vault) are
explained in the [glossary](glossary.md).

## Holds

| Boundary | Enforced by | What it stops |
|---|---|---|
| Pod Security `restricted` by default on every namespace. The site's infrastructure namespaces are exempt, and project namespaces are relaxed to `baseline` with user namespaces required (next row) | the site's API server (`admission-control-config-file`, written by the join script) | privileged containers, host PID/IPC/network, hostPath, added capabilities, root, no seccomp. *Measured.* |
| Project namespaces: `baseline` plus mandatory user namespaces | a ValidatingAdmissionPolicy at every site refusing a pod in a project namespace unless `hostUsers: false`, it runs in a Kata VM (`runtimeClassName: kata-clh`, a workspace), or every container, init and ephemeral ones included, runs as non-root and never as uid 0 | an image that expects root runs as root only inside its user namespace: on the box it is an unprivileged uid. In a workspace, root is root of its own VM's kernel, not the box's. *Measured.* |
| Placement | a ValidatingAdmissionPolicy at every site: a project's pod may not name its node, and only best-effort pods may tolerate the `wecolab.io/` taints (a toleration without a key counts) | a project's other work landing on a laptop or in someone's idle time, or on a node of its choosing around the scheduler |
| Flux applies a project's apps as the project | each app's Kustomization impersonates the project's ServiceAccount, bound only to the kinds a member may create, in the project's namespace; Flux runs with `--no-cross-namespace-refs`, `--no-remote-bases` and a default service account that may do nothing | nothing committed to an app's folder can create cluster-wide objects, touch another project, or pull manifests from elsewhere |
| The API decides what a person may do | Kubernetes RBAC at the Console's site; non-PVC changes are dry-run as the signed-in person, impersonating users only. Generated PVCs are typed/quantity-validated against every destination's storage grant; destination admission still enforces live quota and capacity | a project cannot borrow the writer's unrelated storage quota or bypass tenant policy; a successful preflight does not promise available remote disk |
| The Console's session | `__Host-` cookies; Member authority re-read on every request; blocked, missing or terminating Members denied before handlers; cross-origin writes refused; page framing refused and names escaped | a stale session does not retain a removed admin's rights; terminating-owner development fallback is denied, and RBAC cleanup is independent of eventual NetBird deletion |
| Site boundary in the kernel's mangle table | iptables (`WECOLAB-POD`); firewall unit required and ordered before **both** k3s service roles, with install/start failures propagated | pods cannot open forbidden host/LAN/mesh connections; failure does not start k3s unguarded. Rule behavior measured on the original lab; reboot gating requires the disposable systemd acceptance scenario |
| Only Nebula reaches k3s | k3s bound to the box's Nebula address, and WeCoLab's `WECOLAB-HOST` chain (IPv4 and IPv6), whether or not ufw is active | the Kubernetes API, the kubelet, flannel's VXLAN and NetBird's metrics are unreachable from the LAN and the internet; on a public box one address holds at most 200 connections on 443 |
| Least privilege on the box | nodes hold the k3s agent token only; the node agent may change only its own node's idle taint (a ValidatingAdmissionPolicy on the node name bound into its token); kata-deploy may change only Kata's labels on its own node the same way, and has no `nodes/proxy`; the Door has its own read-only account | a compromised node cannot fetch the cluster's bootstrap data or CA keys, or reach other nodes' kubelets with a token it finds; a laptop cannot relabel or retaint a node; the Door cannot write to the cluster |
| Nebula firewall | every box's Nebula config, rendered from its certificate's groups | only what the design needs crosses boxes: Warden status between managers, the certificate service from any box to stewards, Git mirroring from stewards, the Door to app ports, the Kubernetes ports within one site |
| Nebula identity | certificates signed by the fabric's CA; host keys born on the host; the certificate service answers a box only from that box's own Nebula address | a box cannot claim another's address or groups, or get a certificate for another box |
| Revocation | the fabric's blocklist, in Git: a removed box's join certificates go in with the commit that removes it, and the writer's revocation loop adds every renewal a steward signed for it; every box applies the blocklist at its next sync, and Nebula drops tunnels whose certificate is blocked | a removed box's certificates stop working everywhere within the hour |
| SSH access follows the Fabric | the hourly sync rewrites a marked block of root's `authorized_keys` with the keys of the fabric's admins and of the members of the project that owns the site | a person removed or blocked loses SSH access within the hour; the box's own keys, outside the block, stay |
| People never touch the boxes' mesh | NetBird carries people only to the Door; mesh-only routes admit NetBird addresses; people's devices join by signing in, so each belongs to a person | a person's compromised laptop reaches app front doors, not boxes, Kubernetes APIs or databases |
| The Door routes only what the Fabric vouches for | an app's route goes only to node ports on its primary site's own boxes; host names are checked; the fabric's own routes outrank every app's; NetBird's `/api/setup` and `/api/instance` are never routed; the DNS-challenge endpoint takes only local requests with the pod's credentials, for challenge names in the zone | a site cannot point the Door anywhere but its own boxes' node ports (the Door's loopback, say); an app cannot take `console.<zone>`; nobody can claim NetBird's identity provider or put records in the zone from outside the Door's pod |
| Secrets at rest in Git | SOPS with age, each secret encrypted only to the sites that need it (fabric-level secrets to the stewards), and always to the recovery key | a copy of the Fabric, a Git mirror or a leaked repository reveals no secret without a site's age key |
| Secrets off the command line | install.sh and Warden pass secrets through files, stdin and the environment | another process on the box cannot read a token or password from its command line |
| Codes are single-use and expire | box and site invites, offer and pool keys kept only as hashes, for a day; a box's invite is used up only by a join that is committed. A person's invite link (a NetBird invite, kept in the Member's status at the writer) sets a password once and expires after three days | a code seen after use, or after it expires, is worth nothing |
| The vault is append-only for a month | Object Lock in compliance mode for 30 days on vaults the Console creates (Storage, Create vault); a bucket entered by hand at deploy has whatever lock its owner set, and the Storage page shows it | nobody, the owner included, can delete or overwrite a backup inside the retention period |
| Project boundary | NetworkPolicies and a LimitRange Warden keeps in every project namespace at every site | a project's pods reach only each other, DNS, the site's API server and the internet on TCP 80 and 443, never private addresses; they are reached only by each other, CloudNativePG and the fabric's Nebula network (the Door). An app that needs another outbound port, SMTP for example, cannot reach it. A container that sets no requests or limits of its own gets small defaults (25m CPU and 32Mi of memory requested, 256Mi of memory at most) |
| Quota and priority | ResourceQuota and PriorityClass at the site that offers capacity | a project cannot take more than it was offered, and takes nothing at a site where it owns nothing and holds no offer; best-effort work yields first |
| Vault HTTP egress | production HTTPS, socket-time DNS/address classification and dialing the validated literal address with original TLS hostname; no redirects or proxy-env inheritance; bounded exchange/body reads | DNS rebinding, mixed public/private answers, loopback/link-local/private and special-use destinations cannot turn a configured vault into control-plane SSRF; explicit development fixtures are not production policy |

Host AppArmor changes use an ownership ledger and digest-guarded restoration: unrelated
policy and operator edits are not overwritten during uninstall. Privileged support tools
are verified before placement and only install-owned copies are removed. Actual reboot,
partial-install and provider-failure observations remain release gates beyond unit tests.

## Release and privileged supply chain

The supported production bootstrap is **not** `curl main/install.sh | sudo bash` or the
Console's unverified `/join.sh` pipe. Download a specific release's manifest and all
platform artifacts, verify GitHub attestation for repository `wecolabhq/wecolab`,
signer `wecolabhq/wecolab/.github/workflows/release.yml`, source ref `refs/heads/main`
and the independently expected 40-hex source commit, then check every artifact digest
before invoking the local installer with root privileges ([exact commands](install.md)).
No candidate has been published or attested by this change; production release and
restore validation remain external prerequisites. A public release tag or checksum
served beside an attacker-replaced script is **not** an independent trust anchor.

Pinned fetched privileged inputs (both Linux amd64/arm64 unless noted):

| Input and purpose | Immutable identity and SHA-256 expectation | Executor / update owner |
|---|---|---|
| WeCoLab installer and Warden/Console | public snapshot SHA, GitHub release-workflow attestation, manifest's seven artifact digests (including source tarball and VERSION); generated CRD `wecolab.io/v1alpha1`, required archive/key/name/placement migrations | installer and host Warden run as root; release owner |
| k3s service installer | `k3s-io/k3s` commit `4dedb15be78017a8ddd5b9e81acd44f3481078ed`, `46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad` | root; host owner |
| k3s executable `v1.36.4+k3s1` | amd64 `835873f37245fc615f547a2fe2af9402a347875f13fa64a1f136de644955ea3f`; arm64 `c920706346d5ad4e5cd3c7bf1bb09ce71ebe07fec829e513e40f1caf98aed8bb` | root; host owner |
| Nebula `v1.11.2` release tarballs | amd64 `6140d33f2ec21ce7f6b655b5bc820e93a684d97e51d0ddcf907324b5b28aac1e`; arm64 `85d10e7bc2d121193c1392a1a919172ded7c413f46e602138281cfa9fa1b0231` | host root; mesh owner |
| SOPS `v3.13.3` release binaries | amd64 `e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b`; arm64 `53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77` | host root / images; secrets owner |
| NetBird client `v0.80.0` release tarballs | amd64 `47ffaba4fc3929f31795bd6c5232d6c29744d3169e2c93e6d9c84624f0ef6405`; arm64 `8cbd99fa068a7b0f3968b2d31dc341acc17dd1e97c3053e61ed5905dbdae7341` | host root (`netbird service install`); identity owner |
| Go toolchain for explicit developer source builds only | `1.26.3` archive amd64 `2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556`; arm64 `9d89a3ea57d141c2b22d70083f2c8459ba3890f2d9e818e7e933b75614936565` | developer root path only; release owner |
| Flux, cert-manager, CNPG, Barman vendor manifests | Flux `v2.9.5` SHA-256 `cc3dcd743af16215838b6937e1fce83745bf24c0dcc6c59737c59df15429caaf`; cert-manager `v1.21.2` `e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f`; CNPG `v1.30.1` commit `2a35abb4628f209d149825ef3c38011e0701ff2f` `37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb`; Barman `v0.15.0` `1c483eae12a7424ad28ac66bdfee771b510ee8e234cb8756c3ade7254cef2fad` | root bootstraps Fabric state; controllers use Kubernetes service accounts; platform owner |
| Forgejo and Kata container images | multiarch index digests `codeberg.org/forgejo/forgejo:15.0.9-rootless@sha256:caf1bca332f95cdcf124227a4bfa3b49bbbfbbc8a5e4a97406921cb581165413` and `quay.io/kata-containers/kata-deploy:4.2.0@sha256:8878e275eeb611f5ed1613009db195f4034b25fea461aef61d407fead58872d3` | Kubernetes privileged Kata deploy daemon; platform owner |

Only hashes embedded in a previously authenticated release count. The offline cache
is a transport: each payload is rehashed before installation; interrupted transfers
do not promote partial files. Vendor/operator manifests and Forgejo/Kata index images
are pinned separately; inspect the **other** operator-manifest image references and Kata
runtime dependencies before claiming complete supply-chain provenance. Digest pinning
ensures bytes, not an upstream software vendor's signing identity.

For advisories, a named human release owner should check upstream CNPG, k3s, Nebula,
NetBird, Forgejo, Kata, Flux, cert-manager, Barman and each certified recipe's owner.
Record the upstream advisory and exact affected pinned version, assign triage, test a
replacement digest on a disposable fabric, retain test/restore evidence and tell affected
operators when upgrade action is required. Neither an unstaffed SLA nor untested recipe
certification is implied. Production S3 socket guard refuses private/loopback or redirect
destinations; dev explicitly permits loopback fixtures. Terminating Member requests lose
authorization immediately. B2 calls have a bounded exchange and body read, so a stalled
response cannot indefinitely retain the writer tick.

## Does not hold, and why

| Gap | Reality | Mitigation |
|---|---|---|
| Kernel escape | Pods share the box's kernel. User namespaces and `restricted` shrink the attack surface; they do not make a kernel exploit impossible. | Workspaces, where people run their own code, get their own kernel in a Kata VM (decision 26). Other apps share the box's kernel. |
| Every steward is trusted with fabric-level secrets | The Nebula CA key and the fabric's service tokens are encrypted to every steward, so any steward can take over as writer. A steward's root can read them. | The fabric is for people who trust each other's stewards (decisions 5 and 11). A fabric that outgrows that splits the CA from the rest. |
| The site owner sees everything placed at their site | Volumes are files on the owner's disk; secrets decrypted at a site are readable by its root. | The trusted-peer boundary, by design. Encryption of specific data is an app concern. |
| Immediate revocation of a box | A blocked certificate stops working at each box's next sync, up to an hour later; a box that cannot reach a steward keeps the blocklist it has until its own certificate expires. | Keep certificates short. |
| Images are whatever the deployer names | Public registries, no signature check. | A fabric registry with signing is on the roadmap. |
| One public entrance | Until a second public site exists, losing the first takes public names and people's mesh sign-in offline (apps keep running at their sites). NetBird runs only at the first public site, so a second one brings back public names, not sign-in. | Add a second entrance (docs/operations.md). |

## In one sentence

A workload deployed through WeCoLab cannot become root on the box, cannot reach the box's other
services or its LAN, and cannot take more than it was offered; it can still exploit a kernel bug like
any container anywhere, and every steward is trusted with the fabric's keys.
