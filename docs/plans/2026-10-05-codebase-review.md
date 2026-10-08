# Codebase review and comparative research — 2026-10-05

**Status:** Historical review, documented on 2026-10-06. Findings are open unless the linked implementation tracker records completion with evidence. This document does not claim fixes have shipped.

**Implementation:** [Remediation plan and coverage matrix](2026-10-06-review-remediation.md).

**Relationship to earlier work:** This is a follow-up to [the September 29 hardening plan](2026-09-29-hardening.md), not a claim that its changes were never made. Preserve that plan as historical context. The findings below concern remaining behavior observed during the October 5 review.

## Verdict and scope

WeCoLab has a compelling purpose and substantial engineering behind it. The reviewed state is not yet a demonstrated basis for irreplaceable data or unconditional isolation and recovery guarantees. The most valuable differentiator is a small cooperative cloud with explicit capacity sharing and recoverable applications. The immediate work is assurance, not more catalog entries or another framework.

The review covered the Go controllers and Console; Fabric/Git mutation paths; Kubernetes templates; installer and embedded copy; catalog importer and generated catalog; macOS node; website; tests; and architectural, security, and operational documentation. Four focused reviews covered control-plane recovery, Console/security, provisioning/macOS, and web experience. Critical paths were independently examined and exercised in disposable component/browser scenarios.

This was a broad engineering review, not an exhaustive line-by-line security certification. No production host, real multi-site failover, live identity provider revocation, or full VM boot was exercised. The review changed no source files. Temporary smoke harnesses and local servers were removed afterward.

### Evidence vocabulary

- **Reproduced:** The actual function, script, or browser code exhibited the behavior in an isolated scenario. This is not proof of a full production exploit or disaster.
- **Static:** The relevant code and failure/interleaving were traced. The complete operational failure was not executed.
- **Contract risk:** The published promise is stronger than the available evidence or enforced mechanism; not necessarily an undocumented design choice.
- **High:** Security-boundary failure, recovery/data-integrity risk, or a valid core operation blocked.
- **Medium:** Material reliability, operational ownership, maintenance, or usability defect.

Paths and line references below describe the reviewed source; follow the named behavior if later edits move lines. Historical test results are not a fresh verification of a later checkout.

## The roast, grounded in code

> The disaster-recovery system treats the existence of `backup.info` as a certificate of competence. Failed backups can also fill out paperwork.

F01 is the supporting defect: unsuccessful Barman backup metadata can suppress retry and satisfy part of the recovery proof.

> The README promises no two writers. The simulation promises no two *uncondemned* writers. A partitioned database does not stop taking writes because the control plane has disowned it.

G01 distinguishes the documented manual-failover trade-off from physical fencing. The existing handover mechanism is valuable; forced promotion is a different guarantee.

> The checked-in GitHub Actions workflow protects the brochure, not the cloud.

G04 records that `.github/workflows/website.yml` is the only checked-in workflow at review time. Local Go and native tests exist; the criticism is absence of equivalent CI gates, not absence of tests.

> The catalog is a translation pipeline wearing an app-store badge.

F11 and G06 concern runtime translation and ongoing ownership. The documentation already distinguishes upstream validation from WeCoLab validation; preserve that honesty rather than claiming the project concealed the distinction.

## Findings register

### F01 — Failed backups can count as usable recovery evidence

**Priority:** High. **Evidence:** Component reproduction plus source tracing.

**Locations:** `internal/warden/s3.go:190–195` (`InspectS3`); `internal/warden/apps.go:230–245` (`proven`) and `429–431` (`NeedsBackup`).

`InspectS3` identifies a base backup by a key ending in `/backup.info` and uses its object modification time. It never reads the metadata status. Barman's uploader can publish `backup.info` marked `FAILED` in its failure/finalization path, not only for a successfully completed backup.

**Upstream evidence:** [Barman's cloud backup uploader](https://github.com/EnterpriseDB/barman/blob/REL_3_X_master/src/barman/cloud.py), reviewed on October 5, writes failed backup metadata during finalization. This URL names a moving upstream branch; R1 must check the exact deployed Barman version before implementing its parser.

The isolated smoke against the real inspection/gating code reported:

```text
FAILED backup metadata: considered backup=true
metadata reads=0
retry needed=false
```

This can suppress a needed backup retry and satisfy the backup portion of the proof used before destroying/rebuilding another database copy. The smoke did not destroy a database or prove actual data loss.

**Required outcome:** Only usable completed backup metadata for the intended database incarnation/archive and recovery history qualifies. Unreadable, incomplete, failed, malformed, or unrelated metadata must not authorize destruction. Keep bounded retry/backoff for failed backups.

**Task:** R1 in the [recovery plan](2026-10-06-recovery-remediation.md).

### F02 — Vault requests can bypass endpoint validation

**Priority:** High. **Evidence:** Redirect behavior reproduced; complete admission-to-exploitation chain and DNS rebinding statically traced.

**Locations:** `cmd/console/storage.go:167–180` (`vaultEndpoint`); `internal/warden/s3.go:41–55`; `internal/bootstrap/template/system/wecolab/warden.yaml:26`.

The Console resolves and validates an endpoint once. The later S3 client follows redirects and resolves destinations independently. Warden uses host networking. A project-controlled public endpoint can direct subsequent requests into a manager's private/local network; rebinding is another time-of-check/time-of-use gap.

The actual `InspectS3` component followed an HTTPS endpoint to a harmless loopback HTTP server twice, with no inspection error. The fixture supplied its own local TLS trust; this was not a production exploit or evidence that the Console directly accepts a loopback URL.

**Required outcome:** Enforce the destination policy at connection time, reject redirects/downgrades, and prevent proxy/resolver paths from bypassing it. Preserve only explicitly enabled development-fabric access.

**Task:** S1 in the [security plan](2026-10-06-security-remediation.md).

### F03 — Terminating Members can retain Console authority

**Priority:** High. **Evidence:** Static.

**Locations:** `cmd/console/auth.go:350–357`; `internal/warden/people.go:180–209`.

Session revalidation accepts an unblocked Member without rejecting `deletionTimestamp`. A finalizer can retain that Member while NetBird cleanup fails. An existing administrator cookie can therefore retain elevated authority during the cleanup outage, including access to privileged invitation operations.

**Required outcome:** A terminating Member is denied during session revalidation, login lookup, and authorization regardless of external cleanup progress. Keep the cleanup finalizer; do not trade remote credential cleanup for local revocation.

**Task:** S2 in the [security plan](2026-10-06-security-remediation.md).

### F04 — Concurrent vault rotations can regress application credentials

**Priority:** High. **Evidence:** Static concurrency trace.

**Location:** `cmd/console/storage.go:235–300`.

The vault Secret commit and application rekeying are separate operations. Rotation A can commit K1; B can commit K2 and rekey applications; A can then distribute its captured K1. The vault and applications disagree. Depending on retirement timing, a key remains valid indefinitely or an application receives a retired key.

**Required outcome:** A durable versioned rotation/reconciliation contract must reject stale snapshots and converge after concurrency, conflict, restart, or partial completion. Retirement requires current-version reports, not just completion of one HTTP handler.

**Task:** S3 in the [security plan](2026-10-06-security-remediation.md).

### F05 — App recreation reuses the previous database archive identity

**Priority:** High. **Evidence:** Identity collision reproduced; provider/recovery consequences static.

**Locations:** `internal/warden/role.go:169–177` (`ArchiveName`); `cmd/console/deploy.go:184–187`; `docs/operations.md:64–73`.

Deletion intentionally preserves vault objects. A new app with the same project/name uses the same destination and generation-one archive name. The isolated identity check produced `review-db-home` for both incarnations.

This risks the new database encountering a nonempty archive or recovery discovery selecting the previous database's history. No live restore from the wrong history was attempted.

**Required outcome:** Persist an immutable, non-reused database archive identity. Migrate existing databases without renaming their active Barman namespaces or deleting Object-Locked history. Ordinary redeploy preserves identity; delete/recreate allocates a new one.

**Task:** R2 in the [recovery plan](2026-10-06-recovery-remediation.md).

### F06 — Firewall failure does not prevent k3s startup

**Priority:** High. **Evidence:** Static systemd dependency analysis.

**Locations:** `install.sh:393,496–501`; identical embedded `cmd/console/join.sh`.

`Before=k3s.service` orders execution but does not require successful isolation setup. The k3s units do not require the firewall unit's success. A failed firewall initialization can be followed by workload startup.

**Required outcome:** Agent and server startup require successful security-boundary initialization, including after reboot. Test failure of the unit, not just the order of text in generated service files.

**Task:** H1 in the [host plan](2026-10-06-host-remediation.md).

### F07 — Failed Nebula reload is forgotten on an identical next sync

**Priority:** High. **Evidence:** Script reproduction.

**Location:** `install.sh:269–282`; identical embedded copy.

The sync writes new files before reloading Nebula. A failed reload leaves those files in place. The next identical bundle produces no change flag and skips the reload, leaving the daemon on old credentials/configuration/blocklists.

The isolated script smoke observed first run exit 1, second run exit 0, and only one attempted `reload nebula`.

**Required outcome:** Reload remains required until it succeeds; file equality alone is not evidence that the daemon applied the state. Invalid bundles must still never replace working configuration.

**Task:** H2 in the [host plan](2026-10-06-host-remediation.md).

### F08 — Writer-local quota blocks a valid remote deployment

**Priority:** High. **Evidence:** Static.

**Locations:** `cmd/console/deploy.go:315–325`; `internal/warden/projects.go:209–219`.

The Console dry-runs generated PVCs against the writer's Kubernetes API. A project granted persistent capacity only at another site has zero storage quota at the writer. A valid remote-target deployment can therefore fail admission against the wrong capacity boundary.

**Required outcome:** Retain security/schema validation and tenant authorization while evaluating resource admission at the intended site. Do not fix this by granting unrelated capacity at the writer or bypassing tenant constraints.

**Task:** S5 in the [security plan](2026-10-06-security-remediation.md).

### F09 — Native B2 requests can block writer coordination

**Priority:** Medium. **Evidence:** Static.

**Locations:** `internal/warden/vault.go:85–97`; serial writer work in `internal/warden/writer.go:105–149`.

Native B2 requests use the default HTTP client without an overall deadline. Synchronous key-retirement work can hold up subsequent writer coordination.

**Required outcome:** Bound the request and response lifetime with cancellation/deadlines. Report failure and allow subsequent reconciliation; do not introduce unbounded retries or swallow the error.

**Task:** S6 in the [security plan](2026-10-06-security-remediation.md).

### F10 — Public and mesh names are not atomically reserved

**Priority:** Medium. **Evidence:** Static concurrency trace.

**Locations:** `cmd/console/deploy.go:124–133,351–361`; `internal/warden/entrance.go:412–445`.

Uniqueness checks precede the conditional Git transaction. Concurrent deployments editing different application files can both pass, leaving duplicate public/mesh names that routing subsequently rejects.

**Required outcome:** Claim/release names atomically with the app mutation, with a shared conflict scope and ownership checks. Fail closed on collisions discovered during migration.

**Task:** S4 in the [security plan](2026-10-06-security-remediation.md).

### F11 — Catalog translation breaks Authentik's worker database configuration

**Priority:** Medium. **Evidence:** Static artifact/translation trace.

**Locations:** `cmd/console/web/catalog.json:761–774`; `hack/catalog-import.py:162–163`; `cmd/console/catalog.go:157`.

The generated worker receives `localhost` as PostgreSQL host, database name, and user after PostgreSQL is extracted into CNPG. Sidecar rendering does not receive the database reference used by the main container.

**Required outcome:** Correct generic main/sidecar database translation and test realistic compose fixtures; do not patch one app's generated JSON by hand. Regenerate from the recorded upstream snapshot and verify an actual app lifecycle.

**Task:** U1 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

### F12 — Interrupted k3s installation is mistaken for completion

**Priority:** Medium. **Evidence:** Static.

**Location:** `install.sh:412–421`.

With cached binaries, interruption after copying k3s but before upstream service installation leaves an executable. The next run treats `command -v k3s` as completion and skips missing service configuration.

**Required outcome:** Resume based on required service/configuration completion and preserve existing fabric identity. An executable alone is not an installation marker.

**Task:** H3 in the [host plan](2026-10-06-host-remediation.md).

### F13 — AppArmor ownership can erase pre-existing host policy

**Priority:** Medium. **Evidence:** Static.

**Location:** `install.sh:340–343`, with uninstall ownership handling.

Installation overwrites and marks ownership of an existing containerd AppArmor profile. Uninstall can remove/unload policy that was not originally WeCoLab's.

**Required outcome:** Track actual ownership and preserve/restore pre-existing content and activation state. Detect external edits instead of silently overwriting them during uninstall.

**Task:** H4 in the [host plan](2026-10-06-host-remediation.md).

### F14 — Lab reload distributes the wrong architecture

**Priority:** Medium. **Evidence:** Static.

**Location:** `hack/lab/reload.sh:51–56`.

Both architectures are built, but the distribution/import step selects the amd64 archive for every manager.

**Required outcome:** Select each manager's actual architecture; refuse unknown architectures. Cover mixed-architecture destinations, not only the writer's architecture.

**Task:** H5 in the [host plan](2026-10-06-host-remediation.md).

### F15 — Failed Mac activity publication suppresses retry

**Priority:** Medium. **Evidence:** Static.

**Location:** `mac/WeCoLabCore/NodeVM.swift:69–73`.

The native node caches its new activity mode before discarding an atomic-write failure. An unchanged next mode is then not republished. The guest may continue using stale activity state while the person is active.

**Required outcome:** Cache only successfully published state, retry failures even when the desired value is unchanged, and surface errors without claiming success.

**Task:** H6 in the [host plan](2026-10-06-host-remediation.md).

### F16 — Secondary Console panels stay stale while marked live

**Priority:** Medium. **Evidence:** Browser reproduction.

**Location:** `cmd/console/web/index.html:543–545,673–684`.

The periodic state request does not refresh domains and other separately loaded panels. In the browser fixture, 17 state requests occurred alongside one domains request while the surface continued to say “live.” DNS validation and pending invitations can appear stuck until navigation or mutation refreshes their panel.

**Required outcome:** Refresh the visible panel with its own freshness/error state, without destroying drafts, selections, or focus. Discard late responses that no longer apply.

**Task:** U2 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

### F17 — Clipboard failure falsely reports successful secret copying

**Priority:** Medium. **Evidence:** Browser reproduction.

**Location:** `cmd/console/web/index.html:525`.

The copy action does not await `navigator.clipboard.writeText()`. Permission rejection still reports “copied”; closing the dialog clears the once-displayed secret.

**Required outcome:** Acknowledge only a fulfilled copy operation. Preserve selectable text and clear manual-copy/error guidance when the Clipboard API fails or is unavailable.

**Task:** U3 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

### F18 — Essential deployment choices lack keyboard semantics

**Priority:** Medium. **Evidence:** Browser/source inspection.

**Location:** `cmd/console/web/index.html:400–402` and deployment rendering.

Clickable spans implement database, mesh, and target-site controls without native keyboard behavior.

**Required outcome:** Use labelled native controls with keyboard-operable state and visible focus. Verify the complete deployment flow without a pointer.

**Task:** U4 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

### F19 — Catalog options appear editable but the backend overrides them

**Priority:** Medium. **Evidence:** Static UI/API contract trace.

**Locations:** `cmd/console/web/index.html:625`; `cmd/console/deploy.go:70–75`.

Catalog deployment displays interactive database/mesh controls although the backend imposes the recipe's settings. The submitted/displayed choice can disagree with the durable deployment.

**Required outcome:** Show enforced recipe choices as enforced and explain why. Keep the server authoritative; do not permit unsafe catalog overrides merely to match the UI.

**Task:** U4 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

### F20 — Operational dialogs lack accessible modal behavior

**Priority:** Medium. **Evidence:** Browser/source inspection.

**Location:** `cmd/console/web/index.html:681–683`.

Dialogs do not establish an accessible modal role/name, initial focus, focus containment, inactive background, Escape behavior, or focus restoration.

**Required outcome:** Apply one correct dialog lifecycle to destructive confirmations and one-time-secret dialogs. Closing must not imply a successful operation; destructive work must remain explicit.

**Task:** U4 in the [Console/catalog plan](2026-10-06-console-catalog-remediation.md).

## Contract and assurance gaps

### G01 — Forced promotion is not physical fencing

**Evidence:** Contract risk. `README.md:5–11`, `internal/warden/role.go:48–81`, `internal/warden/sim_test.go:23–25`, and `docs/decisions.md:94–104`.

Sites act on their local Git view. A forced promotion cannot make a partitioned old primary stop accepting writes. The simulation excludes “condemned” databases from its invariant. The detailed decisions already acknowledge the two-site/third-vote trade-off and manual writer choice; it is inaccurate to describe that as a secretly implemented automatic consensus protocol.

**Outcome:** Distinguish planned token handover from force; require an explicit, recorded out-of-band fencing procedure and narrow the public guarantee. An operator assertion is not machine-verified fencing. Autonomous safe failover requires a separately chosen, enforced fencing/authority mechanism and is not smuggled into this hardening plan.

**Task:** R3.

### G02 — An archive interval is not a verified recovery-loss bound

**Evidence:** Contract risk. `internal/warden/status.go:113–140`, `cmd/console/deploy.go:208`, `docs/operations.md:43–49`.

`archive_timeout=60s` governs WAL segment switching, not upload/replay delay. Old archived WAL plus healthy conditions is insufficient to infer “no newer writes.” A healthy primary and replica also do not establish an end-to-end maximum loss interval.

**Outcome:** Separate the requested RPO, observed archive age, replay progress, and unknown exposure. Only fresh, same-history measurements can substantiate compliance. Missing telemetry must not become green. Do not convert bytes to seconds without a valid observation basis.

**Task:** R4.

### G03 — Database protection is not whole-application protection

**Evidence:** Contract risk. `docs/operations.md:31–49`; `internal/warden/status.go:173–180`; Console app cards.

Local persistent volumes are explicitly documented as neither replicated nor moved. An app-level “Protected” badge obscures that distinction. Backing up Postgres does not automatically preserve uploads, files, or workspace state.

**Outcome:** Display database, files/volumes, recovery-point confidence, and last verified restore separately. Until file recovery is implemented and tested, call persistent files local-only; do not invent a backup backend as a UI fix.

**Tasks:** R4 and U5.

### G04 — CI does not gate the system's core guarantees

**Evidence:** Repository workflow inventory. `.github/workflows/website.yml` is the only checked-in workflow at review time.

**Outcome:** Gate Go vet/tests, installer-copy consistency and shell behavior, native tests, and selected disposable real-stack scenarios. Keep unit/model tests; add integration evidence, not more source-text assertions.

**Tasks:** A1 and A3 in the master plan.

### G05 — Root installation needs an identifiable release contract

**Evidence:** Supply-chain design risk. `README.md:44–48`, `install.sh`, `hack/publish.sh`.

The public bootstrap command executes mutable `main` as root. Existing checksums, version pins, and the snapshot privacy/secret gate are strengths, but they do not make that bootstrap reference immutable. Upstream installer execution also needs a documented trust/update policy.

**Outcome:** Publish a versioned, immutable bootstrap/release manifest with authenticated checksums/provenance; document upstream dependencies, security intake, staged upgrade checks, and recovery. Do not silently fall back to `main` or “latest.”

**Task:** A2.

### G06 — Catalog ownership must extend past import

**Evidence:** Maintenance risk. `docs/development.md:80–96`; `docs/console.md` catalog limitations; F11.

The recorded snapshot contains 226 entries and one failed import. Neither successful import nor upstream validation is WeCoLab lifecycle certification.

**Outcome:** Separate provenance/import/upstream validation from WeCoLab-tested version/architecture and deploy–upgrade–backup–restore outcomes. Start with a bounded supported set, named ownership, advisory intake, and visible unsupported/not-tested states.

**Task:** U1.

### G07 — Console maintainability needs boundaries, not a framework replacement

**Evidence:** Maintainability recommendation associated with F16–F20.

**Outcome:** After the behavioral fixes, split the embedded document by stable responsibilities where useful, preserving the embed contract, escaping, CSP, and API. Keep the implementation simple; a React migration is not an acceptance criterion.

**Task:** U4.

### G08 — Native seed test failed during the review

**Evidence:** Observed test failure, cause unconfirmed.

`swift test` reported 14 passed and one failed: `seedIsANoCloudISO()` at `mac/Tests/CoreTests.swift:256`. The failure was an exact payload comparison on the reviewed macOS 27 arm64 host. It does not establish that VM boot is broken. Do not re-pin incidental serialization or assume a root cause without inspecting the seed as consumed by cloud-init.

**Outcome:** Diagnose the round trip; preserve semantic user-data correctness and a real seed/boot check. Resolve or explicitly explain platform prerequisites; do not skip the failing test to make CI green.

**Tasks:** H6 and A1.

### G09 — Public comparisons and isolation claims need bounded wording

**Evidence:** Product/documentation recommendation. `website/src/pages/index.astro`, `website/src/pages/comparisons.astro`; the Workspace claim that whatever runs in it stays in it.

**Outcome:** Describe Kata's isolation boundary and residual host/upstream trust without an absolute security promise. Position WeCoLab around cooperative capacity and demonstrated recovery, with dated sources and limitations rather than catalog size. Do not claim competitor equivalence or certification not exercised.

**Task:** U5.

## Comparative research: September 5–October 5, 2026

This is the review's original 30-day window. Documenting the review on October 6 does not shift the research dates or imply a new search. Perplexity was used for discovery through its public UI; primary release notes/advisories were checked independently.

[Perplexity research session](https://www.perplexity.ai/search/80b18e31-3e8f-477b-a5e4-3c58ce09c71b). It gave incorrect Dokploy versions and missed relevant Incus/YunoHost updates. The table below follows primary evidence instead. Search availability and model output are not themselves verification of release claims.

| Project and relevance | Verified in-window signal | Useful adaptation, not blind imitation |
|---|---|---|
| Coolify: self-hosted deployment UX | [v4.3.18, September 8](https://github.com/coollabsio/coolify/releases/tag/v4.3.18): missing-backup alerts, direct-to-S3 volume archives, preserving in-progress edits. [v4.3.22, September 18](https://github.com/coollabsio/coolify/releases/tag/v4.3.22): once-per-occurrence distributed scheduling. | Make backup freshness, volume scope, and reliable UI state first-class (R4, U2, U5). A scheduler's once-only job coordination is not site fencing. |
| Dokploy: self-hosted PaaS | [v0.30.6, September 8](https://github.com/Dokploy/dokploy/releases/tag/v0.30.6): server-side SSO enforcement, secret-provider, DNS, and query-correctness work. | Enforce invariants server-side and support the whole deployment lifecycle (S2–S5, U1). |
| CloudNativePG: database lifecycle | [1.30.1/1.29.3, September 23](https://cloudnative-pg.io/releases/cloudnative-pg-1-30.1-released/): failover correctness fixes; 1.30.1 gates startup on primary Lease ownership. | Exercise the real operator state machine, not only a model (R1–R4, A3). WeCoLab already pins 1.30.1; this review does not recommend an unnecessary version bump. A per-cluster Lease is not a cross-site fence. |
| Incus: infrastructure security boundaries | [7.5 announcement, September 25](https://linuxcontainers.org/incus/news/): security fixes involving authorization, migration, backups, host paths; cluster-member metrics. | Treat backup/restore/migration as security operations; maintain advisory intake and adversarial tests (S1, H4, A2, A3). Incus vulnerabilities are not automatically WeCoLab vulnerabilities. |
| YunoHost: maintained app ecosystem | [Lufi advisory, September 14](https://forum.yunohost.org/t/lufi-critical-vulnerability-fixed-in-0-07-4-ynh1/42998); [Conduit advisory, September 27](https://forum.yunohost.org/t/conduit-two-security-issues-fixed-with-0-10-14-ynh1/43080/1). | Adopt recipe ownership, update notices, and advisory response (U1, A2). These are application advisories, not new YunoHost platform releases. |
| OpenNebula: operational lifecycle | [7.4.1 notes, dated September 9](https://docs.opennebula.io/7.4/software/release_information/release_notes/resolved_issues_741/): explicit configuration migration, S3-compatible Restic backup configuration, operational fixes. | Version upgrade/runbook contracts and required migrations (R2, A2); do not make upgrades depend on undocumented operator knowledge. |
| Garage: cooperative distributed storage | [v2.4.0/v2.4.1, September 6/8](https://garagehq.deuxfleurs.fr/_releases.html). Its [S3 compatibility reference](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/) lists missing Object Lock APIs. | Relevant neighbor, not a drop-in immutable vault. Verify actual retention/restore capabilities before substituting a provider (A3). “S3-compatible” is not a compliance-retention guarantee. |

**Closest philosophical comparison: Co-op Cloud.** Its [maintainer upgrade workflow](https://docs.coopcloud.tech/maintainers/upgrade/) emphasizes compatibility, configuration versioning, and operator-facing release notes. This is evergreen guidance, not verified news from the window. Claimed September recipe releases from Perplexity could not be substantiated and are not used as evidence.

**Product direction:** Borrow deployment usability from Coolify/Dokploy and recipe stewardship from Co-op Cloud/YunoHost. Differentiate on cooperative capacity and demonstrated cross-site recovery. Do not replace the architecture with these projects or compete primarily on imported recipe count.

## Strengths to preserve

- Token-based planned handover, including refusal to retarget after token publication.
- Refusal to silently initialize an empty replacement primary when history should exist.
- Conditional Git edits and preservation of superseded/divergent history.
- Production startup refusing missing authentication; PKCE and purpose-bound signing; cross-origin protection and HTML escaping.
- One-time invitation handling and confirmations for destructive operations.
- Explicit capacity/placement policy, workload isolation work, and bounded/symlink-resistant native share handling.
- Pure decision functions and deterministic/table-driven tests, including simulated move sequences.
- Existing installer shell stubs and development fabric; extend rather than replace them.
- Operational documentation that acknowledges local-volume and trust limitations.
- Website documentation rendered from source with working built-site search/deep links.
- Snapshot publishing with privacy/secret checks and explicit upstream version pins.

## Historical verification record

These are results of the October 5 review, not commands rerun when writing this document.

| Check | Observed outcome | Limit |
|---|---|---|
| `go vet ./...` | Passed | Static analysis, not a recovery proof. |
| `go test -count=1 ./...` | Passed | Unit/contract/model coverage does not establish the full fabric's behavior. |
| `swift test` in `mac/` | 14 passed, one failed: `seedIsANoCloudISO` | macOS 27 arm64; cause unconfirmed; no full VM boot. |
| Individual `bash -n` checks of installer/dev/lab/publish/Kata scripts | Passed | Syntax only. |
| `cmp install.sh cmd/console/join.sh` | Passed | Embedded installer matched the source. |
| `npm run build` in `website/` | Passed: 19 pages, 13 pages indexed by Pagefind | Does not test Console APIs. |
| Actual S3 client redirect smoke | Followed HTTPS to loopback HTTP twice | Local listener only, production chain not exploited. |
| Actual failed-backup inspection/gate smoke | Failed metadata counted without reading it; retry suppressed | No real database destruction. |
| Archive identity smoke | Delete/recreate identities collided | No provider restore performed. |
| Actual Nebula sync failure smoke | Reload not retried after an identical bundle | Stubbed service manager, not a production node. |
| Console browser fixtures | Stale secondary panel, false copy success, missing keyboard/dialog semantics observed | Disposable API fixtures, not a real fabric. |
| Website browser smoke | Desktop/mobile navigation, built search and documentation fragments worked | Initial dev-server image 404s were not production defects: built images returned HTTP 200. |
| Native CLI help | Launched and printed help | Not virtualization/guest integration. |

Do not report the development-server image misses as a defect: the production build generated and served the images correctly. Temporary harnesses were not retained as permanent tests; the implementation plans specify durable behavioral regressions and real-stack gates.

## Completion policy

Every F/G identifier maps to a task in the [implementation coverage matrix](2026-10-06-review-remediation.md#coverage-matrix). Closing a finding requires the intended behavior, relevant callsites/generated artifacts/docs, a regression where useful, and observed runtime evidence. Compilation alone, a mocked echo, a renamed warning, or softened wording without fixing a concrete defect does not close it.

The operational priority is: make “protected,” “revoked,” and “recoverable” mean something the system can substantiate.
