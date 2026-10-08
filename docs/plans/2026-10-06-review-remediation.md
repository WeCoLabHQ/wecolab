# Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every concrete finding and assurance gap from the October 5 review with bounded changes, migration safeguards, behavioral checks, and observed recovery evidence.

**Architecture:** Preserve WeCoLab's per-site k3s/CNPG, Git desired state, Console, and host/native boundaries. Repair the failure paths rather than replacing the architecture. Keep manual operator-fenced recovery explicit; do not add autonomous quorum failover, a new backup backend, or a frontend framework as incidental hardening.

**Tech Stack:** Go/Kubernetes/controller-runtime, Forgejo/SOPS, CloudNativePG/Barman/S3, Bash/systemd/Nebula, Swift/Virtualization.framework, Python catalog tooling, embedded vanilla JavaScript, Astro/Pagefind, GitHub Actions.

---

**Created:** 2026-10-06. **Status:** Remediation code is implemented in the working tree; full release/real-stack acceptance is **blocked**, not passed. No commit, push, release publication, production change or live-provider operation was performed. The implementation/evidence ledger below supersedes the original prospective wording. Unchecked phase/task steps retain their full acceptance meaning; a component test does not close an unrun fabric, provider or native-VM gate.

## Implementation and observed evidence — updated 2026-10-08

| Task | Implemented boundary | Observed verification and remaining acceptance |
|---|---|---|
| S1 | Production S3 HTTPS, DNS/address and socket-time checks; no redirects or environment proxy; explicit development mode. | Guarded HTTP/socket fixtures and race suite pass. No production provider was contacted. |
| S2 | Terminating Members lose ordinary and elevated request authority before external cleanup completes. | HTTP authorization/finalizer-outage regression passes; identity provider is a fixture, not a live production identity. |
| S3 | Vault coordination revisions, monotone credentials, restart reconciliation and exact-report retirement. | Conflict/crash/migration behavior tests pass. Real-provider rotation/retirement remains an A3 gate. |
| S4 | Atomic Git public/mesh claims, collision-blocking migration and conditional release with app removal. | Transaction/migration tests and actual concurrent public/mesh claim, refusal, release and reacquisition passed on the owned fabric. |
| S5 | Destination grants govern storage admission without exempting tenant security/schema checks. | Admission/quota behavior tests pass. Destination PVC binding/data I/O and capacity refusal remain unrun. |
| S6 | Native B2 requests have deadlines, bounded responses and cancellation. | Timeout/cancellation and subsequent writer work checks pass. No live B2 call was made. |
| R1 | Bounded, validated DONE metadata and same-history proof gate destructive recovery. | Actual CNPG/Barman failed-only-archive refusal, retained Cluster/PVC identity, three Flux fence checks, completed retry and independent restore passed. Development vault only; real-provider retention remains blocked. |
| R2 | Unique archive identities, preserved legacy archives and gated restartable migration. | Identity/migration/retirement tests pass. Pre-change whole-fabric upgrade/readback remains unrun. |
| R3 | Forced moves require current App identity, source, explicit physical-fence assertion and recorded operator evidence. | Real partition, unacknowledged-force refusal, independent fencing, promotion and standby rebuild passed. Retained incarnations preserve their role and fence until guarded deletion. No automatic physical fence is claimed. |
| R4 | Receipt-aged same-history progress, separate database/file scope and identity-matching operator restore records. | Real measured replay degradation/recovery, timeline change, automatic current-history backup and independent CNPG/Barman readbacks passed. Actual restore recording and matching API/UI projection passed; scope remains database-only. |
| H1 | Both k3s roles require successful isolation. | Actual systemd in an owned, network-isolated disposable guest refused both roles on exit 42, then ran both after exit 0. Actual iptables/mesh reachability remains a separate gate. |
| H2 | Failed Nebula apply/reload remains pending for identical-input retry. | Shell behavior tests and an actual guest-to-steward mesh connectivity helper passed. Full reload-failure/identical-bundle/host-baseline scenario remains blocked by its separate guest fixture. |
| H3 | Interrupted pinned k3s installation resumes without replacing identities or adopting unexplained state. | Installer interruption/idempotency cases pass. Full disposable-host install/remove rehearsal remains unrun. |
| H4 | External AppArmor policy ownership survives install/remove. | Policy ownership/failure cases pass. Real AppArmor restore rehearsal remains unrun. |
| H5 | Reload selects each manager's architecture and propagates transfer/import failures. | Fake-SSH/scp executable matrix and both Linux distribution builds pass; no real mixed-architecture fleet was used. |
| H6a | Mode publication retries failures and stays fail-closed across VM start/restart. | Native behavioral tests pass. Actual VM/shared-directory failure smoke remains unrun. |
| H6b | NoCloud test uses mounted consumer-visible media rather than an incidental ISO byte layout. | Mounted `cidata` seed and complete native suite pass: 20 tests. No actual guest boot is claimed. |
| U1 | Generic main/sidecar DB references, main-volume preservation and separate certification provenance. | Eight importer tests and Go catalog tests pass. Pinned import has 227 entries, 183 importer-deployable; **zero passed WeCoLab certifications**. Recipe lifecycle acceptance remains unrun. |
| U2 | Active panels refresh without replacing drafts, caret or open-dialog focus. | Served embedded Console browser smoke preserves a domains draft/caret through polling. |
| U3 | Rejected clipboard writes leave the one-time secret selectable; deliberate close clears it. | Actual browser rejection/manual-selection/close smoke passes; no false copy success. |
| U4 | Keyboard controls, locked server-owned recipe fields, modal focus trap, inert background and focus return. | Served keyboard/modal/catalog smoke passes, including opener replacement during polling. Not an assistive-technology certification. |
| U5 | Separate recovery/database/file evidence, honest Workspace isolation and sourced comparison claims. | Console protection surface and built root/subpath website reviewed in Chromium. |
| A1 | Read-only pinned-action product CI, shell/copy gate, race tests, native architecture gate and isolated integration artifact retention. | Local workflow commands pass as listed below. No GitHub-hosted workflow or privileged integration job was dispatched. |
| A2 | Hashed release manifest, verified bootstrap payloads, advisory ownership and staged upgrade protocol. | Release/upgrade tests, cross-builds and real Git smart-HTTP stale-revision rejection pass. No release was published and no existing fabric was upgraded. |
| A3 | Named scenarios, ownership guards, exact restore evidence, conditional recorder and strengthened external proof boundaries. | Five local recovery scenarios passed; actual restore recording/API/UI and standalone uninstall passed. Full integration failed on protected-main Forgejo mirror convergence. Six external scenarios refused missing prerequisites. Eighteen runner regressions pass; these are not substitutes for blocked acceptance. |

### Original component checks — 2026-10-06

- `cmp install.sh cmd/console/join.sh` before repairing build targets; Bash syntax checks for every checked-in shell entry point.
- `go test -race -count=1 ./...`, `go vet ./...`, `make dist`, and `make crds`.
- Catalog Python unittest discovery: 8 passed; recovery runner discovery: 7 passed.
- `cd mac && swift test`: 20 passed, including mounted NoCloud media.
- Website root and `BASE_PATH=/wecolab` builds; actual rendered Console/root/subpath browser checks.
- Real PostgreSQL primary/replica sampling and data readback, paused replay while the primary advances, resumed replay, physical shutdown of the old primary, timeline-changing promotion and new writes. Only the run-owned containers/network were removed.
- Installer-generated systemd units in a new owned guest: both roles refused injected firewall failure and started after success. Smoke-helper fixes include a noexec-safe shell invocation and avoiding a redundant reset of an unloaded unit. All smoke guests and the temporary Go harness were removed.
- `hack/dev/recovery.sh --help`; `env -u WECOLAB_DEV_PREFIX -u WECOLAB_DEV_OWNER hack/dev/recovery.sh recreate` exited 1 before Docker access. Its sanitized `result.json` records `Outcome: failed`, empty `Operations`/`Checks`, and no fabricated restore identity.

### Unmet release gates, not waived

At the original 2026-10-06 checkpoint, Docker's backing filesystem reported **2.5 GiB available, 99% used**; the full disposable fabric requires roughly 15 GiB before restore data. Host filesystem free space does not change that Docker limit. Existing `wcl-*` containers are user data and were neither pruned nor repurposed.

The trusted-main CI recovery job therefore uses an ephemeral isolated `wecolab-recovery` runner and enforces 30 GiB free Docker storage for the multi-scenario run. GitHub's [standard runner allocation](https://docs.github.com/en/actions/reference/runners/github-hosted-runners#standard-github-hosted-runners-for-public-repositories) is 14 GB and is not assumed sufficient. Provisioning that labelled runner is an operator prerequisite; no hosted/self-hosted job was dispatched here.

The dated local follow-up below records which real-stack gates have since run. Real-provider Object Lock/rotation requires explicitly authorized disposable credentials; upgrade requires a pre-change fixture, a real restore receipt and verified revocation of the old writer credential. Native guest boot/publication and Kata/mixed-architecture host checks require their supported disposable hardware. No passed per-app restore record or catalog certification was manufactured from unit, browser or standalone PostgreSQL evidence.

Read-only review corrections were integrated for vault/app race boundaries, stale recovery receipts, main-volume import loss, recovery-runner failure paths and exact-commit upgrade inventory/CAS. Remaining acceptance requires the unavailable environments and blocks release assurance; an unrun scenario can still reveal additional defects.

### Follow-up execution — 2026-10-07

- User-approved unused build-cache removal on local Docker Desktop increased available Docker filesystem space from 2.5 GiB to **25 GiB**. Images, containers and volumes were retained. The local 15 GiB prerequisite is no longer blocked by disk; the separate 30 GiB CI runner requirement is unchanged.
- Running the exact CI subnet selector against real Docker networks exposed `IPAM.Config: null` on networks without address pools. It failed with `TypeError` before the fix; treating that null list as empty now selects an unused, non-overlapping subnet successfully. No network was changed by this read-only selector check.
- Provisioned the separately owned `a3-7eff34eb5e2a` fabric on `198.19.26.0/24`; the first post-bootstrap disk check reported **18 GiB available**. Existing `wcl-*` resources were not used. Docker Desktop mount aliases exposed a false preflight refusal; ownership now checks the actual shared-directory device/inode and owner marker rather than requiring identical host path spelling.
- The real CNPG 1.30.1 API rejected numeric `monitoring.metricsQueriesTTL: 0`. New deployments and legacy migration now emit the duration string `"0s"`. The migration regression failed before the fix and passed afterward; a reloaded Console then deployed PostgreSQL 18.6 and completed real Barman backups.
- The first metadata read exposed nullable initial archive generations and an unexpected-exception path that lost its result file. The runner now handles `Archive: null` as generation one and retains failed evidence while propagating unexpected exceptions. All **10** runner tests pass, including wrong-owner/mount refusal, null generations, failure retention and archive-boundary selection.
- Actual Barman Cloud 3.20.0 metadata and its installed `CloudBackupUploader.backup` implementation both show `server_name=cloud`. This internal label is not the object archive name. Warden and the runner now bind metadata to the validated object key/prefix instead; completion/system/timeline/WAL checks remain. `go test ./internal/warden ./cmd/warden -count=1` passes. A signed real-object metadata read returned completed backup `20261007T174116`, archive `id-86848a2be8ce7f222c0f8e89bcc32bfb-pub`, system `7693982141011980312`, timeline `1`. These diagnostic runs do **not** constitute a passed restore drill.
- **Passed real same-name recreation:** `/tmp/wecolab-recovery-a3-7eff34eb5e2a/2026-10-07T180325Z-recreate-59dc196b17ed/result.json`. The old and new incarnations have distinct archive identities, and both independent CNPG restores reproduced their respective 256-row SHA-256 readbacks. This is database-only evidence on the local disposable vault, not provider-retention certification.
- The first name-race attempt restored its data but failed fixture setup: its generated offer name was 38 characters, exceeding the Console's 32-character limit. Its result remains `failed`; fixture offers now use short unique names. The real `warden record-restore` command refused that failed artifact with `restore scenario has no completed passing result`.
- Reviewing the real artifact also exposed inaccurate component labelling and implicit latest-backup selection. Subsequent runs query the actual Warden executable and restored PostgreSQL version, and set CNPG's `recoveryTarget.backupID` to the backup named in their evidence. The earlier artifact's `warden-sites` value was a k3s version, not a Warden version; it is retained unchanged as historical diagnostic context.
- After the production corrections, `cmp install.sh cmd/console/join.sh`, `go vet ./...`, and `go test -race -count=1 ./...` all passed. The actual served Console was opened through a temporary read-only proxy; before operator recording it showed **Never verified**, not an invented restore time.
- A real concurrent public-name claim exposed Forgejo's HTTP 500 `PushRejected Error:` with `(incorrect old value provided)` for a Git compare-and-swap loss. The writer now recognizes that exact conflict signature, fetches a new snapshot and reruns ownership validation; unrelated failures remain failures. Its ownership regression failed before the fix and passed afterward.
- **Passed full public/mesh name-claim recovery:** `/tmp/wecolab-recovery-a3-7eff34eb5e2a/2026-10-07T192422Z-name-race-a7be7e393851/result.json`. Public outcomes were `[409, 200]`; mesh outcomes were `[200, 409]`. Release/reacquisition assertions completed and the independent PostgreSQL readback matched SHA-256 `b7a72fa109ea05b800cfd2eb43f17004842a53cbb59adbfb06c42a51bb548f07`. Intermediate fixture failures remain failed artifacts; offers now use valid short names, projection checks wait for observed state, and mesh fixtures use distinct public hostnames.
- Barman's real base backup completed in about eight seconds, before the original fault injector could act. A run-owned base-upload bandwidth limit made the fault observable without changing WAL archiving. The real native sidecar contained one Barman parent and three upload children; selecting the single process-tree root produced both Backup phase `failed` and object metadata `Status: FAILED`. The ObjectStore arguments were restored. This smoke alone does not pass the full destructive-rebuild scenario.
- Review identified that a protection observation during `STARTED` could masquerade as post-failure proof. The failed-backup scenario now requires an observation after the independent read of `FAILED`, and records both timestamps. Production compatibility review found no actionable defect.
- The actual `warden record-restore` command refused failed evidence, a mismatched actor, and a changed App revision. A fresh Console preview independently confirmed the name-race App's revision had changed since its readback. Historical artifacts were not rewritten and the revision check was not bypassed; a new readback is required before operator recording.
- The six externally gated scenarios were invoked with their actual missing prerequisites and refused before mutation: `rotation`, `provider-contract`, `remote-storage`, `host-failure`, `upgrade`, and `recipe-lifecycle`. Their failed evidence directories are under `/tmp/wecolab-recovery-a3-7eff34eb5e2a/2026-10-07T1941*`; these refusals are not scenario passes.
- After the fault-injection and observation-cutoff corrections, `cmp install.sh cmd/console/join.sh`, `go vet ./...`, `go test -race -count=1 ./...`, and all **10** recovery-runner tests passed again.
- The first full failed-backup attempt proved that a fresh post-failure observation still selected completed backup `20261007T200137`, not failed backup `20261007T200148`: failure was independently observed at `20:02:08.538486Z`, and product evidence was observed at `20:05:57.935517845Z`. Its completed retry restored the expected database checksum. The overall artifact remains **failed** because the subsequent takeover called a nonexistent HTTPS listener on private `home`.
- The recovery runner now uses the existing Nebula-bound Console NodePort for private-site calls. A real private-site preview succeeded. Before releasing the old primary's retained fence, its exact pre-force App SHA and archive were confirmed unchanged in the current writer and home's database was confirmed still in recovery; the rejected force request had not committed. Both sites then converged on the restored `pub` writer at epoch 3. The evidence wait also allows the documented five-minute vault scan plus scan completion/projection, without relaxing its post-failure cutoff.

### Writer takeover correction — discovered during A3

The second failed-backup run reached the private Console but its first post-takeover App
commit received Forgejo 403. `TestStaleFollowerPassCannotCloseSuccessfulTakeover` reproduces
the cause deterministically: a follower pauses during peer lookup, takeover commits, and
the stale follower subsequently removes `fabric` from the push allowlist.

Implementation plan:

1. Add `withWriterLock(context.Context, coordinationv1.CoordinationV1Interface, func(context.Context) error) error`
   in `internal/warden/writer_lock.go`, backed by client-go lease election for
   `wecolab-system/wecolab-writer-transition`. Each operation uses a distinct holder identity.
   Cancellation or lease loss cancels the operation; normal release waits until it stops.
   Refused acquisition never invokes the mutation callback.
2. In `Writer.Sync`, discover peers outside the lock, then re-read `GitClaim` under it.
   An obsolete claim skips the pass. Role admission and follower repository mutation are
   serialized; unrelated leader maintenance stays outside the critical section.
   Wrap `TakeOver`'s read/protection/commit/rollback sequence in the same lease and remove
   the obsolete retry for a competing local protection update.
3. Initialize coordination clients once in Console and Warden startup and in the one-shot
   takeover command. Pre-create the lease in the system manifests; give the Console only
   named-lease `get`/`update` permission in `wecolab-system`, not cluster-wide lease access.
   Migrate all callsites and existing takeover tests without a no-lock production fallback.
4. Verify serialized competing operations, cancellation/loss, denied acquisition, stale
   follower suppression and takeover rollback. Run the full race suite, reload both
   components only in the owned lab, and rerun the failed-backup drill. Keep all previous
   failed artifacts unchanged.

Observed implementation evidence:

- The stale-follower regression failed before serialization and passes afterward. Lease
  acquisition, competing operations, cancellation, loss, and panic cleanup are covered.
  The panic regression independently failed before deferred release was added.
- `cmp install.sh cmd/console/join.sh`, `go vet ./...`, `go test -race -count=1 ./...`,
  all **10** recovery-runner tests, and `bash -n hack/dev/test.sh` pass after integration.
- The owned lab loaded the corrected Warden and Console images. Both Warden rollouts
  completed. The real `install.sh takeover` acquired the local lease and made `pub` the
  writer at epoch **5**. The additive lease and named Console permission were committed
  only to the disposable Fabric, not to the source checkout.
- During the earlier retained fence, `pub-pub`'s one-hour development certificate expired
  at `2026-10-07T20:53:31Z`. The live steward's existing `CertService` issued a replacement
  for its registered public key through an explicit offline administrator operation.
  Its CA was unchanged, Nebula validated it against the existing private key, and
  `pub-pub` returned Ready. Before restoring networking, the current Git preview still
  named `pub` as primary and home's database was independently confirmed in recovery.
  This fixture recovery is not a passed host-failure scenario.
- The next full retry remained **failed** before takeover: its backup completed before
  interruption. The pinned plugin's
  [ObjectStore cache](https://github.com/cloudnative-pg/plugin-barman-cloud/blob/v0.15.0/internal/cnpgi/instance/internal/client/client.go)
  retains settings for ten whole seconds; the sidecar's logged command omitted the newly
  patched throttle. The runner now waits through that cache lifetime and checks the
  setting both when applying and restoring it. A real smoke then produced independently
  read `FAILED` metadata for backup `20261007T223752` and restored the original arguments;
  see `/tmp/wecolab-recovery-a3-7eff34eb5e2a/backup-cache-smoke.json`.
- Existing integration absence probes returned success when Docker queries failed, and
  the uninstall comparison returned success with a missing baseline. These were reproduced
  with real failing commands. The probes now propagate query errors, bind the correct
  box, and require readable nonempty baselines before uninstall; comparison errors cannot
  pass. Negative smoke probes now fail, and actual absence of `dev/notes` still succeeds.
  All **10** runner tests and shell syntax checks pass after these harness corrections.
- Final review reproduced premature lease release on renewal failure: client-go released
  before canceling the mutation. Automatic release is now disabled; callback cleanup
  precedes an ownership-checked, resource-version-guarded release. The new regression
  failed before the correction; the full Go race suite, vet, copy check, shell syntax,
  and **12** recovery-runner tests passed afterward.
- The next failed-backup run (`2026-10-07T224442Z-failed-backup-3ad8c8ab7215`)
  was deliberately canceled before force after review found unsafe old-primary restart.
  Its result is **failed**; interruption lost the in-memory operation log. Independent
  inspection found both sites running, App primary still `pub`, the injected Backup
  failed, and upload arguments restored. No passed drill is inferred.
- Force drills and integration step 4 now apply a persistent CNPG process fence before
  power-off and require a new Cluster UID on standby reconstruction. Failed recovery
  no longer automatically restarts the old site. A live owned-fixture smoke observed
  `pg_ctl: no server running` both before and after deleting/recreating its fenced pod.
  The annotation was retained; no old-primary fence was cleared to obtain a pass.
- Two further false-positive regressions failed before correction: partition recovery
  accepted changed/missing original rows, and replay-lag accepted a permanently broken
  exporter as `unknown`. Forced readbacks now match all original sentinel bytes;
  replay-lag requires fresh healthy measurements before and after a measured
  `outside-objective` interval. Full real scenarios are still pending at this checkpoint.
- The corrected lease was loaded into both live Wardens/Consoles; actual `install.sh
  takeover` promoted `home` to epoch 6, then `pub` to epoch 7, with the public Console
  following each writer. Evidence: `lease-release-live-smoke.json` in the same artifact root.
- Reload exposed the laptop agent's `CreateContainerError`: `/wecolab` inherited a
  private guest root mount. The development image now marks its own mount namespace
  shared before starting systemd. A separately owned, network-isolated guest booted
  with `/ shared` and an active multi-user target, retained shared propagation after
  restart, and was removed. The live owned laptop was repaired in its own namespace;
  its node-agent rollout then completed. No shared Docker-host policy was changed.

### Backup fault sequencing — 2026-10-08

- `/tmp/wecolab-recovery-a3-7eff34eb5e2a/2026-10-07T231456Z-failed-backup-f6e1dec03f34/result.json`
  remains **failed**. It proved post-failure selection of completed backup
  `20261007T232229` and an independent historical restore matching
  `5dcbbb4b6668a59cfea4fb827fe88f997adb820c129023b2d7c483bb70a9ab31`.
  Home's actual base-backup worker failed, but the runner mistakenly tracked a queued
  ScheduledBackup, which subsequently completed. Kubernetes Backup events independently
  identified the running and pending jobs; pending targets are now excluded and refused
  before process access. The new regression failed before the correction; all **13**
  runner tests and integration shell syntax then passed.
- The failed run retained the old process fence and powered-off site. Its development
  certificate expired during the wait. Operator recovery used the existing registered
  public key and CA, validated the replacement against the original private key and full
  Nebula configuration, and never removed the old database fence. After the real
  completed home backup, `pub` reported a healthy home-sourced replica and independent
  SQL returned `pg_is_in_recovery() = true` with all 256 original rows.
- Queued automatic backups must remain unable to replace a failed-only archive before
  the refusal assertion. A proposed data-only endpoint override was rejected by live
  evidence: the plugin appends its configured endpoint later, overriding it. The
  replacement fault intentionally passes an invalid argument only to subsequent base
  backups. A real smoke recorded Backup `followup-proof-df59f51bee` as failed, observed
  WAL archiving continue, independently confirmed completed backup `20261008T000750`
  remained selected, and restored original arguments. Evidence:
  `successor-backup-fault-smoke.json` under the artifact root.
- The first worker is still interrupted against the real vault so its own FAILED
  metadata is retained. Successor-command failure now spans the destructive-refusal
  check and is removed before the explicit successful retry. The full corrected drill
  is rerunning; the diagnostic smoke is not a passed scenario.
- The next full artifact, `2026-10-08T001717Z-failed-backup-d92d2d54f612/result.json`,
  remains **failed**: the prolonged throttled home upload exhausted the Barman sidecar's
  memory (`OOMKilled`, exit 137). Its historical restore checksum still matched, but
  no destructive-refusal or complete scenario pass is inferred.
- The throttled-worker/invalid-argument injector is superseded by a narrow bucket-policy
  denial of the run archive's `base/*/*.tar*` uploads. The real diagnostic
  `backup-upload-denial-smoke.json` records failed Barman backup `20261008T011659`,
  continued WAL archiving, restored bucket policy, and a completed retry whose independent
  restore matches `95a273f9e44d6519eba33f61eec1c65ec8eee9cdf143b37b76619385877cf576`.
  The obsolete worker-selection helper/test was removed; all **12** remaining runner
  tests and shell syntax checks pass. This diagnostic is explicitly not a full scenario.
- A later Flux reconciliation removed the default-manager database fence. The real
  `flux-fence-ownership-smoke.json` reproduces removal on an app-managed ConfigMap and
  retention with `flux-client-side-apply`. Force drills now use that manager and require
  an explicit Flux reconciliation before checking the stopped postmaster and powering off.
- The old pub Cluster was not a verified replacement: its creation time predates the
  force, and its standby spec alone was insufficient evidence. It remained in
  `RebuildWaiting`. Operator recovery started only its API with `disable-agent: true`,
  observed no PostgreSQL/container-runtime processes, and re-established the fence on
  the same UID. A temporary CNPG admission match condition covered only that owned
  Cluster's metadata-only fence update while workloads were stopped; every original
  admission condition was restored before removing the agentless startup override.
  Evidence: `persisted-fence-recovery.json`. This recovery is not a host-failure pass.
- **Passed full failed-backup acceptance:** `2026-10-08T012912Z-failed-backup-913d33f51930/result.json`
  under the same artifact root. Fresh product evidence selected completed backup
  `20261008T013638`, not failed `20261008T013651`. During home's failed-only archive,
  the old Cluster UID `c926393a-22b3-4403-8a98-a13848719e41` and PVC UID
  `fd19363c-b8bc-413a-9f15-df4aa8145586` remained unchanged and PostgreSQL stayed fenced
  after pub restarted. Only the completed retry allowed standby replacement.
  Independent restoration of all original rows plus the survivor row matched
  SHA-256 `8b169ac4a362e5cce6d7dedd791fae85d6faa651a5616b584fdd87b900f82959`.
- **Passed actual operator recording and API/UI projection:** `warden record-restore`
  committed the successful artifact to the disposable Fabric as
  `f6a4ffc7cfaaad68bc634a1b6f5c5d825051f7ec`, authenticated as
  `system:serviceaccount:wecolab-system:wecolab-warden`. Independent hashing matched the
  durable evidence digest `2f76329dec625c909b351e6fc28a03ab1be9f0dcc73be94044615ac903ff65b2`.
  Both sites received the record; the live API and rendered Console showed
  `2026-10-08T01:57:33Z` with `database` scope for only `dev/rec-913d33f51930`.
  Other app cards remained “Never verified”; files were not promoted to protected.
  The actual browser surface was captured and closed. See adjacent
  `record-restore.txt` and `record-projection.json`. No source-repository commit was made.

### Post-promotion history and continuous fence correction — 2026-10-08

The first real replay-lag run, `2026-10-08T020109Z-replay-lag-805ca3630f34/result.json`,
remains **failed**. It measured healthy exposure at 29.77 seconds, exceeded the five-minute
objective while paused, recovered to 22.28 seconds after resume, observed the archive outage
and recovery, and restored all pre-promotion rows. Rejoin then stalled: home's completed
backup `20261008T021916` has timeline 1, but fresh writable-primary samples have timeline 2
for the same system ID. The deletion guard correctly reports the mismatch. `NeedsBackup`
incorrectly treats that old-history completion as sufficient and never requests a replacement.
The timeout retained an earlier startup API error; that error was not the continuing cause.

The original pub Cluster also lost its transient fence after a later desired-role apply.
Its UID and creation time were unchanged. The exact removal actor is not established by
managedFields timestamps alone; pinned Flux SSA cleanup does not explicitly target
`flux-client-side-apply`. Thus the earlier passed failed-backup artifact proves its recorded
assertions and restore bytes, not continuous fencing throughout later reconciliation.
Its authenticated restore record remains actual database-only readback evidence. A stronger
retention assertion and another full failed-backup run are required. Pub's postmaster was
independently confirmed stopped again, and the owned pub container was powered off.

Bounded implementation plan:

1. In `internal/warden/apps.go`, pass the current archive and measured history into
   `NeedsBackup`; validate the fresh, writable, healthy primary report in `ensureBaseBackup`.
   Only a completed backup matching that archive, system ID and timeline suppresses a due backup; preserve existing in-flight
   and hourly-failure cooldowns. Keep `ensureBaseBackup`'s existing behavior boundary and
   update every caller. Gate immediate/scheduled backups on actual local primary readiness,
   not only desired primary. Reproduce the missing post-promotion Backup with the existing
   fake Kubernetes client and cached peer-report fixture before changing production code.
2. In `PlanAt` and `internal/warden/role.go::DBPatch`, make the retained old-generation,
   non-serving Cluster's fence part of its Flux-desired annotations. Derive it from the
   existing archive-generation lifecycle, not a new controller, CRD or quorum protocol.
   Preserve the fence while `MayDestroy` refuses and while deletion is pending. A missing
   old Cluster or current-generation replacement must not inherit it. Keep the independent
   operator pre-power-off/startup fence requirement. Test this generation transition before
   changing the planner, and verify the actual fence after repeated role reconciliation.
3. Update the runner's failed-only assertion to recheck the stopped postmaster and retained
   fence immediately before allowing the successful retry. Run targeted regressions, reload
   the owned lab, prove it schedules a timeline-2 backup, and rerun affected real scenarios.
   Use fresh owned fabrics for remaining full acceptance runs to avoid accumulated restore
   targets exhausting lab capacity; keep every historical failed artifact unchanged.

Correction checkpoint:

- The new Go regressions reproduced old-timeline completion suppressing a required backup,
  premature standby/stale/unknown-history requests, missing desired old-generation fencing,
  and scheduled backups enabled before actual primary readiness. They passed after the
  bounded corrections. The destructive-rebuild proof was not weakened.
- A runner regression then reproduced a missing process fence being overlooked during
  rejoin. The runner now powers the old site off and refuses that observation, rather
  than letting a later replacement conceal it. Failed-backup requires three explicit
  post-role Flux reconciliations without reapplying the fence. Rebuild readbacks compare
  the replacement's rows and timeline, and replay-lag records the automatic promoted-history
  backup before requesting any manual backup on the new primary.
- `cmp install.sh cmd/console/join.sh`, `go vet ./...`, `go test -race -count=1 ./...`,
  shell syntax, and all **13** runner tests passed. The runner tests passed again after
  adding the replacement readback and automatic-backup evidence checks.
- Independent reviews found no blocker in current-history scheduling or generated fence
  placement; the identified missing post-reconciliation runner assertion is now implemented.
  Actual runtime acceptance of these corrections is still pending at this checkpoint.
- The owned Docker lab reached only 4.8 GiB free. Its labelled containers, volumes and
  network were removed with the ownership-guarded lifecycle; historical evidence and the
  owned dependency cache were retained. The stopped former primary was never restarted.
  Resuming bootstrap exposed macOS `cp -n` returning 1 for an existing destination;
  an isolated reproduction preserved the destination and returned 1. The cache seed now
  explicitly skips existing destinations. Fresh owned bootstrap is in progress.
- Fresh owned bootstrap completed successfully with the retained cache. **Passed full
  replay-lag acceptance:** `2026-10-08T031940Z-replay-lag-06b5d93651e0/result.json`
  under the same artifact root. Healthy exposure was 29.25 seconds before pause,
  became `outside-objective`, then recovered to 33.86 seconds. Archive outage/recovery,
  idle replay and promotion to timeline 2 also completed.
- Without a manual new-primary backup request, the corrected controller completed
  timeline-2 backup `20261008T033659` for system `7694132140719755288`. The retained
  Cluster `bfec2cd9-9bf5-4999-8f37-d60e0f518542` stayed fenced until replacement by
  standby `862e1189-b462-41f0-be87-d0fbb0e655e8`, whose timeline and all 513 rows matched.
  Independent pre/post-promotion restores both matched
  `2cd0fa4911b4b40d29e3d182b9dea6eaab3e88a71458461cfcb2abff7f3bf0c7`.
  Final restore evidence pins completed timeline-2 backup `20261008T034402`.
  Partition acceptance is now running on another fresh, identically owned fabric.
- **Passed full partition acceptance:** `2026-10-08T035441Z-partition-93278726715f/result.json`.
  The disconnected old primary committed a write, unacknowledged Force was refused, and
  the independent process/power fence preceded promotion. Old Cluster
  `05cbba04-cf48-454b-b8f4-c144b02ab216` was replaced by standby
  `8dbfd790-a245-4ef4-b6ea-5bf3345d235e`, which refused writes and replayed the survivor's
  rows. The timeline-2 backup `20261008T040813` restored every required sentinel byte;
  SHA-256 `6c1835739fb672902dc26e35e31cd7de0848564e495f4ab53eb0911516f8731f`.
  The full existing integration/uninstall suite is now running from step 1 on a fresh
  owned fabric, not resumed from a later phase.
- The first full integration attempt, `20261008T040939Z-owned-lab-reset.json`, remains
  **failed** at its initial planned move (curl exit 22). The live API independently showed
  `Ready=True` alongside `StandbyStaged=False`, `WithinRPO=False`, and no validated
  backup. This is the intentional separation of availability from recovery, not proof
  that planned-move admission should be relaxed.
- The integration harness now waits for all three existing move gates—`PrimaryHealthy`,
  `StandbyStaged`, and `WithinRPO`—with no handover before each planned request.
  An isolated execution of the actual shell function accepted all-ready and refused
  an unstaged replica, unknown replay, a missing gate, and an active handover. Shell
  syntax passed; the full integration suite is rerunning from step 1 on another fresh
  owned fabric.

### Full integration upstream blocker and isolated uninstall — 2026-10-08

- `20261008T042449Z-owned-lab-reset.json` remains **failed**, not a partial pass. The
  corrected readiness waits allowed real planned moves home/back, token observation,
  Door routing, independent forced fencing, standby reconstruction and planned return.
  Deletion then timed out because the two Fabric histories did not converge after
  takeover: pub claimed epoch 3 while home retained epoch 2 and its divergent deletion
  commit `4693bf20b4612a6bee3f94e654fd49691a0df77a`.
- The real Forgejo 15.0.9 rootless image
  `sha256:caf1bca332f95cdcf124227a4bfa3b49bbbfbbc8a5e4a97406921cb581165413`
  retained `branch_filter=superseded-*`, but its mirror error attempted to delete the
  remote default `main`. Protected-branch rejection also rejected the branch that should
  preserve the old history. The guarded local copy was not erased.
  `forgejo-filtered-mirror-blocker.json` records sanitized claims, branch IDs and actual
  mirror errors without credentials.
- This contradicts the documented [filtered-mirror contract](https://forgejo.org/docs/v15.0/user/repo-mirror/).
  The pinned [mirror caller](https://codeberg.org/forgejo/forgejo/raw/tag/v15.0.9/services/mirror/mirror_push.go)
  unconditionally sets `Mirror: true`, and the pinned
  [Git implementation](https://codeberg.org/forgejo/forgejo/raw/tag/v15.0.9/modules/git/repo.go)
  consequently appends `--mirror`. The current upstream branch also retains that call.
  No corrected released pin was found. Removing branch protection or fabricating a
  successful preservation receipt is not an acceptable workaround; full integration
  remains a release blocker pending a verified upstream correction or a separately
  reviewed transport change.
- **Passed standalone uninstall only:** after retaining diagnostics, actual `FROM=8`
  uninstalled mac, home and pub and matched each original captured host inventory.
  `standalone-uninstall.json` includes the baseline hashes and `FullScenario=false`.
  The shell's generic “all checks passed” footer is not a full-suite pass when resumed
  at step 8. Steps skipped by that invocation retain their earlier failed/unrun status.
  The subsequent stronger failed-backup/fence-retention run is recorded below.

### Retained-role fencing and acceptance proof boundaries — 2026-10-08

- `2026-10-08T053832Z-failed-backup-b83f3a6634aa/result.json` is **failed**.
  Its independent historical restore matched
  `aec435ac29b22a2a730ae065090e323f152f890e20215dd4dee2569e41d96f0d`, and the
  failed-only new archive correctly held destructive rebuilding. Repeated role
  reconciliation then exposed the old Cluster losing its process fence. The old
  container was powered off; neither that partial readback nor the earlier failed-backup
  pass establishes continuous fencing under this stronger check.
- The retained Cluster had entered CNPG's primary-to-replica transition.
  [CNPG 1.30.1 transition cleanup](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/reconciler/replicaclusterswitch/reconciler.go)
  explicitly removes the all-instance fence. The correction preserves observed
  `replica.primary`, `source` and `promotionToken` for the retained incarnation while
  keeping its desired fence; it does not request that transition before guarded deletion.
  Planned handovers and new-generation role selection remain unchanged.
- Regression cases for an old primary, a previously token-promoted primary and an old
  replica failed before that correction. The Warden suite, `go vet ./...`, and
  `go test -race -count=1 ./...` passed afterward. Independent protocol review found
  no blocking issue.
  **Passed full corrected failed-backup acceptance:**
  `2026-10-08T061732Z-failed-backup-cf760d30c90f/result.json`
  (`06:17:32Z`–`06:42:24Z`). The current harness verified three post-role Flux
  reconciliations without reapplying the fence. Old Cluster
  `94f6423f-c751-4b54-a945-341db21bd265` and PVC
  `d0bf20a3-ae10-4397-9955-d3e47169e2ba` remained intact and stopped while home
  contained only FAILED backup `20261008T063149`. After a completed retry, replacement
  standby `8b5a41b5-2437-47b7-9141-e2c66a61e061` was observed. Independent restoration
  of completed timeline-2 backup `20261008T063922` matched all survivor rows:
  SHA-256 `3531eba4a3891bd4042651d5bfd300ae97ab7e5b3baa95a7689696281d61ef48`.
- Another regression proved that the runner could overlook a lost annotation if a
  later reconciliation reapplied it. Post-reconciliation fence loss now refuses
  immediately and powers off the old site. Initial fencing still waits for the first
  stopped postmaster; established fencing is not allowed to disappear and recover.
- Review also found weak checks in the still-blocked provider, host, recipe and upgrade
  scenarios. Expired/missing/naive retention deadlines now fail before delete attempts;
  active retention metadata and denied-key refusal are recorded separately. The provider
  boundary regression passed, but no real retention bucket was exercised.
- The new host mesh probe actually read the exact home Kubernetes CA from the owned mac
  guest through `nebula1`. Warden's manager-only port was correctly inaccessible to
  that worker; the probe uses the permitted own-site Kubernetes API instead. This is
  connectivity-helper evidence, not systemd failure-injection or host-uninstall acceptance.
- Recipe gates now reject stale Deployment generations/old Ready replicas and require a
  reviewed in-container database probe. Each main/sidecar must commit a fresh challenge
  and read all prior rows; independent SQL verification rejects fabricated stdout.
  The full recipe path includes a recovery-gated planned move, independent restore and
  source uninstall. A pinned candidate and its native database probe are still missing;
  no recipe certification is claimed.
- Recipe review caught two additional false proofs: the sentinel table belonged to
  `postgres`, not the application's non-superuser, and identical image sets could allow
  the previous template to pass immediately after update submission. The harness now
  grants only SELECT/INSERT on its fixture table and waits for a newer Deployment
  identity/generation. A same-image generation regression passes.
- **Passed bounded recipe-helper smoke:** `recipe-native-probe-smoke.json` records a real
  ARM64 Kubernetes Deployment transition from generation 1 to 2 without changing images.
  Both main and sidecar connected using the CNPG-generated non-superuser credentials,
  refused access before the fixture grant, then committed/read fresh challenges before
  and after the transition. All four exact readbacks matched; actual image digests and
  execution architecture were recorded. The exact owned namespace was removed.
  `FullCandidateLifecycle=false`: these PostgreSQL-client containers are not a certified
  catalog recipe. Earlier throwaway fixture attempts failed readiness, PodSecurity or
  pre-publication service checks; none is counted as a pass and security policy was not
  relaxed. The successful fixture reused CNPG and waited for its real writer endpoint.
- Upgrade schema-report mismatch is now labelled accurately and must fail for its specific
  reason. The old-writer credential must separately fail a real Git mutation with HTTP 401
  after upgrade. `old-writer-auth-probe-smoke.json` proves that the helper accepted an
  actual Forgejo authentication refusal and rejected an unrelated-path failure using an
  unknown disposable token; it explicitly does **not** prove old-release revocation or
  a full upgrade. The authorized old-release fixture remains unavailable.
- Final local checks passed: **18** recovery-runner tests, runner help, changed shell
  syntax and installer-copy parity. The previously recorded full Go/race/vet run covers
  the final Go changes; later corrections were in Python/docs. Provider/host/auth helper
  review found no blocking defect; recipe review's two findings were corrected and
  exercised in the real non-superuser/generation smoke above.
- Final teardown is recorded in `final-owned-cleanup.json`: all four exactly owner-labelled
  containers, four volumes and the isolated network were removed; no Docker resource
  carrying this run's owner label remains. Every pre-existing foreign resource ID was
  preserved. The exact
  marker-owned cache and temporary smoke/reset helpers were removed; images/build cache
  were not pruned. The owned read-only Console proxy was stopped. Historical sanitized
  evidence remains under `/tmp/wecolab-recovery-a3-7eff34eb5e2a` with an owner-only directory.
  This remediation was subsequently checkpointed locally in `4c014f1`, `a7e1e87`,
  `fd885ed` and `e1007b5`; nothing was published.

### Writer-convergence research and fixture inventory — 2026-10-08

- The [confidence-gated plan](2026-10-08-writer-convergence.md) records primary-source
  research, the local checkpoint boundaries and separate external-fixture prerequisites.
- A real pinned Forgejo fixture accepted an exact-ref in-memory Git transfer from a
  non-admin collaborator without changing protected `main` or unrelated refs. Collision,
  non-fast-forward main and bad-credential attempts were refused. The transfer also ran
  in a non-root, read-only scratch container without Git or a shell.
- The same investigation proved a second blocker: `/git/commits/<sha>` can return an
  unreferenced commit that a fresh mirror clone does not retain. At that checkpoint,
  `CopyAnswer.Has` did not establish durable preservation before repository recreation.
- No production transport/dependency had changed at that checkpoint. Repeated/concurrent
  takeover custody and legacy-mirror quiescence were unproved; the bounded prototype was
  not full-fabric acceptance. Evidence: `/tmp/wecolab-convergence.VfH5FpVn/result.json`.
- Fresh local Go race/vet, 18 runner tests, 8 importer tests, 20 native Mac tests,
  installer-copy parity, shell syntax and website build passed before checkpointing.
- Provider fixture inputs and an authorized amd64/KVM host were not found; the public
  releases API returned no releases. Docker had about 21.3 GiB free, below the 30 GiB
  full-acceptance prerequisite. No foreign resource was pruned to bypass that gate.
- Exhaustive bounded recipe selection leaves Guacamole 1.6.0, whose `guacd` sidecar
  does not use PostgreSQL. No native probe can honestly satisfy the current every-container
  database requirement for that recipe; no catalog certification was added.

### Writer-convergence implementation accepted — 2026-10-08

- Replaced scheduled mirroring and object-existence receipts with bounded pure-Go
  exact-ref transfer and receiver-serialized custody. Independent implementation reviews
  reported no remaining findings; the fresh complete Go vet/race suite passed.
- Actual deployed successive and overlapping transfers preserved all six historical refs,
  independently verified by a mirror clone. The protected refs resisted a real legacy
  `git push --mirror` deletion attempt.
- Provisioned a separate 64 GiB ARM64 Colima/VZ guest without changing the shared Docker
  daemon. Complete systemd recovery and database restore/readback passed.
- The complete `FROM=1` fabric rerun passed, including both-site app/data deletion,
  writer/Console convergence, certificate renewal/revocation and exact uninstall
  inventory equality. Initial failures and the harness sequencing corrections remain
  recorded rather than reclassified as successes.
- The [convergence record](2026-10-08-writer-convergence.md) contains timestamps,
  artifact locations, resource identities and limits. Its deployed-convergence gate is
  closed; B2 rotation, real-provider retention, actual old-release upgrade, native recipe
  lifecycle and amd64/KVM acceptance still lack the separately listed prerequisites.
- Owner-checked cleanup removed the lab, dedicated VM/data and temporary build/probe
  files. Docker's default context remained `desktop-linux`. Sanitized artifacts were
  retained; nothing was pushed, published or released.



## Start here

1. Read the [audit and evidence register](2026-10-05-codebase-review.md), including the historical verification limits and dated primary research.
2. Follow the phases below; the coverage matrix is the authoritative mapping from review IDs to implementation tasks.
3. Work one small behavior change at a time: regression or concrete reproducer, minimal fix, targeted check, actual smoke, docs, review, scoped commit. Update task status only from observed outcomes.
4. Keep the [September 29 plan](2026-09-29-hardening.md) historical. This plan closes remaining defects; it does not reopen every prior work package.

### Work-package documents

| Plan | Tasks | Responsibility |
|---|---|---|
| [Security and transaction boundaries](2026-10-06-security-remediation.md) | S1–S6 | Guarded S3 egress, Member revocation, rotation, name claims, target-site admission, bounded B2 work. |
| [Database recovery and status](2026-10-06-recovery-remediation.md) | R1–R4 | Valid backup evidence, durable archive identity, operator fencing, measured recovery/protection contracts. |
| [Hosts and macOS](2026-10-06-host-remediation.md) | H1–H6 | Required firewall, recoverable reload, interrupted install, host policy ownership, architecture selection, native mode/seed behavior. H6 has separately reviewable H6a/H6b steps. |
| [Console and catalog](2026-10-06-console-catalog-remediation.md) | U1–U5 | Generic recipe translation, certification ownership, freshness, clipboard, accessibility, truthful protection and positioning. |
| This document | A1–A3 | CI, immutable releases/upgrades, real-stack acceptance and restore evidence. |

The complete scope is 20 concrete findings (F01–F20), nine contract/assurance gaps (G01–G09), and 24 top-level implementation tasks. The ledger separates implemented source from full acceptance; an unchecked external gate is not silently waived.

## Decisions and non-goals

- **Safety before breadth.** Fix the high-priority backup, egress, identity, concurrency, firewall and reload defects before increasing the advertised supported surface.
- **No automatic failover claim.** R3 retains explicit operator-fenced force and records its limitations. An independently enforced lease/quorum/fencing backend requires a separate design, not an unreviewed addition here.
- **No silent data migration.** Existing archive namespace names and retained objects stay intact. The R2 migration records identity before new-format deployments resume.
- **No fake protection.** R4/U5 distinguish running, database prerequisites, local files, measured recovery confidence and actually verified restores. Unknown is a real state, not a successful fallback.
- **No new file-backup engine in this plan.** Local-only files are labelled honestly; A3 verifies them when a recipe claims a supported restoration path. Enabling general replicated file recovery is a separate product scope, not hidden inside a status fix.
- **No framework rewrite.** Keep the embedded Console and central escaping; bounded module extraction is allowed after behavior is corrected.
- **No automatic certification of imports.** Upstream validation, successful import, and WeCoLab-tested lifecycle evidence are separate facts.
- **No provider swap.** Garage is relevant research, but missing Object Lock APIs prevent treating it as an equivalent immutable vault. Verify the existing provider contract first.
- **No source-text test expansion.** Tests must catch behavior, boundaries, state transitions, errors or concurrency. Remove incidental wording/implementation assertions rather than pinning new strings. Browser paths need an actual served surface, not only snapshots of generated HTML.
- **No new public release during plan execution without authorization.** Prepare/build/verify artifacts locally and in CI; publishing and operating on production remain explicit operator actions.

## Phase order and ownership

### Phase 1 — Close immediate security and host safety holes

- [ ] **S1** — socket-time S3 destination restriction.
- [ ] **S2** — terminating Member authorization rejection.
- [ ] **S6** — bounded native B2 requests.
- [ ] **H1** — firewall success required before either k3s role.
- [ ] **H2** — failed Nebula apply/reload remains retryable.
- [ ] **R1** — classify completed backup metadata correctly, after S1.
- [ ] Begin **A1** with honest Go/shell/native CI reporting; native failure is not waived.

**Exit:** Known FAILED metadata does not qualify as proof; a malicious redirect cannot reach a denied network destination; terminating Members lose authority despite cleanup failure; critical host configuration cannot be silently skipped. Unit/fixture checks plus disposable runtime checks must be recorded. A full recovery claim remains blocked until Phase 4.

### Phase 2 — Make mutations and identity durable

- [ ] **S3** — versioned vault rotation/reconciliation/retirement.
- [ ] **S4** — atomic public/mesh name reservations.
- [ ] **S5** — site-correct storage admission without relaxing tenant policy.
- [ ] **R2** — unique database archive identity and coordinated existing-app migration.
- [ ] **H3** — resume interrupted k3s installation safely.
- [ ] **H4** — preserve external AppArmor ownership.
- [ ] **H5** — select each manager's actual architecture.

**Exit:** Repeated/concurrent operations converge or fail clearly, rather than corrupting keys, names or archive history. A remote-only storage grant works at its destination. Upgrade and rollback boundaries are documented and exercised on copies before any real rollout.

### Phase 3 — Make recovery and operator information truthful

- [ ] **R3** — explicit operator-fenced force contract and bounded public wording.
- [ ] **R4** — measured recovery confidence, separate data scopes, defined restore evidence.
- [ ] **U1** — correct generic sidecar DB translation and establish lifecycle certification rules.
- [ ] **U2** — visible-panel freshness without lost edits or late-response overwrites.
- [ ] **U3** — truthful copy success and recoverable one-time-secret UI.
- [ ] **U4** — keyboard controls, authoritative recipe choices, correct modal lifecycle.
- [ ] **U5** — consume R4's protection contract and update Workspace/comparison claims.
- [ ] **H6a** — retryable, fail-closed native activity publication.
- [ ] **H6b** — diagnose the known seed failure and verify the NoCloud consumer/boot path.

**Exit:** Operator-visible state distinguishes observations from promises. Force requires its explicit acknowledgement; failed copy/reload/publication is not reported as success. A small supported recipe set has real lifecycle evidence, not inherited upstream badges. Complete A1's native gate after H6b.

### Phase 4 — Establish repeatable release and disaster-recovery evidence

- [ ] **A2** — immutable bootstrap, release manifest, advisory intake and staged upgrade workflow.
- [ ] **A3** — real CNPG/Barman/Forgejo restore, partition, provider-retention and migration scenarios.
- [ ] Finish **A1** integration jobs using A3's reproducible runner and sanitized evidence artifacts.

**Exit:** Every accepted fix has observed evidence and updated docs; release artifacts are attributable; successful recovery reads back actual data and states its file scope. No blanket “all data protected” or “one-minute maximum loss” wording remains unsupported.

### Parallelism without shared-file races

Parallel implementation is safe only after the source/contract prerequisites are fixed. One integration owner controls shared files and schema migrations.

- S1 and R1 both own `s3.go`: **S1 → R1**.
- S3/S4/S5 and R2/U1 all affect deployment/Git/secret behavior: apply those shared-file changes serially in that order, rebasing and rerunning relevant behavior checks at each integration boundary.
- R1/R2/R4 share `role.go`, `vault.go`, `siteagent.go`, API fields and recovery tests: **R1 → R2 → R4**. R3 also touches move/Console handlers and must integrate before final R4/U5 acceptance.
- H1–H4 share `install.sh` and `join_test.go`: one host owner executes them sequentially and regenerates `join.sh`; H5 and H6 can proceed independently after their own prerequisites.
- U1–U5 share the embedded page: one Console owner avoids simultaneous writes; **R4 → U5**. R3's dialog change integrates before U4's final modal review.
- A1 has a small early verification slice and a later integration-job slice. Do not wait for every bugfix to add the Go/shell gate, and do not claim the native/integration jobs pass before H6/A3.
- Security, host, and native slices with disjoint files may run concurrently. Shared docs and generated CRDs have the same integration owner; regeneration happens from the merged sources, not competing generated copies.

## Shared data and migration contracts

These are the implementation contracts. Git-only coordination paths were moved outside Flux's `fabric/` discovery tree during review; no Kubernetes manifest is excluded.

| Contract | Owner and rules |
|---|---|
| `AppSpec.ArchiveID` / JSON `archiveID` | R2. Immutable per database creation; migrate old apps explicitly to their existing `Spec.Database` namespace prefix; new IDs are random `id-<128-bit-hex>`. No missing-field fallback after migration. |
| Vault Secret `key-version`, `mutation-revision`, `retirement-phase`; per-site `VaultKeyVersion` | S3. Explicit version migration; a shared changed-vault CAS serializes app/rotation mutations against retirement. Old reports cannot authorize deletion or regress keys. |
| Name-claim files and completed-migration marker | S4. Changed within the same conditional Git operation as app mutations; claims remain until final app removal. Read-only Git paths are not CAS protected. |
| `coordination/placement-revision.json` | S5. Changed by all supported placement-authority and App placement mutations so concurrent grant changes force deployment reevaluation. |
| Force request `Fencing {ArchiveID, PreviousPrimary, ExpectedAppSHA, Method, Evidence}` | R3. A writer preview identifies the exact App revision and history-bearing source; validate inside the mutation. Operator assertion is not physical proof. |
| `VaultStatus.BackupEvidence` | R1. Proposed structured backup/history identity and valid completion evidence. A `LatestBackup` timestamp alone is insufficient for destructive decisions. |
| `/api/state` `app.Protection` | R4 producer; U5 consumer. Exact nested fields/states are defined in R4/U5; database/file scope and nullable measured exposure/restore timestamps must not be inferred by the browser. |
| Operator-recorded restore evidence | R4 reads, A3 records after a real drill. See the record format below; never substitute catalog certification or backup completion for per-app restore evidence. |
| Recipe certification data | U1. Separate upstream provenance from actual version/image/architecture/scenario evidence; never prepopulate unexercised success. |

**Migration sequencing:** Inventory and snapshot the current Git desired state and operator recovery material; stop affected mutations; deploy additive schemas; conditionally migrate durable records; roll every affected controller/Console; validate unchanged active archives and credentials; enforce new required schema; resume. S3 and S4 have their own crash-restart/migration gates. Do not run a new namespace algorithm on some sites while others still derive it from app names. Missing sites are a migration blocker, not permission to fabricate success.

## Task A1 — G04/G08: Gate the product, not only the website

**Files:** Create proposed `.github/workflows/verification.yml`; extend `Makefile` only for reusable real commands; retain `.github/workflows/website.yml`; update `docs/development.md`. Consume H5's proposed `hack/lab/reload_test.go`, U1's importer tests, and A3's future recovery runner. Inspect `go.mod`, `mac/Package.swift`, `website/package.json` and current tool pins before selecting runner toolchains.

- [ ] Add Linux Go verification with Go selected from `go.mod`, read-only workflow permissions, bounded timeouts, dependency caching keyed to the lockfile, and immutable action revisions. Run `go vet ./...`, `go test -race -count=1 ./...`, and compile Linux amd64/arm64 distribution artifacts. Do not skip tests under race instrumentation to manufacture green status.
- [ ] Check the installer copy **before** any command that repairs it. `make test` depends on `make join`, which can silently copy over drift; it is not by itself a non-mutating CI consistency gate. Run the following root commands with failure propagation:

```bash
cmp install.sh cmd/console/join.sh
bash -n install.sh
bash -n cmd/console/join.sh
bash -n hack/dev/fabric.sh
bash -n hack/dev/test.sh
bash -n hack/dev/snap.sh
bash -n hack/lab/reload.sh
bash -n hack/publish.sh
bash -n hack/kata-render.sh
go vet ./...
go test -race -count=1 ./...
make dist
```

Check newly added shell entry points individually as they land. Shell-stub behavior tests and real systemd acceptance remain necessary; syntax is not security verification.

- [ ] Add an ARM macOS job using a runner/Xcode version that supports `mac/Package.swift` and the documented deployment target. Run `swift test` from `mac/`; mount/inspect the NoCloud ISO in the native scenario. The observed G08 failure remains a blocker until diagnosed; no `continue-on-error`, blanket skip, or new byte-for-byte golden string. Full Virtualization.framework boot is a separately recorded supported-hardware gate if hosted CI cannot provide it, not an implicitly passing job.
- [ ] After U1, run its Python unittest fixture command with pinned/importer-compatible Jinja2 and PyYAML dependencies, then Go catalog tests. Preserve the recorded catalog upstream commit. Do not regenerate against a moving upstream branch during CI.
- [ ] After A3, add a trusted-branch/manual integration job on an isolated runner with adequate Docker disk and privilege for the existing `hack/dev` systemd containers. Never expose provider credentials or privileged self-hosted runners to untrusted pull requests. Separate credential-free emulated-provider checks from explicitly authorized real-provider retention checks.
- [ ] Exercise the workflow commands locally/in a disposable runner and deliberately introduce one temporary failing assertion/copy mismatch in a throwaway checkout to prove each gate fails. Remove the intentional failure before committing. Confirm a real run produces sanitized logs and exits nonzero for an unsuccessful scenario. Keep branch-required status checks aligned with jobs that can actually execute; do not install an always-green wrapper around inaccessible hardware.
- [ ] Update `docs/development.md` with the exact jobs, toolchain prerequisites, fixture/live distinction and native/integration limitations. Review workflow token permissions and trigger scope, then commit A1's early and final slices separately.

**Acceptance:** Go, shell, native and appropriate integration failures are visible and blocking under their declared release policy. The website workflow still validates docs at root and subpath. No green native/DR claim is based on an unavailable runner.

## Task A2 — G05: Publish attributable releases and explicit upgrade operations

**Files:** Modify `install.sh` and regenerate `cmd/console/join.sh`; inspect `internal/bootstrap/bootstrap.go`, `internal/bootstrap/image.go`, `cmd/warden/main.go`, `hack/publish.sh`, `mac/Plugins/` installer embedding, `README.md`, `docs/install.md`, `docs/operations.md`, `docs/development.md`, `docs/security.md`. Create proposed `hack/release.sh` and `.github/workflows/release.yml` only for artifact construction/verification; use the existing snapshot/privacy publish gate, not a second public-source path. Proposed release manifest is an output artifact, not a checked-in pile of binaries.

- [ ] Inventory every privileged fetched script/binary and container/operator pin, including the WeCoLab bootstrap and upstream k3s/NetBird installers. Record exact version, immutable source reference, digest/attestation expectation, update owner, and whether content executes as root. Do not falsely claim the existing checksums/pins are absent; close the mutable-root-bootstrap gap.
- [ ] Define one versioned release manifest containing source snapshot commit, artifact filename/platform/SHA-256, installer SHA-256, pinned upstream source identities, generated CRD/API compatibility version, and required migration identifiers. Build amd64/arm64 binaries and embedded installer from the same tree, verify byte consistency, and produce authenticated provenance tied to the expected repository/workflow identity. A checksum downloaded from an attacker-controlled replacement endpoint is not an independent authenticity guarantee.
- [ ] Make the documented bootstrap download an explicit immutable release artifact, verify authenticated provenance/digest **before** `sudo bash`, and refuse missing/mismatched artifacts or unrecognized release provenance. The initial trust anchor must be named (expected project/release signer or GitHub attestation identity) and verification tools/prerequisites documented. Do not pipe an unverified installer into root and claim its later self-check protects bootstrap. Development-source installation remains an explicitly labelled developer path, not silent fallback to `main`/`latest`.
- [ ] Pin privileged upstream scripts by exact immutable revision plus digest or verified upstream artifact; refuse a changed payload. Keep the existing supported platform/architecture matrix and legitimate offline-cache path. Add behavior tests/fixtures for a valid pinned artifact, wrong digest, missing manifest/platform, changed upstream payload, corrupt cached binary, and interrupted download; assert no execution on rejection, not merely that an error string appears.
- [ ] Encode R2/S3/S4 migration ordering in the release notes and upgrade preflight: verify recovery material and current backups, block incompatible mixed versions, perform required durable migrations, then run post-upgrade authorization, archive-name and restore smoke. Explain the exact downgrade boundary; never suggest rolling back an old binary that interprets a new ArchiveID differently.
- [ ] Add an operational security intake procedure to `docs/security.md`/`docs/development.md`: track upstream CNPG/k3s/Nebula/NetBird/Forgejo/Kata and certified recipe advisories, record affected pinned versions, designate an owner when publishing, test updates and notify operators. Reuse U1's recipe ownership data. Do not promise a response SLA without an assigned human owner.
- [ ] Build/verify a candidate release locally and in a nonpublishing CI run. In a disposable VM, install it, corrupt a copied artifact and verify refusal before execution, interrupt/retry the installer, then upgrade a pre-migration fabric copy and verify original data. Run A3's compatibility/restore checks for the advertised support matrix. Review provenance, secret/privacy gate and runbook, then commit scripts/workflow/docs. Publishing remains a separate authorized action.

**Acceptance:** Operators can identify exactly what runs as root and why it is trusted; failed verification prevents execution. Both architectures, offline inputs, native embed and generated schemas match the release. Upgrade notes describe mandatory migrations and tested recovery, not an optimistic “rerun installer.”

## Task A3 — G04 and cross-cutting closure: Prove data recovery and boundaries on the real stack

**Files:** Extend `hack/dev/test.sh` and `hack/dev/fabric.sh` where necessary; create proposed `hack/dev/recovery.sh` for named, resumable scenarios and evidence collection. Add real-stack fixture manifests beneath proposed `hack/dev/fixtures/recovery/` only when consumed by those scenarios. Extend `cmd/warden/main.go` for the proposed controlled restore-record command and `internal/warden/recovery.go` for evidence validation/storage as defined below. Update `docs/development.md`, `docs/operations.md` and U1's certification evidence only after outcomes are observed.

**Dependencies:** Relevant S/R/H/U implementations integrated; R4 defines the read model and validates the record below before U5 consumes it. A3 writes real records only after the drill. The existing dev fabric deliberately disables NetBird and uses development authentication; it cannot establish S2's production identity-provider behavior by itself.

### Scenario runner contract

- [ ] Make `hack/dev/recovery.sh <scenario>` accept the explicit names in the table below and refuse unknown names. Assert the intended disposable fabric/network before mutations; never discover an arbitrary production cluster from the user's current kubectl context. Use unique fixture app names and restore targets. Do not silently destroy existing `wcl-*` containers: stop and request an isolated test environment if they belong to another run.
- [ ] Record tool/component versions, inputs, selected app/archive/timeline/backup IDs, start/end, exact attempted operations, sanitized outputs, and outcome in a per-run artifact directory. Mask invitations and provider keys. A scenario exits nonzero on failed assertions or missing prerequisites; it cannot count “did not run” as passed.
- [ ] Use application-visible sentinels: insert numbered, committed database rows; write known file bytes to any supported persistent volume; read data through the app where possible and directly from restored Postgres for exact comparisons. HTTP 200 from nginx beside a database is not database recovery proof. Restore to an isolated target before deleting the original fixture data.

| Scenario argument | Steps and success criteria | Findings/tasks exercised |
|---|---|---|
| `failed-backup` | Interrupt a real Barman backup after metadata publication. Confirm failed status, no qualifying recovery proof/destructive rebuild, and a later eligible retry; repair provider access, complete a backup, restore and read sentinel rows. | F01, R1/S1 |
| `recreate` | Back up incarnation A; remove/recreate the same app name as B; write different rows; restore each independently and compare identities/data. Old immutable history remains; no archive collision. | F05, R2 |
| `partition` | Keep old primary writable while partitioned to expose the risk; unacknowledged force fails. Fence its write path, record explicit force evidence, promote target, read/write target data, and confirm old primary cannot write before controlled rebuild. | G01, R3 |
| `replay-lag` | Sustain writes, pause replay and archive separately, and observe measured/unknown exposure with correct history identity; resume, catch up, verify rows, then repeat idle/timeline-change cases. No healthy-flag shortcut. | G02, R4 |
| `rotation` | Send concurrent rotations from two Console processes; stop/restart the writer mid-operation; delay a site report; ensure all app keys converge to current ID/version and no live needed key is retired. | F04/F09, S3/S6 |
| `remote-storage` | Give a project storage only at the nonwriter site, deploy a real PVC workload there, verify Bound and data I/O; insufficient/revoked grant and exhausted destination admission remain visible failures. | F08, S5 |
| `name-race` | Concurrent cross-project deploy/rename/mesh claims to one name; exactly one accepted owner routes. Remove it and verify the next owner can claim only after committed release. | F10, S4 |
| `host-failure` | Use disposable systemd guests for failed firewall startup, identical-bundle reload retry, interrupted install and existing-AppArmor restoration; mesh access and original host state remain intact. | F06/F07/F12/F13, H1–H4 |
| `provider-contract` | In an explicitly authorized disposable provider bucket, verify the required Object Lock/versioning/retention semantics, signed listing/metadata reads and a real Barman restore. A key unable to remove a currently retained object gets the expected provider refusal. Do not delete or unlock retained objects as cleanup. | R1, G05, Garage compatibility warning |
| `recipe-lifecycle` | Deploy a bounded candidate, exercise main/sidecar DB and files, upgrade to a pinned candidate version, back up, restore to a separate target and verify rows/file checksums. Certify only the exact successful version/architecture/data scope. | F11/G03/G06, U1/U5 |
| `upgrade` | Start a pre-change fixture with real data and archive generations; run the declared staged migrations and release upgrade; retain names/credentials, then perform readback/recovery. Explicitly test interruption/restart and documented downgrade refusal. | S3/S4/R2/A2 |

Run production-authentication S2 separately with production mode enabled and a disposable identity provider/client fixture: delete a logged-in administrator, make NetBird cleanup fail, and confirm both ordinary and elevated requests are denied while cleanup remains pending. Browser fixture success alone does not prove this. Use H6's supported ARM Mac for actual seed boot and mode publication; use H5's architecture-specific smoke for reload. These are mandatory named evidence items even when not all can run inside the Docker fixture.

### Per-app restore evidence contract

- [ ] Define proposed `AppSpec.RestoreVerification` as a nullable durable record with `ArchiveID`, `BackupID`, `Timeline`, `CompletedAt`, `Actor`, `EvidenceSHA256`, and `Scope` (`database` or `database-and-files`). Add its actual API/CRD/deep-copy fields during R4's schema work and validate the record against the App's current identity and inspected backup. An old-incarnation/timeline record is historical only and does not populate current `Protection.LastVerifiedRestore`. Future timestamps, malformed hashes, absent backup identity or unrecognized scope are refused.
- [ ] Add a proposed `warden record-restore --namespace <ns> --app <name> --evidence <file>` command for an authorized operator after a successful drill. The evidence file must be the completed scenario runner result, include passed data-readback comparisons and matching identities, and exclude secrets. The command re-reads the current App/backup identity from the writer, checks permissions and the conditional Git precondition, computes the evidence file SHA-256 itself, and records the authenticated/explicit operator identity without trusting a forged arbitrary actor from the file. It does not accept a bare timestamp as recovery proof. Store the complete sanitized evidence artifact in the declared release/operations evidence location and its digest in Git.
- [ ] Mark this as **operator-recorded restore evidence**, not independent cryptographic attestation or a promise of future recoverability. An operator with authority to forge desired state remains trusted. Recipe certification does not fill this per-app field. R4 projects a current matching completed record; U5 displays the time and scope or “Never verified.” A record with `Scope=database` never turns local file volumes into protected files.
- [ ] Add behavior tests for recording a successful matching drill, failed/incomplete drill rejection, stale App identity/conflict, a different backup/timeline, absent authorization, attempted actor substitution, wrong artifact digest and DB-only scope. Exercise the real command against a disposable writer and confirm the API/UI projection changes only for the matching app.

### Running and retaining evidence

Prerequisites: dedicated disposable Docker fabric (the existing harness needs privileged systemd containers and roughly 15 GB free before additional restore data), supported ARM Mac for H6, and explicit approval/credentials for a real provider's disposable retention bucket. Lack of these prerequisites is a reported release-gate blocker; never weaken the test or borrow production credentials.

After adding the scenario runner, the implementation commands are:

```bash
set -euo pipefail
export WECOLAB_DEV_PREFIX="rec-$(openssl rand -hex 6)"
export WECOLAB_DEV_OWNER="operator-$(openssl rand -hex 16)"
: "${WECOLAB_DEV_SUBNET:?set an unused isolated /24 before starting}"
export WECOLAB_RECOVERY_ARTIFACTS="${TMPDIR:-/tmp}/wecolab-recovery-evidence"
trap 'hack/dev/fabric.sh down' EXIT

# Configure each scenario's declared credentials/hardware/old-version fixture first.
# Use a fresh owned fabric for each scenario; promotion can change the writer.
for scenario in failed-backup recreate partition replay-lag rotation remote-storage name-race recipe-lifecycle upgrade; do
  hack/dev/fabric.sh up
  hack/dev/recovery.sh "$scenario" || exit
  hack/dev/fabric.sh down
done

# The legacy integration suite uninstalls its fixture. Run it on a fresh fabric.
hack/dev/fabric.sh up
make dev-test
hack/dev/fabric.sh down
```

`make dev-test` includes uninstall near the end, so run it **after** the additional recovery scenarios. `host-failure` and `provider-contract` run only when their declared disposable systemd/provider prerequisites are configured. Review the artifact directory and remove only the run's own ephemeral resources; retained provider objects remain until their configured retention expires, with their cost/owner recorded. Never run a global Docker prune, wildcard bucket deletion, or unrelated VM purge.

- [ ] Execute the matrix, record actual successes/failures/skips separately, fix implementation failures and repeat only the relevant changed scenario. Add successful evidence to the release gate and catalog certification metadata where applicable; never backfill a pass from an expected result in this document.
- [ ] Update `docs/development.md` with runner commands, platform needs and failure injection; update `docs/operations.md` with exercised restore/upgrade/fencing steps and file-scope limitations. Review data-loss/security outcomes and commit the harness/docs/recording path.

**Acceptance:** An independent operator can reproduce the successful restoration from identified immutable inputs and verify actual application data. Failure injection demonstrates refusal/uncertainty instead of false protection. Every release claim names its environment, provider and data scope; unexplored combinations remain unverified.

## Coverage matrix

Each row has one accountable task and, where necessary, an integration/consumer gate. The audit remains historical; completion is tracked here and in task checkboxes with implementation commit and evidence references added when available.

| Review ID | Issue | Accountable task | Additional gate |
|---|---|---|---|
| F01 | Failed backup metadata qualifies | R1 | A3 `failed-backup` |
| F02 | Vault redirect/rebinding SSRF | S1 | Production-guard component smoke |
| F03 | Terminating Member remains authorized | S2 | Production-mode cleanup-outage scenario |
| F04 | Concurrent key rotation regresses credentials | S3 | A3 `rotation` |
| F05 | Same-name archive identity reuse | R2 | A3 `recreate`/`upgrade` |
| F06 | Firewall ordering fails open | H1 | Disposable real systemd failure |
| F07 | Nebula reload failure forgotten | H2 | Identical-bundle two-run scenario |
| F08 | Wrong-site PVC admission | S5 | A3 `remote-storage` |
| F09 | Unbounded B2 blocks writer | S6 | Blocked-response/next-tick smoke |
| F10 | Cross-project public/mesh collision | S4 | A3 `name-race` |
| F11 | Sidecar DB translation broken | U1 | A3 `recipe-lifecycle` |
| F12 | Interrupted k3s install cannot resume | H3 | A3 `host-failure` |
| F13 | External AppArmor policy ownership lost | H4 | A3 `host-failure` |
| F14 | Wrong-architecture lab image distribution | H5 | Mixed-architecture disposable smoke |
| F15 | Native mode write failure suppresses retry | H6a | Real disposable VM/share scenario |
| F16 | Secondary Console panels stale | U2 | Served browser polling/draft scenario |
| F17 | Failed clipboard copy reported successful | U3 | Clipboard rejection browser scenario |
| F18 | Essential controls inaccessible by keyboard | U4 | End-to-end keyboard/screen-reader smoke |
| F19 | Editable controls disagree with server policy | U4 | Catalog/custom deploy round trip |
| F20 | Missing modal focus/lifecycle semantics | U4 | Dialog keyboard/background/focus scenario |
| G01 | Force is not physical fencing | R3 | A3 `partition`; bounded public claims |
| G02 | Archive interval is not a loss guarantee | R4 | A3 `replay-lag` |
| G03 | Protected app hides local-only files | R4 | U5 and A3 file-scope readback |
| G04 | Only website has checked-in CI workflow | A1 | A3 real-stack release gate |
| G05 | Mutable root bootstrap/release trust | A2 | A3 `upgrade`/`provider-contract` |
| G06 | Catalog lacks end-to-end ownership proof | U1 | A3 certification/readback |
| G07 | Embedded Console needs bounded separation | U4 | Served asset/CSP/escaping smoke if split |
| G08 | Seed ISO test failed; cause unknown | H6b | A1 native gate and actual seed boot |
| G09 | Unbounded isolation/comparison wording | U5 | Rendered root/subpath website review |

### Research-to-action coverage

| Reviewed source | Adopted lesson | Implementation |
|---|---|---|
| Coolify backup alerts, volume scope and draft preservation | Visible backup/protection state and non-destructive polling | R4, U2, U5; no implicit new file-backup engine |
| Dokploy server-side identity/DNS/secret handling | Enforce authority and allocation at durable server boundaries | S2–S5 |
| CNPG failover/Lease fixes | Verify pinned upstream behavior on the real stack | R1–R4, A3; 1.30.1 already pinned |
| Incus boundary/security fixes | Treat host paths, migration and restore as security operations | H4, A2, A3 |
| YunoHost app advisories | Recipe maintainers and security intake | U1, A2 |
| OpenNebula mandatory migration workflow | Versioned preflight, migration and recovery playbooks | R2, A2, A3 |
| Garage compatibility limits | Test Object Lock/restore semantics, not an “S3-compatible” label | A3 `provider-contract` |
| Co-op Cloud maintainer workflow (evergreen) | Recipe ownership and tested upgrade compatibility | U1, A2, U5 |

## Final implementation handoff

A task is done only when all of the following are true:

- [ ] Its concrete issue is fixed at the original boundary, not hidden by a warning suppression or special-case input.
- [ ] All affected callers, API/CRD/generated copies, tests and user docs match the new contract; no stale compatibility bypass remains after a completed migration.
- [ ] Targeted checks and actual surface/command/scenario ran; evidence distinguishes fixtures from real upstream systems and lists any unavailable gate.
- [ ] Concurrent/crash/migration cases named in the task are covered, not merely the happy path.
- [ ] A reviewer has checked the result against the audit ID and acceptance criteria.
- [ ] The task has a scoped implementation change set and sanitized evidence reference recorded in this tracker. Commits are intentionally not created without user authorization; component checks cannot substitute for its remaining runtime acceptance.

**Recommended first implementation batch:** S1, S2, S6, and the sequential H1/H2 host slice, followed immediately by R1. Keep one integration owner for shared files and do not publish broader recovery claims until A3 has passed.
