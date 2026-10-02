# Architecture

WeCoLab joins the sites collaborators run (homelabs, offices, small businesses' servers) into one cooperative
cloud. This document is the whole design: the pieces, where each runs, how they agree without a hub, and what
happens when something is lost. The choices behind it, and when to revisit them, are in
[decisions.md](decisions.md). Every term it uses, with its Kubernetes or standard equivalent, is in
[glossary.md](glossary.md).

## The rule it is built to

> Losing any one site, the first included, loses nothing, starting from two sites; and the fabric gets
> stronger with every site that joins.

Every other property follows from it: the desired state lives at every site, each site runs its own
share, and every site derives what it does from that desired state rather than from a coordinator or
from what other sites say.

## Boxes, sites and roles

- **fabric** (lowercase): the whole cooperative, meaning its sites, people, apps and keys. **The Fabric**
  (capitalised): the Git repository that holds the fabric's desired state, with a full copy at every site.
- **Box**: one Linux machine, or the small Linux VM a Mac runs. Every box runs Nebula and k3s on the host.
- **Site**: one k3s cluster, owned by one project (usually one collaborator's). Its first box is the site's
  **manager** (the k3s server); more boxes join it as **nodes** (k3s agents). A Mac is a node whose work
  yields while its person uses it.
- **Public site**: a site whose manager has a public address. It runs the **Door** (public entrance),
  **Names** (the fabric's DNS zone) and a Nebula **lighthouse and relay**. The first site of a fabric is
  always public. The first public site also runs **NetBird** for people's devices.
- **Steward**: a site trusted with the fabric itself: it holds the fabric-level secrets (the Nebula CA
  key among them), renews certificates, runs a Console and can become the writer. The first site is a
  steward; the fabric's owner or an admin decides which other sites are. A site that is not a steward
  still runs apps and holds the secrets of the apps placed on it, nothing more.
- **The writer**: the steward whose copy of the Fabric accepts commits. Its Console is the one people
  use; it also does the few fabric-wide jobs (the steps of planned moves, the blocklist, people's mesh,
  domain checks, rebuild requests, retiring old vault keys). The first site is the writer until a person
  switches it: an admin in the Console, or root on another steward's manager with `install.sh takeover`.

```text
             people (phones, laptops)                         the internet
                     │ NetBird                                      │ 80/443, DNS 53
                     ▼                                              ▼
 ┌──────────────── first site: public, steward, writer ─────────────────────────────┐
 │ host: Nebula (lighthouse, relay)   NetBird client   k3s server                   │
 │ k3s:  Door + Names + entrance Warden   NetBird server   Forgejo (writer)   Flux  │
 │       Warden   Console   CloudNativePG + Barman plugin   apps                    │
 └──────────────────────────────┬───────────────────────────────────────────────────┘
                                │ Nebula (host to host, encrypted, NAT traversal via lighthouse/relay)
          ┌─────────────────────┴───────────────────────┐
 ┌──── site vince: steward ─────────┐       ┌──── site sam ──────────────────────────┐
 │ host: Nebula   k3s server        │       │ host: Nebula   k3s server              │
 │ k3s: Forgejo (copy)   Flux       │       │ k3s: Forgejo (copy)   Flux   Warden    │
 │      Warden   Console (standby)  │       │      CloudNativePG   apps (standbys)   │
 │      CloudNativePG   apps        │       │ node: a Mac (best effort)              │
 └──────────────────────────────────┘       └────────────────────────────────────────┘
                  │ WAL archive and base backups (object storage, Object Lock) │
                  └──────────────────────────► the vault ◄─────────────────────┘
```

## Projects, people and shared capacity

- **Project**: a group of people and their apps. At every site it is a Kubernetes namespace of the same
  name; on the people mesh it is a NetBird group. Every site is owned by one project, whose members manage
  its boxes and its offers.
- **Member**: a person's record in the Fabric: email, role, projects, SSH keys, and whether they are
  blocked. There are three roles. *owner* is the person who installed the fabric: an admin the Console
  never changes or removes. *admin* sees and changes everything and has SSH to every box. *member* changes
  only what belongs to their projects (apps, vaults, domains, and the boxes and offers of sites the
  projects own) and has SSH to the boxes of those sites. A blocked member keeps no access.
- **Offer**: capacity a site's project shares. It sets CPU, memory and storage for each project holding
  it, optionally only on named boxes. A project holds an offer when the offer names it, when it redeems
  one of the offer's keys, or through a pool (below). Warden then gives the project a ResourceQuota at
  that site, sized by the offers it holds there. A project may deploy only to sites it owns or holds an
  offer at.
- **Pool**: offers from many sites under one quota. Every project in the pool gets that quota (or each
  offer's own size, where the pool sets none) at each site offering into it. Admins make pools.
- **Best effort**: an offer of capacity that comes and goes (laptops, idle time). Holders' pods there run
  at the lowest priority and are evicted first. Databases never run there.

## The Fabric

The Fabric is a Git repository holding the whole desired state. Every site runs **Forgejo** with a
full copy. The writer's copy accepts the Console's commits and pushes every commit to every other
site's copy the moment it is made (Forgejo push mirrors), retrying every two minutes for a site that is
away. Every copy protects its `main` branch. Only the mirroring account may push to it, and never with
force. The Console and Warden may also commit, but only at the writer, and only once the writer's copy
holds the Fabric. So a copy that has gone its own way (an old writer back from the dead) is refused by
Git itself. **Flux** at every site applies its own local copy, so a site that loses every other site
keeps running what it has.

Every change to the Fabric, the Console's and Warden's alike, is a conditional edit (`fabric.Edit`): the
files are read from the writer's copy with their hashes, the change is decided from them, and the commit
is made only if none of the files it changes changed meanwhile; otherwise they are read and the change
decided again. The copy Flux applied to a site's cluster lags Git by up to a minute and is never the basis
of a change.

```text
crds/                        WeCoLab's CRDs
system/                      the platform, applied by Flux
  base/                      namespaces, priority classes, admission policies, the fabric's settings
  vendor/                    pinned upstream manifests: Flux, cert-manager, CloudNativePG, Barman Cloud plugin
  wecolab/                   Warden, Forgejo, the node agent
  steward/                   stewards only: the Console
  entrance/                  public sites only: Door, Names, the entrance Warden
  people/                    the site running NetBird only
secrets/                     fabric-level secrets, SOPS-encrypted to the stewards and the recovery key
fabric/                      the fabric's objects, applied at every site
  sites/  members/  offers/  pools/  domains/  projects/<project>.yaml  apps/<project>/<app>.yaml
projects/<project>/<app>/    an app's workload, applied only at the app's sites
keys/                        each site's age public key, and the recovery key's
.sops.yaml                   who secrets/ was encrypted to at install; unused afterwards, since the
                             Console or Warden names each file's recipients when it writes it
```

The fabric's settings (its DNS zone, its Nebula network, the writer and its epoch, which site runs
NetBird, the certificate lifetime, the blocklist of Nebula certificates) are one ConfigMap in
`system/base/fabric.yaml`, not a CRD. Changing the writer is a commit to it.

### What the Fabric holds

| Kind | Scope | In the Fabric | Key fields | Written by | Read by |
|---|---|---|---|---|---|
| App | the project's namespace | `fabric/apps/<project>/<app>.yaml` | `sites`, `primary`, `handover`, `deleted`, `rpo`, `hostname`, `mesh`, `workload`, `database`, `archive` | the Console (deploy, move, mesh name, delete); the writer's Warden (a handover's token and its end, archive generations, removing a deleted App) | Warden at every site (its own part, and removing the app from a site it left), the writer's Warden, the entrance Warden, the Console |
| Site | cluster | `fabric/sites/<site>.yaml` | `owner` (a project), `index`, `steward`, `public.address`, `boxes` (name, Nebula address, role, laptop, key, join certificates), `nextBox` | install (the first site); the Console (joins, stewards, removed boxes) | Warden at every site, the certificate service, the entrance Warden, the Console |
| Member | cluster | `fabric/members/<name>.yaml` | `email`, `name`, `role`, `projects`, `sshKeys`, `blocked` | install (the owner); the Console | Warden at every site (RBAC), the certificate service (SSH keys), the writer's Warden (NetBird users), the Console on every request |
| Offer | cluster | `fabric/offers/<offer>.yaml` | `site`, `boxes`, `cpu`, `memory`, `storage`, `to`, `pools`, `bestEffort`, `keys` (hashes) | the Console (the site owner's members; a redeemed key) | Warden at every site (quotas, placement), the Console (where a project may deploy) |
| Pool | cluster | `fabric/pools/<pool>.yaml` | `projects`, `quota` (cpu, memory, storage), `keys` (hashes) | the Console (admins; a redeemed key) | Warden at every site, the Console |
| Domain | cluster | `fabric/domains/<project>.<domain>.yaml` | `name`, `project` | the Console | the writer's Warden (checks its DNS records), the Console (which hostnames a project may use) |
| Namespace (a project) | cluster | `fabric/projects/<project>.yaml` | its name | install (the first project); the Console (admins) | Flux and Warden at every site |
| ConfigMap `fabric` (the settings) | `wecolab-system` | `system/base/fabric.yaml` | `zone`, `email`, `network`, `writer`, `epoch`, `people`, `certLife`, `blocklist` | install; a takeover (the Console or `warden takeover`); the blocklist (the Console when it removes a box, the writer's Warden) | Warden, the certificate service, the Door, the Console |

Status is never in Git: Warden writes it only into its own site's cluster (Apps' and Members' at every
site, Domains' at the writer).

### Kustomizations at every site

The join script creates these Flux Kustomizations, all reading the site's own Forgejo:

| Kustomization | Path | Notes |
|---|---|---|
| `crds` | `./crds` | never pruned |
| `flux`, `cert-manager`, `cnpg`, `barman` | `./system/vendor/<name>` | upstream manifests, vendored at a pinned version; never pruned |
| `system` | `./system/wecolab` | the platform: namespaces, policies, Warden, Forgejo, the laptop node agent, Kata Containers on KVM boxes |
| `fabric` | `./fabric` | pruned; namespaces and data are never pruned |

Flux's kustomize-controller runs locked down (`--no-cross-namespace-refs`, `--no-remote-bases`,
`--default-service-account=default`): a Kustomization that names no service account applies as nobody.
Every platform Kustomization names `kustomize-controller`; every app's names its project's own account.

Warden adds the rest from what the Fabric says about its site: at stewards `secrets` (decrypted with the
site's age key into Secrets in `wecolab-system`) and `steward` (the Console); `entrance` at public sites;
`people` (the NetBird server) at the steward the fabric's settings name for it; and one Kustomization per
app placed there (below).

## Warden

Warden is this repository's controller. The same binary runs at every site:

**Site mode**, at every site:

- **Apps placed here.** For each App whose `spec.sites` includes this site, Warden derives this site's
  role from the App's spec (see "An app's primary, from Git"), keeps what it must remember in the App's
  status here, and hands Flux one Kustomization: the app's folder, applied as the project's
  ServiceAccount, with the site's role as JSON patches. An app with a database runs only at its primary,
  and not while a planned move is in flight. Every Service becomes a NodePort with
  `externalTrafficPolicy: Local`, so it answers only on boxes running a ready pod. When an app is taken
  off this site, Warden deletes the app's database and volumes here, but only once the rule in "Data"
  allows it.
- **Projects.** For each project that may use this site (it owns the site, or holds an offer here directly
  or through a pool): the boundary (LimitRange, network policies) and a ResourceQuota sized by its offers.
  Projects with no rights here get a zero quota.
- **People.** Kubernetes RBAC bindings from Member records, each owned by its Member so it goes with it, so
  the Console's dry runs decide who may do what.
- **Nebula.** At stewards, the certificate service on the manager's Nebula address, port 8094. A box posts
  the certificate it holds to `/nebula/<box>`, only from its own Nebula address, and gets its CA bundle,
  lighthouses, firewall rules, the fabric's blocklist and its SSH keys, and a new certificate for the key
  registered in the Fabric only when one is due. The service keeps the few newest certificates it signed
  for each box, and the join certificates the Fabric records on its boxes, and publishes them at
  `/nebula/issued` for the writer's revocation loop.
- **Status.** Publishes what is true here at `http://<manager's Nebula address>:8093/status`: nodes and
  capacity, each database's CloudNativePG status, workloads, volumes, the role it applied for each app and
  the archive its database was built for, each app's endpoints (the Nebula address and port of every box
  running a ready pod), what each vault looks like, the id of the vault key each database app's Secret
  holds here, and the writer and epoch its own copy of the Fabric names. Other sites read a site's status
  only from its manager's Nebula address, refuse an answer that names another site, and use it only about
  that site and only as far as the Fabric allows.
- **The writer's part.** At every site, Warden keeps its copy's branch protection and push mirrors as the
  writer's election says (see "The writer").

**Entrance mode**, in the Door's pod at public sites: writes Traefik's routes and the fabric's DNS zone for
Names (see "The Door and Names"). It also answers Traefik's DNS challenge (lego's `httpreq` provider) by
adding the challenge's TXT record to that zone, so the wildcard certificate for mesh names needs no DNS
credentials anywhere.

**Writer duties**, at the writer only: makes the planned moves' steps and records a new database once its
primary has backed it up (see "An app's primary, from Git"); blocklists every certificate issued for a
box that has left the Fabric; keeps NetBird's users and groups in step with Members and renews NetBird's
service token two months before it expires; checks custom domains; commits a new archive generation when
a standby database reports it cannot recover (at most once per half hour per database, never for the
primary or a planned move's origin); and deletes a vault's retiring keys at B2 once every site of the
project's database apps reports the vault's current key.

**Node-agent mode**, on laptop nodes: keeps the `wecolab.io/idle` taints on the node while its person uses
it. It may change only that taint, and only on its own node.

## The writer

Each site follows the writer its own copy of the Fabric names, unless a steward publishes a claim with a
higher epoch (ties go to the lowest site name); a claim naming a site that is not a steward counts for
nothing. At the writer, Warden admits the Console to its copy (once the copy holds the Fabric) and keeps a
push mirror to every other site's copy. Everywhere else, the copy admits only the mirroring account and
has no push mirrors. A copy whose head is not on the writer's history (the writer's Warden answers
`GET /fabric/commits/<sha>` on its status port) starts over from the writer's: at a steward, its own
commits are first pushed to the writer as a branch `superseded-<site>-<commit>`. Nothing starts over from a
writer whose copy is empty.

## An app's primary, from Git

Every site of an app derives its database's CloudNativePG replica settings from the App's spec in Git
alone (`RoleAt`): `spec.primary`, and during a planned move `spec.handover {id, from, token}`. No site
asks another which site is primary.

**Steady state.** Every site takes `spec.primary` as the primary; the primary's database is writable and
every other site's replays its archive.

**Planned move.** A person picks a new primary, and the Console refuses unless the move can complete. The
target's standby must be staged (StandbyStaged: up, built for its current archive, with a ready
instance). For a new move, the primary must also be healthy (PrimaryHealthy) and writable, and it must be
archiving WAL with the target's replica healthy (WithinRPO). An app without a database has nothing to
hand over: its move only changes `primary`. For an app with one:

1. The commit sets `primary` to the target and `handover {id, from}` with the old primary.
2. The old primary demotes. Every site, the target included, keeps following the old primary and replays
   its archive, which ends at the token, so none promotes. The app is stopped.
3. The old primary publishes the demotion token its database minted for this handover, never one it
   already showed when it first saw the handover.
4. The writer copies the token into `handover.token`.
5. With the token in Git, every site takes the new primary, which promotes with it.
6. Once the new primary reports that token as the last one it promoted with, and is healthy, the writer
   clears the handover, and the app starts at the new primary.

Before step 4 the move may be retargeted to another standby, or cancelled by moving back to the old
primary. After it, a planned move elsewhere is refused, because another site promoting with the same token
would fork the history: move again when this one is done, or force. No data is lost.

**Forced move.** A person forces the move when the primary is gone. The commit sets `primary` with no
handover and gives every other site a new archive generation; the new primary promotes without a token,
and every other site's database is rebuilt from the vault as a standby (see "Data").

**Generations in Git.** Every move records its target's archive generation, and the writer records a new
database's once its primary has a base backup. A recorded generation is how Git shows the database was
made: from then on no site makes it again by initdb.

## Data

Each app's PostgreSQL is a CloudNativePG cluster at every site of the app, in CloudNativePG's
distributed topology: primary at one site, standby clusters elsewhere. They replicate only through
the **vault**: object storage (Backblaze B2, S3, R2, MinIO) under Object Lock, where each site's
database archives WAL and base backups to its own folder, `<db>-<site>` or `<db>-<site>-g<n>` after a
rebuild. Standbys replay the primary's archive. Base backups run only at the primary, which takes one as
soon as its vault holds none for its current folder, so a standby can bootstrap from it; while backups
fail it tries again hourly, since each attempt leaves Object-Locked files. The primary also takes a base
backup every night at 02:00; standbys never do, nor does any site while a handover is in flight. Every
database made from the vault keeps the app's own database name and owner (the app's workload name).

Nothing destroys a database without proof from Git and from the survivor. A site deletes its copy of an
app's database, to rebuild it under a new archive generation or because the app left the site, only when
Git says it is not the primary, not a planned move's origin, and no handover is waiting for a token, and
the primary reports itself the primary, writable, healthy, promoted with the handover's token if Git has
one, and with a base backup in its vault. Until then a database waiting to be rebuilt follows its role but
keeps writing its own archive. The primary can never be removed from an app's sites.

WeCoLab never replaces a lost database with an empty one. The site holding the app's history is the
primary, or, during a planned move, the old primary until its token is in Git. There, Warden lets Flux
create an empty database only for a brand-new app: Git records no archive generation or handover for it,
and Flux has never created one at that site. In every other case, a missing database suspends the app's
delivery at that site with a `DatabaseMissing` condition, until a person forces the primary to a site
that has the database.

Deleting an app is the one decision that removes data at every site, and it is explicit: the Console
marks the App `spec.deleted`, each site removes its part, and the writer removes the App from the Fabric
once no site reports anything left of it. An App merely missing from Git (a mistake, a rewritten copy)
deletes nothing: every site stops the app and keeps its databases and volumes for a person to delete.

Volumes (PersistentVolumeClaims) are site-local: each site of an app has its own. They are never
replicated and never in the vault. An app with a database runs only at its primary, so after a move its
database follows it but its files do not: the new primary uses its own site's volumes, and the old
primary's stay where they are. A catalog app's uploaded files are on such volumes. An app without a
database runs at every site it names at once, each with its own volumes; the Door sends people to the
primary's. An app that needs its data at two sites keeps it in its database.

## Networking

**Nebula** carries all traffic between boxes. Its network is `10.77.0.0/16`; a site with index *i*
takes `10.77.i.0/24` for its boxes: its manager is host 1, and every later box takes the site's `nextBox`,
so a removed box's address is never given to another. Certificates use Nebula's v2 format and carry groups:
`site-<name>`, `manager` or `node`, `steward`, `entrance`, `laptop`. Every box's Nebula firewall admits only
what the design needs:

| Port | From | To | For |
|---|---|---|---|
| 8093/tcp | managers | managers | Warden status |
| 8094/tcp | any box | stewards' managers | the certificate service |
| 30300/tcp | stewards | managers | Forgejo: the writer pushing to every copy |
| 30000-32767/tcp, but 30300 | entrances | every box | apps behind the Door, and the Console (30800 at stewards) |
| 6443/tcp | own site | managers | Kubernetes API |
| 10250/tcp; 8472/udp | own site | every box | kubelet, flannel VXLAN |

Sites never route pod traffic to each other; everything crossing sites is between Nebula host
addresses. So every site uses k3s's default pod and service ranges, k3s binds the box's Nebula address,
its NodePorts listen only on the Nebula network, and flannel's VXLAN runs over the Nebula interface. A
project's network policies admit traffic from its own pods and from the Nebula network, which is how the
Door's requests arrive.

On the host, WeCoLab's own iptables chains run before k3s starts, whether or not ufw is active.
`WECOLAB-HOST` (IPv4 and IPv6) drops the Kubernetes API, the kubelet, flannel's VXLAN and NetBird's
metrics from anywhere but loopback, Nebula and the pod interfaces, and on a public box limits each source
to 200 connections on 443. `WECOLAB-POD` keeps pods off the box's other services, its LAN and the meshes:
they reach the internet, their cluster, DNS (the box's own resolver too), the Kubernetes API and kubelet,
at a public box the Door's 80 and 443, and over Nebula only Warden's status, the certificate service and
Forgejo.

**NetBird** carries people. Its server runs at the first public site; people sign in with its built-in
identity provider, which is also the Console's sign-in. A person's device joins by signing in with the
NetBird app, so it belongs to that person; no setup keys are handed to people. The Door's box is the one
box on the people mesh, in its own group. People's devices reach only the Door's NetBird address, where
mesh-only app names are served; they never reach the boxes.

**The Door and Names.** Public app names resolve to the public sites' addresses (Names answers for the
fabric's zone, delegated once from the owner's domain). The Door terminates TLS (Let's Encrypt) and
forwards to the app's node port on the boxes where `spec.primary`'s site says a ready pod answers; an
endpoint counts only when its address is one of that site's boxes in the Fabric and its port is in
30000-32767, and no other site's word is taken. Mesh-only names, `<app>-<project>.mesh.<zone>`, resolve to
the Door's NetBird address and admit only NetBird addresses, under one wildcard certificate the fabric
obtains through its own DNS. `console.<zone>` goes to the writer's Console, as the writer's election has
it.

The entrance Warden writes Traefik's routes as typed configuration, never text assembled from names. Every
host name is checked before it is written; inside the zone an app may take only one label the fabric does
not use itself; a name two apps claim is served for neither; the fabric's own routers (the Console,
NetBird) outrank every app's. NetBird's `/api/setup` and `/api/instance` are never routed: the install
reaches them on the box itself. The DNS-challenge endpoint answers only on the box's loopback, only with
basic auth from random files written inside the Door's pod, and only for `_acme-challenge.` names in the
zone. Names binds the default route's interface by name, which works where a cloud NATs the public address
1:1. The Door runs as its own ServiceAccount, which may read Sites, Apps and the fabric's settings and
nothing else.

## Secrets

Every secret is generated where it is used, except the few a person brings from outside (the domain and
email at install, the owner's password, an object storage account key, an outside sign-in provider's
secret). [secrets.md](secrets.md) lists them all. In the Fabric:

- **Fabric-level secrets** (`secrets/`) are encrypted to every steward's age key and to the recovery key.
- **An app's secrets** (`projects/<p>/<a>/*.sops.yaml`) are encrypted to the app's sites, the stewards,
  and the recovery key.
- **Each site's age key** is born at the site and never leaves it; the public half is in `keys/`.
- **The recovery key**'s private half is written once at install to `/root/wecolab-recovery-card.txt`, for
  the owner to copy offline. With it and any copy of the Fabric, everything can be rebuilt; the tool that
  does it is not built yet.

The Console encrypts each file to those recipients as the Fabric's Sites stand when it writes it, and when
a steward is added or removed it re-encrypts every sealed file in the same commit.

## The Console

The Console is the web interface, served through the Door at `console.<zone>`. It runs at every steward;
the writer's is the one routed and the only one that accepts changes. Every change is decided from the
Fabric as Git holds it, tried first as a dry run as the signed-in person against its site's API, and
committed with `fabric.Edit` with that person as the author; then it asks its site's Flux to fetch the
commit at once. Only new objects and the secrets it stores are also written straight to the local API. It
reads live state from every site's published status.

Who someone is comes from their sign-in; what they may do comes from their Member, read again on every
API request, so a person blocked, removed or demoted loses it at once. The session cookies are
`__Host-` cookies, so no app under the zone can set them. The Console refuses a state-changing request a
browser marks as coming from another origin (a member's app at `<app>.<zone>` is same-site, not
same-origin) and may not be framed. The page renders every name through one escaping template. It reads
NetBird's service token from the `netbird` Secret each time it uses it, since the writer rotates it.

It also hosts the **join endpoint**: a box presents a one-time invite and its Nebula and age public keys,
and gets back its certificate, its site's settings and, for a new site, its place in the Fabric.

## Joining

1. The Console mints a one-time invite for a new site, a new Linux box or a Mac. The invite holds only the
   Console's address and a random code; its hash is kept at the writer for a day.
2. On the box, the join script makes the box's Nebula key and (for a site) its age key, and calls the join
   endpoint. The writer's Console names the box `<site>-<host>` (unique across the fabric, at most 63
   characters), gives it the site's next box number (or, for a new site, the next site index), signs its
   certificate, and records the box with that certificate's fingerprint (and for a site, the site and its
   age key) in the Fabric in one conditional commit. A Mac must join on a Mac's invite and a Linux box on a
   Linux box's. The invite is used up only when that commit succeeds; a refused join leaves it good.
3. The box gets back what it needs: its certificate and Nebula settings, its SSH keys, and its k3s token. A
   node gets the site's agent token only; a manager gets the server token and the agent token it gives its
   server. The box starts Nebula, then k3s over it. A new site also starts Forgejo with a copy of the Fabric
   (the writer pushes to it from then on) and Flux on it; everything else arrives from the Fabric.
4. From then on, every hour, the box asks a steward's certificate service for its settings, SSH keys and,
   when due, a new certificate. It swaps its files only once Nebula accepts the answer on a staging copy.

## What happens when something is lost

| Lost | What keeps working | What stops | Recovery |
|---|---|---|---|
| A site that is not the writer | every other site, every app whose primary is elsewhere | apps whose primary was there, until moved | force the primary elsewhere; the site rebuilds its databases from the vault when it returns |
| The writer | every site keeps running what it has | every change, moves included; a planned move in flight waits for it | switch the writer to another steward (docs/operations.md) |
| The only public site | apps keep running at their sites; Nebula tunnels that exist stay up | public names, new people signing in, new Nebula discovery | add a second public site before it matters (people's sign-in stays at the first) |
| A lighthouse | existing tunnels | finding hosts not already talked to | a second public site is a second lighthouse |
| A box's certificate expires | nothing on that box's tunnels | the box drops off the fabric | remove the box, run `install.sh uninstall` on it (its k3s data goes), and join again with a new invite; not possible for a site's manager yet |
| Every steward | apps keep running | all changes | a new box, the recovery card and any copy of the Fabric (not built yet) |

## Versions

Pinned in the install script and the Fabric's `system/`; checked quarterly. As of 2026-09-28:

| Component | Version | Notes |
|---|---|---|
| Nebula | 1.11.2 | v2 certificates only; `pki.disconnect_invalid: true` |
| k3s | v1.36.4+k3s1 (Kubernetes 1.36) | user namespaces are GA; needs Linux 6.3+ |
| Flux | 2.9.5 | source and kustomize controllers |
| Forgejo | 15.0.9 (LTS), rootless | |
| SOPS | 3.13.3 | the Console, Warden and install script shell out to it |
| cert-manager | 1.21.2 | required by the Barman Cloud plugin |
| CloudNativePG | 1.30.1 | fixes a lost demotion token during replica switchover |
| Barman Cloud plugin | 0.15.0 | always set `serverName` explicitly |
| Traefik | 3.7.13 | the Door |
| CoreDNS | 1.14.7 | Names; the zone file is written by the entrance Warden |
| NetBird server | 0.79.0 | unattended first run through `POST /api/setup` |
| Kata Containers | 4.2.0 | kata-deploy rendered without Helm's hooks (`hack/kata-render.sh`); Cloud Hypervisor only |
