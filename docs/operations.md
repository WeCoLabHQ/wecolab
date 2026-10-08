# Operations

What a person does, and what the fabric does in answer. Everything here starts in the Console unless it
says otherwise; every change it makes is a commit you can read in the Fabric's history. WeCoLab's own
words (site, box, steward, writer, handover, vault, Door) are explained in the [glossary](glossary.md).

Commands on a box use a **verified local release**, never a moving branch or an unverified
`/join.sh` response. Follow [install.md](install.md#2-install-the-first-site) to authenticate
the source commit, manifest and payload digests before root execution. Keep that directory:

```bash
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh" <argument>
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

**Forced move (independently fence the old primary first).** Loss of contact does not prove
that a primary has stopped writing. Physically power it off, or isolate **all** its write paths
and disable automatic restart, before ticking Force. The dialog fetches the current Git App
revision and history-bearing primary. Select the fencing method, enter operator evidence,
and acknowledge isolation. The API rejects a bare Force flag, stale revision, wrong archive
identity or wrong source; the commit records the authenticated actor, time and assertion.
That record is not physical fencing and does not make an unsafe partition safe.

The target promotes without a token. Writes it has not replayed may be lost; neither
`archive_timeout=60s` nor a recently uploaded object bounds the loss. Check measured recovery
evidence; unknown means no bounded claim. Every other site gets a new archive generation,
but rebuild waits for a healthy writable survivor with a validated DONE backup matching its
current database history. Force only to a site that already has the database; otherwise it
stays stopped with `DatabaseMissing`. Site-local files do not follow the move.

Do not simply restart the powered-off old database to rejoin: its persisted primary
can start before Git and role reconciliation catch up. Keep an independent database/write
fence across startup until the old Cluster is replaced by a verified standby. When the old
site is accessible before power-off, CNPG's
[`cnpg.io/fencedInstances=["*"]`](https://cloudnative-pg.io/docs/1.30/fencing/) annotation
stops its postmaster and persists across pod recreation. Set it with
`kubectl annotate ... --field-manager=flux-client-side-apply`: Flux removes annotations
written with the default kubectl manager. Reconcile the app's Flux Kustomization and
verify that both the annotation and stopped postmaster remain before power-off.
Do not clear that annotation on the old Cluster. Field-manager selection alone is not
continuous-fence proof: verify it again after role reconciliation and throughout rejoin.
Warden includes the fence in Flux's desired state and preserves the retained Cluster's
existing primary/source/promotion-token settings until guarded deletion. This matters:
CNPG 1.30.1's [replica-transition cleanup](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/reconciler/replicaclusterswitch/reconciler.go)
removes its all-instance fence. Changing the old incarnation into a replica before
deletion can therefore undo the operator's fence. The replacement must have a new UID,
remain in recovery, and replay the survivor's history. If the old
fence disappears, stop the old site. If no persistent fence was established, keep it
powered off until an operator can establish a safe rejoin boundary.
See [Flux's field ownership guidance](https://fluxcd.io/flux/faq/#why-are-kubectl-edits-rolled-back-by-flux).

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

**Boxes for workspaces.** install.sh labels a box that has `/dev/kvm` `wecolab.io/kvm=true`, when it joins
and at every converge (`sudo bash install.sh` on the box), so a box whose virtualization was turned on later
gets it from a converge. On amd64 the label brings the kata-deploy DaemonSet (`kube-system`), which installs
Kata Containers and restarts k3s on that box once; running pods keep running. Removing the label, or the box,
undoes the install there. `kubectl get nodes -L wecolab.io/kvm,katacontainers.io/kata-runtime` shows which
boxes can run a workspace.

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

**Rotate a vault key.** Storage → Rotate key commits a strictly increasing `key-version`
and `mutation-revision` with the new bucket-scoped key and the retiring key IDs. The writer
repairs every current Git database App Secret to that exact version, preserving its other
secrets and SOPS recipients. Interrupted repairs resume; stale repairs cannot overwrite a
newer rotation. Deployment, placement, deletion and repair change the same encrypted vault
file, so concurrent mutations conflict rather than escape the inventory.

Retirement checks every current App Secret and fresh direct reports from every intended
site for the exact key ID **and** version. Pending App deletion blocks a new retirement
claim. A durable `retirement-phase` then prevents affected mutations while the provider
deletion is in flight or uncertain. A timeout is not success: restore connectivity and let
the writer retry; never clear this claim by hand. Missing credentials, sites or versions
keep old keys valid. Hand-entered vaults still require provider-side manual rotation.

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
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh" uninstall
```

Uninstall deletes this box's k3s databases, volumes and Fabric copy: remove or evacuate the
box deliberately first. Its ownership ledger controls removal of Nebula, firewall chains,
SSH-key blocks, NetBird and their units. Shared packages are left installed.
Pre-existing AppArmor files are backed up and restored only if the installed file still
matches WeCoLab's recorded digest. An operator edit is preserved, reported as a conflict,
and original recovery material is retained under `/var/lib/wecolab-apparmor-recovery.*`.
Remove the box in the Console too, so its certificate is blocked.

Both k3s roles require the firewall unit and successful setup before startup. A failed
firewall is a boot failure, not permission to start unguarded. Nebula sync validates the
candidate before installation, propagates failed apply/reload, and leaves failed work
retryable. Interrupted k3s installs retain ownership and the installation mode until the
service is verified; rerun the same verified installer instead of removing the pending
state. Developer lab reload builds for each manager's actual architecture and never
records a failed reload as successful.

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
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh" takeover
```

This is the way for a planned change and when the writer's site is gone alike. Root on a steward's
manager is trusted with the whole fabric already. That site's Warden, from its own copy of the Fabric:

1. lets its own Console commit to its copy (its branch protection admits the Console's account);
2. commits the change of writer with an epoch above every one it knows (its copy's and every steward's
   claim), and starts pushing every commit to every other copy;
3. publishes the claim in its status at once, from its own copy; the Door routes `console.<zone>` to it.

The CLI, Console and Warden serialize local writer-role changes with the Kubernetes Lease
`wecolab-system/wecolab-writer-transition`. Warden rechecks its Git claim after acquiring it,
so a reconciliation started before takeover cannot subsequently close the new writer's copy.
The local Kubernetes API and lease permissions are required; there is no unlocked fallback.
This coordinates processes at one site, not a quorum between sites, and does not fence
database primaries.

It needs no Console: with the writer's site gone, `console.<zone>` may lead nowhere (the Door routes it to
the writer named in Git, and with one public site the Door itself may be gone). Settings has the same
button, **Take over as writer here**, but it shows only on a steward's own Console, and the Door routes
people only to the writer's.

Every other steward's Warden, seeing a stronger claim, closes its own Console's write access.
For a divergent copy it first drains Forgejo, then asks the winning Warden to retain the losing
main and **all earlier preservation branches** under its writer-transition Lease. New branch names
are `superseded-<site>-<full commit ID>`. A receipt covers exact refs and repository identity;
object existence or an ancestry response alone never authorizes deletion. Conflicting branch names,
changed source refs, missing credentials or an incomplete receipt retain the source copy.
A non-steward that still holds former-writer or unmanaged history requires operator custody.

Warden removes legacy scheduled mirrors. Before accepting custody, the receiver protects
`superseded-**` against remote mirroring accounts and observes a fresh Recreate rollout after
changing that protection. A repository-bound completion marker avoids repeated restarts.
Do not manually remove these protections or marker/journal annotations to bypass a refusal.
An uncertain DELETE leaves `wecolab.io/git-recreation` on the writer-transition Lease: custody,
takeover and ordinary repository creation stay blocked. Warden's sequential recovery drains
Forgejo, Ensures the repository and restores protection without retrying DELETE. An orphan-directory
failure remains blocked for operator recovery; do not erase either history.

The pure-Go transfer preflights an 8 MiB compressed and aggregate expanded-history budget,
including reconstructed delta targets, and at most 16,384 objects. A larger history is refused
with the source retained. Increasing a container memory limit alone does not change these limits.
Preserve/export the complete histories before addressing that capacity boundary; do not truncate,
force-overwrite or delete them to make convergence appear successful.

The exact-ref custody path passed deployed successive/overlapping transfers, a complete
fresh-fabric run and uninstall verification; see the
[convergence evidence](plans/2026-10-08-writer-convergence.md).
During any transition, verify all sites and the public Console report the expected writer
and epoch before issuing the next mutation. A successful takeover command only records
the claim; it does not mean every site or the public Console has converged.

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

**Backup evidence.** The vault inspector reads bounded Barman `backup.info` metadata.
Only DONE records with valid completion time, database system/timeline and WAL range,
under the exact current archive, count as completed backups. FAILED, incomplete, malformed,
unreadable or cross-history objects are not proof; an S3 modification timestamp is not a
backup completion time. This evidence permits neither an unbounded data-loss promise nor
the claim that a restore was tested. General point-in-time restoration remains an
operator procedure; the Console does not provide a restore button.

Automatic base-backup requests require a fresh, healthy, writable primary-history
measurement. A completed backup of an earlier system, timeline or archive does not
suppress the current-history request. Scheduled backups remain suspended until the
local database is actually writable and healthy in its desired archive, not merely
named primary in Git. Existing in-flight and hourly failure cooldowns still apply;
rebuild remains blocked until matching completed evidence is observed.

New databases receive immutable random `spec.archiveID` values. Archive folders beneath
`s3://<bucket>/<project>/<app>/` include that identity, site and generation. Recreating an
App name cannot discover its previous incarnation's retained backup as current evidence.
The gated migration preserves legacy `<database>-<site>[-g<n>]` names exactly; it does
not rename or copy retained objects. Never erase `archiveID` or revert to a binary that
infers identity from the App name.

**Measured recovery.** CNPG 1.30.1 exports the configured read-only recovery query through
its owned instance's port 9187 `/metrics`, using the exporter `pg_monitor` role. Warden
samples database system ID, timeline, role and WAL position; no tenant database password
is added. Same-history primary/replica positions plus bounded observation ages can prove
`within-objective`. An upper bound larger than the objective alone proves neither success
nor failure: `outside-objective` requires older uncovered WAL with a sufficient lower
bound. Stale, missing, regressed or incomparable samples are `unknown`.

Each site report carries a receiver-local, non-serialized monotonic receipt timestamp.
Cached reads do not renew it; missing or older-than-15-second receipts cannot authorize a
recovery claim. Wall clocks from different sites are not compared to infer replay lag.
Database prerequisites, local files, measured recovery and operational readiness remain
separate. Persistent volumes are site-local and have no general WeCoLab backup.

**Record a verified drill.** After an isolated runner has actually restored and read back
data, retain its sanitized result and run the verified binary:

```bash
warden record-restore --namespace <project> --app <app> --evidence /private/drill-result.json
```

The operator's kubeconfig authenticates the actor and App update permission. Writer Git
credentials must be supplied separately; the command checks the expected App blob SHA,
archive/system/timeline, matching current completed backup, passed checksum readbacks and
explicit `database` or `database-and-files` scope before a conditional commit. Failed or
stale evidence cannot update `spec.restoreVerification`. This is an authenticated operator
assertion about the recorded drill, not independent certification or proof of future
recoverability. With no matching record, the Console says **Never verified**.

**Vault network policy.** Production endpoints require HTTPS and public addresses. Each
new socket resolves once, rejects mixed public/private results and special-use addresses,
and dials a validated IP while retaining TLS hostname verification. Redirects and proxy
environment variables are not followed. Only the Fabric's explicit development setting
permits local HTTP fixtures; do not use it to bypass a production private-endpoint refusal.
S3 and native B2 exchanges and response reads have bounded deadlines; a stalled provider
is an error and a retry on a later writer tick, never evidence of success.

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

Upgrade only from an authenticated release and only after an actual disposable restore
has read back the existing databases. [Development: staged upgrade
preflight](development.md#candidate-releases-and-upgrade-preflight) defines the exact
receipt and commands. Stop all old Console/Warden writers and revoke their Forgejo write
grant before beginning: a new Git maintenance marker cannot fence an old binary.

The installer and Git manifests do not distribute upgrade images automatically. Before
the platform stage, prepare both architecture images and import the matching pair on
every box. Use the immutable version from the verified release's `VERSION`:

1. Use the verified release's four Linux binaries, not an unreviewed source build. Set
   `<tag>` below to its `VERSION` value (the source snapshot's short commit).
2. Make images with the verified `warden image`, once for each machine type (`amd64`,
   `arm64`), and the matching verified SOPS payload:

   ```bash
   a=amd64 v=<tag> sops=/var/lib/wecolab/bin/sops-v3.13.3.linux.$a
   warden image --name ghcr.io/wecolabhq/warden:$v --arch $a --out warden-$v-$a.tar "$HOME/wecolab-release/warden-$a"=/warden $sops=/usr/local/bin/sops
   warden image --name ghcr.io/wecolabhq/console:$v --arch $a --out console-$v-$a.tar "$HOME/wecolab-release/console-$a"=/console $sops=/usr/local/bin/sops
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
4. On the writer manager, follow the documented `begin → schemas → migrate → platform →
   complete` sequence with the verified new binary, operator kubeconfig and private
   Forgejo credentials. `begin` checks the exact writer-Git App/site inventory, offline
   snapshot/recovery hashes and per-database restore receipts; local primary metadata is
   also inspected directly. Wait for additive schemas at every site before migration.
5. Migration preserves legacy archive names, initializes credential versions, backfills
   route claims and placement revision, and adds CNPG recovery-exporter queries to
   existing database manifests. Colliding legacy route owners block migration; resolve
   them explicitly, never select a winner by iteration order.
6. Wait for every site's new schema-aware controller report before `complete` reopens
   protected mutations. Repeat the restore/readback and authorization checks on the
   upgraded disposable copy before accepting a production rollout.

Coordination records live under `coordination/`, outside Flux's recursively discovered
`fabric/` manifests. Claims are released only with final App removal after site cleanup;
placement and vault revisions are changed by the same conditional Git transaction as
the decision they protect. Reading a file's SHA without changing it is not a lock.

Do not downgrade after migration to controllers that ignore these contracts. Rollback
requires the offline pre-migration snapshot and matching binaries in a fenced isolated
recovery operation; never rewrite the live Fabric or retained object namespaces.

Reapply host setup with the verified local installer, not `curl | sudo bash`. On an
enrolled box it reapplies pinned host packages, certificate sync and firewall without
changing Fabric secrets. An existing k3s version is not automatically upgraded by this
operation; older binding/token settings remain guarded by the host firewall until a
deliberate evacuate/uninstall/rejoin.

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
