# A fresh fabric, start to finish

This is the whole path from nothing to two sites running a replicated app. Every step after the first
command happens in the Console or by pasting a one-time invite. WeCoLab's own words (fabric, site, box,
steward, writer, Door, vault, project, offer) are explained in the [glossary](glossary.md).

## What you need

- **A domain** whose DNS you can edit, for example at Cloudflare. The fabric gets a zone under it, such as
  `fab.example.org`, and answers that zone itself.
- **One box with a public address** for the first site: Ubuntu 24.04 or 26.04, or Debian 13 (Linux 6.3 or
  newer, for user namespaces), amd64 or arm64, 2 CPUs, 4 GB of memory, 40 GB of disk. A small VPS is enough.
  Its firewall must let these in: TCP 22, 53, 80 and 443; UDP 53, 3478 and 4242. Nothing else on it may listen
  on 80, 443, 8081, public 53, or UDP 3478 and 4242, nor run its own k3s, Nebula or NetBird client: the
  install checks and stops.
- **Home boxes** for more sites, same system requirements, with outbound internet. Nothing needs to be
  opened on a home router.
- **For workspaces** (desktops in their own VM), an amd64 box with KVM (`/dev/kvm`): a desktop or server
  with virtualization on in its firmware, or a VPS that offers nested virtualization. install.sh labels such
  a box, and Kata Containers is installed on it ([console.md](console.md#workspaces)).
- **An object storage account** for the vault. Backblaze B2 is the tested one; any S3-compatible store
  with Object Lock works (you then create buckets yourself).

## 1. Delegate the zone

At your DNS host, create two records pointing the fabric's zone at the public box. Here the zone is
`fab.example.org` and the box is `203.0.113.10`:

```text
ns1.fab.example.org.   A    203.0.113.10
fab.example.org.       NS   ns1.fab.example.org.
```

At Cloudflare these are two records with the proxy off. The fabric holds no credential for your DNS
host; you come back to it only to add name servers, for a second public site or a secondary (Optional,
below).

## 2. Install the first site

On the public box, first obtain the exact release tag and **40-hex source snapshot commit**
from the published release announcement. No release was published as part of this remediation:
these commands are usable only after an authorized release exists. Install `gh` (GitHub CLI),
`jq`, `shasum`, `git`, `curl` and `bash` in your unprivileged account. Download all seven
release payloads and the manifest to a directory that only you can write; the tag identifies
the download, while the expected commit and attestation establish trust independently:

```bash
RELEASE_TAG='<announced-immutable-tag>'
SOURCE_COMMIT='<announced-40-hex-public-snapshot-commit>'
mkdir -m 700 -p "$HOME/wecolab-release"
gh release download "$RELEASE_TAG" -R wecolabhq/wecolab -D "$HOME/wecolab-release" \
  -p 'release.json' -p 'source.tar.gz' -p 'install.sh' -p 'VERSION' -p 'warden-*' -p 'console-*'
gh attestation verify "$HOME/wecolab-release/release.json" -R wecolabhq/wecolab \
  --signer-workflow wecolabhq/wecolab/.github/workflows/release.yml \
  --source-ref refs/heads/main --source-digest "$SOURCE_COMMIT" --deny-self-hosted-runners
git clone https://github.com/wecolabhq/wecolab.git "$HOME/wecolab-source"
git -C "$HOME/wecolab-source" checkout --detach "$SOURCE_COMMIT"
bash "$HOME/wecolab-source/hack/release.sh" verify "$HOME/wecolab-release" "$SOURCE_COMMIT"
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh"
```

The final command is forbidden if **any** verification fails. The manifest attestation
authenticates the expected repository, release workflow, main source ref and source commit;
the manifest SHA-256 values then authenticate every platform's binary and installer.
Downloading a checksum beside an untrusted installer alone would not prove its origin.
Keep this verified release directory to join more boxes and for `people`/`takeover`/`uninstall`.
The builder embeds the identical installer in the Console and native Mac source. Native
binary/VM release packaging and a production restore drill remain separate gates.
It asks for:

- **the zone**, `fab.example.org`;
- **your email**, the owner's sign-in and the address Let's Encrypt writes to;
- **the site's name**, the first label of the host name by default;
- **your project's name**, from your email by default;
- **your password**, twice, for signing in to the Console, near the end.

The site's and project's names are lowercase letters, digits and inner dashes, at most 32 characters, and
not one the fabric keeps for itself (`console`, `door`, `mesh`, `recovery`, ...).

For later first-site commands use that verified local installer (and its verified binaries):

```bash
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh" people
```

Replace `people` with `uninstall` or `takeover` only when the corresponding operation is
intended. Never pipe `/join.sh` or a moving `main` branch into root.

Without a terminal (from automation, or `ssh` without `-t`):

1. Set `WECOLAB_ZONE`, `WECOLAB_EMAIL`, `WECOLAB_SITE` and `WECOLAB_PROJECT`. With `WECOLAB_PASSWORD` set
   too, the install does everything.
2. Without the password, it does everything except your password and the people mesh (NetBird, which
   phones and laptops use to reach the fabric), and says so.
3. Afterwards, in a terminal on the box, run the script with `people`, as above. It runs `warden people`.
   If it is interrupted, run it again: it continues where it stopped.

Then, in about ten minutes, it:

1. waits for the zone's DNS delegation;
2. installs digest-pinned Nebula and SOPS artifacts and uses the release's preverified
   Warden/Console binaries (`WECOLAB_SRC` builds are an explicit developer-only path);
3. makes this box's Nebula key, then, in one step (`warden bootstrap`), the fabric's Nebula certificate
   authority, this site's age key, the fabric's recovery key and every generated secret, and writes the
   Fabric's first state from WeCoLab's template with every secret encrypted;
4. starts Nebula, with this box as the first lighthouse and relay;
5. sets up WeCoLab's host firewall and installs k3s over Nebula, listening only on the box's Nebula
   address, with Pod Security enforced and user namespaces required for projects;
6. starts Forgejo and pushes that first state to it as the Fabric's first commit;
7. starts Flux on it; from here the Fabric brings up CloudNativePG, Warden, the Console, the Door, Names
   and NetBird;
8. runs the people step (`warden people`): creates you as the owner in NetBird's identity provider, makes
   the fabric's NetBird service token and commits it encrypted to the Fabric, and puts this box on the
   people mesh as the Door;
9. prints the Console's address and where the recovery card is.

The recovery card holds the recovery key's private half. With it and any copy of the Fabric, the whole
fabric can be rebuilt. It is written once to `/root/wecolab-recovery-card.txt`, readable only by root, and
never printed. Copy it somewhere offline, then delete the file with
`shred -u /root/wecolab-recovery-card.txt`.

If the install stops partway, run it again. Once the box belongs to a fabric, running the script with no
argument only re-applies the box's own setup (packages, Nebula, the certificate sync, the firewall),
fixing anything missing. It never makes a fabric again or changes the fabric's keys, tokens or
certificates. The script with `uninstall` removes WeCoLab from the box (operations.md).

## 3. Sign in

Open `https://console.fab.example.org` and sign in with your email and password. In **Settings**, add the
object storage account key: for B2, an application key allowed to create buckets and keys. The Console
stores it encrypted in the Fabric and never shows it again.

Then open **Storage** and choose **Create vault** next to your project. It makes a bucket with Object
Lock (30 days, compliance mode) and a key that reaches only that bucket. Do it once per project, before
its first app with a database. Without an account key, give a bucket, endpoint and key by hand in the
Deploy form instead, once per project.

## 4. Add a second site

A site belongs to a project, its owner. For a collaborator's site, first create their project and invite them
into it on **Members** (step 6). Then open **Add a site** (sidebar, under Operate) and give:

- its **name**;
- its **owner**, the project;
- **Steward**, whether it is a steward. A steward can decrypt every secret of the fabric, including every
  site's k3s tokens and the Nebula CA key that admits new boxes, and it can become the writer. Tick it
  only for a site whose owner you trust with all of that;
- **Public**, only if its first box has a public address (Optional, below).

The Console shows a one-time invite (`wcl2.…`). Copy **only the invite token**, never its
suggested unverified `curl …/join.sh | sudo bash` command. On the home box repeat the
download and attestation checks above for the **same source commit and release**; then run:

```bash
sudo WECOLAB_BIN="$HOME/wecolab-release" bash "$HOME/wecolab-release/install.sh" 'wcl2.…'
```

It takes about five minutes: Nebula, k3s, Forgejo with a copy of the Fabric, Flux, and
everything else from the Fabric. The site appears as Ready in **Sites**.

A fabric keeps working through the loss of any one site only when at least two sites are stewards: make
your second site one.

A project places apps only at sites it owns or holds an offer at. If the new site belongs to a
collaborator's project, your apps can use it once they offer capacity to your project: in **Sites**,
under Offers, with enough storage (a database takes 20Gi, each volume 5Gi).

## 5. Add boxes to a site

In **Sites**, on the site's card, choose **Add a box** (the site's owners and admins see it). On another
Linux box at that site, run the command it shows. For a Mac, choose **Add a Mac** and paste the invite
into WeCoLab for Mac (mac/README.md); a Mac cannot join on a Linux box's invite, nor a Linux box on a
Mac's. Work runs on a Mac only while nobody is using it, and only apps placed there through a
best-effort offer; databases never run there. A box's name is `<site>-<host>`, unique across the
fabric: a second box with the same host name is refused.

## 6. Invite people

In **Members**, invite by email, as an admin or a member of projects. Within seconds the invite's link
appears under **Pending invites**. Choose **Show link** and send it to the person yourself: nothing is
emailed. The link sets their password once and expires after three days; a new one then takes its place
under Pending invites. Then they sign in to the Console.

To reach apps published on the mesh from a phone or laptop, they install NetBird, choose the self-hosted
server `https://mesh.fab.example.org` and sign in with the same account; the device is theirs and goes
when they do. SSH keys a person adds in the Console (**Add a site**, Your SSH keys) reach root on the
boxes of the sites their projects own within the hour (an admin's, every box).

## 7. Deploy

In **Deploy an app**, pick a catalog app or an image, its project, its sites and its primary. The primary
is the site the Door sends people to. For an app with a database, it holds the writable database, and
every other site the app names keeps a standby: a copy that follows the primary through the vault. An app
with a database needs its project's vault first (step 3).

The Console commits it; each site runs its part within a minute or two, and the app answers at
`<app>.fab.example.org` (or the hostname you gave). With **Publish on the mesh** it also answers people on
the mesh at `<app>-<project>.mesh.fab.example.org`. There is no mesh-only app yet.

Only the database is replicated and backed up. Files the app keeps on a volume (uploads, for most catalog
apps) stay at the site that wrote them.

## Check that it worked

- **Sites** shows every site Ready and every box Ready.
- An app with a database shows its vault's newest backup and WAL on the **Storage** page.
- **Change primary** on that app (Apps) moves it to the other site in a couple of minutes, and back.

## Optional

- **A second public site** keeps public names up when the first is gone (people's sign-in, NetBird, stays
  at the first). Create it on **Add a site** with **Public** ticked. Its box needs what the first box
  needed (What you need): the same firewall openings, and nothing else listening on those ports. The join
  detects the box's public IPv4 address; if it cannot, run the verified local installer with
  `sudo WECOLAB_BIN="$HOME/wecolab-release" WECOLAB_PUBLIC=<address> bash "$HOME/wecolab-release/install.sh" 'wcl2.…'`.
  The site becomes a second lighthouse, relay, Door and name server.

  Then add it at your DNS host. The zone numbers its name servers by their public addresses sorted as
  text, not by when they joined, so the new site may be `ns1` and the first box `ns2`. Ask the zone which
  is which, `dig +short A ns1.fab.example.org @203.0.113.10` and the same for `ns2`, and make your DNS
  host's records match:

  ```text
  ns1.fab.example.org.   A    <ns1's address>
  ns2.fab.example.org.   A    <ns2's address>
  fab.example.org.       NS   ns1.fab.example.org.
  fab.example.org.       NS   ns2.fab.example.org.
  ```
- **Secondary DNS** from a free service (Hurricane Electric, BuddyNS): see operations.md.
