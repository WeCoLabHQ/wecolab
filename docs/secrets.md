# Secrets

Every key in a fabric, who makes it, where it lives and who can read it. The rule behind the table: the
fabric generates every secret it can, where it is used, and a person types only what comes from
outside. Secrets reach commands through files, stdin and the environment, never a command line.
WeCoLab's own words (site, box, steward, writer, vault) are explained in the [glossary](glossary.md).

## What a person provides

| Secret | When | Stored |
|---|---|---|
| The zone and email | install prompt | plain, in the Fabric's settings |
| The owner's password | install prompt (`warden people`) | NetBird's identity provider (hashed); never in the Fabric |
| An object storage account key | Console, Settings | `secrets/storage.sops.yaml`; never shown back |
| An outside sign-in provider's client secret | *Not built yet* | |
| A vault's bucket and key, when not made on the Storage page (Create vault) | Console, first database deploy | `secrets/vault-<project>.sops.yaml` |

## What the fabric generates

| Secret | Made by | Lives | Readable by |
|---|---|---|---|
| **The recovery key** (age) | install | public half in `keys/recovery.age.pub`; private half on the recovery card only | whoever holds the card |
| **A site's age key** | that site's join | private half in the site's `flux-system/sops-age` Secret; public half in `keys/<site>.age.pub` | that site |
| **The Nebula CA key** | install | `secrets/nebula-ca.sops.yaml` | stewards, the recovery key |
| **A box's Nebula key** | that box | `/etc/nebula/host.key`, mode 0600; only the public key leaves the box | that box |
| **A box's Nebula certificate** | the writer's Console at join, then a steward's certificate service, from the registered public key | `/etc/nebula/host.crt`; checked hourly, renewed when a third of its life is left | public |
| **A site's k3s tokens** (the server token, and the agent token nodes join with) and **its Forgejo mirroring password** | the writer's Console when the site joins (install, for the first site) | `secrets/site-<site>.sops.yaml`; the server token also on the site's managers, the agent token on every box of the site | stewards, the recovery key |
| **A site's Forgejo token** (the fabric account, for Flux, Warden and the Console at that site) | that site's join or install | the site's `wecolab-system/git` Secret, and Flux's credential `flux-system/fabric-git`; never in the Fabric | that site |
| **Forgejo's secret key** | that site's join or install | the site's `wecolab-system/forgejo` Secret; never in the Fabric | that site |
| **NetBird's service token** (the Console's and Warden's access to the people mesh) | the people step (`warden people`), then the writer two months before it expires | `secrets/netbird.sops.yaml` | stewards, the recovery key |
| **What NetBird shows once** (its setup token, the first service token) | the people step | `/var/lib/wecolab/people.json` on the first box, mode 0600, until the step finishes; then only a mark that it is done | that box's root |
| **NetBird's own keys** (its auth secret, cookie and store encryption keys) | install | `secrets/netbird.sops.yaml`, in its `config.yaml` | stewards, the recovery key |
| **The Console's session key** | install | `secrets/console.sops.yaml` | stewards, the recovery key |
| **An app's secrets** (passwords, the vault key copy) | the Console, at deploy; a redeploy keeps them | `projects/<p>/<a>/secret-*.sops.yaml` | the app's sites, stewards, the recovery key |
| **A project's vault key** | the Console, from the object storage account key (Storage, Create vault) | `secrets/vault-<project>.sops.yaml` and a copy in each database app's secret | stewards, the app's sites |
| **Database passwords** | the Console, once per app, for an owner and a database named after the app; kept across redeploys | the `<app>-db-app` Secret in the app's folder, the same at every site (CloudNativePG uses it instead of making its own), and the database | the app's sites |
| **The Door's DNS-challenge credentials** | the Door's pod, when it starts | a volume inside the Door's pod | the Door's pod |
| **Box and site invites** | the Console | shown once, good for a day; only a hash is kept, in the writer's `wecolab-system/invites` Secret | nobody |
| **Pool and offer keys** | the Console | shown once, good for a day; only a hash is kept, in the Fabric | nobody |
| **A person's invite link** (a NetBird invite token) | the writer's Warden | the Member's status at the writer, until it is used; good for three days | admins (Members page), the writer's site |

NetBird's data (people, devices) lives at the site running it and is not backed up yet (docs/operations.md).

## Encryption in the Fabric

SOPS with age. Only `data` and `stringData` are encrypted; names and kinds stay readable so a diff shows
what changed. Each file is encrypted to:

- `secrets/`: every steward and the recovery key;
- `projects/<p>/<a>/`: the app's sites, every steward and the recovery key.

The Console names the recipients each time it encrypts a file, from the Sites and keys in Git as it read
them for that commit. When a steward is added or removed, the same commit re-encrypts every encrypted file
of the Fabric; a deploy encrypts the app's secrets to its sites as they are then. `.sops.yaml` is written
at install and not used after. Flux at each site decrypts with the site's own key while applying; nothing
decrypted is ever written back to Git.

## Rotation

| Key | How |
|---|---|
| NetBird's service token | the writer, two months before it expires: the new token is committed, the old one deleted a day after |
| The object storage account key | enter the new one in Settings; revoke the old one at the provider |
| A project's vault key | Storage → Rotate key commits a strictly increasing `key-version` and `mutation-revision` with the new key; each database app's encrypted Secret receives the current version. The writer repairs interrupted app rekeys from Git, including apps added during rotation. Retirement waits for each current Git database app Secret and a newly fetched report from every intended site with the exact current key ID **and** version. A durable `retirement-phase` claim pauses rotation and affected app mutations while the writer verifies each provider deletion; after a restart it checks/deletes remaining claimed keys again. Never delete the only live key or infer deletion from a timed-out provider response. |
| A box's Nebula key | remove the box in the Console and delete its Kubernetes node, run the install script with `uninstall` on it (what k3s held there, volumes and databases included, goes with it), and join it again with a new invite: the join makes a new key. A site's manager cannot be removed, so its key cannot be rotated yet |
| The Nebula CA | *Not built yet* (the CA lasts five years): a new CA is to join every box's bundle, boxes are re-signed at their next renewal, the old CA leaves a certificate lifetime later |
| A site's age key, Forgejo tokens, the Console's session key, NetBird's own keys, the recovery key | *Not built yet.* |

The one-time upgrade gate initializes legacy vault and app `key-version` to `0`, vault
`mutation-revision` to `0`, and explicit archive identities before reopening deployment.
Missing versions afterward are an error, not an invitation to assume version 0. A failed
rotation before the Git commit can leave a provider key without any Git reference: inventory
the account and revoke that orphan deliberately; never guess that an unreferenced key may
be deleted automatically. If the writer cannot finish a retirement phase, restore provider
availability, current app Secrets and fresh reports; do not clear the claim by hand or
revert to a binary that retires based only on key IDs. Vault calls have bounded deadlines:
a timeout is recorded as an error and retried on the writer's next tick, not success.

## When a key is lost

- **One site's age key**: that site cannot decrypt new commits. Re-keying a site is not built yet.
- **Every steward**: the fabric keeps running but nothing can change. Rebuilding from the recovery card is
  not built yet (docs/operations.md, "Rebuild from the recovery card").
- **The recovery card**: nothing is lost while a steward lives. Rotating the recovery key is not built yet.
