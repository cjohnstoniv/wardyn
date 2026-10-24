> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# macOS VM sandboxes on a Mac

- A macOS VM sandbox (`machine_kind: macos_vm` on a run) is a macOS guest under Apple's Virtualization framework, started by a registered `wardyn-runnerd` on a Mac.
- It is a preview and off by default, in two places: the organisation and the Mac.
- Wardyn does not ship macOS. Each Mac downloads Apple's restore image and builds its own base image once.
- This page is for the person or admin who runs the Mac. There is no console control in this release: a run asks for it through the API or the CLI request body.
- The Linux counterpart is [Linux VM sandboxes](linux-vm-sandboxes.md).

## What a run gets

| Part | What it is |
|---|---|
| The machine | A macOS guest with the CPUs and memory the run was given, never more than the runner's per-run limits. It runs as the standard user `agent` |
| The disk | A copy-on-write clone of the Mac's base image, made for the run and deleted when the run ends |
| The agent | The Wardyn guest agent in the guest starts the harness and serves terminals. It dials out to the runner over a virtual socket; nothing connects in |
| Tools | Apple's Command Line Tools (git, clang, python3) and the pinned harness CLIs, installed once into the base. A Mac offers only the harnesses its base can run |
| Network | One virtual NIC, wired to the run's own network relay and proxy in Docker. The guest has no other way out |
| Desktop | The guest's own Screen Sharing, reached only through the runner and the organisation's gateway. Recording and the clipboard rules apply as for a Linux VM desktop |
| Confinement | The VM boundary, which the runner treats as the `CC3` family. The run's recorded `confinement_class` stays the one it asked for |
| Sign-ins | Kept on the run's virtual disk, not in memory ([Credentials and sign-ins](#credentials-and-sign-ins)) |

- Not in this release: folder mounts, Linux VMs on a Mac, drives, reviving a stopped VM, and runner terminal recording. Each is refused with a reason.
- macOS VMs need a pool where every runner offers them; a mixed pool refuses them. This matches Linux VMs.
- A Kubernetes runner refuses the kind.

## Before you start

| Requirement | Detail |
|---|---|
| The Mac | Apple silicon, macOS 27 or later. Apple's unattended setup of the guest needs macOS 27 on the host |
| Docker | A running engine on the Mac, such as Docker Desktop. The guest's network relay and proxy run in it, and the runner needs it to start |
| Disk | About 85 GiB free to prepare, then about 107 GiB free to start a second VM ([Disk](#disk)) |
| Power | On AC power to start preparing ([Preparing the base image](#preparing-the-base-image)) |
| A login | Someone logged in on the Mac ([Logins, FileVault and restarts](#logins-filevault-and-restarts)) |
| Company-managed | The Mac's trust class is `managed_device` and its posture file says `mdm_managed` ([Which Macs qualify](#which-macs-qualify)) |
| The package | `wardyn-runnerd.pkg` from this release, which installs the VM helper and the shared ledger ([Installing with a macOS package and MDM](hybrid-laptops.md#installing-with-a-macos-package-and-mdm)) |

## Turn it on

1. In the organisation, set `WARDYN_MACOS_VMS=1` ([`WARDYN_MACOS_VMS`](../ENV.md#wardyn_macos_vms)) and restart `wardynd`. With it off, create refuses the kind.
2. Install the runner package on the Mac and register the Mac as a runner ([Claiming a runner](hybrid-laptops.md#claiming-a-runner)). Set its per-run limits for a macOS guest ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)).
3. Turn the Mac's own switch on: `WARDYN_RUNNER_MACOS_VM=1` in the runner's environment, or the managed preference `MacOSVM` from your MDM, or answer yes when `wardyn-runnerd setup` asks.
4. Have a security operator mark the Mac `managed_device` ([Which Macs qualify](#which-macs-qualify)) and push the MDM posture file.
5. Prepare the base image: `wardyn-runnerd macos-vm prepare` ([Preparing the base image](#preparing-the-base-image)).
6. Check it with `wardyn-runnerd macos-vm status`. A run is placed on the Mac only when the base is ready.

- Either switch off, and nothing changes: no capability is offered and no code path runs.

## Settings

| Setting | Where | Meaning |
|---|---|---|
| `WARDYN_MACOS_VMS` | The organisation | `1` lets a run ask for `macos_vm`. Off by default |
| `WARDYN_RUNNER_MACOS_VM` | The Mac's runner | `1` offers macOS VMs from this Mac once its checks pass, else logs the fix. Off by default |
| `MacOSVM` | The managed preferences profile | The same switch, from your MDM. Only a macOS profile sets it ([Managed preferences](hybrid-laptops.md#managed-preferences)) |

## Which Macs qualify

- A macOS VM runs only on a Mac that both of these hold for:
  - its trust class is `managed_device`, which only a security operator sets (`wardyn runner trust <runner-id> managed_device --reason "<why>"`), held for a second person under four-eyes;
  - its posture says `mdm_managed`, with the MDM's root-owned posture file as the evidence (`/Library/Application Support/Wardyn/posture.json`).
- Both are assertions. Wardyn does not yet check the Mac's MDM device certificate; that arrives in the next patch.
- A Mac not marked `managed_device` is refused `placement_capability`. A Mac whose posture is missing or unmet is refused `runner_posture_unmet`.
- Posture is self-reported and does not constrain a root developer ([Resident credentials and posture](hybrid-laptops.md#resident-credentials-and-posture)).

## Apple's licence and the two-VM limit

- Apple's licence allows up to two additional copies or instances of macOS on a Mac for development, testing, macOS Server or personal non-commercial use. The framework refuses a third. Wardyn counts first.
- One ledger on the Mac counts every macOS VM, whichever login or purpose: a run, preparing the base, and a tools update. Preparing holds one of the two slots.
- A third VM is refused with the reason. Docker's own Linux VM does not count.
- Nothing in a Wardyn release contains macOS. The restore image comes from Apple to the Mac that uses it, and it is never uploaded, exported or stored in a registry.

## Preparing the base image

`wardyn-runnerd macos-vm prepare` builds the Mac's base image. It is unattended and takes a while beyond the download. It runs once per Mac, and again for a newer macOS.

| Step | What happens |
|---|---|
| 1. Check | The host, Docker, free disk (the 85 GB above) and AC power. A failed check stops with the fix |
| 2. Download | Apple's current restore image, about 25 GB, in resumable 64 MiB ranges. The SHA-256 is recorded |
| 3. Validate | The image is loaded and checked against what this Mac supports. A bad image is deleted and fetched once more |
| 4. Install | macOS is installed into a sparse 50 GB disk |
| 5. First boot | Apple's unattended setup creates the guest's setup account and turns on Remote Login. This happens exactly once per base |
| 6. Stage 1 | Over a network with no egress, Wardyn installs the guest agent, creates the `agent` user, turns on Screen Sharing and turns off sleep, screen lock and automatic updates. It then turns Remote Login off and drops the setup password |
| 7. Stage 2 | Through a prep-only proxy that allows Apple's software-update hosts, the guest agent installs the Command Line Tools. The runner pushes the pinned harness CLIs in; the guest needs no egress for them |
| 8. Finish | The manifest is written and the base is marked ready. The downloaded image is deleted unless `--keep-ipsw` is set |

- **Power.**
  - Preparing starts only on AC power.
  - The download pauses on battery at the end of its current range and continues on power.
  - Every later step starts only on power and is never paused, because interrupting an install has undefined results.
  - If a later step is interrupted anyway, by a power loss, a restart or a crashed helper, Wardyn discards the partial base and restarts from the install once the Mac is on power. The downloaded image is kept.
- **Setup by hand.** If the guest's Remote Login has not answered after 10 minutes, preparing stops and says macOS asked for setup by hand. The Mac's person then finishes it once in the local window and turns on Remote Login. The steps and a short one-time code are in `wardyn-runnerd macos-vm status`.
- **The desktop.** Apple has said Screen Sharing turned on from the command line may let a viewer only watch. Preparing ends with a control check, and `status` reports whether the desktop is `control`, `observe` or `none`. With `observe`, macOS VMs still run agent-only, and the desktop is not offered until a person turns on control for the base.
- **Newer macOS.** When Apple's current image is newer than the base, `status` says so. Run `prepare` again; runs keep using the old base until the new one is ready, then it is deleted.

## How many VMs a Mac offers

- The Mac offers a number, `max`, of at most 2. It is the runner's own live macOS VM runs plus how many more the Mac's ledger can admit now. The runner recomputes it every 15 seconds.
- Two limits apply, and both can set it to 0:
  - The runner's declared memory totals. Docker's memory setting decides them: a total cannot pass the memory Docker reports, and a per-run limit cannot pass its total.
  - The Mac's memory budget: its memory, minus what Docker reports, minus 4 GiB for macOS. Each VM costs its guest memory plus about 1 GiB of host overhead.
- Left at the defaults, a runner's total is half of Docker's memory, shared among its runs. That is usually below one macOS guest's minimum, so set the limits when you register (`--total-memory`, `--per-run-memory`, `--total-cpu`, `--per-run-cpu`).
- `wardyn-runnerd setup` shows this arithmetic with the Mac's own numbers and names what to change.

| Mac memory | Docker memory | Limits (GiB) | `max` |
|---|---|---|---|
| 32 GiB | 12 GiB | `--total-memory 12 --per-run-memory 6` | 2: the runner's total holds 2 x 6, and the budget (32 - 12 - 4 = 16) holds 2 x (6 + 1) |
| 16 GiB | 4 GiB | `--total-memory 4 --per-run-memory 4` | 1: the budget is 8, one guest needs 5 and two need 10 |
| 16 GiB | 8 GiB | Any | 0 if the base's minimum is over 3 GiB: the budget is 4 |

- A run whose applied CPU or memory is below the base's minimum is refused, naming the minimum. The server keeps a run within the runner's per-run limits and never raises them.
- A start is also refused when the Mac's available memory is under the guest's plus 2 GiB, and when the Mac is too hot.
- **An idle login's Docker.** The ledger knows only logins that hold an entry. A second person's login that runs its own Docker with no macOS VM running is not subtracted from the budget. The live memory check above is the only guard in that case.

## Disk

- Preparing needs about 85 GiB free. Starting a VM reserves its disk's logical size plus 1 GiB (51 GiB), and needs that much free plus a 5 GiB floor. A second VM therefore needs about 107 GiB free when it starts.
- The reservation bounds each VM's growth on the live volume, because a clone's private blocks cannot pass the disk's logical size.
- **What it does not bound.** Space held by the Mac's own APFS snapshots, such as Time Machine's hourly local snapshots, is outside the reservation.
  - While a snapshot keeps a clone's earlier blocks, rewriting them uses new space, and a deleted run's blocks stay allocated until the snapshot goes.
  - So with local snapshots, macOS VMs can use more disk than they reserved. Each start reads the live free space, so space a snapshot holds counts against the next VM.
  - Wardyn changes no snapshot or backup setting to close this gap.
- Other programs writing to the same volume are outside it too. Every 30 seconds the runner checks free space; below 5 GiB it ends the newest macOS VM run. That check is best effort.
- Docker's disk limits (`--total-disk`, `--per-run-disk`) measure Docker's free disk, not this volume, so they do not apply to macOS VMs.

## Logins, FileVault and restarts

- Apple's framework is not safe to run from a daemon, so macOS VMs run only while someone is logged in on the Mac. The runner runs in that person's login session.
- With FileVault on, automatic login is off. After a restart, the Mac offers no macOS VMs until someone logs in.
- A logout ends that login's runner and its VMs with the session. A restart ends every VM, and the runs end as sandbox lost.
- A package upgrade does not stop a running VM. The new runner adopts it, or ends it cleanly with a reason if it cannot speak the helper's protocol.

## The shared ledger

| Path | Owner and mode | Holds |
|---|---|---|
| `/Users/Shared/Wardyn` | `root:wheel` `0755` | The ledger's parent |
| `/Users/Shared/Wardyn/admission.lock` | `root` `0644` | The lock every change takes |
| `/Users/Shared/Wardyn/macos-vm-admission` | `root` `1777`, sticky like `/tmp` | One small file per VM, mode `0644`, owned by the runner that made it |

- The package's installer creates these and replaces a pre-planted directory or lock. A runner never creates them: if they are missing or not owned by root, the Mac offers no macOS VMs and says to reinstall the package.
- A runner deletes only its own entries. The sticky bit stops another login's runner from removing them.
- **Other local accounts can deny service.** Every local account can write the ledger directory and open the lock. A local user can add entries that hold this Mac's slots, memory or disk, or hold the lock, and stop macOS VMs until they stop or the Mac restarts. Forged entries cannot be cleared reliably.
  - It is a denial of service for macOS VMs only. A forged entry cannot admit a VM past the cap, and the framework's own cap stays the backstop.
  - A runner waits at most 10 seconds for the lock, then refuses, saying the ledger is busy.
  - A local user who can do this can already exhaust the Mac's memory or disk directly.
- **A login that ended with a VM live.** Another login's runner can free its slot and memory once the helper is gone.
  - Its disk stays reserved, because the run's files are in that person's private directory, which no other login can read or delete.
  - The runner reports the held space apart from the freed capacity.
  - It is released when that person next logs in and their runner removes the files. A restart also drops it.
- **Leftovers.** Entries, tombstones and temporary files from an earlier boot are never counted, but only their owner or root can delete them.
  - If a person never logs in again, their small ledger files and their run directories stay.
  - An admin removes them as root: the files under `/Users/Shared/Wardyn/macos-vm-admission`, and `runs/` under `~/Library/Application Support/Wardyn/macos-vm.noindex/` in that person's home.
  - A root cleanup service is not in this release.
- **A leaked slot.** Apple's framework can hold a VM slot after a VM ended. The runner then lowers `max` by one and reports that the Mac needs a restart. A restart is the only recovery reported.

## Backups, Spotlight and removal

| Where | What Wardyn does |
|---|---|
| `~/Library/Application Support/Wardyn/macos-vm.noindex/` | The `.noindex` name keeps Spotlight out. The first prepare also adds a Time Machine exclusion for this directory, which follows it |
| Any other setting | Nothing. Wardyn changes no other backup, snapshot or index setting |

- When a run ends, Wardyn deletes its run directory and clone from the Mac's volume. The end-of-run line says deleted only after that succeeds, else pending, then failed with the reason.
- **That is not secure erasure.** A copy can outlive it:
  - in the Mac's local snapshots, which Time Machine takes about hourly and keeps for 24 hours, and which no folder can be excluded from;
  - in freed disk blocks;
  - in a backup tool that does not honour the Time Machine exclusion.
- Wardyn does not ask a Mac to purge any of these.

## Credentials and sign-ins

- Wardyn brokers some credential kinds: the proxy injects them into a request in Docker, and the value never enters the guest.
- Other kinds do enter the guest. A secret that arrives as an environment variable or a file, and an SSH key, are in the guest's memory and on its disk while the run lives.
- A delivered value is in the guest's memory while the run lives. On the Mac's disk, the copies are inside the run's clone disk in the Mac user's home. Wardyn deletes the disk at the end of the run, which is not secure erasure (above).
- **The desktop's VNC password.** It is 8 random characters generated on the Mac, per run (once per base if the guest cannot change it per run). It is used only between the helper and the guest over a virtual socket, and is not sent to the organisation or the browser.
- The Mac's own user can read a live run's disk, as the owner of a Docker laptop runner controls the engine. Posture does not constrain a root developer.
- A person may sign in to websites and apps in the desktop. Those sign-ins enter the sandbox and are kept on the run's virtual disk, because a macOS guest has swap and keeps its keychain and browser profiles on disk. The Linux promise that they live only in memory does not hold here.
- An agent set "Sign-ins in a desktop: Not allowed" is refused a macOS desktop, because a macOS keychain cannot be made read-only in this release.
- The guest's `agent` user has no admin password. The setup account's password is dropped when preparing ends.

## Limits in this release

| Limit | Detail |
|---|---|
| Proxy certificate | Tools that use the macOS keychain (Safari, `gh`, `kubectl`) do not trust the run's proxy certificate, so hosts the proxy inspects show certificate errors in them. Node, Python, git and curl are pointed at it |
| Harnesses | Claude Code, Codex CLI and "Your own tools", depending on what the base installed. If Apple's Command Line Tools cannot install, only "Your own tools" is offered |
| Folders | A folder mount is refused |
| Desktop | One fixed 1920 x 1080 display. An `observe` result (see above) leaves macOS VMs agent-only |
| Clipboard | Paste in is allowed and copy out is blocked by default, as for other desktops, if Apple's Screen Sharing carries the standard clipboard messages. Otherwise nothing is copied either way |
| Heat and battery | New VMs are refused at serious or critical thermal state. Battery use and lid-close sleep on MacBooks were not measured |
| Guest patching | The guest's automatic updates are off, so a run's OS is predictable. Prepare again to move to a newer macOS |

## Upgrades

- An upgrade of the runner package does not repeat the macOS install.
- When the guest agent in the base differs from the runner's, the runner runs a tools update: it clones the base, boots it, applies the new agent and pins, and keeps the result. It takes one slot like any VM, for minutes.
- A base the new runner cannot accept is marked "update needed", and the Mac offers no macOS VMs until it is prepared again.

## Remove it

1. Stop the Mac's runs, or wait for them to end.
2. Run `wardyn-runnerd macos-vm remove`. It deletes the base and the downloaded image under `macos-vm.noindex/`. It does not touch the ledger files or the Time Machine exclusion.
3. Unset `WARDYN_RUNNER_MACOS_VM` (or the `MacOSVM` preference) and restart the runner.

## When a Mac stops offering macOS VMs

| What you see | Cause and fix |
|---|---|
| Not offered; the runner says macOS 27 is needed | The Mac runs an older macOS. Upgrade it |
| No Mac in the pool offers macOS VMs | No runner in the pool has a ready base, or the pool mixes runners that do not. Run `status` on each Mac |
| Not offered after a restart | Nobody has logged in since the restart, and FileVault is on |
| The runner says the ledger is missing or damaged | Reinstall the runner package |
| The runner says the ledger is busy | Another account holds the lock, or the ledger is crowded with entries. See [The shared ledger](#the-shared-ledger) |
| A third VM refused | The Mac already runs two, or preparing holds one |
| `max` is 0 | Docker's memory or the runner's limits leave no room ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)) |
| A run ended because the Mac ran low on disk | Free space fell below 5 GiB, possibly held by snapshots ([Disk](#disk)) |
| The runner says the Mac needs a restart | The framework still holds a slot. Restart the Mac |
| Preparing waits | For AC power or for Docker to start |
| Preparing stopped because macOS asked for setup by hand | Finish setup once in the local window, as `status` describes |

## What the host can see

| On the Mac | Who can use it |
|---|---|
| `macos-vm.noindex/runs/<run>/` (disk, auxiliary storage, control, agent and desktop sockets, helper log) | The logged-in user only (mode `0700`) |
| `macos-vm.noindex/bases/` and `cache/` | The logged-in user only |
| `macos-vm.noindex/prepare.log` | The logged-in user. No password or secret value is logged |

- The helper runs as the logged-in user, never root, with one entitlement: to use Apple's virtualization. It has no networking entitlement.
- The VM listens on no TCP port on the Mac, and the proxy's credential injection never leaves Docker.

## The run record

- Migration `0234_macos_vm_kind` widens `agent_runs.machine_kind` to allow `macos_vm`, and the desktop sign-in `store` column to allow `run_disk`. It changes no row.
- A run records its guest macOS build (`guest_os`) and the Mac's (`host_os`) in `vm_machine`.
