# Writer convergence: research, confidence gate and implementation plan

## Decision — 2026-10-08

Implement the pure-Go exact-ref transport **together with** receiver-serialized custody,
not a branch-filter adjustment or an unverified Forgejo upgrade. The implementation,
independent review, complete Go race/vet suite, deployed successive/overlapping custody,
systemd recovery, full `FROM=1` fabric run and uninstall acceptance have passed.
The convergence release gate is closed; the five separate fixture blockers below remain.

Warden's image contains static binaries, not a Git executable, and runs with a read-only
root filesystem. The transport preflights pack inflation within fixed memory budgets rather
than relying on the host's Git executable or unbounded in-memory history.

User authorization: research documentation, repositories and issues; plan before
implementation and proceed only with confidence; checkpoint existing remediation
without publishing; provision explicitly disposable acceptance fixtures where inputs
and capacity permit. The implementation checkpoint did not authorize publication.
The user subsequently authorized a private push followed by gated public source publication
after security checks. No release creation was requested.

## Checkpoint before transport work

At this checkpoint, the commits were local on `fix/review-remediation`, with no push,
release or public snapshot:

- `4c014f1`: host installation, reload and Mac mode transitions.
- `a7e1e87`: coordinated control-plane, schema, upgrade and recipe contracts.
- `fd885ed`: owner-scoped acceptance and release-provenance tooling.
- `e1007b5`: documentation, evidence register and remaining release gates.

Fresh verification before checkpointing: `go vet ./...`,
`go test -race -count=1 ./...`, 18 recovery-runner tests, 8 catalog-import tests,
20 native ARM Mac tests, installer-copy parity, shell syntax, and website build.
The system Python lacked Jinja2; the eight importer tests passed using the existing
`/tmp/wecolab-catalog-venv`. This is not a full-fabric acceptance pass.

## Primary sources and local contracts

1. [Forgejo v15 mirror documentation](https://forgejo.org/docs/v15.0/user/repo-mirror/)
   says filtering restricts branches and `--mirror` applies when no filter is set.
2. [Pinned v15.0.9 service](https://codeberg.org/forgejo/forgejo/raw/tag/v15.0.9/services/mirror/mirror_push.go)
   configures filter refspecs but invokes Git with `Force: true, Mirror: true`.
   [Its Git wrapper](https://codeberg.org/forgejo/forgejo/raw/tag/v15.0.9/modules/git/repo.go)
   turns those options into `-f --mirror`. This is the implementation, not the
   documented filter-only behavior.
3. [Branch-filter request #7242](https://codeberg.org/forgejo/forgejo/issues/7242)
   and [implementation #7823](https://codeberg.org/forgejo/forgejo/pulls/7823)
   establish the feature's intended scope, not a correction of the observed failure.
4. [Source rollback #14273](https://codeberg.org/forgejo/forgejo/issues/14273),
   [fix #14324](https://codeberg.org/forgejo/forgejo/pulls/14324), and
   [v15 backport #14332](https://codeberg.org/forgejo/forgejo/pulls/14332)
   concern leftover **fetch** refspecs rolling back source refs after a push.
   The pinned source already contains that cleanup and still sets `Mirror: true`.
   It is not evidence that a release upgrade fixes protected-main deletion.
5. [Git push documentation](https://git-scm.com/docs/git-push) distinguishes
   exact source:destination refspecs from mirroring, deletion and force updates.
6. [go-git v5.19.3 push implementation](https://github.com/go-git/go-git/blob/v5.19.3/remote.go)
   and [options](https://github.com/go-git/go-git/blob/v5.19.3/options.go)
   were inspected, including create-only lease behavior. The prototype used v5.19.3;
   the implementation now pins that same version.
7. Investigated paths included `internal/warden/writer.go` and the old `HasCommit`/
   `Recreate` guards, `writer_lock.go`, `internal/bootstrap/image.go`, and the Warden/
   Forgejo deployment templates. The replacement lives in `writer_custody.go`,
   `writer_quiesce.go`, `writer_journal.go`, and `internal/fabric/ref_transport.go`/
   `ref_budget.go`. No Go language server was available.

## New runtime evidence

Artifact: `/tmp/wecolab-convergence.VfH5FpVn/result.json`.
Owner: `research-abe1b496586c40df831d84848600c20b`.

The disposable Forgejo used the exact production pin:
`codeberg.org/forgejo/forgejo:15.0.9-rootless@sha256:caf1bca332f95cdcf124227a4bfa3b49bbbfbbc8a5e4a97406921cb581165413`.
Two private repositories shared an ancestor and then diverged. `main` remained
protected against force pushes, with a separate non-admin mirroring collaborator.

An in-memory go-git prototype fetched the losing main and pushed only its exact
preservation branch using the collaborator's credentials. Observed:

- Winning `main` and an unrelated branch remained byte-for-byte at their original refs.
- The losing commit became reachable through the named branch in an independent Git clone.
- Repeating the transfer was idempotent.
- A conflicting preservation branch was refused even when its old commit was an
  ancestor, so an ordinary fast-forward could otherwise have overwritten that name.
- A divergent push to `main` and an invalid credential were refused.
- The transfer also ran in a non-root, read-only **scratch** container with a 256 MiB
  limit, all capabilities dropped and no Git executable, shell or writable worktree.
  This small fixture is not a representative full-Fabric memory measurement.

### Object existence is not durable custody

A separate commit was created on a temporary branch at the destination. After deleting
that branch, Forgejo's `/git/commits/<sha>` still returned the commit. A fresh mirror
clone did **not** contain it. This directly contradicts treating `HasCommit` as proof
that a commit is retained by a branch.

Before this change, `Writer.follow` permitted recreation after `CopyAnswer.Has` became
true; `ServeHTTP` obtained that value from the object lookup above. The earlier A3 run
retained its old copy, but that observation is not a general guarantee of this guard.
A failed/partial transfer may leave objects without the required preservation ref.

### Protocol risks identified during research

- A later writer transition can recreate the repository holding earlier preservation
  branches. Transferring only the current main head does not preserve that full ref set.
- A preservation branch can arrive after a receiver snapshots its refs but before it
  recreates its repository. An object lookup or a successful push alone does not
  serialize that race with writer transition.
- Old scheduled mirror jobs can still act during a mixed-version cutover. Merely
  deleting their configuration is not proof that all in-flight operations stopped.

These became required design/test cases; the original prototype did not reproduce
multi-site history loss.

## Alternatives

| Approach | Decision |
| --- | --- |
| Change filter or select another Forgejo tag | Reject without corrected source and exact-image reproduction; inspected code still uses mirror mode. |
| Native Git subprocess from Warden | Reject as an immediate fix: executable, helpers and writable runtime are absent. Packaging them expands bootstrap and release-artifact contracts. |
| Pure-Go exact-ref transport with serialized custody | Recommended candidate; runtime transport works, but controller protocol still needs the gates below. |
| Separate immutable history archive repositories | Possible alternative if custody cannot be made safe with existing transition leases; changes storage, retention and operator recovery contracts. Do not silently substitute it for branches on the writer. |

## Implementation plan, gated before production edits

### 1. Establish durable handoff semantics

Design the receiving Warden's preservation operation under the same local writer
transition lease that guards takeover/recreation. The receiver must own the transfer
and ref installation, rather than merely acknowledge an uncoordinated incoming push.
Validate the registered source steward and winning claim/epoch; do not add an
unauthorized general Git proxy to the status port.

Pin the losing main and **all existing preservation refs**. Use full commit IDs in
new deterministic branch names; refuse a conflicting existing ref even if it could
fast-forward. A receipt must identify the exact destination repository, winning claim,
ref names and commit IDs. A raw Git object lookup is never a deletion authorization.

The losing site's transition lease must exclude new preservation receipts while its
outgoing ref set is transferred and it is recreated. Prove that every acknowledged
ref survives a subsequent receiver takeover. Define stale-claim, timeout, restart and
lost-response behavior as retaining the old copy and retrying; never weaken `main`
protection to recover availability. Account explicitly for old/in-flight mirror jobs
before permitting this protocol to run during an upgrade.

**Confidence gate:** demonstrate serializable custody for overlapping takeovers and
preservation requests, not only successful exact-ref pushes. If that cannot be shown,
retain the release blocker and review the separate immutable-archive alternative.

### 2. Add narrow transport and behavior regressions

Planned code locations: a focused transport file under `internal/fabric`,
`internal/warden/writer.go`, `internal/warden/writer_lock.go`, and their existing tests.
Keep the static-image/read-only contract. Review dependency/security and memory impact
before choosing a product dependency version.

Use exact, non-forced `main` replication and create-only preservation refs. Do not
retain a scheduled `--mirror` path that can delete unrelated or historical refs.
Skip unchanged destinations; avoid re-fetching a full history per peer when the same
pinned source snapshot can serve the pass. Bound operations by context and keep
credentials out of URLs, process arguments, artifacts and errors.

Regression matrix, failing before each corresponding fix:

- dangling object is not a preservation receipt;
- exact-ref collision, including ancestor collision;
- source head/ref set changes during handoff;
- preserved refs survive a second takeover and repository recreation;
- overlapping receipt/recreation, stale winning claim and receiver restart;
- lost push/receipt response retries without duplicate branches or premature deletion;
- missing credentials, unavailable peers and unprotected/empty destination cases;
- steady replication preserves unrelated refs and refuses non-fast-forward main;
- quiesced legacy mirrors cannot re-enter the cutover.

Replace obsolete mirror-wiring assertions with these consumer-visible invariants;
do not re-pin mocked API call sequences. Preserve existing takeover/lease coverage.

### 3. Verify the complete deployed path

Provision an owner-labelled fabric only after the storage prerequisite below is met.
Run `hack/dev/test.sh` from `FROM=1` with no skipped stages; inspect convergence through
the public Console, deletion on both sites and uninstall. Exercise the actual Warden
image, not a host-only helper. Also repeat two successive takeovers with preserved
branches and the overlapping-handoff scenario; capture sanitized refs/claims and
resource identities. Run the Go race/vet suite once after integration edits.

Only after successful observations may the operations release gate be removed and the
transport change checkpointed as implemented. A partial or failed run does not close
this gate; the observed outcomes and any corrections are recorded below.

## Remaining acceptance fixtures

No `WECOLAB_*`, `AWS_*`, B2, Backblaze, supported cloud-token or Terraform input names
were present in the inspected process environment. Scoped configuration-name inspection
found no provider fixture configuration. The tool SSH registry has no configured hosts.
These observations do not assert that credentials cannot exist elsewhere.

| Requested fixture | Provisioning outcome and exact missing prerequisite |
| --- | --- |
| B2 rotation | Not provisioned: disposable account/key with required bucket/key-management access is not supplied. Use `WECOLAB_B2_ACCOUNT_KEY_ID` and `WECOLAB_B2_ACCOUNT_SECRET` privately with the disposable-account opt-in; do not paste secrets into chat. |
| Real provider retention and denied-delete | Not provisioned: HTTPS endpoint, explicitly disposable Object-Lock bucket, scoped write key and independent denied-delete key are absent. Emulator Object Lock is not a substitute. |
| Older-release upgrade | Public repository releases API returned `[]`. Historical source commits exist but are not published releases. The old-release fixture and independently verified target artifacts/recovery inputs are absent; no publish authorization was inferred. |
| Separate systemd guest | Provisioned `wcc-20261008-host` in the owner-isolated ARM64 VM. The complete `host-failure` scenario passed: firewall startup gates, interrupted-install resumption, failed reload/identical retry, mesh connectivity, external AppArmor restoration, uninstall inventory equality and actual PostgreSQL restore readback. |
| amd64/KVM destination | Local Docker reports `aarch64`; no configured authorized remote host with actual `/dev/kvm` was discovered. Emulation is not KVM evidence. |
| Real recipe lifecycle | Exhaustive bounded catalog selection leaves only Guacamole 1.6.0 (no unsupported fields or main/sidecar volumes). Its `guacd` sidecar has **no PostgreSQL bindings**. No current bounded candidate has native PostgreSQL consumers in every container as this runner requires. |

The recipe gate is overbroad for non-database sidecars. Do not inject database tools or
credentials into `guacd`, remove real upload volumes, add a dummy sidecar, or label the
earlier PostgreSQL-client fixture a catalog certification. Review a role-aware probe
contract: native queries for declared database consumers, actual service behavior for
non-database sidecars, and separate proof of a genuine DB-using sidecar where required.
Listmonk v6.1.0/v6.2.0 are upstream version candidates, not a selected passing fixture:
its uploads volume and lack of a DB sidecar leave the current acceptance criteria unmet.

The original shared Docker VM had **22,344,280 KiB (about 21.3 GiB)** free, below
the **30 GiB** acceptance requirement in `.github/workflows/verification.yml`.
Host filesystem space was not counted as Docker backing-store headroom. The isolated
VM recorded below resolves this capacity prerequisite without pruning or resizing the
shared daemon.

Provider inputs, an authorized amd64/KVM host, and actual verified release artifacts
remain missing. Keep those acceptance items blocked separately; neither the transport
prototype nor the new ARM64 guest covers them.

## Prototype review and cleanup

Independent read-only reviews of the custody plan and fixture/evidence boundaries
reported no findings and agreed that production implementation must remain gated.
They did not execute tests or certify the unimplemented protocol.

Owner-checked cleanup removed the one Forgejo container, its two volumes, its isolated
network and the scratch prototype image. No Docker resource with this investigation's
owner label remained. All foreign container/volume/network IDs present immediately
before cleanup remained afterward. Build cache and unrelated images were not pruned.
Throwaway source, binaries and Git clones were removed; the owner-only `result.json`
retains sanitized observations and the prototype source hash.

## Implementation and verification outcomes

- Removed scheduled mirror creation/sync and object-existence acknowledgements.
  Ordinary replication is exact and fast-forward-only. Custody refs are create-only;
  identical retries reuse retained refs, and name collisions fail closed.
- Added authenticated, receiver-serialized custody, all-prior-ref handoff, source
  rechecks, drain-before-snapshot and durable uncertain-deletion recovery.
- Review found and corrected an inbound third-site mirror race and a preservation-name
  collision. Owner-only `superseded-**` protection plus a repository-bound, observed
  rollout barrier fences already-admitted legacy operations. Interrupted protection
  updates retain the pending marker. Final focused reviews reported no findings.
- A real compressed-history regression first accepted an over-budget history; it now
  refuses before allocating the decoded store. Preflight also rejects oversized delta
  target sizes and object counts. Limits are 8 MiB compressed/aggregate expanded history
  and 16,384 objects, not a universal 256 MiB process RSS guarantee.
- Verified: `go vet ./...`, `go test -race -count=1 ./...`, the subsequently added
  interrupted-inbound-fence regression under `-race`, `go mod verify`, and launch-shell
  syntax. These do not substitute for the deployed acceptance run.
- A subsequent full race run exposed an unrealistic journal test fixture: Kubernetes'
  generated fake accepts stale Lease updates without resource-version conflicts.
  Journal regressions now use the existing HTTP Lease fixture that enforces compare-and-swap.
  The two affected journal cases passed 30 race-enabled repetitions, followed by a fresh
  complete vet/race run, module verification and launcher syntax check.
- Provisioned an owner-isolated Colima/VZ ARM64 guest: 6 CPUs, 12 GiB RAM, 64 GiB data disk,
  initially 62,366,220 KiB free. Docker Desktop's context, resources and capacity
  were not changed. Every lab command explicitly selects this guest's Docker socket.
- Fresh owner-labelled lab, prefix `wcc-20261008`,
  subnet `198.19.84.0/24`, zone `custody.wecolab.test`. Bootstrap exposed a harness
  readiness race: the Door returned 502 before Flux created the Console. The harness
  now waits for a read-only writer-authority response before its first non-idempotent
  site creation; it does not blindly retry the POST.
- The initial nested k3s launch exhausted the isolated VM's 128 inotify instances.
  Raising only that guest to 1,024 and rebuilding the owner-labelled fabric completed
  installation. The three boxes became Ready. The real failed-Nebula-reload/identical-retry
  probe passed on the installed public box.
- Deployed divergent transfers `pub → home@2 → pub@3` retained the original dangling
  history and both losing heads. Home's source repository changed from ID 1 to 2.
  Probe interruptions were preserved: one read crossed Forgejo's restart window; another
  sampled status before the asynchronous takeover epoch appeared. Resumption verified
  live refs and marker contents instead of replaying completed mutations.
- The concurrent receipt was refused with HTTP 409 while the Console elected `home@4`.
  Pub's repository changed from ID 2 to 3 without losing any history. A final takeover
  returned all six preservation refs to `pub@5`; an independent Git mirror clone read
  every marker. The mirroring credential could create/delete an ordinary probe ref but
  an actual `git push --mirror` could not delete a protected preservation ref.
- This exercised `ghcr.io/wecolabhq/warden` image digest
  `sha256:f20d54a08410cde33492fc3b618910f7c171f48f56999dcf367b7ec29092482e`
  under the deployed 256 MiB limit. Both final Warden pods reported zero restarts;
  this is fixture evidence, not a general peak-RSS bound.
- Additional probe-only corrections are retained in the artifacts: `install.sh takeover`
  executes inside the Warden pod, so the overlap probe must use the independent Console
  while deliberately pausing that pod. Explicit refspec control writes also need
  `remote.origin.mirror=false` when using a `git clone --mirror` checkout.
- Deployed probe artifact directory:
  `/tmp/wecolab-custody-runtime.5lAPwA9e`. Both the resumed deployed custody smoke
  and the complete full-fabric/uninstall run passed; details follow.


### Complete systemd fixture acceptance

The `host-failure` runner passed at **2026-10-08T19:29:06Z**. Artifact:
`/tmp/wecolab-custody-runtime.5lAPwA9e/host-recovery/2026-10-08T192239Z-host-failure-290cc0b8414e/result.json`.
It exercised actual systemd failures, resumed enrollment, mesh connectivity before
and after reload recovery, restoration of a pre-existing loaded AppArmor profile,
and exact before/after host inventory equality. Its isolated PostgreSQL 18.6 restore
read back the original rows with matching SHA-256
`70f32943a832a546ec916badd2863ee93df15de7f76518c6324979c363727c99`
from backup `20261008T192755`, system ID `7694380729340235799`, timeline 1.
This is local database/host evidence, not provider or catalog-recipe certification.

The first runner attempt exited after a successful resumed install because Console
registration had not propagated through Flux. The runner now waits for the registration
view and still requires exactly one new box; it never retries enrollment. That failed
artifact is retained. Its owned guest was uninstalled and its registration/node removed
before the complete scenario was rerun. All 18 recovery-runner unit tests passed.

### Full-fabric mutation-sequencing correction

The first `FROM=1` run passed readiness, deployment, both planned moves, the
operator-fenced power-off recovery, standby rebuild under generation 2, and the
planned return. It then failed to delete `notes` at pub. Git proved a caller race:
pub's epoch-7 claim `9857aa39cd2382024d5e12706ba755e357666c7c` was committed at
`20:07:13Z`; the public Console still reached home and committed deletion
`1368c6ec9d62cf8df72a03968738cee2f37165e7` at `20:07:15Z` from the same parent.
The latter is retained on pub as a protected superseded branch, not lost or merged
into the winning history.

The full-suite caller now waits for both sites' expected writer/epoch and the public
Console's matching authority before another mutation. During the intentional outage
it checks only the survivor's private Console, without waiting for the powered-off
site. The same barrier precedes certificate revocation after the final takeover.
Evidence: `/tmp/wecolab-custody-runtime.5lAPwA9e/full-suite-initial-failure.json`.
The complete `FROM=1` rerun below closes this gate; no test stages were skipped.

### Complete fabric and uninstall acceptance

The full `FROM=1` rerun passed from **2026-10-08T20:28:35Z to 21:09:20Z**, exit 0,
on the freshly installed owner-labelled fabric, after an owner-scoped demo reset.
Artifact: `/tmp/wecolab-custody-runtime.5lAPwA9e/full-suite.json`.
The harness source SHA-256 before and after execution was identical:
`db98af346db6bd673e0dcb3a9c5beb3bfcc5a7c59193deae14439297578f76a8`.

Observed stages:

1. Every site and box Ready; `notes` deployed and served through the Door.
2. Planned database moves to home and back to pub.
3. Independent old-postmaster fencing and pub power-off; home writer at epoch 8,
   forced database recovery, pub rebuilt as a generation-2 standby and planned return.
4. Pub writer at epoch 9, with both sites and the public Console converged before
   deletion; no app resources or data left at either site, and the Fabric entry removed.
5. Certificate renewal on all three boxes; home takeover at epoch 10, a new commit
   reaching pub, and `install.sh takeover` returning to pub at epoch 11.
6. Mac certificate blocklisting and Fabric removal.
7. Exact pre-install/post-uninstall inventory equality on mac, home and pub.

The run emitted CloudNativePG's warning that `metricsQueriesTTL: 0` disables its
automatic TTL behavior. It did not fail acceptance; no performance certification is claimed.

After the integration corrections, fresh verification passed: `go vet ./...`,
`go test -race -count=1 ./...`, `go mod verify`, all 18 recovery-runner unit tests,
and `bash -n` for both `hack/dev/fabric.sh` and `hack/dev/test.sh`.
These outcomes remove the operations convergence gate, not the separate B2,
real-provider retention, old-release, recipe-lifecycle or amd64/KVM gates.

### Owner-scoped acceptance cleanup

After successful uninstall, owner-checked `hack/dev/fabric.sh down` removed the lab.
The isolated Docker daemon then had no containers, no volumes and no owner-labelled
networks. The dedicated Colima profile and its data were deleted; `colima list --json`
reported no profiles, and Docker's default context remained `desktop-linux`.
Only the matching owner-stamped development cache and named temporary image exports,
binary, probe scripts and fixture files were removed. Shared Docker resources, images
and build caches were not pruned.

Sanitized success/failure artifacts and temporary probe source checksums remain under
`/tmp/wecolab-custody-runtime.5lAPwA9e`; the exact resource inventories are in
`cleanup-before.json` and `cleanup-resources.json`. Captured fixture credentials were
cleared from the probe runtime. At the implementation checkpoint, repository changes were
local; no push, publication or release had been performed.

### Pre-publication security checks — 2026-10-08

- Three independent read-only reviews covered custody/transport, control-plane
  authorization/recovery/upgrades, and privileged host/release tooling. No confirmed
  publication blocker was identified. These were static reviews, not new fixture acceptance.
- Gitleaks found one false positive: a credential-free Keycloak JDBC service address.
  The repository configuration retains default rules and requires both the exact catalog
  path and exact non-secret match. A real scanner smoke accepted that URL while rejecting
  a synthetic GitHub token in the same file, the same match in another file and a changed
  match in the catalog. The outgoing history and exported source tree then passed.
- Configured private-term scanning found no matches. Personal fixture identifiers were
  additionally removed from this document; their exact values remain in private local
  artifacts, not the public source.
- Updated the website's `markdown-it` from 14.1.0 to 14.3.2 and transitive
  `http-cache-semantics` from 4.2.0 to 4.3.0. `npm audit` reported zero vulnerabilities.
  Both root and `/wecolab` builds passed; the rendered publication documentation passed
  a Chromium smoke with no browser errors or horizontal page overflow.
- `govulncheck` v1.8.0 reported no affected packages or reachable vulnerable symbols.
  It separately reported [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) for the
  unimported `golang.org/x/crypto/openpgp` package; this is not a zero-advisory module scan.
- Fresh vet, complete Go race tests, module verification, installer-copy parity,
  Linux amd64/arm64 distribution builds, all 18 runner tests, 8 importer tests,
  20 native Mac tests and shipped shell syntax checks passed. The first Go enumeration
  raced `npm ci` removing dependency directories; the full command passed when rerun
  after installation completed. No application code was changed to mask that failure.
- Source publication must still use `hack/publish.sh`: private main first, then the
  identical public tree as a new snapshot with public-only ancestry and identity.
  No acceptance gate listed above is cleared by publishing the source.
