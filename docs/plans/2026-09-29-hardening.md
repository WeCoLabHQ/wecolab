# Hardening plan, 2026-09-29

The review of 2026-09-29 (eight reviewers, about 150 findings) found one critical bypass that is already
fixed (68aed30) and a long tail that comes from a few causes. This plan removes the causes, not only the
findings. Each rule below names the bug class it removes, the simplest mechanism, and where the idea
comes from (the research of the same day: Chick-fil-A's edge platform, Rancher Fleet, Flux multi-tenancy,
Deuxfleurs, Defined Networking/Tailscale/Talos/Omni, and the upstream sources of k3s, Forgejo,
CloudNativePG, NetBird, Nebula, Traefik, lego, Go and Kubernetes at the versions we pin).

## How the bugs came in

1. **v1's trust model survived the move to v2.** v1 had one trusted hub, so whatever the system published
   was believed. v2 made every site a peer, and some peers are friends trusted with capacity, not with the
   fabric. Carried-over code still believes a peer's `/status` (routing endpoints, writer claims, role
   hints, text in the Console page) and still adopts `spec.primary` when a site has no status, which was
   safe with one hub status and is not with one status per site.
2. **Structured formats built with strings.** Traefik config, the zone file, HTML, shell commands and Git
   paths are assembled with `fmt.Sprintf`/template literals from names, so every name is an injection.
3. **Two sources of truth.** The Fabric is Git, but the Console and the writer decide changes from the
   Flux-applied cluster copy, which lags Git by up to a minute, and `Git.Commit` re-reads each file's hash
   just before writing, which turns every write into last-writer-wins.
4. **An implicit distributed state machine.** Database moves are spread over spec, per-site status and
   peers, with no written invariants; destructive steps (drop, rebuild) act on local views.
5. **Privilege by convenience.** The Door reuses Warden's cluster-admin account; every node gets the k3s
   server token; the node agent may patch every node; the Console may impersonate any group.
6. **Host exposure by omission.** k3s listens on every address and VXLAN always binds the wildcard; we
   relied on ufw happening to be there.
7. **Identity without an inventory.** Join certificates are not recorded, IPs are reused, removal is a
   one-shot best effort, SSH keys and NetBird setup-key devices outlive their person.
8. **Stateful orchestration in bash.** Secrets pass through argv; a re-run can overwrite the site key.
9. **No contract tests between layers.** The page, install.sh, the Door and Warden each assumed shapes
   nobody checked, which is how v1's pages survived.

## The rules

**R1. Git decides; sites report only about themselves, and reports are checked against Git.**
(Fleet: a cluster's identity comes from its credential and it may write only its own status; CFA: route
on what you can verify, their 2025 post shows self-reported health hid failures; CloudNativePG's
distributed topology is declarative.)
- A peer's `/status` is fetched from that site's manager Nebula address (Nebula authenticates source
  addresses: `firewall.go` rejects a source outside the peer certificate's networks), so we know who
  said it. What it says is used only within that site's own scope and only after checking it against the
  Fabric: endpoints must be `ip:port` with the IP one of that site's boxes and the port in 30000-32767;
  writer claims count only from stewards, highest epoch wins; everything else is display-only text.
- **Database roles come from Git, not from consensus.** The App spec carries `primary` and, during a
  planned move, `handover {id, from, token}`. Each site derives its CloudNativePG replica settings from
  those fields alone (see WP-D). The writer's only duty is copying the old primary's demotion token into
  `handover.token` and clearing `handover` when the new primary reports it promoted with it. A forced move
  is `primary` changed with no handover and a new archive generation for **every** other site
  (CloudNativePG: "Failover ... requiring the former primary to be re-cloned"; a rebuilt cluster needs a
  fresh `serverName` because barman refuses a non-empty archive). `DecideRole`'s peer consensus goes.

**R2. Change the Fabric only against what Git says, conditionally.** (Forgejo per-file `sha`: 409
"sha does not match", multi-file commits atomic; Garage `--version`, Nomad `-check-index`.)
- One primitive, `fabric.Edit(ctx, who, msg, paths, fn)`: read the paths at the writer's HEAD with their
  blob hashes, let `fn` return changes, commit them conditioned on those hashes, retry `fn` on a conflict.
  Every Console mutation and every Warden commit uses it. The cluster copy is a view for pages and for
  sites to act on, never the basis of a change.
- Allocation happens inside `Edit`: a site's box addresses come from `Site.spec.nextBox` (never reused),
  site indexes from the Sites in Git, epochs from `system/base/fabric.yaml` in Git.

**R3. Names are data, never syntax.**
- One package, `internal/validate`, defines every name: labels (projects, sites, apps, hosts),
  DNS names (hostnames, domains), emails, and reserved names (`recovery`, system namespaces, `console`,
  `mesh`, `ns1`, ...). The same rules are CRD validation (markers and CEL), so nothing written to Git by
  any path can carry syntax.
- Every sink serialises structurally: Traefik config from typed structs with `yaml.Marshal` and explicit
  priorities (system routers always win; Traefik's default priority is the rule length); zone records only
  from validated names; the page renders through one escaping `html` tag and event delegation (no inline
  handlers with interpolated strings); secrets travel in files, stdin or environment, never argv.

**R4. Destroy only with positive proof from Git and from the survivor.** (CFA: "highly recoverable over
highly available"; CloudNativePG's empty-archive check.)
- A site deletes its database only when Git says it is not the primary and either the app no longer lists
  it or its archive generation changed, and, for a rebuild, the primary reports itself promoted and
  healthy with a base backup in its vault. The primary can never be removed from an app's sites (CEL:
  `self.primary in self.sites`). A primary whose database is missing is never re-created empty: its
  delivery stays suspended with a condition until a person moves the primary.

**R5. Each component holds only what it uses.** (k3s: the server token is "essentially full
administrator access", `agent-token` cannot fetch bootstrap data or CA keys; Talos workers get no CA
keys; Flux lockdown flags.)
- Nodes join with the k3s agent token; the server token stays on managers.
- One ServiceAccount per component, scoped: the Door reads Sites, Apps and the settings ConfigMap; the node
  agent may patch only its own Node (ValidatingAdmissionPolicy on the pod-bound token's
  `authentication.kubernetes.io/node-name`); the Console impersonates users only and reads Secrets only in
  its namespaces.
- Flux runs with `--no-cross-namespace-refs`, `--no-remote-bases` and `--default-service-account`, and the
  platform Kustomizations name their service account explicitly.

**R6. Only Nebula reaches WeCoLab's ports.** (k3s: "VXLAN port ... should not be exposed"; Deuxfleurs and
Talos: default-deny host firewalls.)
- k3s `bind-address` is the box's Nebula address. A small WeCoLab-owned INPUT chain accepts lo, nebula1
  and pod interfaces and drops 6443/tcp, 10250/tcp and 8472/udp from anywhere else, whether or not ufw is
  active, and is removed on uninstall.
- The certificate service moves to its own port (8094), open to any box; `/status` (8093) is open to
  managers only. The certificate service signs only for the box whose Nebula address the request comes
  from, only when due, and keeps a bounded record.
- The Door never proxies NetBird's `/api/setup` or `/api/instance`; its DNS-challenge endpoint requires
  basic auth from pod-local files (lego `HTTPREQ_USERNAME_FILE`/`HTTPREQ_PASSWORD_FILE`) and accepts only
  `_acme-challenge.` names in the zone.

**R7. Access is declared in Git and reconciled, never granted once.** (Deuxfleurs: node keys and admin
SSH keys in Git; OCM: deleting the ManagedCluster revokes at once; Nebula: the blocklist must reach every
host.)
- Revocation is a loop at the writer: every certificate fingerprint issued for a box that is no longer in
  the Fabric (join certificates recorded on the Box in Git, renewals published by stewards) is added to the
  blocklist in Git; expired entries are pruned.
- SSH keys reach boxes in the hourly Nebula sync and live in a marked block of `authorized_keys`, rewritten
  each time, so removing a person removes their key within the hour.
- People's devices join NetBird by signing in (peers owned by their user), not by setup keys (setup-key
  peers belong to nobody and survive the person). RBAC bindings are owned by their Member and go with it.
  The NetBird service token is rotated before its expiry.

**R8. Hosts converge idempotently; fabric operations live in Go.** (CFA's 2025 host agent: ordered
idempotent plugins that revert and alert; NixOS/Talos declarative hosts.)
- install.sh keeps host steps (packages, binaries, units, iptables), each check-before-act and recorded
  for uninstall. It never re-bootstraps a fabric that exists. The people step becomes `warden people`
  (Go: password read from the terminal, the setup token kept 0600 until the service token is in the
  Fabric, every step resumable). `warden upgrade` re-renders `system/` and `crds/` into the Fabric as one
  commit, which is what "an upgrade is a commit" needs.

**R9. Contracts are tested.**
- A Go test checks that every `fetch()` in the page names a registered route. A test checks install.sh
  reads only fields the join response has. Door config generation is tested with hostile names. The dev
  fabric test adds a forced move during a planned switchover and box removal revocation.

## Contracts (the foundation commit, before any work package)

These are fixed first so work packages can proceed in parallel without disagreeing.

1. `internal/validate`: `Label`, `DNSName`, `Email`, `Reserved`, with tests.
2. API (`api/v1alpha1`, CRDs regenerated):
   - `AppSpec.Handover *Handover {ID, From, Token}`; CEL `self.primary in self.sites`, handover.from in
     sites; patterns on sites, primary, hostname (DNS name, max 253).
   - `SiteSpec.NextBox int` (the next box number at the site; never reused).
   - `Box.Certs []IssuedCert {Fingerprint, NotAfter}` (join certificates, newest last, at most 8); patterns
     on `Box.Name`, `Box.IP` (ipv4), `Public.Address` (ipv4).
   - `Domain.spec.name` DNS-name pattern.
   - `AppSpec.Force` and `AppStatus.Switchover` are removed by WP-D (owner of app_types.go after this).
3. `fabric.Edit` with per-file hashes and retry on 409/422/non-fast-forward; `Commit` stays as a blind
   write built on it, for writes not based on a read.
4. Constants: `nebula.PortCerts = 8094`; Site secret keys `warden.KeyK3sServer = "k3s"`,
   `warden.KeyK3sAgent = "agent"`, `warden.KeyMirror = "mirror"`.
5. `warden.PlannedMove(spec, to, id)` and `warden.ForcedMove(spec, to)` (internal/warden/moves.go).
6. Join JSON: request gains `laptop bool`; response `k3sToken` is the agent token for nodes and the server
   token for managers, and managers also get `k3sAgentToken`; response and the certificate service's
   bundle carry `sshKeys` (the keys that box should have now).

## Work packages

Each runs in its own git worktree and branch, owns the files listed (edits elsewhere only where the
package says so), keeps `go build ./... && go vet ./... && go test ./...` green, and commits with a
message that says what and why. Deletion over addition; the ladder in the session's style rules applies.

**WP-A. Console surface** (cmd/console/auth.go, members.go, web/index.html; main.go only the route
table, `network` and `deviceKey`)
- `__Host-` cookie names; the mux wrapped in `http.NewCrossOriginProtection()` (rejects `same-site`
  requests from sibling app subdomains; lets curl's `POST /join` through).
- The page renders only through an escaping `html` tagged template and `data-*` + one delegated click
  handler; numbers coerced with `Number()`; one `api()` helper that checks `r.ok` and shows errors;
  `#inv-cmd` cleared on close and the dialog's lead text passed per use; default project from
  `ME.projects[0]`; Deploy re-rendered when state arrives; the offer site picker keeps its selection;
  Resources builds its whole header row; Takeover shown only when `isSteward`.
- v1 leftovers removed: the Mesh page shows people's devices and policies only, and "Add a device" explains
  signing in with the NetBird app (`deviceKey` and its endpoint go); the seed-backup card, `Reseed`, and
  v1 wording ("propagated", "Apply an App", "hub") go.
- A Go test: every `fetch()` method and path in index.html matches a registered route.

**WP-B. Door** (internal/warden/entrance.go and tests; internal/bootstrap/template/system/entrance/*,
system/people/*; cmd/warden/main.go only the entrance command's flags)
- Traefik dynamic config from typed structs and `yaml.Marshal`; explicit priorities: system routers
  (console, mesh) 1000000, tenant routers 1000; route and file names `<project>.<app>`.
- Every hostname checked with `validate.DNSName` before it is written; invalid routes skipped and logged.
- Endpoints accepted only as described in R1; the console route follows `Effective` (stewards only).
- Mesh names are `<app>-<project>.mesh.<zone>`; duplicates are skipped and logged.
- The DNS-challenge endpoint: basic auth from files written by the init container into a shared
  emptyDir, `HTTPREQ_USERNAME_FILE`/`HTTPREQ_PASSWORD_FILE` on Traefik, FQDN must be
  `_acme-challenge.<name>.` inside the zone, value must be 43 base64url characters.
- NetBird routes exclude `/api/setup` and `/api/instance`.
- The Door runs as `wecolab-door`, read-only (Sites, Apps, the settings ConfigMap), token mounted only in
  the Warden container; Traefik and CoreDNS non-root with `NET_BIND_SERVICE` only and RuntimeDefault
  seccomp; Traefik timeouts set; CoreDNS binds the default-route interface by name (written by the init
  container), so 1:1-NAT clouds work.

**WP-C. Console Fabric writes** (cmd/console/sites.go, settings.go, deploy.go, domains.go, storage.go,
fabric.go; main.go every handler except those WP-A owns)
- Every mutation through `fabric.Edit`, reading Git; no decision from the cluster copy. The `s.git == nil`
  branches go.
- Join: the invite is consumed only when the commit succeeded (restored otherwise); box number from
  `nextBox`; box names unique across the fabric and at most 63 characters; reserved and invalid names
  refused; `laptop` in the request must match the invite (a laptop never takes a site invite); nodes get
  the agent token, managers the server and agent tokens (a new site's secret gains `agent`); the join
  certificate's fingerprint is recorded in `Box.Certs`.
- Moves: `setPrimary` uses `PlannedMove`/`ForcedMove`; the target must be one of the app's sites.
- Deploy: merges onto the App and folder in Git (keeps `archive`, `handover`, existing generated
  secrets); sites must be in `warden.Allowed` for the project; hostnames validated; mesh names unique.
- Domains validated as DNS names; a domain verified for one project cannot be claimed by another.
- `takeover`: epoch from Git (highest known + 1), protection opened only after the commit succeeded.
- `setSteward` refuses to demote the writer; bad request bodies are 400s.
- `/api/settings` adds `isSteward`. `removeBox` blocklists the recorded certificates.

**WP-D. Database roles from Git** (internal/warden/role.go, apps.go, status.go, siteagent.go, peers.go,
projects.go, moves.go, a new handover.go; api/v1alpha1/app_types.go; cmd/warden/main.go only to start
the handover loop)
- `RoleAt(app, self)` replaces `DecideRole`. The CloudNativePG settings at a site:
  - no handover: every site `replica.primary = spec.primary`; the primary site needs no token.
  - handover without token: the `from` site `replica.primary = spec.primary` (it demotes; its Warden
    remembers the demotion token it already had, keyed by `handover.id`, and publishes only a new one);
    every other site `replica.primary = from`.
  - handover with token: every site `replica.primary = spec.primary`; the primary site also
    `promotionToken = token` (the token is not bound to a target: any replica at exactly that LSN may use
    it, so a retarget after demotion works).
- `handover.go` (runs at the writer): copies a fresh demotion token from the `from` site's report into Git
  with `fabric.Edit`; clears `handover` once the primary site reports `lastPromotionToken == token`.
- R4's guard as one function used by rebuild and drop, with table tests; a primary with a missing
  database is suspended with a condition, never re-created empty; the `-previous` bootstrap goes.
- Gates (StandbyStaged, WithinRPO) are computed for the move's target.
- `ensureBaseBackup` backs off (at most one attempt per hour while backups fail).
- Tenancy: best-effort placement restricted with a ResourceQuota scoped to the `wecolab-best-effort`
  PriorityClass (zero unless the project holds a best-effort offer at the site); the tenant 6443 egress
  rule scoped to the site's own box addresses.
- v1 leftovers: `plain()` overriders become JSON patches; "hub"/"Karmada" wording; `Switchover` and
  `Force` removed from the API.
- A simulation test: random sequences of spec changes (planned, retarget, cancel before token, force) and
  database events; invariants: never two sites whose settings allow promotion for the same history; no
  destroy without R4's proof; every move completes when all sites are up.

**WP-E. Identity and the writer** (internal/warden/certs.go, writer.go, people.go; internal/fabric/
forgejo.go; internal/nebula/*; internal/netbird/netbird.go; cmd/warden/main.go only the certificate
server)
- The certificate service on 8094 with server timeouts; POST `/nebula/<box>` only from that box's Nebula
  address; signs only when due (a blocklisted certificate is due); records at most 3 per box; the bundle
  gains `sshKeys` (admins and members of the site's owner project).
- Nebula firewall: 8094 from any box; 8093 from managers; 30300 from stewards.
- `Sign` refuses an expired CA and a key that does not match it.
- The revocation loop of R7 at the writer.
- `writer.go` reads writer and epoch from its own Git copy; re-checks `Head` before `Recreate`; never
  recreates when the writer's head is empty; non-steward copies recreate when they diverged from the
  effective writer; a site that is no longer a steward protects `main` for `mirror` only and deletes its
  push mirrors; `Effective` ignores a claim by a non-steward; `Recreate` treats a half-done state as
  missing.
- `people.go`: bindings carry an owner reference to their Member; NetBird client with timeouts; people's
  devices by sign-in only; the service token rotated before expiry; v1 helpers (`SetupKey`,
  `EnsurePolicy` for boxes, `DeletePolicy`) removed if unused.

**WP-F. Hosts and install** (install.sh and its copy cmd/console/join.sh; internal/bootstrap/;
internal/warden/kustomizations.go; template/system/base/*, system/wecolab/*, system/steward/console.yaml
RBAC only; hack/dev/*; cmd/warden/main.go only new `people` and `upgrade` commands)
- k3s: `agent-token` in the server config, agents join with it; `bind-address` = the Nebula address.
- The WECOLAB-HOST chain of R6 and pod isolation both ordered `Before=k3s`; pods may reach the box's
  local DNS resolver.
- `create` refuses to bootstrap again once the fabric exists (install.json without secrets); re-runs
  converge host steps only.
- `uninstall` cannot stop halfway (`|| true` where failure is fine).
- Secrets never in argv (git via `GIT_CONFIG_*` environment, curl headers from files or stdin).
- `warden people` replaces the bash people step; the box's own NetBird client is refused if not ours.
- `warden upgrade` renders `system/` and `crds/` for this fabric and commits the difference with
  `fabric.Edit`.
- Preflight: ports the Door and NetBird need are free.
- sops for both architectures fetched into `$STATE/bin` even when sops exists; Go, when needed, under
  `$STATE/go`, never `/usr/local/go`.
- SSH keys: a marked block in `authorized_keys`, written at join and by every sync.
- The sync script validates a response (`nebula -test` on a staging copy) before swapping files.
- Laptops register with the idle taint (NoSchedule and NoExecute) from the start.
- Node agent: ValidatingAdmissionPolicy for its own node only. Console RBAC: impersonate users only,
  Secrets through namespaced Roles. The userns policy covers init and ephemeral containers and
  container-level `runAsUser: 0`.
- Flux lockdown flags patched into the vendored manifest; platform Kustomizations (install.sh and
  kustomizations.go) name `kustomize-controller` as their service account; the `secrets` Kustomization
  has no post-build substitution.
- Join accepts `WECOLAB_LAPTOP=1` (sends `laptop: true`); a test checks install.sh reads only fields the
  join response has.

**WP-G. WeCoLab for Mac** (mac/ only)
- A guest-generation stamp; a v1 guest (or `share/netbird.json`) requires a reset, and the app has Reset.
- The join is a systemd oneshot with retries and a done marker, not a one-shot `runcmd`.
- Joins send `laptop: true` (`WECOLAB_LAPTOP=1`); the app says "Add a Mac" everywhere.
- Share files: no symlinks, regular files only, bounded reads, off the main actor.
- SIGTERM stops the VM cleanly; the invite is checked only when provisioning, and cleared by reset.
- Health probe on 8094.

**WP-H. After the merge** (docs/, dead code, the dev test)
- Docs follow the code (security.md, architecture.md, operations.md, install.md, decisions.md gains the
  rules above as decisions 14-22).
- The dev test adds a forced move during a planned switchover and box-removal revocation.

## Out of scope for now
- Short-lived certificates with renewal over the public Door (needed before certificates can be much
  shorter than 30 days, since expired boxes cannot reach a steward over Nebula).
- Several public sites serving one consistent zone (mesh records and DNS-challenge records at every Door).
- Image distribution without a registry beyond install.sh and the Console's `/dl/`.

## Review
Each work package is reviewed adversarially against its list and the rules before it is merged: every
item done or explicitly deferred with a reason, tests that fail if the rule breaks, no new string-built
config, no secret in argv, no decision from the cluster copy, no destructive action without R4's proof.
