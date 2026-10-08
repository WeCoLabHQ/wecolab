# WeCoLab for Mac

A Mac cannot run k3s, so this runs one small Debian VM through Virtualization.framework and, inside it,
the standard join (`install.sh`). The Mac becomes a laptop node of its owner's site, over Nebula.
Paste an invite (Console: Sites → your site → Add a Mac), set what the Mac contributes, press Start node.

```text
WeCoLabCore/      the VM logic, no UI
  Config.swift      config model, invite check, paths, lock, reading what the guest publishes
  Provision.swift   Debian image download + SHA512 check, sparse grow, NoCloud seed (user-data), reset
  NodeVM.swift      the VM actor: devices, start/stop, balloon + priority when the person is active
wecolab-node/     headless CLI on the same core
WeCoLab/          SwiftUI menu bar app and its main window
Plugins/          compiles ../install.sh into WeCoLabCore at build time
Tests/            config and its migration, invite, user-data and its join wrapper, seed ISO, guest version,
                  first start and reset, sparse grow, share reading, balloon target
```

## Build

```bash
make test     # unit tests (swift test)
make sign     # dist/WeCoLab.app and dist/wecolab-node, ad-hoc signed with WeCoLab.entitlements
make dmg      # dist/WeCoLab.dmg
```

Virtualization.framework refuses to run without the `com.apple.security.virtualization` entitlement.
An unsigned `swift build` binary fails with *"Invalid virtual machine configuration. The process doesn't
have the “com.apple.security.virtualization” entitlement."*; `make sign` fixes that. The signature is
ad-hoc, which is enough on the Mac that built it. A DMG for other people's Macs needs a Developer ID
signature and notarization, otherwise Gatekeeper blocks it.

## Use

WeCoLab is a menu bar app. Its icon, top right (the WeCoLab mark: a ring, a triangle, three sites; dimmed
while the node is stopped), opens a menu with the VM's state, the mode (you are `active` or `idle`), the
box's Nebula address (shown as Mesh; it is not the people mesh) and the join, then Start node / Stop node,
Open WeCoLab and Quit. Quit, or a SIGTERM (as `wecolab-node down` sends), shuts the guest down first.

The main window takes the invite and **what this Mac contributes** (CPUs, Memory, Disk). WeCoLab is in
the Dock only while that window is open; closing it leaves the node running. The window opens when WeCoLab
starts, unless WeCoLab starts the node itself (below).

The first start shows the Console the invite joins through and asks before going on: that Console gets
root in the VM, on your network. An invite whose Console is anything but `https://<host>` (with an
optional port) is refused, so what is shown is the host the guest will talk to. Only the first start
reads the invite. Afterwards the window offers **Reset** instead, which deletes the VM (disk, seed,
identity, share) and the spent invite. Remove the Mac's box in the Console first (Sites → the site → the
box → Remove): joining again takes the same box name, `<site>-mac-<name>`.

Like Docker Desktop's resource settings, what this Mac contributes is a reservation: the node has it all the
time while WeCoLab runs, and nothing takes it back. The main window (Open WeCoLab in the menu) also holds:

- **When idle**: *Idle after* N minutes, *Idle CPUs*, *Idle memory*. Once nobody has touched the Mac for N
  minutes the node may use up to these: up to every core, and all memory but 2 GiB. Never less than the
  reservation; raising the reservation raises them with it.
- **Start WeCoLab at login**: a login item (`SMAppService.mainApp`). Its status shows under the toggle;
  macOS may ask you to allow it in System Settings, General, Login Items.
- **Start the node when WeCoLab opens**: once the VM exists, the node starts at launch and the window
  stays closed. With both on, the Mac rejoins its Site after every login without anyone opening anything.

Memory in the main window applies within seconds while the node runs, up to the size it started with.
CPUs, disk and everything under When idle apply at the next start.

The CLI does the same, headless:

```bash
dist/wecolab-node up --invite - --cpus 2 --memory 4 --disk 20 --idle-cpus 6 --idle-memory 12
                             # --invite - reads the invite from stdin (paste it), never from the command
                             # line, where every process can read it; stays in the foreground; flags left
                             # out keep their saved value; the first start asks to confirm the invite's Console
dist/wecolab-node status     # both profiles, VM, mode, Nebula address, join progress (last lines of the join log)
dist/wecolab-node down       # asks the guest to power off; forces it after 60 s (also stops the app's VM)
dist/wecolab-node reset      # deletes the guest and the invite; remove the box in the Console first
```

The app and the CLI share `config.json`. Only one VM runs per user (`vm.pid` is a lock), so they never share
a disk.

## What happens

First `up`: download `debian-13-genericcloud-arm64.raw` (3 GiB, raw because Virtualization.framework
reads raw images) from cloud.debian.org, check it against the published `SHA512SUMS`, clone it to
`disk.img` (an APFS clone, no extra space) and grow that sparsely to the disk size. A NoCloud seed
(`hdiutil makehybrid -iso -joliet`, label `cidata`) carries `meta-data` and `user-data`. The user-data
sets the hostname to `mac-<short name>` (at most 30 characters: the node is `<site>-<host>`, at most 63,
and a site name may take 32), writes `install.sh` (compiled into the binary, never downloaded) to
`/opt/wecolab/join.sh` and the invite to a root-only file, and enables three units: the share's mount, the
host-channel timer, and `wecolab-join.service`, which runs `install.sh` with the invite as its argument
(sourced in a subshell, so the invite is in no process's argv) and `WECOLAB_LAPTOP=1` (the Console refuses
a Mac on an invite that is not a Mac's), and logs to `/var/log/wecolab-join.log`. A failed join (network,
a busy apt, a Stop in the middle) is retried, at every boot and after 15 s growing to 10 min, until one
succeeds and leaves `/var/lib/wecolab-mac/joined`; an attempt that finds `install.sh`'s own done marker
counts as one that succeeded. Each attempt first finishes an interrupted `dpkg`, then runs `install.sh`
again, which skips what an earlier attempt finished. The Console uses up the invite only when it has
recorded the Mac; a VM that is reset needs a new one.

The seed test mounts a transient `hdiutil makehybrid` ISO in a temporary directory (never the
user's VM), checks its `cidata` label and `meta-data` keys, and reads the mounted cloud-config
as cloud-init's NoCloud consumer would. It checks the decoded installer against `install.sh`,
the root-only invite, the join service and its first-boot commands. The Oct 5 failure compared
mounted `user-data` to a second, independently generated invite: the test's JSON-backed invite
can serialize its dictionary keys in different orders. That byte equality did not establish
an ISO or guest-boot defect; the test now uses one invite and checks consumed content instead.
Do not print the mounted `user-data`: it contains the invite. A passing ISO test does not
prove guest boot; the release gate still requires a disposable ARM VM with a throwaway
invite/fabric, observing cloud-init `wecolab-join`, its log, Nebula and node-agent, then
removing only that VM and its throwaway identity.

`guest-version` records which WeCoLab for Mac made `disk.img`. v1 kept a NetBird guest at the same paths;
a disk without the stamp, or a share with v1's `netbird.json`, is not started until it is reset.

The VM: the idle profile's CPUs and memory, EFI boot, virtio disk + seed, NAT network with a fixed MAC
(cloud-init binds the network config to it), a memory balloon, entropy, a virtio console to `console.log`,
and a virtio-fs share. Every 5 s the balloon follows whether you are active (below).

Everything is in `~/Library/Application Support/WeCoLab/`: `config.json`, the image, `disk.img`,
`guest-version`, `seed.iso`, EFI variables, machine identifier, MAC address, `console.log`, `vm.pid`, `share/`.
`config.json` holds every setting, the app's and the CLI's: both profiles, the idle minutes, the two startup
choices and the invite; a `config.json` from before the idle profile loads with idle equal to the
reservation. `config.json` and `seed.iso` hold the invite and are readable by the user only. The login item
itself is registered with macOS (System Settings, General, Login Items), not stored here.

## Host-guest channel

`share/` on the Mac is `/wecolab` in the guest (virtio-fs tag `wecolab`, mounted at boot by `wecolab.mount`,
before `k3s-agent`, so the node agent's pod never sees an empty directory).

| file | written by | meaning |
|---|---|---|
| `mode` | the Mac | `active` or `idle`; the guest logs it every 30 s (`journalctl -u wecolab-share`) |
| `wecolab-join.log` | the guest, every 30 s | the last 64 KiB of `/var/log/wecolab-join.log`; each attempt ends with `wecolab-join exit=N` |
| `nebula.json` | the guest, every 30 s | the box's Nebula address and name, and whether a steward's certificate service (8094) answers over Nebula |

The guest can write anything there, so the Mac reads these files off the main thread, never through a
symlink, only when they are regular files, only their last few KiB, and without control characters.

## Reserved, and more when idle, honestly

Idle means no keyboard, mouse or tablet input for the chosen minutes
(`CGEventSource.secondsSinceLastEventType(.combinedSessionState, eventType: kCGAnyInputEventType)`;
the `.null` event type does not measure input: here it returned 342,000 s while the Mac was in use).

Virtualization.framework fixes a VM's CPU count and memory when it is created, and its balloon can only
take memory back below that size. So the VM is created with the idle profile, and every 5 s:

- **Active:** the balloon's target is the reserved memory, never less, so the guest hands what it has above
  that back to macOS. The idle taint evicts the best-effort apps, so the guest runs only its own services
  and its CPUs sit quiet. The VM's process keeps its normal priority: macOS's background state
  (`PRIO_DARWIN_BG`) would also throttle its network, and the guest's Nebula tunnels did not hold under it,
  so the site could not reach the node.
- **Idle:** the balloon is released to the whole idle profile.

With the idle profile equal to the reservation the balloon never moves.

Kubernetes is told through the `mode` file: the site's node agent (a DaemonSet on laptop nodes) keeps the
`wecolab.io/idle` taint on the node whenever the mode is not `idle`. Of the apps' pods, only best-effort
ones may run on a laptop node. They can start only while the idle taint is off, and are evicted a minute
after it comes back (when the person returns).

Mode publication is atomic and the cached state advances only after the share write succeeds.
The app reports publication errors in its window; the headless CLI writes them to stderr.
Both retry on the next five-second tick. On an idle-to-active transition the Mac removes any
stale `idle` file before writing `active`: a failed active write leaves no idle grant, so the
node agent keeps/reapplies its idle taint. If removal fails, the VM is stopped instead of
continuing to advertise workload eligibility. Failed idle writes retain the non-idle
backstop; the balloon target is still applied every tick. To test actual guest convergence,
use a dedicated test account and a disposable VM/share, never change permissions on the
user's existing VM share.

A Mac runs only the apps without a database of projects whose every offer held at its site, directly or
through a pool, is best effort (Console: Sites, New offer, tick best effort; an offer that names boxes must
name the Mac's). The site owner's own apps and every database stay off it, so a Mac at a site without such a
holder runs no apps.

## Not done yet

- No download progress; the first start is a silent 3 GiB wait.
- Going from idle to active, the balloon asks for everything above the reservation back at once, while the
  taint's drain takes up to a minute.
- When the Mac sleeps the node goes NotReady; nothing drains it first.
- Leaving a site: `down`, remove the box in the Console (Sites → the site → the box → Remove), which
  blocks its Nebula certificates, then `reset`. The Console does not delete its Kubernetes node yet: a
  member of the site's project deletes it on the site's manager (`kubectl delete node <site>-mac-…`).
- A join whose answer was lost after the Console accepted it keeps retrying with a spent invite; the log
  says so, and the way out is to remove the box, reset and add the Mac again.
