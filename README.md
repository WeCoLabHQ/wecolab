# WeCoLab

**Website:** https://wecolabhq.github.io/wecolab/

A cooperative cloud for every place you and your collaborators run machines: homelabs, offices, a small
business's server room, a rented box. Run an app at a site you own, keep a live copy at another site, keep
an immutable third copy nobody can delete, and when a site dies the other site's copy becomes the app,
without two writers ever existing.

WeCoLab starts working with two sites, keeps working when any one site is gone, and gets stronger with
every site that joins. There is no hub: every site holds the whole desired state and runs its own share.

It also gives people a desktop in the browser: a **Workspace** is a full Linux desktop that runs in its own
VM (Kata Containers) on any box with KVM, reached only from your people mesh
([docs/console.md](docs/console.md#workspaces)).

A *fabric* is one such cooperative: its sites, people, apps and keys. *The Fabric*, capitalised, is the
Git repository holding its desired state. These and every other word WeCoLab uses, with their
Kubernetes equivalents, are in [docs/glossary.md](docs/glossary.md).

![The Console's overview of a fabric of three sites, all ready, two of them stewards](docs/images/console-overview.png)

The Console, where every change is a commit to the Fabric ([docs/console.md](docs/console.md)). Names and
addresses in the pictures are examples.

## What runs where

| Piece | What it is | Where |
|---|---|---|
| **The Fabric** | a Git repository holding the whole desired state | a full copy at every site (Forgejo); one copy takes writes |
| **Flux** | applies the site's own copy of the Fabric | every site |
| **Warden** | this repository's controller: each site's role for each app, from Git; quotas, certificates, status | every site |
| **Console** | the web interface; every change it makes is a commit | every steward; the writer's is the one in use |
| **Nebula** | the mesh between boxes | every box |
| **The Door** | public entrance: TLS for app names, forwards to each app's primary site | public sites |
| **Names** | the fabric's own DNS zone | public sites |
| **NetBird** | the mesh for people's phones and laptops | the first public site |
| **CloudNativePG** | each app's PostgreSQL, primary at one site, standbys elsewhere | the operator at every site; databases at the sites of database apps |
| **The vault** | object storage under Object Lock: WAL archive and backups | outside the fabric (B2, S3, R2, ...) |
| **Kata Containers** | a VM of its own for each workspace (a desktop in the browser, by Selkies) | boxes with KVM, amd64 |

## Start

On a fresh Linux box with a public address, after delegating a DNS zone to it:

```bash
curl -fsSL https://raw.githubusercontent.com/wecolabhq/wecolab/main/install.sh | sudo bash
```

It asks for the zone, your email, the site's and your project's names and your password, then prints the
Console's address and where it wrote the recovery card (`/root/wecolab-recovery-card.txt`). Every other
box joins with a one-time invite from the Console.
The whole path is in [docs/install.md](docs/install.md).

## Documentation

- [docs/glossary.md](docs/glossary.md): every term, with its Kubernetes or standard equivalent
- [docs/architecture.md](docs/architecture.md): how it fits together, and why
- [docs/install.md](docs/install.md): a fresh fabric, start to finish
- [docs/console.md](docs/console.md): using the Console: deploy, move and delete apps, storage, domains, people
- [docs/secrets.md](docs/secrets.md): every key, who makes it, where it lives
- [docs/operations.md](docs/operations.md): switchover, failover, rebuilds, adding and removing boxes, the writer
- [docs/security.md](docs/security.md): what a workload can and cannot do to a box
- [docs/decisions.md](docs/decisions.md): the choices this is built on and when to revisit them
- [docs/development.md](docs/development.md): the code, tests, and a whole fabric on one laptop
- [mac/README.md](mac/README.md): WeCoLab for Mac, a Mac as a laptop node

## License

WeCoLab is free software under the [GNU Affero General Public License v3.0](LICENSE): you may use, study,
change and share it, and whoever runs a changed WeCoLab for others must offer them its source.
