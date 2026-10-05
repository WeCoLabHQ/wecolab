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
make test        # go vet, the join.sh copy, unit tests
make build       # bin/warden and bin/console for this machine
make dist        # Linux binaries of both for amd64 and arm64; install.sh turns them into images
make crds        # regenerate api/'s deep copies and internal/bootstrap/template/crds
cd mac && make test   # WeCoLab for Mac
```

The decisions that must never go wrong are pure functions with table tests: an app's role at a site and
its database patches (`RoleAt`, `DBPatch` in `internal/warden/role.go`), moves (`PlannedMove`,
`ForcedMove`), the gates a planned move needs (`MoveGates`), the writer's steps (`WriterStep`), the guard
before destroying a database (`MayDestroy`, `PlanAt`), the blocklist (`Revoke`), which certificate a box
gets (`internal/nebula`, `CertService.Bundle`), and the files an app's folder holds (`internal/fabric`).
`internal/warden/sim_test.go` plays random moves and database events (lost databases, sites away, sites on
an old copy of Git) through those functions and checks that no two sites may promote for one history, that
nothing is destroyed without proof, and that every move completes once all sites are up.

Contract tests hold the layers together: every `fetch` in the Console's page names a registered route
(`cmd/console/page_test.go`); install.sh reads only fields the join response has, and its firewall, SSH-key,
k3s and sync functions are run in bash against stubs (`cmd/console/join_test.go`); the Door's configuration
is generated from hostile names (`internal/warden/entrance_test.go`); the Console's handlers run against an
in-memory Forgejo that refuses stale hashes like the real one (`cmd/console/fabric_test.go`).

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
the binary. It is generated from a checkout of HomelabOS by `hack/catalog-import.py`, which puts WeCoLab's own
entries, `hack/catalog-own.json` (the Workspace), first. With jinja2 and pyyaml installed:

```bash
python3 hack/catalog-import.py /path/to/HomelabOS > cmd/console/web/catalog.json
```

It renders each role's `service.yml` and compose template with stubbed Ansible variables and translates the
result mechanically: the main container, a database when the compose has Postgres, sidecars, a volume per
data path, and what is host incompatible (`unsupported`, refused by the Console) or attention. Each entry's
`validated` and `limitations` come from HomelabOS's `docs/development/service-validation-results.json`. The
file records `source` (the HomelabOS repository and branch, which the script names, and the commit it read),
`generated` (when) and `failed` (roles the import could not render, with the error). The current file is
from commit `411f2c6` of branch `feat/service-batch`, generated 2026-09-28: 226 entries, one failed.

## A whole fabric on one laptop

`hack/dev` runs a fabric in Docker: three boxes as privileged Ubuntu 24.04 containers running systemd,
on a Docker network that stands in for the internet, plus S3-compatible object storage for the vault.

| Container | Plays |
|---|---|
| `wcl-pub` | the first site: public, steward, writer |
| `wcl-home` | a second site, a steward, behind "NAT" (it has no route in except through Nebula) |
| `wcl-mac` | a node of `home`, joined as a laptop |
| `wcl-vault` | object storage with Object Lock (SeaweedFS; bucket `wecolab-dev`, keys in `hack/dev/s3.json`) |

```bash
make dev-up      # build binaries, start the containers, install pub, join home and mac
make dev-test    # the end-to-end checks below; FROM=4 hack/dev/test.sh starts at step 4
make dev-down    # remove everything
hack/dev/fabric.sh reload       # rebuild Warden and the Console and restart them at every site
hack/dev/fabric.sh shell home   # a root shell on a box
hack/dev/fabric.sh console GET /api/state   # the Console's API as the owner, through the Door
```

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

The fabric needs about 15 GB free in Docker's disk.

`make dev-test` checks, in order:

1. every site and box is Ready and every site publishes its status over Nebula;
2. a database app deployed from the Console runs at `pub`, with a standby at `home`, and answers through
   the Door;
3. a planned switchover to `home` completes with the token handed over (while it is in flight `pub`
   replays its own archive and stays demoted at one token), the Door follows, and back again;
4. a forced move, made during a planned one, rebuilds the old primary's database from the vault under a new
   generation;
5. deleting the app removes it and its data at both sites;
6. every box renews its certificate before it expires;
7. `home` takes over as writer, commits, and `pub` follows; `pub` takes it back with `install.sh takeover`;
   then removing the laptop's box puts its certificate on the blocklist at every site;
8. `install.sh uninstall` leaves every box as it was before WeCoLab (`hack/dev/snap.sh` records each box
   when it starts; the fabric is gone afterwards, so run `make dev-up` again).

## Trying a fix on a real lab

`hack/lab/reload.sh` rebuilds Warden or the Console from this checkout under the version a lab already
runs and loads it on every site's manager, without a release:

```bash
LAB_WRITER=root@203.0.113.7 LAB_SITES="me@192.0.2.10 me@192.0.2.11" hack/lab/reload.sh warden console
```

The images keep their tag, so nothing else changes; a change to the Fabric's `system/` still needs
`warden upgrade` (run it in the writer's Warden pod: `kubectl -n wecolab-system exec deploy/wecolab-warden
-- /warden upgrade --version <tag>`). The next release replaces what the script loaded.

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

## Conventions

- Upstream tools do the work; our code decides and wires. Before adding code, check whether Kubernetes,
  Flux, CloudNativePG, Nebula or Forgejo already does it.
- Read the upstream documentation for the pinned version before implementing against a tool.
- Anything that holds state lives in the Fabric or at the site it belongs to, never only in a process.
- A failure is reported as a status someone can read, never only in a log.
