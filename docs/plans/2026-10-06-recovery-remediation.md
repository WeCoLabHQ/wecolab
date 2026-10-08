# Recovery Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make database backup evidence and archive identity trustworthy, constrain forced promotion to an honest operator-fencing contract, and distinguish measured recovery confidence from app readiness.

**Architecture:** Keep CNPG/Barman responsible for PostgreSQL recovery, Git responsible for durable desired state, and per-site agents responsible for their own observations. Strengthen the evidence consumed by existing pure decision functions instead of introducing a new consensus service or backup engine. Preserve existing archive namespaces during a coordinated schema migration; use unique identities for newly created databases.

**Tech Stack:** Go, Forgejo conditional Git edits, Kubernetes CRDs/controller-runtime, CloudNativePG 1.30.1, Barman Cloud plugin, S3 Object Lock, PostgreSQL, the existing Docker development fabric.

---

[Master plan](2026-10-06-review-remediation.md) · [Historical audit](2026-10-05-codebase-review.md).

This plan maps **F01 → R1; F05 → R2; G01 → R3; G02/G03 → R4**. R1–R4 code, generated schemas and behavior tests are implemented in the working tree. The integrated race suite and standalone PostgreSQL replay/promotion smoke pass; these do **not** establish CNPG/Barman/Forgejo disaster recovery. The original checklists remain full acceptance requirements. The master [evidence ledger](2026-10-06-review-remediation.md#implementation-and-observed-evidence--2026-10-06) records unrun fabric/provider/upgrade gates. No successful restore record was fabricated.

## File map and shared contracts

| Files | Responsibility |
|---|---|
| `internal/warden/s3.go`, `vault.go`, `siteagent.go` | Retrieve, classify, bound, cache and publish recovery evidence. Security S1 owns the outgoing HTTP policy; R1 builds on it. |
| `internal/warden/apps.go`, `role.go`, `moves.go`, `handover.go`, `status.go` | Backup retry, immutable archive names, promotion/destruction decisions, condition computation. |
| `api/v1alpha1/app_types.go` | Durable app archive identity and move metadata; regenerate `api/v1alpha1/zz_generated.deepcopy.go` and `internal/bootstrap/template/crds/wecolab.io_apps.yaml` with `make crds`. |
| `cmd/console/deploy.go`, `main.go`, `storage.go` | Preserve identities on redeploy, enforce force-request contract, project truthful evidence into APIs. |
| `internal/bootstrap/bootstrap.go`, `cmd/warden/main.go` | Versioned upgrade ordering and explicit migration of existing App records. |
| Existing `internal/warden/{s3,role,moves,status,sim,siteagent,handover}_test.go`, `cmd/console/fabric_test.go`, `internal/bootstrap/bootstrap_test.go` | Behavior, boundaries, concurrency, migration and model tests. |
| Proposed `internal/warden/recovery.go`, `recovery_test.go` | Bounded same-history progress sampling and recovery-confidence calculation, only after R4's contract is implemented. No alternate routing/authority layer. |
| Proposed `cmd/console/protection_test.go` | API behavior for database/file/recovery-point/restore distinctions. |
| `hack/dev/test.sh`, proposed `hack/dev/recovery.sh` | Real operator/Barman/Postgres failure and data-restoration scenarios. Master A3 owns final integration of the scenario runner. |

During implementation use language-server references before changing exported symbols, and migrate all producers/consumers together. Do not rely on missing CRD fields surviving admission. S3's proposed `key-version`, `retirement-phase`, and `VaultKeyVersion` remain credential-rotation fields; R2's archive identity is independent. Sequence shared `deploy.go`, `role.go`, `siteagent.go`, `main.go` edits under one integration owner.

## Task R1 — F01: Require usable completed backup evidence

**Files:** Modify `internal/warden/s3.go:146–202`, `vault.go:27–55`, `siteagent.go:45–85`, `apps.go:230–245,429–465`, `status.go:143–159`; extend `s3_test.go`, `role_test.go`, `siteagent_test.go`, `status_test.go`. Update `docs/operations.md`, `docs/architecture.md`, `docs/console.md`.

**Dependency:** Security S1 first, so metadata downloads use the guarded S3 transport. Retain current request cancellation and the deliberate hourly retry limit for failed Object-Locked backup attempts.

- [ ] Add a failing inspection regression in `s3_test.go` using the real `InspectS3` parser and an `httptest` S3 fixture. Return an object listing containing `base/<backup-id>/backup.info` and recent WAL, then serve metadata with `status=FAILED`. Assert no qualifying backup and no positive destruction proof. Include a valid older completed backup beside a newer failed one to ensure “newest object” does not win. The following is a complete starting negative fixture; add the positive and error cases below before implementation:

```go
func TestInspectS3RejectsFailedBackup(t *testing.T) {
    now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
    srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch {
        case r.URL.Query().Has("object-lock"):
            fmt.Fprint(w, `<ObjectLockConfiguration><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
        case r.URL.Query().Get("list-type") == "2":
            fmt.Fprintf(w, `<ListBucketResult><Contents><Key>app/db/base/20261005T120000/backup.info</Key><LastModified>%s</LastModified></Contents></ListBucketResult>`, now.Format(time.RFC3339))
        case strings.HasSuffix(r.URL.Path, "/backup.info"):
            fmt.Fprintln(w, "status=FAILED")
        default:
            http.NotFound(w, r)
        }
    }))
    defer srv.Close()
    st := InspectS3(context.Background(), S3{Endpoint: srv.URL, HTTP: srv.Client()}, "vault", "app/db/")
    if !st.LatestBackup.IsZero() {
        t.Fatalf("failed backup qualified: %v", st.LatestBackup)
    }
    if !NeedsBackup(&st, nil, "db", now.Add(2*time.Hour)) {
        t.Fatal("failed metadata suppressed an eligible backup retry")
    }
}
```

The fixture requires `context`, `fmt`, `net/http/httptest` imports in addition to the file's existing imports. Its injected client supplies local TLS trust only; S1 independently tests the production socket policy. If the final parser returns an explicit inspection error for failed metadata, separate “valid inspection found no completed backup” from transport/parser failure so an ordinary failed attempt remains retryable.

- [ ] Run `go test ./internal/warden -run 'TestInspectS3RejectsFailedBackup|TestNeedsBackup|TestMayDestroy' -count=1`. Expect the new negative case to fail before the fix. Do not treat the historical reproduction as a need to repeat an uninstrumented whole review.
- [ ] Extend the existing signed S3 GET path to retrieve an exact listed object key without path traversal, double escaping, unsigned redirects, or ignored body-read errors. Parse the pinned Barman `backup.info` format as data, never evaluate it. Require `status=DONE`, valid backup identifier, timezone-aware completion time, database system identifier, timeline, and required WAL bounds. Cap individual metadata reads at 1 MiB and the complete inspection with its existing context deadline; truncated/oversized/unparseable responses are errors, not completed backups. Validate future or inconsistent timestamps rather than using an object's upload time as backup completion.
- [ ] Add a proposed structured `BackupEvidence` value to `VaultStatus` carrying archive name, backup ID, system identifier, timeline, start/end WAL bounds, completion time, and observation time. Keep `LatestBackup` as the actual completion time for existing storage presentation during the same cutover; every boolean recovery decision must consume validated evidence, not a nonzero timestamp alone. Do not cache evidence across archive identity or credential-version changes.
- [ ] Select the newest **qualifying** completed backup under the exact archive prefix. An unreadable listing/metadata candidate cannot silently become healthy; ordinary FAILED/STARTED candidates are rejected without suppressing retry when the rest of the inspection succeeded. Match the primary's system identifier and current history/timeline before `proven` allows destruction. Where the pinned CNPG status cannot establish that identity, report missing evidence and hold destruction rather than guessing. Use the validated native CNPG report or R4's read-only sampler; do not infer promotion history from file modification time.
- [ ] Extend table tests for DONE/FAILED/STARTED/missing status, malformed metadata, missing end WAL, truncated response, pagination, older DONE plus newer FAILED, a different archive/system/timeline, stale/absent observations, and a valid current-history backup. Preserve failed-attempt backoff and in-progress Backup behavior in `NeedsBackup`. Extend `TestMayDestroy` so a matching healthy primary with wrong/old backup evidence still refuses destruction. Record the distinction between a completed backup, required WAL availability, and a successfully exercised restore: metadata alone never proves all three.
- [ ] Run the targeted checks below, then smoke with real Barman/CNPG in the disposable fabric: force a backup failure after metadata publication, inspect its status, verify retry eligibility and blocked destructive rebuild, restore service, complete a backup, and verify the primary/standby can actually restore its data. Keep the original database until the successful restore is read back. A3 owns the reproducible combined harness and provider-retention gate.

```bash
go test ./internal/warden -run 'Test.*(InspectS3|NeedsBackup|MayDestroy|Vault|Compute)' -count=1
```

**Acceptance:** FAILED metadata cannot stop a due retry or satisfy a destruction gate. Success requires matching recovery evidence, not just a filename or recent timestamp. Transport/parser uncertainty holds destructive actions. Update the named docs, review the changed gates against data-loss scenarios, and commit R1 only after focused tests and component smoke pass; full recovery confidence remains gated by A3.

**Rollback:** Do not restore the filename-only proof. Preserve old databases/vault objects and report blocked recovery if evidence is unavailable. Do not delete Object-Locked artifacts to make a test or archive appear empty.

## Task R2 — F05: Give each database creation a durable archive identity

**Files:** Modify `api/v1alpha1/app_types.go`, generated deep copies/CRD, `cmd/console/deploy.go`, `internal/warden/role.go`, `siteagent.go`, `moves.go` only where identity preservation requires it, and the upgrade path in `internal/bootstrap/bootstrap.go` / `cmd/warden/main.go`. Tests: `role_test.go`, `moves_test.go`, `sim_test.go`, `siteagent_test.go`, `cmd/console/fabric_test.go`, `internal/bootstrap/bootstrap_test.go`. Docs: `docs/operations.md`, `docs/architecture.md`, `docs/development.md`, `docs/install.md`.

**Dependencies:** R1; coordinate after S3/S4/S5 when editing deployment/transaction contracts. Existing `Archive` generations retain their move/rebuild meaning.

- [ ] Add failing behavior tests for same-name delete/recreate, ordinary redeploy, two concurrent creates, move/rebuild, and migration of generation 1 and generation >1. Assert independently created databases get different Barman namespaces, whereas redeploy/migration retains the active namespace exactly.
- [ ] Add proposed immutable `AppSpec.ArchiveID string` (`json:"archiveID"`) for database-bearing apps. At first creation allocate `id-` plus 128 cryptographically random bits encoded as lowercase hex, inside the conditional creation transaction; retries retain the identity of the committed winner. Never use Kubernetes UID, process memory, a timestamp, or the human-readable app name as the new identity. A non-database app has no archive identity. The authoritative archive naming contract becomes:

```text
ArchiveID + "-" + site                         generation 0 or 1
ArchiveID + "-" + site + "-g" + generation     generation > 1
```

- [ ] Migrate **existing** database App records by writing `ArchiveID = Spec.Database`. Existing databases already archive as `<db>-<site>[-gN]`; this explicit one-time value preserves their namespaces without a permanent missing-field compatibility branch. Newly generated IDs begin `id-...` and do not use the legacy `...-db` suffix. Retain existing ObjectStore destination paths; archive identity distinguishes same-name incarnations below that path. Preserve legacy archive generations, handovers, credentials and database owner secrets.
- [ ] Implement the upgrade as a staged, restartable migration under a maintenance gate that temporarily refuses new deployments, moves, and schema-sensitive mutations. Inventory every existing database from the writer's Git tree, record the intended old/new namespace equality, apply an additive CRD, write all identities conditionally, upgrade the controllers/Console, and only then require the field for database apps and resume mutations. If any site cannot complete the version/schema gate, hold the upgrade; do not let old and new controllers create divergent namespaces. Confirm namespaces at each stage with the generated CNPG/Barman manifests and real archive listing. Do not set an identity only in the cluster view and lose it on the next Flux apply.
- [ ] Change `ArchiveName`, external source rendering, cache keys and any archive discovery to use the persisted identity. Refuse a database app missing an identity after migration; surface an actionable migration error. Ordinary redeploy, project secret rotation, planned move, and forced rebuild preserve it. Deleting the app leaves immutable history alone; its later recreation gets a different identity. Do not “fix” collision by deleting old backups or resetting generation state.
- [ ] Run `make crds`, targeted tests, and a disposable upgrade/recreate scenario. Start a pre-change database with a sentinel row, back it up, upgrade, verify unchanged archive names and row data, delete/recreate the same app name, write a different sentinel, then restore each incarnation into an isolated database and verify neither contains the other's new writes. The old archive remains discoverable only when explicitly selecting that old incarnation for recovery.

```bash
make crds
go test ./internal/warden -run 'Test.*(Archive|Role|Moves|Database|Simulation|VaultLook)' -count=1
go test ./cmd/console ./internal/bootstrap -count=1
```

**Acceptance:** Identity survives redeploy, restart, moves and rotation; delete/recreate never reuses it. All existing active namespaces remain byte-for-byte unchanged through migration. Every site runs a schema-aware version before new creation resumes. Update recovery instructions to name the incarnation, review the migration/rollback boundary, and commit R2 with generated artifacts.

**Rollback:** Before any new-identity database is created, a failed rollout may revert binaries only after verifying every manifest still addresses its original namespace and mutations remain stopped. After new identities are used, do not run an old binary that derives archives from app names. Repair forward with the database/archives intact; do not mass-rename provider objects or overwrite history.

## Task R3 — G01: Make force an explicit operator-fenced recovery operation

**Files:** Modify `cmd/console/main.go:574–630`, `cmd/console/web/index.html` failover dialog/action, `internal/warden/moves.go`, `sim_test.go`, `moves_test.go`, `cmd/console/fabric_test.go`; use durable App metadata in `api/v1alpha1/app_types.go` if the latest force record is projected to status. Update `README.md:5–11`, `docs/decisions.md`, `docs/operations.md`, `docs/architecture.md`, `docs/console.md`, and conflicting website recovery wording.

**Decision:** Retain manual force, not automatic quorum/lease failover. Require an explicit operator assertion that the prior primary's write path has been fenced and record the actor/evidence with the Git mutation. This is a procedural safety boundary, not a machine-verified guarantee. The documentation must say so. Merely hiding routes, assigning a higher Git epoch, or marking an old database condemned is not fencing.

- [ ] Add request-handler regressions: a database force request with no fencing confirmation is refused without Git changes; an acknowledgement for the wrong app incarnation/old primary or an out-of-date mutation is refused; unauthorized members cannot force; a planned move keeps its existing token contract; an app without a database does not pretend database fencing occurred.
- [ ] Define the proposed force request extension, alongside `To`/`Force`: `Fencing {ArchiveID, PreviousPrimary, ExpectedAppSHA, Method, Evidence}`. Add an authenticated, project-authorized `GET /api/apps/{ns}/{app}/force-preview` that reads the writer's Git App and returns its exact blob SHA, ArchiveID and history-bearing source (`Serving(spec)`, including a handover in flight). `Method` is `power-off` or `write-path-isolated`; `Evidence` is a bounded nonempty operator reference, never a credential. The dialog obtains this preview before confirmation. Inside the conditional edit, require the same SHA, ArchiveID and source; a changed revision returns 409 and requires a fresh preview/acknowledgement. This prevents replay after an A→B→A cycle, not just a different app name. Record a unique force-operation ID, authenticated actor, destination, time and evidence in the durable force metadata and audit commit. Update route registration and behavior tests; no arbitrary peer status supplies the authority.
- [ ] Update the dialog so force requires a separate explicit acknowledgement, identifies the source/destination, explains data-loss and local-file scope, and states that the software cannot verify an isolated host was powered off. Keep the standard planned handover as the preferred path. Update API callers and the development-fabric forced-move scenario; no hidden compatibility path that accepts an old bare `Force:true` request.
- [ ] Clarify simulation invariants: model physically writable nodes separately from nodes selected by the accepted Git history. Add a deterministic partition scenario demonstrating why condemnation alone does not stop writes, and a fenced scenario where the old primary cannot accept writes before the target promotes. Preserve the token/retarget tests; do not rename the existing invariant to imply a stronger proof than it supplies.
- [ ] Exercise the actual disposable two-site path. Continue writing a sentinel on the isolated old primary to demonstrate the risk before fencing. An unacknowledged force must fail. Stop or verifiably isolate its write path, issue the explicit force request, write on the replacement, and confirm the original cannot serve writes. Bring the former primary back only into the documented rebuild path with original volumes held until safe proof exists. Never run this experiment on production data.

```bash
go test ./cmd/console -run 'Test.*(Primary|Move|Force|Fenc)' -count=1
go test ./internal/warden -run 'Test.*(Move|Retarget|Simulation|Fenc)' -count=1
```

Name new tests to match the targeted filters and confirm they actually ran; a zero-test selection is not a pass. Update docs and public guarantees in the same commit, review the operator steps, and commit R3.

**Acceptance:** No database force proceeds via an unacknowledged HTTP request; stale confirmations cannot force a different history. Published claims distinguish planned handover and operator-fenced recovery. A deliberately false operator assertion is explicitly outside the guarantee; do not label G01 as automatic partition safety.

**Rollback:** Preserve the stricter API and audit record. Do not restore bare forced promotion as a backward-compatible fallback. If an autonomous failover guarantee is later required, obtain a separate design decision for an enforceable fence/authority backend before implementation.

## Task R4 — G02/G03: Expose measured recovery confidence and protection scope

**Files:** Modify `internal/warden/status.go`, `siteagent.go`, `role.go`, `vault.go`; create proposed `internal/warden/recovery.go` and `recovery_test.go`; update `cmd/console/main.go` app projection and `storage.go` evidence fields; add `cmd/console/protection_test.go`. For read-only telemetry, modify generated CNPG objects in `cmd/console/deploy.go` and `internal/warden/role.go` as needed, without creating a separate metrics backend. Tests: `status_test.go`, `siteagent_test.go`, `role_test.go`. Docs: `docs/operations.md`, `docs/architecture.md`, `docs/console.md`, `README.md` where recovery bounds are stated.

**Dependencies:** R1/R2 and S3's versioned credential/status contract. Publish the final API before U5 renders it. The first step removes incorrect certainty; task completion additionally requires the measured path and its real-stack smoke, not an always-unknown stub.

- [ ] Add failing condition tests: old WAL plus healthy CNPG and no write/replay measurement is **Unknown**, not `Idle=True`; recent archive time without replay evidence does not prove the target caught up; missing/expired/contradictory history identity is Unknown; a fully measured target that covers a recent primary sample can satisfy the declared objective. `MoveGates` may not silently convert Unknown to True. Keep “requested RPO” separate from observed facts.
- [ ] Remove the inference that archive age greater than RPO means “no newer writes.” Represent insufficient measurement using `metav1.ConditionUnknown` with a precise reason and observation age. `Ready` must not turn it into whole-app protection. Preserve a separate operational availability signal so “running” remains visible while recovery confidence is unknown.
- [ ] Add a bounded read-only measurement path using CNPG's existing built-in exporter and its supported custom SQL queries, rather than a privileged shell or new cluster-wide credential. Before generating the ConfigMap/monitoring reference, inspect the **pinned 1.30.1 CRD and exporter configuration** and check the generated Cluster with its real admission schema. [CNPG documents the custom-SQL exporter capability](https://cloudnative-pg.io/info/monitoring/); this plan does not assert that an unverified endpoint or metric name already exists in WeCoLab. Use the following PostgreSQL query contract, export the identifiers as labels and numeric components as gauges, and test permissions through the operator's monitoring role:

```sql
WITH position AS (
  SELECT pg_is_in_recovery() AS recovering,
         CASE WHEN pg_is_in_recovery()
              THEN pg_last_wal_replay_lsn()
              ELSE pg_current_wal_lsn()
         END AS lsn
), checkpoint AS (
  SELECT timeline_id FROM pg_control_checkpoint()
), identity AS (
  SELECT system_identifier FROM pg_control_system()
)
SELECT identity.system_identifier::text AS system_identifier,
       checkpoint.timeline_id::text AS timeline_id,
       CASE WHEN position.recovering THEN 1 ELSE 0 END AS recovering,
       floor(pg_wal_lsn_diff(position.lsn, '0/0') / 4294967296)::bigint AS lsn_high,
       mod(pg_wal_lsn_diff(position.lsn, '0/0'), 4294967296)::bigint AS lsn_low
FROM position, checkpoint, identity;
```

Each 32-bit LSN component is exactly representable in an exporter numeric sample; reconstruct into a checked 64-bit value before comparison. Missing/null results, denied monitoring permissions, changing timeline, wrong pod role, or unsupported exporter configuration remain explicit errors/Unknown. Do not grant a broad tenant superuser or accept a floating-point 64-bit LSN with precision loss.

- [ ] Record bounded primary-position samples and target replay samples tagged with database ArchiveID, system identifier, timeline, source site/pod role, and freshness. Retain a finite ring (64 samples at the existing 15-second controller cadence is sufficient for the initial path); clear it on identity/timeline/primary change and restart. A target must replay at least a sampled primary position from the **same history** before that sample supports a recovery bound. Reject cross-timeline numerical LSN comparison without proven ancestry. If a required older sample is absent, report Unknown rather than extrapolating.
- [ ] Compute a conservative exposure upper bound from the age of a covered primary sample, not WAL bytes divided by a guessed write rate. Use monotonic elapsed ages at the producing process plus bounded request duration/receipt age when consuming a site report, rather than subtracting arbitrary unsynchronized remote wall clocks. Publish wall-clock observation times for humans but do not use them alone as mathematical proof. Bound sampling/network work and reject expired reports; no fresh target report means no claim of current replay. Report `within-objective` only when this conservative bound is at most the configured objective. Report `outside-objective` only with positive measured failure of the defined replication-freshness target; otherwise use `unknown`, not an invented exact loss estimate.

Implementation clarification: an exposure **upper** bound above the objective proves neither success nor failure. `outside-objective` requires an older uncovered primary observation with a sufficient **lower** bound relative to the replica observation. Primary and replica report delivery ages are independent; local receipt age expires retained reports even when a cached report is polled repeatedly. Missing chronology, expired receipts or incomparable history remains `unknown`.

- [ ] Add the following **proposed** `Protection` field to each `/api/state` app, preserving that endpoint's existing uppercase property convention. All timestamp/number pointers can be null; missing evidence is never replaced with zero-as-success. R4 owns the producer and API tests; U5 owns rendering:

```json
{
  "Protection": {
    "Database": {
      "State": "unknown",
      "ObservedAt": null,
      "BackupCompletedAt": null,
      "Reason": "No validated current-history backup observation"
    },
    "Files": {
      "State": "local-only",
      "Reason": "Persistent volumes do not replicate or move between sites"
    },
    "RecoveryPoint": {
      "State": "unknown",
      "ObservedAt": null,
      "ExposureUpperBoundSeconds": null,
      "Reason": "No fresh comparable primary and replay samples"
    },
    "LastVerifiedRestore": null
  }
}
```

Database states: `protected|degraded|unknown|not-applicable`. Here `protected` means validated database recovery prerequisites, **not** whole-app protection or a restore drill. Files states: `local-only|unknown|not-applicable`; local-only when desired workload manifests contain persistent file mounts, unknown when the manifest/placement facts are unavailable, not-applicable only when there are none. RecoveryPoint states: `within-objective|outside-objective|unknown|not-applicable`. Keep no-database/no-files distinct from successfully protected data.

- [ ] Source `LastVerifiedRestore` only from an actual completed restore record for the same ArchiveID/history. A3 defines the durable evidence record and recorder. Until a record exists, null is the correct business state (“Never verified”), not the last backup timestamp or a hard-coded successful certification. Catalog certification of a recipe does not prove this particular app's data was restored.
- [ ] Add table/API cases for unknown/expired evidence, same-LSN idle database, sustained writes with replay caught up, replay deliberately stalled, timeline changes, wrong system identity, no database, no persistent files, DB plus local-only PVC, nil restoration record and a valid matching record. Keep byte boundaries around `2^32`/`2^53` exact, ring wrap safe, and receipt delay conservative. Check that source site freshness is not reset merely because a stale report was polled again.
- [ ] Run the targeted checks and a real PostgreSQL/replica smoke: write and read back numbered rows, allow replay, pause replay/archiving separately, then resume. Observe loss confidence becoming Unknown/degraded rather than falsely green; after valid samples catch up, observe the measured state recover. Include a truly idle database and a forced timeline change. Verify a DB+PVC app still reports local-only files while its database recovery prerequisites pass.

```bash
go test ./internal/warden -run 'Test.*(Compute|Gates|Recovery|Vault|SiteStatus)' -count=1
go test ./cmd/console -run 'TestProtection' -count=1
```

**Acceptance:** There is an exercised measured-success path and explicit uncertainty/error paths. The 60-second archive setting is never advertised as a maximum loss guarantee. API consumers can distinguish running, archived, replicated, local-only files, and actually restored. Update the affected docs, coordinate with U5, review the evidence arithmetic and record schema, and commit R4.

**Rollback:** Do not restore the old `Idle=True` inference. Disable untrusted measurements and show Unknown if the telemetry integration fails; preserve data and block only actions whose safety truly requires missing evidence. Do not change upstream versions just to avoid investigating the pinned contract.

## Recovery completion gate

- [ ] Run `go vet ./...` and `go test -count=1 ./...` once the integrated implementation stabilizes; regenerate/check CRDs and installer copy only if their sources changed. Do not count tests that match no cases or assert only source text as recovery evidence.
- [ ] Execute A3's real-stack failed-backup, same-name recreation, partition/fencing, delayed archive/replay and restoration scenarios; retain sanitized command output, component versions, original/restored row and file checksums, and outcome for each gate.
- [ ] Keep old data and immutable archives until the gates explicitly establish the intended replacement. A green model simulation, readable backup metadata, or a recent upload is not a substitute for reading restored application data.
- [ ] Update the master tracker only after task acceptance and associated runtime gates are observed. Record remaining external prerequisites explicitly; do not silently narrow a failed gate or mark all recovery work complete from unit tests alone.
