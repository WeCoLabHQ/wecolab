# WeCoLab for Mac

Requires an Apple Silicon Mac running macOS 14 or newer and a site already
created on a supported Linux box. Build with Swift 6, Go (the version in
`network/go.mod` or newer), and Apple's signing tools. The ad-hoc-signed build
is for the Mac that built it, not a notarized release for other Macs.

The app and CLI control one per-user background runner. Each node profile has
its own Debian VM, fabric invite, guest identity, disk and resource settings.
Closing either client does not stop its VMs.

**Untrusted/friend fabrics cannot start in this build.** The default `isolated`
policy rejects startup before provisioning: a verified OS-enforced Mac/LAN
firewall is not implemented or installed. App Sandbox and user-space packet
filtering are not substitutes. `localNetwork` is an explicitly confirmed mode
for trusted fabrics only; it does not guarantee exclusion of the Mac, LAN
devices or peer VMs. Do not switch a friend's node to this mode to bypass the
strict-mode blocker.

```text
WeCoLabCore/      profiles, provisioning, shared control protocol and VM implementation
  Profiles.swift      per-node storage, legacy import and aggregate resource admission
  RunnerClient.swift  shared XPC client and user LaunchAgent registration
  RunnerService.swift serialized admission and worker lifecycle
  Provision.swift     Debian image verification, disk/seed and bounded host channel
  NodeVM.swift        Virtualization.framework devices and memory balloon
wecolab-runner/   background supervisor; neither client owns VM processes
wecolab-vm/       sandboxed worker with a capability for one profile directory
network/         inherited-sandbox Go network backend with bounded flow admission
wecolab-node/    headless client
WeCoLab/         SwiftUI menu bar app and profile window
Plugins/         compiles ../install.sh into WeCoLabCore at build time
Tests/           configuration, provisioning, migration, boundaries and lifecycle regressions
```

## Build

```bash
make test     # Swift tests and Go race tests
make sign     # signed dist/WeCoLab.app, CLI, runner, VM worker and network backend
make test-sandbox # build/sign and exercise a real worker's home-directory capability
make dmg      # dist/WeCoLab.dmg (local development signing by default)
```

CI also builds the SwiftUI executable with the macOS 15 runner's Swift 6.1 compiler.
Keep the login-item `Binding` setter as an explicit closure: passing the actor-isolated
method reference directly triggers [Swift compiler crash #82491](https://github.com/swiftlang/swift/issues/82491)
in that toolchain. The closure preserves the same synchronous main-actor behavior.

Only `wecolab-vm` receives the virtualization entitlement and App Sandbox
capabilities. It resolves the supervisor's bookmark for its own profile; its
network child inherits the sandbox. The app, CLI and runner do not host VMs.
`swift build` alone is insufficient to boot a VM: use the complete signed
package. Keep the CLI's three helper executables beside it.

The sandbox smoke creates a disposable profile under Application Support, checks
that the signed worker can acquire its VM lock, and confirms strict startup stays
blocked without creating a guest disk. It does not start a VM.

`SIGN_IDENTITY` selects the signing identity; the default is ad hoc. Distribution
to other Macs also requires Developer ID signing and notarization. A signed app
alone does not enable strict isolation: that needs a supported, fail-closed OS
enforcement design, an Apple-authorized Network Extension provisioning/signing
setup, installation/approval and verified enforcement before startup is enabled.

## Use

The menu lists nodes and their state; the main window selects a profile and
shows its fabric, VM state, activity, mesh report, settings and runner resource
reservations. **Add node** confirms the invite's exact HTTPS Console: that
fabric's administrators control the guest as root. Each profile requires its
own invite. Never paste invites into shell command arguments.

**Start node / Stop node** affect only the selected profile. Closing the window
or quitting the app leaves VMs running. **Open WeCoLab at login** controls the
UI login item; each profile's auto-start setting controls whether the runner
starts that VM when it launches. A user LaunchAgent owns the runner, independent
of the UI, and appears under macOS Login Items/background permissions.

Resource admission reserves each preparing/running/stopping VM's maximum idle
CPUs and memory, plus 1 GiB per worker. The combined budget leaves one logical
CPU and 4 GiB memory for the host. These are admission limits, not a hard quota
on every host allocation. The guest disk has a fixed virtual capacity but is
sparse on the host. Stop a VM before resetting/removing it.

Memory reservation changes apply while running, up to that VM's boot-time
maximum. CPU, disk, network policy and idle settings require a restart; the UI
and CLI report pending settings rather than presenting them as applied.

```bash
# Trusted fabric only: confirmation includes Console ownership and LAN risk.
dist/wecolab-node add --name Personal --invite - --network localNetwork \
  --cpus 2 --memory 4 --disk 20 --idle-cpus 2 --idle-memory 4
dist/wecolab-node up --node Personal       # returns once the VM is running
dist/wecolab-node list
dist/wecolab-node status --node Personal --json
dist/wecolab-node configure --node Personal --auto-start true
dist/wecolab-node down --node Personal     # guest shutdown, forced after 60 seconds

# Safe default: a profile can be saved, but cannot start without strict enforcement.
dist/wecolab-node add --name Friend --invite -
dist/wecolab-node up --node Friend         # rejected; no weaker fallback

# Remove the box in its fabric Console and wait for node deletion first.
dist/wecolab-node reset --node Personal    # deletes guest/identity/invite, keeps profile
dist/wecolab-node remove --node Personal   # deletes only this stopped profile
```

`--node` accepts a name or full profile ID. Mutations require it when multiple
profiles exist. `--yes` explicitly accepts CLI trust/destruction confirmations;
it does not bypass strict-mode enforcement. Status responses redact invites.
Reset/remove do not revoke remote fabric membership.

### Existing single-node installation

The old CLI/app-owned VM is never taken over while running. The new clients
report its PID and require one graceful stop using the **old** owner before
migration. Do not reset, kill or re-enroll it. On the next runner request,
journaled import moves the existing disk, identity, seed and configuration into
one profile, preserving its enrollment. Its original network behavior is
represented as trusted LAN mode. A partially completed import resumes; conflicting
files or unsafe links block migration instead of overwriting them.

For development, both clients accept `--root PATH` to use a separate state
directory and LaunchAgent. Always use a disposable root for smoke tests, never
the existing node. Keep the installed app/CLI path stable while its runner is
registered. Another client connects to an already registered runner rather than
replacing it with its own build.

## What happens

First start of a permitted profile: download `debian-13-genericcloud-arm64.raw`
from cloud.debian.org, verify its published SHA512 checksum, clone it to that
profile's `disk.img`, and grow it sparsely to the requested capacity. A NoCloud
seed (`hdiutil makehybrid -iso -joliet`, label `cidata`) carries `meta-data` and
`user-data`. New profiles use a stable UUID-derived hostname of at most 30
characters, distinct from the editable display name. The user-data writes the
embedded installer to
`/opt/wecolab/join.sh` and the invite to a root-only file, and enables three units: the share's mount, the
host-channel timer, and `wecolab-join.service`, which runs `install.sh` with the invite as its argument
(sourced in a subshell, so the invite is in no process's argv) and `WECOLAB_LAPTOP=1` (the Console refuses
a Mac on an invite that is not a Mac's), and logs to `/var/log/wecolab-join.log`. A failed join (network,
a busy apt, a Stop in the middle) is retried, at every boot and after 15 s growing to 10 min, until one
succeeds and leaves `/var/lib/wecolab-mac/joined`; an attempt that finds `install.sh`'s own done marker
counts as one that succeeded. Each attempt first finishes an interrupted `dpkg`, then runs `install.sh`
again, which skips what an earlier attempt finished. The Console uses up the invite only when it has
recorded the Mac; a VM that is reset needs a new one.

The seed test mounts only a transient ISO, checks its `cidata` label and decoded
cloud-config, installer, root-only invite and join commands. Do not print
`user-data`: it contains the invite. These tests do not prove fabric enrollment.
A release still requires a disposable ARM VM and throwaway fabric/invite,
observing cloud-init, `wecolab-join`, Nebula and node-agent, then removing only
that VM and its throwaway identity.

`guest-version` is the VM disk's compatibility generation (`2`), not the app's release
version or a report of the guest's installed Warden version. The app bundle's Info.plist
reports version `0.1` (build `1`); `wecolab-node status` reports config, VM and join state,
not a software release version. Installing a newer host app does not replace a current
guest's seed, embedded installer or installed guest software: `Provision.run` reuses its
disk and only grows it if requested. Do not reset a live VM to "upgrade" it; that
deletes its disk and identity and requires removing and rejoining its box. A v1 NetBird
guest at the same paths, a disk without the current stamp, or a share with v1's
`netbird.json`, cannot start until reset.

The VM uses EFI, virtio disk and seed, a fixed MAC, a memory balloon, entropy,
bounded serial capture and virtio-fs. Its Ethernet attachment goes to a private
socketpair, not Virtualization.framework NAT. A per-VM Go/gVisor backend handles
IPv4 TCP/UDP, DNS and DHCP. Flow/handler admission and socket buffers are bounded;
this is not an OS-enforced network isolation boundary.
The pinned generated gVisor `go` branch includes
[UDP packet-overhead accounting](https://github.com/google/gvisor/commit/14dfded19e92b6bfcf49022f81d537fac4278333).
Do not downgrade to a payload-only receive budget: empty datagrams would bypass
it. A regression holds the consumer idle during an empty-datagram burst and
checks both bounded admission and budget recovery after draining.

State lives in `~/Library/Application Support/WeCoLab/profiles/<uuid>/`.
Each profile owns `profile.json`, disk, seed, guest version, EFI variables,
machine identifier, MAC address, console capture, `vm.pid`, `channel.dmg` and
`share/`. Profile directories are private to the user; configuration and seed
contain the invite and have user-only permissions. A worker's disk lock stays
held until its guest stops. The runner serializes admission before asynchronous
provisioning, so concurrent app/CLI starts cannot create duplicate workers for
one profile. Runtime snapshots distinguish configured from running settings.

## Host-guest channel

`share/` is a mounted fixed-size **16 MiB HFS+ image**, exposed as `/wecolab`
through virtio-fs tag `wecolab`. The worker refuses an unmounted/plain directory.
This bounds guest-written channel data independently of the host filesystem's
free space. The image is mounted before worker launch and detached after worker
exit. Legacy channel files are copied into it without following links.

The guest's `wecolab.mount` runs before `k3s-agent`, so the node agent's pod does
not see an empty directory at boot.

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

Mode publication is atomic and the cached state advances only after the share
write succeeds. The worker reports publication errors through the runner to
both clients and retries on the next five-second tick. On an idle-to-active
transition it removes stale `idle` before writing `active`; a failed write leaves
no idle grant. If removal fails, the VM stops rather than continuing to advertise
workload eligibility. The balloon target still applies every tick.
Test actual guest convergence with a dedicated test account and disposable
VM/share; never change permissions on the user's existing VM share.

A Mac runs only the apps without a database of projects whose every offer held at its site, directly or
through a pool, is best effort (Console: Sites, New offer, tick best effort; an offer that names boxes must
name the Mac's). The site owner's own apps and every database stay off it, so a Mac at a site without such a
holder runs no apps.

## Not done yet

- Strict OS-enforced Mac/LAN exclusion is unavailable; untrusted/friend nodes remain blocked.
- The shared runner and worker sandbox do not constitute a guarantee against hypervisor or OS vulnerabilities.
- No download progress bar; the first start shows a download status message but not progress through the 3 GiB image.
- Going from idle to active, the balloon asks for everything above the reservation back at once, while the
  taint's drain takes up to a minute.
- When the Mac sleeps the node goes NotReady; nothing drains it first.
- Leaving a site: `down`, remove the box in the Console (Sites → the site → the box → Remove),
  wait for its site's Warden to delete the Kubernetes node, then `reset`. The same box
  name cannot be rejoined while its old node remains. Certificate blocklisting reaches
  other boxes at their next successful sync (normally within an hour); offline or failing boxes
  retain earlier blocklists. Removing a box is not instant isolation.
- A join whose answer was lost after the Console accepted it keeps retrying with a spent invite; the log
  says so, and the way out is to remove the box, wait for node deletion, reset and add the Mac again.
