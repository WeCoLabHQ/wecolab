# Development

## The code

```text
api/v1alpha1/          the Fabric's objects: App, Site, Member, Offer, Pool, Domain
cmd/warden/            Warden: site, entrance (with entrance-init, which prepares the Door's pod), node-agent,
                       and the bootstrap, people, upgrade, takeover and image steps
cmd/console/           the Console; its web page is embedded
internal/warden/       Warden's controllers: app roles, moves and delivery, the writer, certificates,
                       projects, people, status, the Door's routes and the fabric's zone
internal/fabric/       the Fabric in Git: conditional edits (Forgejo), SOPS, where files live, app folders
internal/nebula/       the certificate authority: signing, and each box's Nebula configuration
internal/netbird/      the part of NetBird's API the Console and the writer use
internal/validate/     what a name may be: labels, host names, emails, the names the fabric keeps
internal/bootstrap/    the Fabric's first commit, the people step and upgrades: template/ (system/, crds/
                       generated from api/), keys, images
install.sh             every box: with no argument it creates a fabric (or converges a box that has one),
                       with an invite it joins one; also people, takeover and uninstall
cmd/console/join.sh    a copy of install.sh the Console serves and embeds (make join keeps it equal)
hack/dev/              the development fabric
hack/kata-render.sh    renders Kata's kata-deploy chart into system/wecolab/kata.yaml (needs helm)
mac/                   WeCoLab for Mac; it embeds install.sh
website/               the public-facing Astro site and searchable source documentation
```

## Build and test

```bash
cmp install.sh cmd/console/join.sh  # non-mutating embed gate: run BEFORE make test/dist/join
bash -n install.sh && bash -n cmd/console/join.sh && bash -n hack/release.sh
go vet ./...
go test -race -count=1 ./...
make dist                         # Linux amd64 and arm64 binaries, mutates join.sh if stale
python3 -m unittest discover -s hack -p 'test_catalog_import.py'
cd mac && swift test               # arm64 macOS 14+ (CI uses macOS 15)
make crds                          # regenerate api deep copies and bundled CRDs after API changes
```

The decisions that must never go wrong are pure functions with table tests: an app's role at a site and
its database patches (`RoleAt`, `DBPatch` in `internal/warden/role.go`), moves (`PlannedMove`,
`ForcedMove`), the gates a planned move needs (`MoveGates`), the writer's steps (`WriterStep`), the guard
before destroying a database (`MayDestroy`, `PlanAt`), the blocklist (`Revoke`), which certificate a box
gets (`internal/nebula`, `CertService.Bundle`), and the files an app's folder holds (`internal/fabric`).
`internal/warden/sim_test.go` plays random moves and database events (lost databases, sites away, sites on
an old copy of Git) through those functions and checks that no two sites may promote for one history, that
nothing is destroyed without proof, and that every move completes once all sites are up.

Behavioral contracts exercise HTTP handlers, conditional Git writes, shell functions and
data consumers. Installer firewall/SSH/k3s/sync functions run against disposable filesystem
and command boundaries (`cmd/console/join_test.go`); the Door is generated from hostile
names; Console handlers use a Forgejo fixture that refuses stale hashes. Browser flows
must also be exercised on the actual embedded page. Source-text/wording assertions are
not substitutes for executing these paths.

## The website

The Astro site lives in `website/`. It renders the root README, the guides in `docs/` and
`mac/README.md` directly from this checkout. Pagefind indexes the rendered documentation during the
production build; there is no second copy of the guides to maintain.

Use Node.js 22.12 or newer (the website CI uses Node.js 24):

```bash
cd website
npm ci
npm run build
npm run preview
```

The preview listens on port 4321 on all interfaces, so other devices on the same LAN can view it.
For local-only access, use `npx astro preview --host 127.0.0.1 --port 4321`. `npm run dev` runs the
local-only development server; use the production build and preview to exercise documentation search.

`BASE_PATH=/wecolab npm run build` builds for a project subpath. `WECOLAB_REPO_PATH` can point at a
different source checkout; it defaults to the parent of `website/`. Generated `dist/`, `.astro/` and
`node_modules/` directories are ignored.

The Website build workflow checks both root and project-subpath builds when the site or its source
documentation changes. On the public repository, a push to main also deploys it to GitHub Pages, at
https://wecolabhq.github.io/wecolab/; what reaches that repository still goes through the gate described
below.

## The catalog

`cmd/console/web/catalog.json` is the Console's catalog ([console.md](console.md#the-catalog)), embedded in
the binary. `hack/catalog-import.py` generates it from the pinned HomelabOS checkout,
then applies `hack/catalog-own.json` and `hack/catalog-certifications.json`.
Use Python 3.14 with Jinja2 3.1.6, PyYAML 6.0.3 and MarkupSafe 3.0.4:

```bash
python3 hack/catalog-import.py /path/to/HomelabOS > cmd/console/web/catalog.json
python3 -m unittest discover -s hack -p test_catalog_import.py
```

It renders each role's `service.yml` and compose template with stubbed Ansible variables and translates the
result mechanically: the main container, a database when the compose has Postgres, sidecars, a volume per
data path, and what is host incompatible (`unsupported`, refused by the Console) or attention. Each entry's
`validated` and `limitations` come from HomelabOS's `docs/development/service-validation-results.json`. The
file records `source` (the HomelabOS repository and branch, which the script names, and the commit it read),
`generated` (when) and `failed` (roles the import could not render, with the error). The current file is
from commit `411f2c6802a73aaf5517ed3b3ff01a083312d5c9` of `feat/service-batch`:
227 entries including Workspace, 183 import-deployable, 44 PostgreSQL entries, 201
main-volume entries and one failed upstream role (`zammad`). Database URI identity is
resolved before sidecar host localization; main/sidecar volume scope is retained.
There are zero WeCoLab-passed lifecycle certifications. Importer tests do not certify
upstream images, and modified image/architecture identities cannot inherit old evidence.

## A whole fabric on one laptop

`hack/dev` runs a fabric in Docker: three boxes as privileged Ubuntu 24.04 containers running systemd,
on a Docker network that stands in for the internet, plus S3-compatible object storage for the vault.
The box entrypoint marks its own mount namespace shared before systemd starts, including
after container restart, so the laptop node agent can use Kubernetes bidirectional mounts.

| Container | Plays |
|---|---|
| `<prefix>-pub` | the first site: public, steward, writer |
| `<prefix>-home` | a second site, steward, behind "NAT" |
| `<prefix>-mac` | a node of `home`, joined as a laptop |
| `<prefix>-vault` | isolated emulator; not proof of real-provider compliance |

```bash
export WECOLAB_DEV_PREFIX="a3-$(openssl rand -hex 6)"
export WECOLAB_DEV_OWNER="$(openssl rand -hex 16)"
export WECOLAB_DEV_SUBNET=198.19.44.0/24  # choose a /24 not used by any existing network
make dev-up      # privileged disposable containers only
make dev-test    # includes uninstall; FROM=4 hack/dev/test.sh resumes from step 4
make dev-down    # removes only this prefix's matching owner-labelled resources
hack/dev/fabric.sh reload       # rebuild Warden and the Console and restart them at every site
hack/dev/fabric.sh shell home   # a root shell on a box
hack/dev/fabric.sh console GET /api/state   # the Console's API as the owner, through the Door
```

Keep the same owner/prefix/subnet for the whole run. Missing ownership or a conflicting
resource is a hard refusal. Existing `wcl-*` resources are not disposable test targets.
No runner is authorized to prune Docker to recover disk or adopt another owner's lab.

The boxes run the same `install.sh` as real boxes. `WECOLAB_DEV=1` changes only what a
laptop cannot provide:

- the public address is the container's address on the stand-in internet;
- the zone's delegation check is skipped, and the fabric's DNS is queried directly;
- Let's Encrypt is replaced by the Door's own certificate;
- NetBird is not started, so the Console signs everyone in as the owner;
- Nebula certificates last an hour and boxes sync every five minutes, so renewal is exercised within a
  test run;
- the kubelet evicts only when less than 1 GiB of disk is left, since the boxes share the laptop's disk.

`WECOLAB_BIN=/src/dist/bin` is set alongside: the first box makes its images from this checkout's `make dist`
binaries instead of cloning WeCoLab and building it on the box. The other boxes download the images from its
Console, as real boxes do (decision 13).

Full acceptance requires at least 30 GiB free in Docker's backing store; allow headroom beyond the initial fabric's footprint.

`make dev-test` checks, in order:

1. every site and box is Ready and every site publishes its status over Nebula;
2. a database app deployed from the Console runs at `pub`, with a standby at `home`, and answers through
   the Door;
3. a planned switchover to `home` completes with the token handed over (while it is in flight `pub`
   replays its own archive and stays demoted at one token), the Door follows, and back again;
4. a forced move after independently fencing the old database and powering off its site
   rebuilds the old primary's database from the vault under a new generation;
5. deleting the app removes it and its data at both sites;
6. every box renews its certificate before it expires;
7. `home` takes over as writer, commits, and `pub` follows; `pub` takes it back with `install.sh takeover`;
   then removing the laptop's box puts its certificate on the blocklist at every site;
8. `install.sh uninstall` leaves every box as it was before WeCoLab (`hack/dev/snap.sh` records each box
   when it starts; the fabric is gone afterwards, so run `make dev-up` again).

A takeover response confirms the local claim, not convergence of every site or the
public Console. Before the next mutation, the suite waits for the expected writer
and epoch at both sites and through the public Console. While the old site is
deliberately powered off, it checks the survivor's private Console instead.
Late writes to a former writer are retained as superseded history, not merged into
the winning branch.

### Recovery scenario evidence

`hack/dev/recovery.sh --help` lists the A3 scenarios: `failed-backup`, `recreate`,
`partition`, `replay-lag`, `rotation`, `remote-storage`, `name-race`, `host-failure`,
`provider-contract`, `recipe-lifecycle` and `upgrade`. Run each against the separately
created owner-labelled fabric **before** the uninstall-containing `make dev-test`.
The JSON artifact records identity, component versions, operations and exact checksum
readbacks; nonzero/failed is never a certification. A successful mock or emulator call is
not a real provider retention result.

Restore readbacks pin CNPG's `recoveryTarget.backupID` to the backup in the artifact;
component versions come from the running Warden executable and restored PostgreSQL.
The failed-backup drill temporarily denies S3 `PutObject` only for the run's current
site archive at `dev/<app>/<archive>/base/*/*.tar*`. Barman must publish `FAILED`
metadata and CNPG must report a failed Backup. The policy leaves `backup.info`,
WAL and other apps' archives writable, and prevents queued backups from quietly
replacing the failed-only archive before destructive-rebuild refusal is observed.
The exact prior bucket policy is restored in `finally`, before a successful retry.
This avoids long throttled uploads exhausting the pinned Barman sidecar's memory.
Calls on `pub` use the public HTTPS Console. Calls inside the private `home` site use
its Nebula-bound Console NodePort, not a nonexistent loopback HTTPS listener.

The host-failure drill waits for the resumed box to appear in the Console's
Flux-backed registration view before requiring exactly one new box. It does not
replay enrollment when that view lags a successful installer exit. Use a separate
owner-labelled guest with installer-generated firewall units and a pre-existing,
loaded AppArmor profile; uninstall must restore both its bytes and load state.

Before powering off the old primary, force drills set CNPG's persistent
`cnpg.io/fencedInstances=["*"]` annotation on its exact old Cluster using
`--field-manager=flux-client-side-apply`. Default kubectl annotations are removed
by Flux reconciliation. The runner explicitly reconciles Flux, then verifies the
annotation and that `pg_ctl status` reports no running server. The fence must survive
pod/container startup; it is not cleared to rejoin. While the old Cluster remains, the
rejoin wait checks its annotation and postmaster; an observed missing fence powers the
old site off and fails the drill, even if a later replacement could have succeeded.
The replacement needs a new UID and must be an unfenced standby. The failed-backup
drill explicitly reconciles Flux three times after `RebuildWaiting`, checking the
stopped old postmaster each time without reapplying its annotation. A lost fence after
reconciliation fails immediately and powers off the old site; a later annotation
reapplication cannot conceal it. Warden preserves the retained incarnation's existing
replica settings so CNPG's demotion cleanup cannot remove the fence. Failed force
recovery does not automatically restart the old primary.
See [CNPG 1.30 fencing](https://cloudnative-pg.io/docs/1.30/fencing/).

Forced-promotion readbacks compare every original sentinel row and marker, separately
accounting for optional partition writes and required survivor writes. Replay-lag evidence
requires a fresh measured `within-objective` baseline, `outside-objective` while paused,
and a new `within-objective` observation after resume; exporter errors or `unknown`
cannot pass this transition. After promotion, it checks the rebuilt standby's timeline
and original rows, and records the automatically completed new-timeline backup before
requesting a manual backup on the promoted primary.

Safe local checks, requiring no Docker lab or credentials:

```bash
hack/dev/recovery.sh --help
python3 -m unittest discover -s hack/dev -p test_recovery_runner.py
env -u WECOLAB_DEV_PREFIX -u WECOLAB_DEV_OWNER hack/dev/recovery.sh recreate
# Last command MUST refuse with nonzero status and a failed evidence artifact.
```

Full acceptance uses the CI prerequisite of at least **30 GiB free in Docker's backing
store**, isolated privileged systemd containers and image pulls. `remote-storage`
additionally needs an authorized amd64/KVM destination; `host-failure` needs a separately
labelled disposable systemd guest.

The three nested k3s processes also share the Docker VM's inotify instance limit.
An isolated Colima guest with `fs.inotify.max_user_instances=128` exhausted it while
starting the laptop's containerd CRI plugin (`failed to create fsnotify watcher:
too many open files`), despite a 1,048,576 file-descriptor limit. Provision at least
1,024 inotify instances persistently in the explicitly disposable VM; do not silently
tune a shared Docker host. A one-off `sysctl -w` is lost on VM reboot: a later owned-lab
reboot restored 128 and prevented Traefik's file watcher from starting, leaving the
Console at HTTP 404 despite a valid route file. Restoring 1,024 and restarting the
Door restored service; box revocation and clean uninstall then passed. The launcher
waits for a read-only writer Console response before creating its first site,
because an available Door is not Console readiness.

`provider-contract` needs explicitly authorized bucket-scoped provider credentials and
a deliberately denied credential; `rotation` needs a disposable B2 account key. `upgrade`
requires offline snapshot/recovery material, a verified new binary, staged images, and
revocation of old writer credentials. Read `--help` for required environment names; never
supply these secrets to untrusted pull-request jobs or publish unsanitized output.

Additional proof boundaries:

- Provider retention needs a timezone-aware, future `RetainUntilDate` on the exact backup
  version. An expired/missing deadline fails before the delete probe. `AccessDenied` from
  the separate denied-delete key is credential-refusal evidence, not by itself proof of
  an active retention lock.
- Host recovery reads the steward's exact Kubernetes CA through the guest's `nebula1`
  before reload injection and after retry, before uninstall. An active Nebula process is
  insufficient. Workers use their own site's permitted Kubernetes port, not manager-only
  Warden status.
- Recipe acceptance also requires `WECOLAB_RECOVERY_RECIPE_PROBE`: an operator-reviewed
  shell script using that candidate's installed database driver and rendered connection
  settings. It runs through `sh -s -- <row-id> <marker>` inside every main/sidecar container.
  Commit that pair to `public.wecolab_recovery(n, marker)` and print only the complete
  `n|marker` rows ordered by `n`, without headers or credentials. The runner supplies a
  fresh challenge, independently reads PostgreSQL, and compares both byte streams.
  A no-op or fabricated stdout cannot satisfy the database write. The harness grants
  the non-superuser application role SELECT/INSERT on this fixture table. The script's
  SHA-256, actual container image identities and execution-node architecture are recorded;
  a missing script or unsupported container fails rather than certifies.
  The upgrade must advance the observed Deployment identity/generation even when images
  are unchanged, with all updated replicas ready. The runner exercises the old/upgraded/moved
  candidate, independently restores its data, then verifies source uninstall. Candidates
  with unsupported local-only files remain refused.
- Upgrade schema-report mismatch checks require the specific rejection reason; they are
  not old-binary downgrade attempts. After completion, a Git mutation using the previously
  working, now revoked old-writer credential must receive HTTP 401. This exercises the
  documented credential fence; it does not claim old binaries understand the new schema.
  The actual old-release/credential-revocation fixture remains required for full acceptance.

The `warden record-restore` consumer validates the completed artifact against fresh
primary evidence and the current conditional Git revision before recording its scope.
Run it only after reviewing real restore readbacks, while the lab is still running and
**before** `make dev-test` uninstalls it. The runner never records a restore automatically;
arbitrary timestamps are not proof.

## Trying a fix on a real lab

`hack/lab/reload.sh` rebuilds Warden or the Console from this checkout under the version a lab already
runs and loads it on every site's manager, without a release:

```bash
LAB_WRITER=root@203.0.113.7 LAB_SITES="me@192.0.2.10 me@192.0.2.11" hack/lab/reload.sh warden console
```

The images keep their tag for a developer-only lab reload; platform changes require an
operator-staged, reviewable `warden upgrade --stage begin|schemas|migrate|platform|complete`,
not the old unguarded `warden upgrade`. Do not use a same-tag reload to bypass a maintenance gate.

## Publishing

This repository is private (`origin`). The public repository, `github.com/wecolabhq/wecolab` (the remote
`public`), receives snapshots through a gate, `hack/publish.sh`: each publish is one commit holding main's
tree, with its own message and a public author, so the private history (its messages, authors and earlier
trees) never leaves.

```bash
git config wecolab.publicAuthor "Name <id+login@users.noreply.github.com>"   # once, kept out of the repository
hack/publish.sh "What changed"          # the gate alone
hack/publish.sh --push "What changed"   # the gate, then the push
```

The gate refuses unless main is committed and pushed to `origin`; gitleaks finds no secret in the tree,
the message or the author; none of the private terms (`~/.config/wecolab/private-terms`, one fixed string
per line: the lab's zone and addresses, hostnames, personal names and emails) appears in a file, a file
name, the message or the author; and no key file, `.env` file or file over 5 MB is present. The terms live
outside the repository because they are private themselves. GitHub's secret scanning and push protection
on the public repository are a second check, after the push.

`.gitleaks.toml` retains the default secret rules and classifies one exact non-secret match:
the credential-free Keycloak JDBC service address in `cmd/console/web/catalog.json`.
The exception requires both that file and that exact match; it does not exempt the file,
other values, commit history or publication metadata.

## Candidate releases and upgrade preflight

`.github/workflows/verification.yml` has independent Linux Go/race/shell/amd64+arm64,
Python 3.14 importer fixture (Jinja2 3.1.6, PyYAML 6.0.3, MarkupSafe 3.0.4), ARM macOS
Swift/NoCloud ISO and main-only disposable Docker fabric jobs. Importer upstream input is
`../homelabos` commit `411f2c6802a73aaf5517ed3b3ff01a083312d5c9`; CI uses
checked-in fixture tests, never a moving upstream branch. Website root/subpath checks stay
in `website.yml`. The trusted-main integration job uses an ephemeral GitHub-hosted
`ubuntu-24.04` runner by default, with privileged systemd containers, a local Docker
engine and Python 3. It still requires **30 GiB free in Docker's backing store**:
insufficient space is a failed prerequisite, never a pass or a reason to prune.
GitHub's [standard runner specification](https://docs.github.com/en/actions/reference/runners/github-hosted-runners#standard-github-hosted-runners-for-public-repositories)
documents 14 GB storage, so each job measures actual capacity rather than assuming
that every hosted runner is sufficient.

```bash
gh workflow run verification.yml --repo WeCoLabHQ/wecolab --ref main
```

`runner-capacity.json` records Docker filesystem space, CPU/RAM, cgroups, inotify
and KVM device visibility; device visibility alone does not certify KVM workloads.
Capacity and success/failure `result.json` artifacts are retained for 30 days,
including the capacity report when the storage gate refuses the run.

The [2026-10-09 complete hosted run](https://github.com/WeCoLabHQ/wecolab/actions/runs/37933951556)
verified public snapshot `e2acb4028a39e8d196c60c295ac94c9ae26a52a7` before the
default changed. All four jobs passed. Integration completed in 2h40m46s:

- `failed-backup`, `recreate`, `name-race`, `replay-lag` and `partition` each
  retained a passing result, including database checksum readbacks.
- The final fabric suite passed readiness, planned moves, forced recovery,
  deletion, certificate renewal, home takeover and project propagation to pub
  within the unchanged 180-second gate, takeover back to pub, box-certificate
  revocation, and uninstall matching all three boxes' pre-install inventories.
- The runner measured 4 CPUs, 15.61 GiB RAM, 83.75 GiB free Docker storage and
  1,280 inotify instances. No pruning was needed. `/dev/kvm` was present but not
  accessible to the runner; this run does not establish KVM workload support.

The workflow handback gate and recovery preflight require `site == "pub"` and
`writer == "pub"` as well as `isWriter`; a generic writer response is not proof
that routing has converged. An isolated regression caught the public Door still
serving home's superseded epoch after pub took over; the following deployment
reached home's Console and was refused by its fenced Forgejo.

The failed-backup drill compares product selection with the newest completed metadata
after the denied attempt, not a baseline frozen before an automatic backup can finish.
Expected and selected completion times, database protection state and `VaultFresh`
are retained before the assertion; unknown protection remains a failure. The initial
home standby must answer read-only before the drill can fence the old primary.
Replay catch-up uses one post-resume primary WAL position; later primary activity
cannot move that target on every probe. The drill still requires all 512 rows to
replay and a fresh measured `within-objective` recovery state before advancing.
Promotion readiness observes writable state and an advanced PostgreSQL timeline
together, inside the existing bounded wait. The old site cannot rejoin based only
on a writable probe taken before a CNPG promotion restart.

Replay-lag artifacts retain each rejected observation's state, reason,
covered-position timestamp and exposure bound; acceptance gates and waits remain
bounded rather than skipping an unavailable healthy-recovery baseline.

Site observation batches tenant workload and app resource reads, with at most four
independent API reads in flight so network latency does not accumulate serially.
Short peer requests share one bounded refresh instead of repeatedly canceling it; only successful
complete reports enter the five-second cache. Recovery samples age through publication,
including slow inventory reads, before they can supply recovery bounds or database
identity. Console file-scope inspection reads one bounded immutable Git archive,
including Git's global PAX metadata, and caches classifications only for that exact
verified commit. Missing, malformed or incomplete evidence cannot inherit an earlier
file-free classification. These paths have targeted live smoke coverage,
regression checks and the complete hosted run above.

Writer lease release re-reads ownership after a resource-version conflict: canceling
an in-flight renewal does not undo an API-server write. Conflict retries remain bounded,
and a successor's lease is never released. Lease and custody regressions use an API
fixture that enforces Kubernetes resource-version compare-and-swap.

The [2026-10-09 hosted run](https://github.com/WeCoLabHQ/wecolab/actions/runs/37912741547)
passed all five recovery scenarios, including writable timeline advancement before
the old site rejoined, and the Linux, catalog and ARM macOS jobs. The final fabric
suite passed planned moves, forced recovery, deletion and certificate renewal, then
failed when a project created after home's takeover did not appear at pub within
180 seconds. That failure now reports each site's Git head/claim, applied writer
and Flux revisions/conditions without dumping credentials. The deadline is unchanged.

A separate owned-lab trace caught the Door returning to pub after already routing
to home's higher epoch, when peer reports failed before Flux caught up. A running
Door now retains its highest observed valid writer claim; newer claims still win,
and a remembered writer must remain a steward. The regression and live smoke keep
home/10 selected while both peer status paths are unavailable and applied settings
still say pub/9. The subsequent complete hosted run above passed propagation;
the earlier failure's missing boundary evidence does not establish that this
route regression was its only cause.

The [first ordinary push after the runner cutover](https://github.com/WeCoLabHQ/wecolab/actions/runs/37954291333)
passed all recovery drills, project propagation and both writer takeovers, but timed
out waiting for the removed laptop's certificate in both stewards' blocklists. Dev
certificates last one hour, so the currently installed certificate can be a renewal
rather than the one recorded at join. Writer revocation now checks membership and
the blocklist in Git directly, without an early return based on an applied Site
that still contains the removed box. The inverse guard remains: a box still in Git
cannot be revoked just because an applied Site has already dropped it.

A deterministic regression failed before this change. In an owned live fabric,
Site application was held suspended after removing a box with a renewed certificate.
The old Warden failed the unchanged 180-second gate despite a steward publishing
that renewal. With the corrected Warden, both stewards applied its blocklist entry
63 seconds after rollout while their Site objects still contained the removed box.
Site reconciliation was then resumed. Failure diagnostics now include the expected
fingerprint, applied certificate inventories/blocklists, Git heads/claims and Flux
revisions/conditions; the acceptance gate and deadline are unchanged.

Fresh manager installs wait for their named Kubernetes Node to exist before waiting
for its Ready condition. A reachable API returning an empty Node list is not node
registration; both registration and Ready waits remain bounded.

The development vault is credential-free/emulated. Its presence alone proves neither an
application-data restore nor B2 compliance retention, production S3 dial policy,
identity-provider enforcement, physical fencing or native Virtualization.framework boot.
Schedule real-provider retention and real seed/VM/restore drills separately; never give
pull request jobs provider credentials. Production S3 guard rejects loopback redirect destinations;
bounded B2 responses permit a later writer tick after a stalled read.

After the private-to-public `hack/publish.sh` privacy gate produces a public snapshot,
an authorized manual public-main `release.yml` run invokes
`hack/release.sh build "<public 40-hex commit>" dist/release`: it produces a pinned
source snapshot tarball, matching installer/VERSION and four Linux binary artifacts
plus `release.json`, and attests the manifest
with GitHub Actions OIDC and uploads a **candidate only**. It does not sign with an external
credential, publish a GitHub Release, provision production, or claim an attestation for an
unpublished run. `hack/release.sh verify DIR COMMIT` checks manifest completeness, exact
artifact/platform digests and the GitHub signer workflow/main-ref/source commit before any
root execution. Intentionally swap an artifact, omit a platform or reject the signer in a
throwaway checkout to prove refusal. A public GitHub Release and native embedding/actual
multiarch installs require separate authorization and disposable-system observation.

For an existing fabric: use the **new verified release Warden binary**, invoked on the
writer manager with a kubeconfig and Forgejo admin/write token; the old container binary
cannot run this protocol. Snapshot the Fabric and recovery card offline, independently
inspect each app's **completed** Barman metadata and recent WAL in its *old* archive,
actually restore a copy into disposable storage and record a data-readback SHA-256.
Do not treat the old Warden's `LatestBackup` (which could be FAILED) as proof.
Stop **every old Console and old writer Warden** and revoke their Forgejo write permission
before begin; keep them fenced until new schema-aware controllers are deployed. A marker
in Git cannot stop a binary that does not know the marker. If you cannot establish this
fence or a real restore readback, do not begin. The operator-controlled Forgejo
credential authorizes the staged Git transaction; prepare a JSON receipt for **every
existing database app** (empty `apps` only if there are none), listing exactly the
Git site's inventory and current primary Barman archive namespace:

```json
{"snapshotSHA256":"<sha256 of offline Fabric snapshot>","recoverySHA256":"<sha256 of recovery card>",
 "writerFencedAt":"2026-10-06T12:00:00Z","writerFenceMethod":"all old writers stopped; their Forgejo write grant revoked",
 "sites":["home","public"],
 "apps":[{"name":"project/app","archive":"legacy-db-home","backupID":"<completed backup ID>",
          "systemID":"<PostgreSQL system ID>","backupCompletedAt":"2026-10-06T11:00:00Z",
          "walObservedAt":"2026-10-06T11:55:00Z","restoredAt":"2026-10-06T11:55:00Z",
          "restoreReadbackSHA256":"<64 lowercase hex from restored data readback>"}]}
```

Keep the snapshot, recovery card, receipt and environment tokens outside Git, under
operator-only permissions. A handwritten receipt is an explicit accountable operator
assertion, not independent evidence that a restoration actually succeeded. For apps
whose primary is the writer's local site, preflight additionally inspects the live
S3 archive directly using `VaultOf` and rejects missing/failed backup metadata or
receipt mismatches; remote primary archives need independent operator readback.

`warden upgrade --stage review` shows the durable gate. With
`WECOLAB_GIT_URL`, `WECOLAB_GIT_TOKEN` and `KUBECONFIG` set in a private
operator shell, run the **verified new** Warden on the writer manager:

```bash
WARDEN="$HOME/wecolab-release/warden-amd64"   # use warden-arm64 on arm64
"$WARDEN" upgrade --stage begin --snapshot /path/to/offline-fabric-copy \
  --recovery-material /path/to/offline-recovery-card --backup-proof /path/to/proof.json \
  --local-site home
"$WARDEN" upgrade --stage schemas
"$WARDEN" upgrade --stage migrate --age-key /path/to/writer.agekey
"$WARDEN" upgrade --stage platform
"$WARDEN" upgrade --stage complete
```

`begin` checks the operator fence assertion, recovery-material hashes, exact old
archive IDs, recent backup/WAL and restoration receipts against App/site inventory
at one immutable Git commit; duplicate or malformed manifests are refused. It also
inspects current vault metadata directly for local-primary apps, including exact
completion time and restore-after-backup chronology. With the `git` executable
available on the operator's manager, it commits the maintenance marker using a
Git smart-HTTP expected-old ref update; any intervening inventory commit refuses
the begin operation instead of admitting an unchecked app or site.
The trusted operator must check observed
backup/restore details rather than manufacturing plausible timestamps. Wait for additive
CRD establishment on every site before `migrate`; stage both architecture images before
`platform`; and wait for rollout everywhere before `complete`. A missing site report
blocks completion rather than silently treating mixed versions as compatible.
The restartable migration backfills old database names as ArchiveIDs, route claims,
credential versions, coordination revisions and existing CNPG recovery-query manifests.
Git-only records live under `coordination/`, outside Flux's `fabric/` discovery tree.
`complete` checks *fresh* site-reported Warden schema versions before admitting
protected mutations again. Verify post-upgrade authorization, unchanged old archive
names and a disposable restore/readback before accepting routine new deployments.
Old binaries cannot safely interpret new ArchiveIDs: after begin, **do not downgrade to an
old Warden/Console**; restore the pre-migration snapshot and matching old controllers only
under a fenced, isolated recovery operation if an actual rollback is required.

The on-call release owner should record CNPG, k3s, Nebula, NetBird, Forgejo, Kata, Flux,
cert-manager, Barman and certified-recipe advisory IDs with affected pinned versions;
appoint a named human owner per publication, test candidate updates on a disposable
fabric, notify operators of actionable upgrades and preserve observed outcome evidence.
No response SLA is implied without a staffed owner.

## Reviews and implementation plans

- [October 5 codebase review](plans/2026-10-05-codebase-review.md): findings, evidence levels,
  verification limits, and comparative research from September 5–October 5, 2026.
- [October 6 remediation plan](plans/2026-10-06-review-remediation.md): priorities, dependency order,
  a complete finding-to-task matrix, implementation status and exercised evidence for the
  security, recovery, host/macOS and Console/catalog changes; unrun external gates remain explicit.
- [September 29 hardening plan](plans/2026-09-29-hardening.md): earlier design context, preserved
  as a historical record rather than overwritten by the later review.

## Conventions

- Upstream tools do the work; our code decides and wires. Before adding code, check whether Kubernetes,
  Flux, CloudNativePG, Nebula or Forgejo already does it.
- Read the upstream documentation for the pinned version before implementing against a tool.
- Anything that holds state lives in the Fabric or at the site it belongs to, never only in a process.
- A failure is reported as a status someone can read, never only in a log.
