# Operations

What a person does, and what the fabric does in answer. Everything here starts in the Console unless it
says otherwise; every change it makes is a commit you can read in the Fabric's history. WeCoLab's own
words (site, box, steward, writer, handover, vault, Door) are explained in the [glossary](glossary.md).

Commands on a box use WeCoLab's install script. Only the first box keeps a copy, as of its install
(`/var/lib/wecolab/src/install.sh`), so run it from GitHub as root with its argument:

```bash
curl -fsSL https://raw.githubusercontent.com/wecolabhq/wecolab/main/install.sh | sudo bash -s <argument>
```

## Apps

An app changes primary in one of two ways: a **planned move**, when the primary is healthy, and a
**forced move**, when it is gone. The Console's move dialog calls them a planned switchover and a
failover, CloudNativePG's words. Both start from the same button, **Change primary**.

**Planned move.** Apps → the app → click the site to move to → Change primary. The Console commits the
move only when it can finish. The target's standby database must be up and ready (`StandbyStaged`). For
a new move, the current primary must also be writable, healthy and archiving WAL, and the target's
standby must keep up with it (`PrimaryHealthy`, `WithinRPO`). If a check fails, the Console names it.
The commit names the new primary and a handover from the old one. The old primary stops taking writes
and publishes its demotion token, which marks where its history ends; the writer copies the token into
the App; the new primary promotes with it; the writer clears the handover. The app is stopped from the
commit until the handover clears, typically under two minutes. No committed write to the database is
lost. The app's card in Apps shows the move in its `Promotion` condition: Demoting, then Promoting, then
Applied.

This covers the database. Files on the app's volumes (uploads, in most catalog apps) are not replicated
and stay at the old primary. The new primary uses its own volume, which holds only what was written there
the last time the app ran at that site. An app that needs its data at two sites keeps it in its database.

While the old primary's token is not yet in the App you may move again: to another standby (the move
retargets) or back to the old primary (the move is cancelled). Once the token is there, a planned move
elsewhere is refused: move again when this one is done, or force.

**Move an app without a database.** The same button. There is nothing to hand over, so no check applies:
the primary changes at once and the Door sends people to the new primary. Such an app runs at every one
of its sites, each on its own volumes, so people then see the new primary's files, not the old one's.

**Forced move (the old primary is gone).** The same, with Force ticked. The new primary promotes without
a token, so writes the old primary had not archived are lost: at most the archive timeout, one minute.
The same commit gives every other site a new archive generation. Each other site rebuilds its database
from the vault as a standby once the new primary is writable, healthy and has a base backup in its vault.
The old primary's site does the same when it comes back. Force only to a site that has the database:
WeCoLab never replaces a lost database with an empty one, so at a site without one the app stays stopped
with `DatabaseMissing`. As with a planned move, files on the app's volumes stay at the old primary.

**A primary lost its database.** When the site holding an app's history has no database (a lost disk, a
reinstalled site), its Warden does not make an empty one: Flux stops applying the app at that site
(Warden suspends its Kustomization) and the app's `Promotion` condition says `DatabaseMissing`. Force a
move to a site that has the database.

**Rebuild a site's database.** *The Console button is not built yet. The writer's automatic rebuild,
described at the end of this entry, is.* App → Rebuild at a site. Commits a new archive generation for
that site; once the primary proves it holds the data (writable, healthy, a base backup in its vault), the
site's Warden suspends the app's Kustomization there, so Flux stops applying it, removes the database and
lets Flux recreate it from the vault. Never at the primary or a planned move's origin. The writer does
this by itself when a standby reports it cannot recover, at most once per half hour per database.

**Delete an app.** App → Delete. The Console marks the App deleted in Git (`spec.deleted`); the Door stops
serving it and every site's Warden removes its part, databases and volumes included, reporting the app as
deleting until nothing of it is left. Once every site of the app answers and holds nothing of it, the writer
removes the App and its folder from the Fabric. A site that is down holds the App in Git until it is back
and has removed its part; a site that left the fabric is not waited for. The same name can be deployed again
once the App is gone. The vault keeps the database's archive as it was (while the app ran, backups and
WAL past its 30-day retention were pruned): nothing in WeCoLab deletes it now, and on a vault the Console
created, Object Lock keeps each file for 30 days after it was written. Volumes have no copy anywhere, so
their files are gone. An App that is merely missing from Git (a mistake, a rewritten copy) never deletes
data: only this mark does.

**Take an app off a site.** In **Deploy an app**, deploy the same app again with the same name, project
and settings, and untick that site (never its primary). The Console keeps the app's secrets. The site
deletes its copy of the database and the app's volumes once the primary proves it holds the data. The
files on that site's volumes have no copy elsewhere and are gone.

## Sites and boxes

**Add a site, add a box.** See install.md, steps 4 and 5.

**Make a site a steward.** Sites → the site → Make a steward. The same commit re-encrypts the fabric's
secrets and every app's secrets to the stewards, the site's key now among them, and its Flux starts
applying `secrets/`. Stop being a steward works the same way, except for the writer, and never for the last
steward. A fabric survives losing any one site only when at least two sites are stewards.

**Make a site public.** *Not built yet for an existing site; invite a new site as public instead.* A site
whose manager has a public address can be a second entrance: Sites → the site → Public, with its address.
Its Warden starts the Door, Names, a lighthouse and a relay there, and every box adds it to its
lighthouses at its next sync. Then add its name server at your DNS host. The zone numbers name servers by
address, not by join order, so check which one is `ns2` first (install.md, Optional).

**Remove a box.** Sites → the site → the box → Remove. The box leaves the Fabric, and the same commit puts
the certificates it joined with on the fabric's blocklist; the writer's revocation loop adds every renewal
a steward signed for it. Every box drops them at its next sync, within the hour, and no steward renews the
box again. Its address is never given to another box. Its site's Warden deletes its Kubernetes node once
the site's copy of the Fabric no longer lists the box; a node younger than ten minutes is left alone, so a
box joining right now is never mistaken for one that left. A site's manager cannot be removed this way: it
leaves with its site.

**Remove a site.** Sites → the site → Remove site (admins). It is refused for the writer, for a steward
(Stop being a steward first: that commit re-encrypts the fabric's secrets without it), for the site that
runs NetBird, and while the site is any app's primary or part of a move in progress (move those apps
first). Otherwise one commit:

- removes the site, its age key (`keys/<site>.age.pub`) and its secret (its k3s tokens and mirror
  password), so nothing encrypted afterwards is readable there;
- takes the site off every app it held a standby for, and drops the offers it made;
- puts the certificates of all its boxes on the blocklist.

From there the writer stops pushing the Fabric to it, the Door and Names stop naming it, and every box
drops its certificates at its next sync, within the hour: the site is off the mesh and receives nothing
more. Then the Console rotates the vault key of every project whose database apps had a standby there
(Rotate a vault key, below), since each of those apps' Secrets carried a copy of it. The site that left is
no longer one of the apps' sites, so the old key is not kept for it: it is deleted at B2 as soon as the
sites that remain report the new one. What the site already holds stays there: its copy of the Fabric up
to that commit, its databases and volumes, and anything it could decrypt. Nothing in WeCoLab can reach
into a site that has left.

**Rotate a vault key.** Storage → the vault → Rotate key. The Console makes a new key restricted to the
bucket with the account key in Settings and writes it into the project's vault, marking the old key
`retiring` in the same commit, then into every database app's Secret. A database app deployed meanwhile
is refused, to be deployed again with the new key. The old key stays valid at B2 until every site of every
database app of the project reports the new one in its status; then the writer's Warden deletes it at B2
and takes it out of the vault. A site that does not answer is waited for, so no site still archiving with
the old key is cut off; Storage shows which sites are awaited, and the writer's Warden logs them. Rotating
again before then retires both old keys. Without the account key in Settings, or with one of an account
that does not hold the bucket, the old keys stay valid and the writer's Warden logs that it could not
delete them. Vaults entered by hand are rotated by hand, at the storage provider.

**When a collaborator leaves.** Everything above, in order, as the fabric's admin:

1. Apps whose primary is at their site: move each to one of yours (Apps). With the site unreachable,
   Force it; the old primary's copy is not waited for.
2. If their site is a steward, Stop being a steward. The fabric's Nebula CA, every site's k3s tokens,
   NetBird's service token and the storage account key were readable there. *Not built yet: rotating
   the CA and the tokens.* Rotate the storage account key at Backblaze now, and enter the new one in
   Settings. A fabric whose steward left on bad terms is safest rebuilt.
3. Remove their site (above). Their boxes are off the mesh within the hour.
4. Remove each of their people (Members → Remove): Console access, bindings at every site, SSH keys
   and their devices on the people mesh go.
5. Their projects' apps on your sites are yours to keep or Delete (Apps); withdraw any offers you made
   them (Sites → Offers).

**Remove WeCoLab from a box.** Run the install script with `uninstall` on the box:

```bash
curl -fsSL https://raw.githubusercontent.com/wecolabhq/wecolab/main/install.sh | sudo bash -s uninstall
```

It removes what WeCoLab added and nothing else: k3s with everything it ran (this box's databases, volumes
and copy of the Fabric), Nebula, the certificate sync, its iptables chains (`WECOLAB-HOST`,
`WECOLAB-POD`), the ufw rules it added, its marked block of SSH keys, and NetBird on the Door's box. Every
step is best-effort, so one that finds nothing to remove does not stop the others. The install records
each of these as it adds it (`/var/lib/wecolab/installed`), and refuses a box that already runs its own
k3s or Nebula, so a box's other services, containers and settings are never touched. Packages it had to
install (such as `jq`) are listed and left, since other software may use them by then. Remove the box in
the Console as well, so its certificate is blocked.

**A box was off too long.** A box whose certificate expired cannot rejoin by itself. Remove it in the
Console and wait for its site's Warden to delete its node (Remove a box, above; k3s refuses a box that
rejoins under the name of a node it still has), run the install script with `uninstall` on it (this
deletes what k3s held on that box: its volumes and databases), then join it again with a new invite. A
site's manager cannot be re-added this way.

## The writer

The writer is the steward whose Fabric copy takes commits. Normally it never changes.

**Take over as writer.** As root on the manager of the steward that should become the writer, run the
install script with `takeover`:

```bash
curl -fsSL https://raw.githubusercontent.com/wecolabhq/wecolab/main/install.sh | sudo bash -s takeover
```

This is the way for a planned change and when the writer's site is gone alike. Root on a steward's
manager is trusted with the whole fabric already. That site's Warden, from its own copy of the Fabric:

1. lets its own Console commit to its copy (its branch protection admits the Console's account);
2. commits the change of writer with an epoch above every one it knows (its copy's and every steward's
   claim), and starts pushing every commit to every other copy;
3. publishes the claim in its status at once, from its own copy; the Door routes `console.<zone>` to it.

It needs no Console: with the writer's site gone, `console.<zone>` may lead nowhere (the Door routes it to
the writer named in Git, and with one public site the Door itself may be gone). Settings has the same
button, **Take over as writer here**, but it shows only on a steward's own Console, and the Door routes
people only to the writer's.

Every other steward's Warden, seeing a steward claim a higher epoch, stops its own Console from committing
and stops pushing; a claim by a site that is not a steward counts for nothing. If the old writer comes back
after committing things nobody else received, its copy has gone its own way and the new writer's pushes are
refused; its Warden then pushes those commits to the new writer as a branch named
`superseded-<site>-<commit>` for a person to read, and takes the new writer's history. A site that is not a
steward simply takes the writer's history.

Take over only when the old writer is really gone or you are moving it on purpose. Two people taking over
at once is settled by the higher epoch, then by the lower site name.

## Certificates

Boxes renew their own Nebula certificates: every hour each box posts the certificate it holds to a
steward's certificate service (port 8094), which answers only a box asking from its own Nebula address. It
signs the box's registered key again when a third of the certificate's life is left, or at once when the
certificate is blocklisted, was issued by an older CA, or no longer says what the Fabric does; otherwise it
hands back the newest certificate it already signed for the box. The same answer carries the CA bundle, the
Nebula settings, the blocklist and the box's SSH keys; the box swaps its files only once `nebula -test`
accepts them, and keeps the old ones as `.bak`. Certificates last thirty days.

**SSH keys.** A member's SSH keys (Console → Add a site → Your SSH keys) reach every box of the sites
their projects own, and admins' keys reach every box, in a marked block of root's `authorized_keys` that
each sync rewrites. Lines outside the block are the box's own. Removing or blocking a person removes their
key within the hour.

**Rotate the Nebula CA.** *Not built yet; the CA lasts five years.* Settings → Rotate the Nebula CA. The
new CA joins every box's bundle at the next sync; boxes are re-signed by it at their next renewal; the
old CA leaves the bundle thirty days later.

## Backups and restore

**The vault** holds every database's WAL and base backups under Object Lock. The Storage page shows each
database's newest backup and WAL, as the app's primary sees them. Volumes are never in the vault.

**Restore a database from the vault by hand.** *Not built yet.* Nothing in the Console or Warden restores
an app's database to a point in time, after a mistake or after the app is deleted. An app's archive is at
`s3://<bucket>/<project>/<app>/` in its project's vault, and stays there after the app is deleted.

**NetBird's data** (people, devices, its identity provider). *Not built yet.* It is to be backed up
nightly by the site running it, encrypted to the stewards and the recovery key, to the fabric's own
bucket.

**NetBird's service token** (the Console's and Warden's access to the people mesh) is renewed by the writer
two months before it expires: the new token is committed to `secrets/netbird.sops.yaml`, and the old one is
deleted a day after the Fabric names the new one.

**Rebuild from the recovery card.** *Not built yet.* When every steward is gone: on a new public box, run
the installer with `--restore`, give it the recovery card and a copy of the Fabric (any site's Forgejo,
or a clone), and it brings the fabric back with the same zone. Point the zone's delegation at the new box
if its address changed. Sites that still run keep their apps and rejoin through the new first box.

## DNS

**Secondary DNS.** *Not built yet (zone transfers).* A free secondary keeps the fabric's zone answering
when every public site is down. At Hurricane Electric (dns.he.net) add the zone as a secondary of `ns1`;
Settings → DNS → Allow transfer to the secondary's transfer address. Then add the secondary's name
servers as `NS` records at your DNS host.

## Upgrades

Versions live in the Fabric (`system/` and `crds/`) and in the install script. An upgrade is a commit, and
Flux rolls it out at every site. `warden upgrade`, from the version being installed, makes that commit: it
renders that version's `system/` and `crds/` (the vendored upstream manifests included) and commits the
difference to the writer's copy as one conditional commit, removing files the version no longer has and
never touching the fabric's settings.

*Not built yet: getting a version's images to every box.* `warden upgrade` moves no images. Until a
registry holds them (decision 13), every box needs the new Warden and Console images before the commit; a
box without them cannot start the new pods. For version `<tag>`:

1. Build the version's binaries in a WeCoLab checkout at that tag: `make dist VERSION=<tag>`. They land in
   `dist/bin/` (`warden-amd64`, `console-arm64`, ...).
2. Make the images with `warden image`, once for each machine type the fabric has (`amd64`, `arm64`).
   They carry SOPS, which the first box keeps in `/var/lib/wecolab/bin/` (the version install.sh pins):

   ```bash
   a=amd64 v=<tag> sops=/var/lib/wecolab/bin/sops-v3.13.3.linux.$a
   warden image --name ghcr.io/wecolabhq/warden:$v --arch $a --out warden-$v-$a.tar dist/bin/warden-$a=/warden $sops=/usr/local/bin/sops
   warden image --name ghcr.io/wecolabhq/console:$v --arch $a --out console-$v-$a.tar dist/bin/console-$a=/console $sops=/usr/local/bin/sops
   ```

3. On every box, copy the two tarballs for its machine type to `/var/lib/rancher/k3s/agent/images/`,
   import them, and pin them so k3s keeps them:

   ```bash
   k3s ctr images import /var/lib/rancher/k3s/agent/images/warden-<tag>-amd64.tar
   k3s ctr images label ghcr.io/wecolabhq/warden:<tag> io.cri-containerd.pinned=pinned
   ```

   and the same for the Console's. On each steward's manager, also copy all four tarballs to
   `/var/lib/wecolab/dist/`: its Console hands them to boxes that join later, and a join without them
   stops.
4. On the writer's manager, with the new version's `warden` (`dist/bin/warden-<arch>`), make the commit:

   ```bash
   export WECOLAB_GIT_URL=http://$(kubectl -n wecolab-system get svc forgejo -o jsonpath='{.spec.clusterIP}'):3000
   export WECOLAB_GIT_TOKEN=$(kubectl -n wecolab-system get secret git -o jsonpath='{.data.token}' | base64 -d)
   warden upgrade --version <tag>
   ```

**Images named `ghcr.io/wecolab/…`.** Fabrics made before 2026-09-30 name their images under
`ghcr.io/wecolab`, a GitHub namespace WeCoLab does not own: a box missing an image would pull it from
there. Moving to `ghcr.io/wecolabhq` is an upgrade like any other. Import and pin the images under the new
names on every box first (steps 1 to 3, Macs included), then `warden upgrade`. Pods keep their old
images until the commit, so the order is safe.

A fabric made before Flux was locked down keeps its Flux as it was, and the command says so: run
`warden upgrade --lock-flux` once every site runs the new Warden, which names the platform's service
account on the Kustomizations install.sh made.

Nebula and the box's own setup are upgraded by running the install script with no argument on the box
(`curl ... | sudo bash`). On a box that belongs to a fabric it only re-applies its packages, the pinned
Nebula, the certificate sync and the firewall, and touches none of the fabric's keys, tokens or
certificates. *Not built yet: upgrading k3s.* A box whose k3s predates `bind-address` and the agent token
runs without them, its ports guarded by `WECOLAB-HOST`, until it leaves (the script's `uninstall`) and
joins again; the re-run says so.

## Looking closer

| To see | Where |
|---|---|
| a site's view of everything | `curl http://<manager Nebula address>:8093/status` from any site's manager |
| what Flux applied | `kubectl -n flux-system get kustomizations` at the site |
| the Fabric's history | the writer's Forgejo, `http://<writer Nebula address>:30300/fabric/fabric` from a steward's manager |
| Nebula | `journalctl -u nebula` on the box; `nebula-cert print -path /etc/nebula/host.crt` |
| what the Console did or refused | `kubectl -n wecolab-system logs deploy/wecolab-console` at the writer's site (below) |
| what the Door refused or failed | `kubectl -n wecolab-system logs deploy/door -c door` at a public site (below) |
| what Warden decided | `kubectl -n wecolab-system logs deploy/wecolab-warden` at the site |

### Logs

Every log line is JSON on the container's standard output; Kubernetes keeps the last 50Mi per container
and loses them when the pod is replaced (an upgrade, a restart). There is no central log store.

- **The Console** logs one line for each request that changes something or is refused: its id, the
  method and path, the status, who (the signed-in person's email), their address, and for a refusal the
  reason they saw. Sign-ins say `event: authn_login_success` or `authn_login_fail`; refusals say
  `authz_fail` (403) or `input_validation_fail` (400), from OWASP's logging vocabulary. Reads that
  succeed are not logged. Query strings, headers, cookies and bodies never are.
- **The request id.** The Console answers every request with `X-Request-Id`, the page shows it with any
  error as "(ref …)", and a commit the request made carries it as a `Request-Id:` trailer. To follow up
  on an error someone saw: `kubectl … logs deploy/wecolab-console | grep <ref>`, and in the Fabric's
  history, `git log --grep 'Request-Id: <ref>'`. "No answer from the Console" means the page never got a
  reply: the change may still have been made, so refresh before trying again.
- **The Door** logs requests answered 400 or above, as JSON, with the Console's request id when there
  is one. It keeps no query string and no header but that id. Apps' routes log nothing: an app's
  visitors are its project's business, not the business of whoever runs the public site. What remains
  are the fabric's own names (the Console, NetBird) and requests that match no route.
- **Warden** logs at info level, errors with a stack trace, times in RFC 3339.
