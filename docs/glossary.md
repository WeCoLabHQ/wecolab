# Glossary

Every word the WeCoLab docs and Console use, what it means, and the nearest Kubernetes or standard term.

Most words here are WeCoLab's own: fabric, site, box, steward, writer, epoch, Door, Names, vault,
handover, archive generation, project, offer, pool, gate, Protected, invite, converge, Warden and Console
among them. The rest are upstream names kept as they are: Flux, Forgejo, k3s, Nebula (with its lighthouse
and relay), NetBird, PostgreSQL (with its WAL), CloudNativePG (with its primary, replica cluster,
demotion token and base backup), the Barman Cloud plugin, Traefik, CoreDNS, SOPS, age, S3 Object Lock and
RPO. For those the last column says *upstream*.

## The cooperative

| Term | What it is | Kubernetes / standard equivalent |
|---|---|---|
| WeCoLab | A cooperative cloud built from collaborators' homelabs. An app runs at one site, keeps a standby at a collaborator's site, and backs up its database to object storage under Object Lock. | A GitOps multi-cluster platform |
| Collaborator | Anyone who runs a site or uses the fabric with you: a friend, a colleague, a neighbour, an organisation. Trust comes from roles (a steward, an admin, a project member), never from the relationship, and a collaborator's site can be removed in one step (operations.md, When a collaborator leaves). | None; a tenant or a federation member |
| fabric (lowercase) | The whole cooperative: every site, person, app and key that belong together. Running `install.sh` without an invite makes a new one. | A fleet of clusters |
| the Fabric (capitalised) | The Git repository holding the fabric's whole desired state: sites, people, projects, apps, encrypted secrets and the platform's manifests. Every site keeps a full copy in its own Forgejo, and Flux applies that local copy. | A GitOps repository (a Flux GitRepository source), copied to every site by Forgejo push mirrors |
| Site | One member's homelab: one k3s cluster, owned by one project. Recorded in the Fabric as a Site object that lists its boxes. | A Kubernetes cluster (k3s); the cluster-scoped CRD `sites.wecolab.io` |
| Site owner | The project named on a Site. Its members manage the site's boxes and offers. Not the same as the *owner* role. | None; the tenant that administers a cluster |
| First site | The site `install.sh` makes without an invite. It is public, a steward and the first writer, and it runs NetBird. | The bootstrap cluster |
| Public site | A site whose manager has a public IPv4 address. It runs the Door, Names and a Nebula lighthouse and relay. | An edge or ingress cluster |
| Entrance | The public-facing part of a public site: the Door, Names and the lighthouse. Also the name of its Nebula certificate group and of its Flux Kustomization. | Edge / ingress tier |
| Steward | A site trusted with the fabric itself. It decrypts the fabric-level secrets (the Nebula CA key, every site's k3s tokens, NetBird's token), renews box certificates, runs a Console and can become the writer. With two stewards the fabric survives losing either one. An admin chooses which sites are stewards. | None; closest is a management cluster holding the PKI and the SOPS keys |
| Writer | The one steward whose copy of the Fabric accepts commits and pushes them to every other copy. Its Console is the one people use, and it does the fabric-wide jobs. A person chooses it; its name and epoch are in the fabric's settings. | A single leader, like a Git primary, chosen by a person rather than elected |
| Epoch | A number stored with the writer. Every takeover raises it. Among stewards' claims the highest epoch wins, and ties go to the lowest site name. | A fencing token, or a leader term as in Raft |
| Writer claim | The writer and epoch a site's Warden publishes in its status, read from its own copy of the Fabric. Only stewards' claims count. | A leader announcement |
| Takeover | A person makes another steward the writer: Settings → Take over as writer, or `sudo bash install.sh takeover` on that steward's manager. | Manual leader failover |
| Superseded branch | `superseded-<site>-<commit>`: commits an old writer made that no other copy received. They are pushed to the new writer as a branch for a person to read. | A Git branch that keeps a diverged history |
| Mirroring account | The Forgejo account the writer pushes with. At every other copy it is the only account that may push `main`, and only as a fast-forward. | A push-mirror user plus branch protection |
| Conditional edit (`fabric.Edit`) | How every change reaches the Fabric. The files are read with their hashes, the change is decided from them, and the commit goes through only if those files did not change meanwhile; otherwise it starts over. | Optimistic concurrency, like `resourceVersion` |
| The fabric's settings | One ConfigMap, `fabric` in `wecolab-system`, kept in `system/base/fabric.yaml`: the zone, the owner's email, the Nebula network, the writer and epoch, which site runs NetBird, the certificate lifetime and the blocklist. It is not a CRD. | A ConfigMap |
| Zone | The DNS subdomain delegated once to the fabric at your DNS host (for example `fab.example.org`), answered by Names. An app's default name is `<app>.<zone>`. | A delegated DNS zone (NS records) |
| Invite | A one-time code (`wcl2.…`) that lets one new site or box join. It is valid for a day and kept only as a hash. Mac invites and Linux invites are not interchangeable. | A single-use join token, like a kubeadm bootstrap token |
| Join script | `install.sh` run with an invite: `curl … /join.sh \| sudo bash -s <invite>`. Each Console serves it. | A node bootstrap script |
| Converge | Running `install.sh` with no argument on a box that already belongs to a fabric. It redoes the box's own setup (packages, Nebula, certificate sync, firewall) and never touches the fabric's keys. | An idempotent configuration-management run |
| Upgrade | `warden upgrade`: commits a WeCoLab version's `system/` and `crds/` to the Fabric; every site's Flux then applies them. | A GitOps version bump |
| Development fabric | `hack/dev`: a whole fabric in Docker on one laptop (containers `wcl-pub`, `wcl-home`, `wcl-mac`, `wcl-vault`). | A local test environment, like kind |

## Machines and networks

| Term | What it is | Kubernetes / standard equivalent |
|---|---|---|
| Box | One Linux machine of a site, or the Debian VM a Mac runs. It runs Nebula and k3s on the host. Named `<site>-<host>`, unique across the fabric. | A Kubernetes node (the machine) |
| Manager | A site's first box. It runs the k3s server, Warden's status and, at a steward, the certificate service. It cannot be removed on its own: it leaves with its site, and removing a site is not built yet. | A k3s server node (control plane that also runs work) |
| Node | Any box that joins a site after its manager. | A k3s agent (worker node) |
| Laptop node | A node running in WeCoLab for Mac's VM. Only best-effort work runs there, and only while its person is not using the Mac. | A worker labelled `wecolab.io/laptop=true` and tainted `wecolab.io/laptop` and `wecolab.io/idle` |
| Idle, active, idle taint | Whether the Mac's person is using it. While they are active, the node agent keeps the `wecolab.io/idle` taint on the node, which evicts best-effort pods after a minute. | A node taint (NoSchedule, NoExecute) |
| Reservation, idle profile | What a Mac gives its VM: the reservation always, while WeCoLab runs; the idle profile, a larger size, only while nobody touches the Mac. | VM resource settings, like Docker Desktop's |
| Host-guest channel | The `share/` folder on the Mac, mounted at `/wecolab` in the VM. It carries the mode file, the join log and `nebula.json`. | A virtio-fs shared folder |
| Nebula | The encrypted network between boxes (`10.77.0.0/16`, one `/24` per site). All traffic between sites crosses it. | *Upstream*: an overlay VPN |
| Nebula address | A box's address on Nebula: `10.77.<site index>.<box number>`. WeCoLab for Mac calls it "Mesh address". | An overlay IP |
| Site index, `nextBox` | A site's number in the Nebula network, and the number its next box gets. Box numbers are never reused. | IP address management (IPAM) |
| Lighthouse, relay | Nebula's discovery host and traffic relay. Every public site is both. | *Upstream* Nebula terms; comparable to STUN and TURN |
| Nebula CA | The fabric's certificate authority for Nebula. Made at install, kept encrypted in the Fabric for the stewards, valid five years. | A PKI root CA |
| Certificate service | Warden on a steward's manager, port 8094 over Nebula. Each box posts its certificate every hour and gets back the CA bundle, its Nebula settings (lighthouses, firewall, blocklist), the stewards' addresses and its SSH keys, and a new certificate for its registered key when a third of its current one's life is left. | An online CA renewal endpoint, like step-ca |
| Certificate sync | The `wecolab-nebula-sync` systemd timer on every box. It calls the certificate service hourly and swaps files only once Nebula accepts them. | A systemd timer |
| Join certificate | The certificate the Console signs when a box joins. It is recorded on the box in the Fabric, so removing the box can block it. | A bootstrap credential |
| Blocklist | The fabric's list of blocked Nebula certificates, in its settings. The Console adds a box's join certificates when it removes the box, and the writer adds every other certificate issued for a box that has left; every box applies the list at its next sync. | A certificate revocation list (Nebula's `pki.blocklist`) |
| People mesh | NetBird: the private network for people's phones and laptops, run at the first public site. Only the Door's box is on it, so people reach apps' mesh names, never boxes. The Console's Mesh page shows it. | *Upstream* NetBird: a WireGuard VPN with single sign-on |
| Mesh-only name | `<app>-<project>.mesh.<zone>`: an app name that resolves only to the Door's NetBird address and admits only people on NetBird. | A private DNS name behind a VPN |
| Door | The public entrance at each public site: one pod with Traefik (TLS from Let's Encrypt), Names and the entrance Warden. It forwards each app's name over Nebula to node ports at the app's primary site. | An ingress controller and reverse proxy (Traefik) acting as a global load balancer |
| Door route | The Door's route for one app's hostname, to its primary site's boxes. | An Ingress or IngressRoute |
| Names | CoreDNS in the Door's pod, answering for the fabric's zone from a zone file the entrance Warden writes. | An authoritative DNS server (CoreDNS), in a role like external-dns |

## Apps and data

| Term | What it is | Kubernetes / standard equivalent |
|---|---|---|
| App | An application placed at one or more sites. Its App object records the workload, the sites, the primary, an optional database, hostname, whether it has a mesh name, RPO and each site's archive generation. | The namespaced CRD `apps.wecolab.io`; like an Argo CD Application with placement |
| App folder, workload | `projects/<project>/<app>/` in the Fabric: the Deployment, Service, Secret and volumes, plus the CloudNativePG Cluster, ObjectStore and ScheduledBackup when there is a database. Each of the app's sites applies it through a Flux Kustomization as the project's ServiceAccount. The workload is the Deployment's name. | A Kustomize directory applied by a Flux Kustomization |
| Catalog | The ready-made apps on the Deploy page, imported from the HomelabOS catalog. Each becomes a workload with a volume per data path, sidecars, and a replicated PostgreSQL when the app uses one. | An app catalog, like a Helm chart repository |
| Primary | The site that receives the app's traffic and holds its writable database (`spec.primary`). An app with a database runs only there; one without runs at all its sites. | *Upstream* CloudNativePG: the primary cluster of a distributed topology; the active site |
| Standby | The app's database at every other site, replaying the primary's WAL from the vault. An app with a database is stopped at its standbys. | *Upstream* CloudNativePG: a replica cluster |
| Active | The site a given site has actually applied as primary (`status.active`). | Observed state in a status field |
| Role | Whether a site is primary or standby for an app, worked out from the App in Git alone, never by asking other sites (`RoleAt`). | None |
| Volume | A PersistentVolumeClaim at one site. Never replicated and never in the vault: after a move, database data follows the app, volume data (a catalog app's uploaded files, say) stays where it was written. | A PersistentVolumeClaim |
| Planned move (Change primary) | Moving an app's primary to a site whose standby is staged, while the old primary is healthy. No committed write is lost; the app stops for a minute or two. | *Upstream* CloudNativePG: a replica-cluster switchover with a demotion and promotion token |
| Handover | The record of a planned move in progress: `spec.handover {id, from, token}` on the App. | None; carries CloudNativePG's demotion token to the new primary |
| Demotion token | The token the old primary's database gives when it stops taking writes. The new primary starts with it, so the database's history cannot split in two. | *Upstream* CloudNativePG `status.demotionToken`, used as `spec.replica.promotionToken` |
| Retarget, cancel | While the token is not yet in Git: moving again to another standby (retarget), or back to the old primary (cancel). | None |
| Forced move (Force) | Making another site primary without a handover, for when the old primary is gone. Up to about a minute of writes may be lost, and every other site's database is rebuilt from the vault. | Failover (manual promotion) |
| Vault | A project's object-storage bucket under Object Lock, holding its databases' WAL and base backups. It is the only path database data takes between sites. Made once per project: on the Storage page from the account key, or by entering a bucket, endpoint and key at the project's first database deploy. | An S3-compatible bucket (B2, S3, R2, MinIO) used through CloudNativePG's Barman Cloud plugin (an ObjectStore) |
| Database Secret | `<app>-db-app`: the password the app's database owner signs in with, and the connection details the app reads (`uri`, `host`, `port`, `user`, `password`, `dbname`). The Console generates it once and keeps it in the app's folder, so it is the same at every site and a move never changes it. | A Kubernetes Secret, named as CloudNativePG would name its own and passed to it as the bootstrap `secret` |
| Account key, vault key | The account key is a Backblaze B2 account key an admin enters in Settings; it is used only to create vaults on B2. A vault key reaches only one project's bucket; each database app keeps a copy. | Cloud storage credentials |
| Object Lock | A bucket setting that makes stored files impossible to delete or overwrite for a retention period. Vaults the Console creates use 30 days in compliance mode. | *Upstream* S3 Object Lock (write once, read many) |
| WAL | PostgreSQL's write-ahead log. The primary archives it to the vault, within a minute of any write (`archive_timeout`); standbys replay it. | *Upstream* PostgreSQL continuous archiving |
| Archive | One site's folder of WAL and base backups in the vault, `<db>-<site>`. Standbys replay the primary's. | A WAL archive (Barman Cloud `serverName`) |
| Archive generation | A per-site number in the App (`spec.archive`). A new generation sends that site's database to a fresh folder, `<db>-<site>-g<n>`, and rebuilds it, because Object Lock keeps the old folder from being emptied. A recorded generation is also how Git shows a database was made. | None; a suffix on Barman's `serverName` |
| Base backup | A full copy of an app's database in the vault, taken only at the primary: once as soon as its folder has none, then every night at 02:00. | *Upstream* CloudNativePG Backup and ScheduledBackup |
| Rebuild | Deleting a site's copy of a database and recreating it from the vault under a new archive generation. It happens only once the primary proves it holds the data. The rebuilt database keeps the app's own database name and owner. | A CloudNativePG recovery bootstrap (restore from object store) |
| The site holding the app's history | The site whose database is the real one: the primary, or during a planned move the old primary until its token is in Git. | The authoritative database timeline |
| `DatabaseMissing` | Shown when the site holding the app's history has no database. Warden suspends the app's delivery there until a person forces the primary to a site that has the database; WeCoLab never makes an empty one in its place. | A status condition reason |
| Delivery, suspended | Delivery is Flux applying an app at a site. Suspended means Warden has paused that app's Flux Kustomization there. | Flux Kustomization `spec.suspend` |
| Deleted mark | `spec.deleted`, what Delete writes on the App in Git. Every site removes its part, data included; then the writer removes the App. An App merely missing from Git deletes nothing: every site keeps its databases and volumes for a person to delete. | None; a deletion recorded in Git, like a finalizer-driven delete |
| Checks, gates | An app's conditions, which the Console works out from what its sites report (only Promotion is kept on the App): PrimaryHealthy, StandbyStaged, WithinRPO, VaultFresh, Routed, Promotion and Ready. The gates are the ones a planned move needs: StandbyStaged, and for a new move PrimaryHealthy and WithinRPO. | Kubernetes status conditions; pre-flight checks |
| PrimaryHealthy | The primary site answers and its database is healthy. | A health condition |
| StandbyStaged | The standby site answers, and its database, built for its current archive generation, has a ready instance. | Replica ready |
| WithinRPO | The primary is archiving WAL and the standby's database is healthy. It reports the newest archived WAL's age against the RPO but never fails on that age. | A replication-lag check |
| VaultFresh | The vault answers, has Object Lock, and its newest base backup is under a day old. | A backup-freshness check |
| Routed | A ready pod of the app answers at the site holding its history, so the Door has somewhere to send people. | A Service with ready endpoints |
| Promotion | Where a primary change stands at a site: Applied, Demoting, Promoting, Rebuilding, RebuildWaiting or DatabaseMissing. It does not count toward Ready. | A status condition |
| Protected | The Console's word for an app whose Ready condition is true: every check but Promotion passes. An app at one site is never Protected, since it has no standby. | The Ready condition |
| RPO | Recovery point objective: how much recent data the owner accepts to lose. The Console's default is 5 minutes. It is only reported, never enforced. | *Upstream*: the standard term |
| Placement | Each app's workload and its health at each site, on the Resources page (admins only). | Workload placement |

## People and access

| Term | What it is | Kubernetes / standard equivalent |
|---|---|---|
| Project | A group of people and their apps. At every site it is a namespace of the same name; on the people mesh it is a NetBird group. Every site is owned by one project. | A namespace (tenant) with its RBAC |
| Boundary | What Warden puts in a project's namespace at each site: default limits, network policies and a quota. There is no quota where the project owns the site, and a zero quota where it holds nothing. | LimitRange, NetworkPolicy and ResourceQuota |
| Member | A person's record in the Fabric: email, name, role, projects, SSH keys and whether they are blocked. Its phase is Invited, Active or Blocked. Warden turns it into RBAC bindings at every site. | A user mapped to RoleBindings; the cluster-scoped CRD `members.wecolab.io` |
| owner, admin, member (roles) | *owner* is the person who installed the fabric: an admin the Console never changes or removes. *admin* sees and changes everything and has SSH to every box. *member* changes only what belongs to their projects (apps, vaults, domains, and the boxes and offers of sites the projects own) and has SSH to the boxes of those sites. | `cluster-admin`, versus a read-only ClusterRole plus a per-namespace role |
| Blocked | A member who has been switched off: no Console access, no bindings, their devices leave the people mesh, and their SSH keys are gone within the hour. | A disabled user account |
| Offer | Capacity a site's project shares: CPU, memory and storage for each project holding it, optionally only on named boxes, optionally best effort. A holder gets a ResourceQuota of that size at that site and may deploy there. | A ResourceQuota granted at another cluster, plus node affinity; the CRD `offers.wecolab.io` |
| Holder | A project that holds an offer: named on it, redeemed one of its keys, or in a pool the offer goes into. | A tenant with a quota |
| Offer key, pool key | A single-use code (`wclo1.…` or `wclp1.…`), valid for a day and kept only as a hash. Redeeming it makes a project a holder of the offer, or puts it in the pool. | An invitation token |
| Pool | Shared capacity across sites. Offers go in, and every project in the pool gets the pool's quota (or each offer's own size, if the pool sets none) at each site offering into it. Admins make pools. | None; a quota tier across clusters; the CRD `pools.wecolab.io` |
| Tier | A pool's quota for each project at each site. | A ResourceQuota template |
| Best effort | Capacity that comes and goes (laptops, idle time). Holders' pods there run at the lowest priority and are evicted first. Databases never run there. | The PriorityClass `wecolab-best-effort` (negative, never preempts) plus tolerations |
| `wecolab-protected` | The priority class of every app and database WeCoLab deploys. Best-effort work never preempts it. | A PriorityClass |
| Domain | A project's own domain for its apps. It needs a wildcard CNAME to `door.<zone>` and a `_wecolab` TXT record naming the project; the writer's Warden checks both. | Custom-domain verification by TXT record; the CRD `domains.wecolab.io` |
| Recovery key, recovery card | An age key pair made at install. Every secret in the Fabric is also encrypted to its public half. The private half is written once to `/root/wecolab-recovery-card.txt` (the card), for the owner to copy offline and then delete. | A break-glass key (an extra SOPS age recipient) |
| Age key (a site's) | A site's own decryption key. The private half is the `flux-system/sops-age` Secret and never leaves the site; the public half is `keys/<site>.age.pub`. Flux decrypts the site's secrets with it. | *Upstream* age, used by Flux's SOPS decryption |
| Sealed file | A Secret in the Fabric (`*.sops.yaml`) whose values are encrypted with SOPS to the sites that need it, the stewards and the recovery key. | A SOPS-encrypted Secret; compare Sealed Secrets |

## The pieces WeCoLab runs

| Term | What it is | Kubernetes / standard equivalent |
|---|---|---|
| Warden | WeCoLab's Go program at every site. One binary runs in several modes (site, entrance, node agent) and also provides the CLI steps (`warden bootstrap`, `warden people`, `warden upgrade`, `warden image`, `warden takeover`). | A Kubernetes controller (controller-runtime) reconciling App, Site, Member, Offer, Pool and Domain |
| Site mode | Warden's main job at every site: it works out each app's role there and hands Flux the app's Kustomization, keeps each project's boundary, turns Members into RBAC, and publishes status. At stewards it also runs the certificate service. | A controller-manager Deployment |
| Entrance mode | Warden as a container in the Door's pod. It writes Traefik's routes and Names' zone file, and answers Let's Encrypt's DNS challenge. | A config-rendering sidecar; like external-dns plus an ACME DNS-01 webhook |
| Writer duties | The jobs only the writer's Warden does: pushing the Fabric to every other copy, the steps of planned moves, recording new databases, the blocklist, NetBird's users, groups and token, domain checks, automatic database rebuilds, and removing deleted Apps. | Leader-only reconcilers |
| Node agent | Warden on laptop nodes. It sets or clears the `wecolab.io/idle` taint on its own node, following the Mac's mode file. | A DaemonSet managing one node taint |
| Request id, ref | The id the Console gives each request (`X-Request-Id`). The page shows it with an error as "(ref …)", the Console's log line carries it, and so does any commit the request made (`Request-Id:`). | A correlation id |
| Status | What a site publishes at `http://<manager's Nebula address>:8093/status`: nodes, capacity, databases, apps and their endpoints, volumes, vaults and its writer claim. Other sites trust it only about that site. | A read-only status endpoint |
| Endpoints | The Nebula address and node port of every box running a ready pod of an app, as its site publishes them. The Door uses the primary's. | Service endpoints (EndpointSlices) across clusters |
| Console | The web interface and its API at `console.<zone>`. It runs at every steward; only the writer's is routed and accepts changes. Every change is tried first as the signed-in person, then committed to the Fabric with that person as author. | A GitOps web UI whose only write path is Git commits |
| People step | The last install step (`warden people`): it creates the owner in NetBird's sign-in, makes NetBird's service token and puts the first box on the people mesh. Run it again with `install.sh people`. | A post-install bootstrap job |
| Flux | Applies the site's own copy of the Fabric: its source and kustomize controllers, locked down so an app applies only as its project. | *Upstream* Flux |
| Forgejo | The Git server at every site holding its copy of the Fabric. | *Upstream* Forgejo |
| k3s | The Kubernetes distribution on every box. | *Upstream* k3s |
| CloudNativePG, Barman Cloud plugin | The PostgreSQL operator at every site, and its plugin that archives to and restores from the vault. cert-manager is installed alongside because the plugin needs it. | *Upstream* CloudNativePG, Barman Cloud, cert-manager |
| NetBird | The people mesh's server and client. | *Upstream* NetBird |
| Traefik, CoreDNS | The Door's proxy and Names' DNS server. | *Upstream* Traefik, CoreDNS |
| SOPS, age | The encryption of every secret in the Fabric. | *Upstream* SOPS, age |
| WeCoLab for Mac, `wecolab-node` | The Mac menu-bar app and its CLI. They run a Debian VM that joins a site as a laptop node. | None; like Docker Desktop's VM |
