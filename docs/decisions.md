# Decisions

Each entry is a choice this repository is built on, why it was made, and what would make us revisit
it. They come from running the first WeCoLab (the Karmada-based lab of September 2026) and are the
reason this is a fresh repository rather than a continuation.

## 1. The Fabric is a Git repository with a full copy at every site

Every site runs Forgejo holding the whole desired state. One copy accepts writes; the others mirror
it. Flux at every site applies its own copy. There is no hub: a site that loses every other site
keeps running what it has, from its own copy.

- **Why:** the goal is a fabric that survives losing any one site, starting from two sites. The
  first WeCoLab used Karmada, whose etcd needs three voting members, so it could never meet that
  goal with two sites, and its hub was a single point of failure.
- **Revisit when:** never for the principle. The writer's election is decision 9.

## 2. Each site derives its role for each app from Git

Every site of an app computes its database's CloudNativePG replica settings from the App's spec in Git
alone: `primary`, and during a planned move `handover {id, from, token}` (`RoleAt`). A planned move is a
token handover: the old primary demotes and takes the demotion token its database minted for this
handover, never one it already showed when it first saw the handover; the writer copies that token into
`handover.token`; the new primary promotes with it, and the writer clears the handover once the new primary
reports it promoted with that token. A forced move changes `primary` with no handover and gives every other
site a new archive generation.

- **Why:** no site decides through another, so a site that lags or cannot reach the others never makes a
  second primary by itself. The first version had each site publish its decision and follow the others';
  the review of 2026-09-29 found a fresh site promoting an empty database and switchovers in flight that
  ignored later changes (decision 14). The token handover itself was drilled live on the first lab in both
  directions.
- **Revisit when:** a planned move must complete while the writer is away: today the writer copies the
  token.

## 3. Databases replicate only through the vault

Each app's PostgreSQL (CloudNativePG) archives its WAL and base backups to object storage under
Object Lock; standbys at other sites replay from there. A site rebuilt from the vault gets a new
archive generation, because Object Lock never empties the old archive.

- **Why:** no site-to-site data path to secure or debug; the third copy nobody can delete comes for
  free. The recovery point is the archive timeout, about a minute.
- **Revisit when:** someone needs seconds of recovery point: add streaming over Nebula.

## 4. Two meshes: Nebula for boxes, NetBird for people

Every box (public entrances, site managers, nodes, Macs) is a Nebula host. People's phones and
laptops use NetBird, whose apps and single sign-on suit them. The Door is on both and is the only
bridge: people reach private apps through it, never the boxes.

- **Why:** NetBird's control plane cannot be made highly available without its enterprise license,
  and the boxes must keep talking when any one box is gone. Nebula's lighthouses are stateless and
  tunnels between known hosts survive their loss.
- **Revisit when:** NetBird ships high availability in the open-source server.

## 5. The Nebula certificate authority is online, host certificates are short-lived

The CA key is kept in the Fabric, encrypted to the stewards, so the writer can admit a new box from a
one-time invite without anyone at a keyboard, and any steward can renew certificates. Host keys are born
on the host and never leave it; only the public half is signed. Host certificates last thirty days and
each box renews its own over Nebula. A renewal only ever re-signs the public key registered for that box
in the Fabric, and only for a request from that box's own Nebula address, so it needs no secret: a
certificate is useless without the key it was issued for.

- **Why:** an offline CA means someone signs every new box by hand. Short certificates bound the damage of
  a leaked host key; the blocklist (decision 20) covers the time until they expire.
- **Revisit when:** the fabric grows beyond people who all trust each other's site managers.

## 6. People type only what the fabric cannot make

Every secret the fabric can generate is generated where it is used: site age keys, Nebula host keys,
tokens, session keys, database passwords, invite codes. A person enters only what comes from outside:
the domain and email at install, an object storage account key, an outside sign-in provider's secret.
Entered secrets go into the Console once, are stored encrypted in the Fabric, and are never shown back.
Codes are shown once and kept only as hashes.

- **Why:** a secret nobody typed cannot be pasted into the wrong place.

## 7. The Console writes only commits

Every change is decided from the Fabric as Git holds it (decision 15), tried first as a dry run, as the
signed-in person, against the local cluster's API, so Kubernetes RBAC still decides who may do what, and
then committed to the Fabric with that person as the author. The Fabric's history is the audit log.

## 8. Platform components come from the Fabric too

Nebula and k3s are installed by the join script; Forgejo and Flux are bootstrapped by it; everything
else (CloudNativePG, the Barman Cloud plugin, Warden, the site entrance, policies, the Door and DNS at
public sites) is in the Fabric's `system/` folder and applied by Flux. A new site installs everything
else from its own copy of the Fabric, and an upgrade is a commit: `warden upgrade` writes a version's
`system/` and `crds/` into the Fabric.

## 9. The writer is chosen by a person, and Git fences the old one

With two sites there is no third vote, so a person decides when the writer moves: one action in another
steward's Console, which commits the new writer with a higher epoch. Each site follows the writer its own
copy of the Fabric names, or a steward that claims a higher epoch; a claim naming a site that is not a
steward counts for nothing. The writer pushes each commit to every other copy, and every copy's `main`
accepts only fast-forward pushes from the mirroring account; an old writer that kept committing alone is
refused by Git, not by our code, and its commits are kept aside as a branch.

- **Revisit when:** a fabric wants unattended writer failover. It needs a third vote: an object-store
  lease with compare-and-swap (AWS S3, Cloudflare R2; Backblaze B2 does not support conditional writes).

## 10. Sites talk host to host

Cross-site traffic is only between Nebula host addresses: Warden status, certificate renewals, Git
mirroring, and the Door reaching an app's node ports. Pods never route across sites, so every site can use
k3s's default pod and service ranges.

## 11. Stewards hold the fabric; other sites hold only what runs on them

A site is a steward when the fabric's admins trust it with the fabric: the Nebula CA, the fabric's
service tokens, every app's secrets, and the right to become the writer. A collaborator's site that only
contributes capacity is not a steward: it decrypts only the secrets of the apps placed on it.

- **Why:** a person contributing a box should not be able to read every project's secrets. Two stewards
  are enough to survive losing either.

## 12. Data is never deleted by a commit dropping a file

Volumes and databases carry Flux's `prune: disabled`. Warden deletes an app's data at a site the app
left, deliberately and only on proof (decision 17); a file that disappears from an app's folder deletes
nothing. Nor does an App missing from Git: every site keeps its databases and volumes for a person to
delete. Deleting an app is an explicit mark in Git (`spec.deleted`): every site removes its part, data
included, and the writer removes the App once every site reports nothing left.

## 13. Images before a public registry

Until the project publishes images to a registry, the first box builds Warden and the Console from source
(or takes the binaries `make dist` built), turns them into image tarballs (`warden image`) and imports them;
every box that joins downloads them from the Console's `/dl/` and imports them. Once published, the Fabric
names released images and nothing is built on a box.

## 14. Git decides; a site speaks only for itself

Whatever affects more than one site is decided from the Fabric, never from what sites say. A site's
`/status` is read from its manager's Nebula address (Nebula drops a packet from outside its sender's
certificate, so the answer is that site's) and must name that site. What it says is used only about that
site and only as far as the Fabric allows: an app's endpoints count only as `ip:port` on one of that
site's boxes with a port in 30000-32767; a writer claim counts only from a steward, highest epoch first;
everything else is shown, never acted on. Database roles come from the App's spec (decision 2).

- **Why:** the first WeCoLab had one trusted hub, so whatever it published was believed. Here every site is
  a peer, and a collaborator's site is trusted with capacity, not with the fabric. Code carried over from the hub
  believed peers' reports: any site of an app that claimed to be active drew the app's users to its own
  boxes, and a site with no status took `spec.primary` and promoted an empty database.

## 15. The Fabric changes only by conditional edits read from Git

One primitive, `fabric.Edit`: read the files from the writer's copy with their hashes, decide, and commit
only if none of the files it changes changed meanwhile (Forgejo refuses a stale hash with 409, a create
where the file now exists with 422, and a commit whose branch moved); on a conflict read and decide again.
Every Console change and every Warden commit goes through it. The cluster's copy, which Flux applies up to
a minute behind Git, is for pages and for acting on, never the basis of a change. Allocation happens inside
the edit: a box's address from its site's `nextBox` (numbers are never reused), a new site's index from the
Sites in Git, the writer's epoch from `system/base/fabric.yaml` in Git. A decision that rests on files read
but not changed (a box name unique across every Site) cannot be conditioned on them, so the Console admits
one join at a time.

- **Why:** changes decided from the cluster copy and written blind made every write last-writer-wins: two
  joins got the same box address and the second erased the first box, a later change to an App undid a
  move, a key could be redeemed twice.
- **Revisit when:** a second process must commit decisions that span files it does not change.

## 16. Names are data, never syntax

`internal/validate` defines every name: labels (sites, projects, apps, boxes, hosts), host names, emails,
and the names the fabric keeps for itself (`recovery`, the system namespaces, `console`, `door`, `mesh`,
`ns1`, `ns2`, `www`, `people`). The CRDs carry the same patterns and CEL rules (an app's primary is one of
its sites), so nothing written to Git by any path carries syntax. Every sink serialises structurally:
Traefik's configuration from typed structs with explicit priorities, zone records only from checked names,
the Console's page through one escaping template, secrets in files, stdin or the environment, never on a
command line.

- **Why:** Traefik configuration, the zone file, HTML, shell commands and Git paths were assembled from
  names with string formatting, so every name was an injection.

## 17. Destroy only on proof from Git and from the survivor

A site deletes its copy of an app's database, to rebuild it or because the app left the site, only when
Git says it holds nothing a move still needs (it is not the primary, not a planned move's origin, and no
handover waits for a token) and the primary reports itself the primary, writable, healthy, promoted with
the handover's token if Git has one, and with a base backup in its vault (`MayDestroy`). The primary can
never be removed from an app's sites. A database missing where the app's history is held is never made
again empty: the primary makes one by initdb only while Git shows no sign it was ever made (no archive
generation, no handover) and Flux never made one at that site. Otherwise the app's delivery there stays
suspended with a `DatabaseMissing` condition until a person moves the primary.

- **Why:** destructive steps acted on local views. A forced move to a site that never had the database
  started an empty primary; the old primary then saw a writable primary with a backup and was rebuilt from
  it, losing the real history. "Highly recoverable over highly available."
- **Revisit when:** a forced move back to a former primary must not count that site's old base backup: the
  check accepts any base backup under the primary's current archive, and would then need the backup's
  timeline.

## 18. Each component holds only what it uses

- Nodes join k3s with the site's agent token, which cannot fetch the cluster's bootstrap data or CA keys;
  the server token stays on managers.
- The Door has its own ServiceAccount, which may read Sites, Apps and the fabric's settings and nothing
  else; its token is mounted only in the Door's Warden container.
- The node agent on a laptop may change only its own node's idle taint (a ValidatingAdmissionPolicy on the
  node name bound into its token).
- The Console impersonates users, never groups, and reads Secrets only in `wecolab-system`, besides its
  site's age key.
- Flux's kustomize-controller runs with `--no-cross-namespace-refs`, `--no-remote-bases` and
  `--default-service-account=default`, and every platform Kustomization names `kustomize-controller` as its
  service account, so a Kustomization that names none applies as nobody.

- **Why:** the Door used Warden's cluster-admin account, every node held the k3s server token (full
  administrator access), and the node agent could patch every node.

## 19. Only Nebula reaches WeCoLab's ports

k3s binds the box's Nebula address. WeCoLab's own `WECOLAB-HOST` chain, for IPv4 and IPv6, drops the
Kubernetes API (6443), the kubelet (10250), flannel's VXLAN (8472/udp) and NetBird's metrics (9091) from
anywhere but loopback, Nebula and the pod interfaces, whether or not ufw is active; it and pod isolation
run before k3s starts, and uninstall removes both. Over Nebula, the certificate service has its own port
(8094 on stewards' managers, open to every box), Warden's status (8093) is open to managers only and
Forgejo (30300) to stewards only. The Door never routes NetBird's `/api/setup` or `/api/instance`, and
its DNS-challenge endpoint takes only requests with basic auth from files inside the Door's pod, for
`_acme-challenge.` names in the zone.

- **Why:** k3s listened on every address, VXLAN binds the wildcard whatever k3s is told, and we relied on
  ufw happening to be there. NetBird's `/api/setup` makes its first caller the owner of its identity
  provider.

## 20. Access is declared in Git and reconciled, never granted once

- Revocation is a loop at the writer: every certificate issued for a box that is no longer in any Site
  (the join certificates recorded on the Box, and the renewals stewards publish) goes into the fabric's
  blocklist in Git, and expired entries leave it. Every box applies the blocklist at its next sync.
- SSH keys come from Members: every hour each box rewrites a marked block of root's `authorized_keys`
  with the keys of the fabric's admins and of the members of the project that owns its site, so a person
  removed or blocked loses access within the hour.
- People's devices join NetBird by signing in, so they belong to their person; no setup keys for people.
  RBAC bindings are owned by their Member and are deleted with it.
- The writer renews NetBird's service token two months before it expires.

- **Why:** access granted once outlived its reason: join certificates were not recorded, box addresses were
  reused, SSH keys were appended once, and setup-key devices belonged to nobody.
- **Revisit when:** certificates must be much shorter than thirty days: a box whose certificate expired
  cannot reach a steward over Nebula, so renewal would need a path through the public Door.

## 21. Hosts converge; fabric operations live in Go

The install script keeps the host steps (packages, binaries, units, firewall), each checked before it acts
and recorded for uninstall. On a box that belongs to a fabric it only converges those steps, never the
fabric's keys, tokens or certificates, and never makes a fabric again. After the Fabric's first commit,
what changes it is Go and goes through `fabric.Edit`: `warden people` (the owner's account and the people
mesh's service token, resumed where it stopped) and `warden upgrade` (a version's `system/` and `crds/`).
Secrets reach commands through files, stdin and the environment.

- **Why:** a re-run of the bash install could overwrite the site's age key and reset its k3s token, and
  secrets passed through command lines any process on the box can read.

## 22. Contracts between layers are tested

Tests check that every `fetch` in the Console's page names a registered route; that install.sh reads only
fields the join response has, and that its firewall, SSH-key and sync functions do what they say when run
in bash against stubs; that the Door's configuration keeps hostile names out; and a simulation plays random
moves and database events against the roles, the writer's steps and decision 17's guard.

- **Why:** the page, install.sh, the Door and Warden each assumed shapes nobody checked, which is how
  leftovers from the first WeCoLab survived.

## 23. Logs record changes and refusals, never secrets, and not other people's visitors

The Console logs every change and every refusal, with who and why, under a request id the page shows
and the commit carries; Git already records what changed. Nothing secret is logged: no query string,
header, cookie or body. The Door logs only errors, and nothing for apps' routes. Each site keeps its
own logs; none are shipped anywhere.

- **Why:** a refused move left no trace anywhere, so nobody could say what the person had seen. Visitor
  addresses are personal data, and the public site's owner has no claim on another project's visitors.
- **Revisit:** a store per site when someone needs a request older than the last restart more than
  once; shipping logs between sites only when following one action across them becomes routine, and
  then system namespaces only.

## 24. The public repository gets gated snapshots, never the private history

Development happens in a private repository. The public one receives main's tree as one commit per
publish, through a gate (development.md, Publishing): no secret, no private term, no key file. Its author
is a public identity.

- **Why:** a history carries more than its last tree: commit messages naming machines and people,
  authors' emails, files once added and removed. Checking one tree and one message is a gate a person can
  trust; rewriting a whole history before each publish is not.
- **Revisit:** when outside contributors need the real history to work from, publish it after one
  rewrite and move development to the public repository.

## 25. A site leaves by one commit; what it held is not taken back

Removing a site is one conditional commit: the site, its key and secret, its place in every app and its
offers go, and its boxes' certificates join the blocklist. The rest follows from Git: the writer stops
pushing to it, the Door stops naming it, boxes drop its certificates. Each site's Warden deletes the
nodes of boxes its Site no longer lists. Vault keys its apps carried are rotated: the new key is
committed with the old one marked retiring, and the writer deletes the old key at B2 once every site of
the project's database apps reports the new one. The site that left is not among them, so it is not
waited for; a site that remains and does not answer is, since deleting the key it still archives with
would stop its archive.

- **Why:** a collaborator can leave on bad terms. Everything they could reach must stop being reachable, and
  every step must follow from what Git says rather than from asking a site that is no longer trusted.
- **Not taken back:** what the site holds already. A steward held the fabric's CA and tokens, so a steward
  leaves only after Stop being a steward, and rotating the CA is the next thing to build.

## 26. Workspaces run in their own VM; databases do not; whole VMs only on request

A workspace (a desktop, a shell, anything a person runs their own code in) runs under Kata Containers'
`kata-clh` RuntimeClass: a pod as usual, with its own kernel in a small VM. It runs only on boxes with
KVM. The VM is its boundary, so the project policy accepts that RuntimeClass in place of `hostUsers:
false`, which Kata refuses. Databases and WeCoLab's own components stay on the default runtime. KubeVirt,
for a project that needs a whole guest OS, is installed at a site only when a project asks, and its VMs
stay on that site's KVM boxes. Built: Kata in `system/wecolab` on boxes install.sh labels
`wecolab.io/kvm=true`, the policy, and the catalog's Workspace (a Selkies desktop, on the mesh only).
KubeVirt is not built.

- **Why:** measured on a 16-core desktop on 2026-10-01. A Kata pod started in 2 to 3 seconds instead of
  1 and cost about 175 MiB; a Selkies desktop ran in one unchanged. `kata-qemu` was slower to start and
  cost more. Postgres under Kata lost 45% of its writes and two thirds of its reads, a price for no gain
  on images we choose. KubeVirt cost 889 MiB at the site before its first VM; a VM booted in about 13
  seconds and passed Pod Security `restricted` with KubeVirt's own seccomp profile.
- **Limits:** a laptop node runs neither. An M1 or M2 Mac cannot nest virtualization; KubeVirt allows
  arm64 VMs only with KVM, and its cross-architecture emulation needs QEMU builds the release does not
  ship. A VM moves between sites as an app does, stopped at one and started at the other, and its disks
  are not replicated, so only a VM without data of its own can move.
- **Revisit:** a Mac with M3 or later on macOS 15, which can nest, brings laptops into both. A project
  whose VM disk must survive losing a site needs disk replication first.

## Not adopted

- **Karmada:** replaced by decision 1. Its useful parts (delivery, per-site differences, status in one
  place) are Flux, Warden's patches, and the sites' published status.
- **Liqo:** later, when a project needs to overflow best-effort work onto another site's spare
  capacity. Until then spare capacity joins a site as a node (how a Mac contributes).
- **Eclipse KuDECO (CODECO):** a research framework for automatic placement across edge clusters,
  federated through a hub (Open Cluster Management). We place deliberately and have no hub. Its
  per-app neighborhoods match how our sites already coordinate.
- **Cloudflare Tunnel:** the Door and our own DNS replace it; Cloudflare stays only as the domain's DNS
  host, for the one-time delegation.
