> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Running a Mac that offers macOS VMs

- This page is for the operator of a Mac that offers macOS VMs: how many it offers, disk, logins, the shared ledger, backups, credentials, the Mac's controls, and what the host can see.
- Requirements and switches are in [macOS VM sandboxes on a Mac](macos-vms.md); faults are in [troubleshooting](macos-vm-troubleshooting.md).

## How many VMs a Mac offers

- The Mac offers a number, `max`, of at most 2. It is the runner's own live macOS VM runs plus how many more the Mac's ledger can admit now. The runner recomputes it every 15 seconds.
- Three limits apply to `max`, and each can set it to 0:
  - Apple's cap: 2 less the macOS VMs the ledger already holds.
  - The Mac's memory budget: its memory, minus what Docker reports, minus 4 GiB for macOS. Each VM costs its guest memory plus about 1 GiB of host overhead.
  - The Mac's free disk, less what running VMs have reserved and a floor, divided by one run's disk reservation.
- The runner's declared totals (`--total-memory`, `--total-cpu`) do not enter `max`. They bound what the organisation lets the runner's runs hold, so a run they hold back waits. A total cannot pass the memory Docker reports, and a per-run limit cannot pass its total.
- To run two VMs, lower Docker's memory allowance in its settings. On a 32 GiB Mac, Docker at 20 GiB leaves a budget of 8 GiB, enough for one guest; at 12 GiB it leaves 16, as in the first row below.
- Left at the defaults, a runner's total is half of Docker's memory, shared among its runs. That is usually below one macOS guest's minimum, so set the limits when you register (`--total-memory`, `--per-run-memory`, `--total-cpu`, `--per-run-cpu`).
- `wardyn-runnerd setup` shows a worked sum with the Mac's own numbers, which includes the runner's total, and names what to change.
- **`status` is this Mac's own sum.** Its `max` comes from Apple's cap, the Mac's memory and its free disk. It does not read the organisation's limits for the runner: Runs at once, the CPU, memory and disk totals, and what other runs reserve. Those can hold fewer.
  - A run those limits hold back waits in the queue, and the run names the limit ([The queue](runner-pools.md#the-queue)).
  - A run the Mac's own `max` leaves no room for is refused at create and when it is promoted, not queued. The sentence depends on the case:
  - A Mac that already runs 2 macOS VMs: "`<name>` already runs 2 macOS VMs, the most Apple's licence allows."
  - A Mac whose `max` is below 2, outside a pool of several runners: "`<name>` has no room for another macOS VM right now."
  - A pool of several runners, where the Mac cannot take the run: "Every runner in this pool must be able to take a macOS VM, and at least one can't right now. Try again later, or choose another pool."
- **Per-run limits.** The organisation sends the runner's per-run CPUs and memory on every connect, and the runner then recomputes `max`.
  - A runner with no per-run limits sends 0 CPUs and 0 memory, which means unset. Each guest then gets the base's minimum.
  - A per-run memory or CPU count that is set but below the base's minimum makes this runner offer no macOS VMs while it stays so. The runner logs the reason and names the flag to raise, `--per-run-memory` or `--per-run-cpu`. The base stays ready.
  - `status` shows the base's minimum CPUs and memory.

| Mac memory | Docker memory | Limits (GiB) | `setup` says |
|---|---|---|---|
| 32 GiB | 12 GiB | `--total-memory 12 --per-run-memory 6` | 2: the runner's total holds 2 x 6, and the budget (32 - 12 - 4 = 16) holds 2 x (6 + 1) |
| 16 GiB | 4 GiB | `--total-memory 4 --per-run-memory 4` | 1: the budget is 8, one guest needs 5 and two need 10 |
| 16 GiB | 8 GiB | Any | 0 if the base's minimum is over 3 GiB: the budget is 4 |

- A runner whose per-run limit is below the base's minimum offers no macOS VMs at all.
- The sums in the table are `setup`'s. `status` reads the ledger's three limits above, so it leaves out the runner's totals.
- The organisation and then the runner each also refuse a run whose applied CPU or memory is below the minimum. The refusal says the shortfall and what lowered it: the profile's ceiling `resources`, its `limits.max_*`, the pool, or the server.
- The server keeps a run within the runner's per-run limits and never raises them.
- A start is also refused when the Mac's available memory is under the guest's plus 2 GiB, and when the Mac is too hot.
- **An idle login's Docker.** The ledger knows only logins that hold an entry. A second person's login that runs its own Docker with no macOS VM running is not subtracted from the budget. The live memory check above is the only guard in that case.

## Disk

- Preparing needs about 81 GiB free (the 25 GiB restore image, the 50 GiB base disk, 1 GiB extra and the 5 GiB floor). Starting a VM reserves its disk's logical size plus 1 GiB (51 GiB), and needs that much free plus a 5 GiB floor. A second VM therefore needs about 107 GiB free when it starts.
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
| `/Users/Shared/Wardyn/sock/` | `root:wheel` `1777`, sticky | One folder per login, `u<uid>/`, made by that login with mode `0700`; each run's sockets sit in a folder inside it |

- The package's installer creates these. A runner never creates them: if they are missing or not owned by root, the Mac offers no macOS VMs and says to reinstall the package.
- **A pre-planted name is moved, not changed.** A path that is already there and not exactly as above is moved into a new root-only `.wardyn-quarantine.*` folder. For `/Users/Shared/Wardyn` itself the folder sits beside it; for anything inside it, the folder sits inside `/Users/Shared/Wardyn`.
  - The installer makes the path again and moves the entries across. It never changes the owner or mode of what it found, and never deletes it.
  - An admin can inspect and delete a quarantine folder as root.
- **The installer waits for the lock.** If any login holds the admission lock for more than 10 seconds, the installation fails closed: macOS VMs stay off. Reinstall the package to retry once nobody holds it.
- A runner deletes only its own entries. The sticky bit stops another login's runner from removing them.
- **The socket folder.** A Unix socket path holds only about 100 bytes, which a path under a home folder can exceed. So a run's control, agent and desktop sockets sit in `/Users/Shared/Wardyn/sock/u<uid>/<run-key>/`, about 56 bytes.
  - The login makes `u<uid>/` itself. On every use it checks that this is a real directory, owned by that login, with mode `0700`.
  - Anything else there refuses that login's macOS VMs with a named reason: a planted name, a link, or a folder with a different owner or mode.
  - It is a denial of service for that login's macOS VMs, never access to a socket. An admin removes the planted name as root.
  - The run's disks and `run.json` stay in the run directory in the person's home. Deleting the run directory deletes its socket folder with it.
  - A runner sweeps the login's socket folders that belong to gone runs. It never sweeps one whose folder changed in the last minute, because a starting helper makes its folder before it binds its control socket. An older folder goes only when its run directory is gone and nothing answers on its control socket.
- **Other local accounts can deny service.** Every local account can write the ledger directory and open the lock. A local user can add entries that hold this Mac's slots, memory or disk, or hold the lock, and stop macOS VMs until they stop or the Mac restarts. Forged entries cannot be cleared reliably.
  - It is a denial of service for macOS VMs only. A forged entry cannot admit a VM past the cap, and the framework's own cap stays the backstop.
  - A runner waits at most 10 seconds for the lock, then refuses, saying the ledger is busy.
  - A local user who can do this can already exhaust the Mac's memory or disk directly.
- **When a slot is released.** A runner frees an entry only once the helper reported the VM stopped, the helper exited and the run directory is gone. There is one exception, a heuristic: a runner may free its own entry for a helper that was killed without that report.
  - It does so when the helper's process no longer exists, no Virtualization or `wardyn-macvm` process of the login created after that helper remains other than another entry's live helper, and the run directory is gone.
  - Nothing says which VM a process belongs to, so it errs toward keeping the entry ([Apple's licence and the two-VM limit](macos-vms.md#apples-licence-and-the-two-vm-limit)).
  - Docker's Linux VM holds the slot only if Docker started after that helper. Which came first is read from the time since the Mac started, which a change of the clock does not move. A process whose creation time cannot be read keeps the slot.
  - Then the Virtualization framework's own cap of two is the backstop.
  - A slot recorded by an earlier release frees once no macOS VM, helper or Docker engine of that user runs. Two kinds wait for their owner instead (below).
- **A login that ended with a VM live.** Another login's runner can free its slot and memory once the helper is gone.
  - Its disk stays reserved, because the run's files are in that person's private directory, which no other login can read or delete.
  - The runner reports the held space apart from the freed capacity.
  - It is released when that person next logs in and their runner removes the files. A restart also drops it.
- **Leftovers.** Entries this release recorded in an earlier boot, their tombstones and temporary files are never counted, but only their owner or root can delete them.
  - If a person never logs in again, their small ledger files and their run directories stay.
  - An admin removes them as root: the files under `/Users/Shared/Wardyn/macos-vm-admission`, that person's `/Users/Shared/Wardyn/sock/u<uid>/`, and `runs/` under `~/Library/Application Support/Wardyn/macos-vm.noindex/` in that person's home.
  - A root cleanup service is not in this release.
- **Files only their owner clears.** A file that does not parse is damage, since the ledger's own writes never leave one. It counts as a live VM, after a restart too, until it is removed. So does a leaked slot that an earlier release recorded, since nothing proves its boot ended.
  - First check that no macOS VM or helper of that user runs: `ps -U <user> -o pid,comm | grep -E 'Virtualization|wardyn-macvm'` prints nothing. Quit Docker Desktop first, since its Linux VM is a Virtualization process too.
  - Only then does the file's owner, or root, delete it from `/Users/Shared/Wardyn/macos-vm-admission`.
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
- **The desktop's VNC password.** It is 8 random characters generated on the Mac per run, or once per base if the guest cannot change it per run. The runner hands it to the guest at start, and the helper signs in to Screen Sharing with it. It is not sent to the organisation or the browser.
- **The desktop's sign-in scheme.** Screen Sharing offers only VNC's password scheme here, which encrypts a challenge with DES under the password (RFC 6143 §7.2.2). Only 8 characters count, so the password is 8 characters from a 57-character alphabet, about 46 bits. The sign-in never crosses a network: it runs over the VM's private channel to the guest's own loopback port.
- The Mac's own user can read a live run's disk, as the owner of a Docker laptop runner controls the engine. Posture does not constrain a root developer.
- A person may sign in to websites and apps in the desktop. Those sign-ins enter the sandbox and are kept on the run's virtual disk, because a macOS guest has swap and keeps its keychain and browser profiles on disk. The Linux promise that they live only in memory does not hold here.
- An agent set "Sign-ins in a desktop: Not allowed" is refused a macOS desktop, because a macOS keychain cannot be made read-only in this release.
- **While the base is built, two secrets are made in the guest.** Neither is a run credential.
  - The setup account's password is random for each prepare. It signs in over SSH, reachable only from the helper's own network with no egress.
  - The helper keeps it in a file in the base's folder on the Mac, readable by the Mac's user only. `edit-base` changes the password from it, because macOS will not let root change it without the old one.
  - It is in no log, `tools.json` or caps, and is not sent to the organisation or the browser. A run's copy of the base does not get it.
  - The setup account keeps that password in the base and in every run made from it, and Remote Login is off. A setup by hand and each `edit-base` rotate it. Provisioning also turns on auto-login for the account, so the guest may hold the password in `/etc/kcpassword` until stage 1 overwrites that file.
  - After a setup by hand, the one-time code is the account's password until stage 1 replaces it as its first step. Its file on the Mac is deleted once the new password has signed in.
  - Remote Login is turned off, and proved off on a live guest, before the base is published. The setup account itself stays in the base.
- The guest's `agent` user is a standard user, not an admin. Its login password is random for each prepare and sits in the guest only in the root-only `/etc/kcpassword`, which auto-login reads.

## The guest and your Mac's controls

- Your Mac may be managed: encrypted, patched, watched by an endpoint agent, and behind your network controls.
- The guest is a separate machine on that Mac, so each control either reaches it, is replaced by a Wardyn control, or is a gap you close.
- This table says which.

| Mac control | In the guest | State |
|---|---|---|
| Network filtering | The guest's one NIC goes to the run's own relay and proxy, with no NAT or bridged device. The proxy allows, denies and audits each request | Today |
| Trust in your organisation's certificate authority | Node, Python, git and curl are pointed at the run proxy's certificate. On macOS 26 the run's System keychain does not trust it ([System trust store](macos-vm-use.md#apps-that-use-only-the-system-trust-store)) | Today for your own CA: a `ca` item is trusted in the guest's System keychain for SSL, on macOS 26 by hand in `edit-base`, and the caps report `org_ca` as `trusted` or `not_trusted` ([The organisation CA](macos-vm-base-image.md#the-organisation-ca)) |
| Endpoint security agent | None is installed in the base. Stage 2 installs the Command Line Tools, the harness CLIs and the guest agent only | Today, in part: a `pkg` item installs your agent's package into the base as admin, with its licence as a `secret_files` entry. A system-extension or helper approval is by hand in `edit-base`, or from your MDM ([Customising the base](macos-vm-base-image.md#customising-the-base)) |
| Privilege management | `agent` is a standard user. Stage 1 refuses to publish a base that holds any other account than it and the setup account, and only the Mac's user can read the setup account's password, kept for `edit-base` | Today |
| Disk encryption | The guest's disk is a file on the Mac's volume, so the Mac's disk encryption covers it when the volume has any. The run's clone is deleted at the end, which is not secure erasure | Today |
| OS integrity (SIP, Gatekeeper) | Stage 1 changes neither, so the guest keeps macOS's defaults | Today: the runner reports the base's `os_version`, `os_build`, `sip` and `gatekeeper` in the caps, as read at prepare, update or edit. They are not live ([Guest posture](macos-vm-base-image.md#guest-posture)) |
| Firewall | Nothing connects in. The guest agent dials out over a virtual socket, and the desktop is reached only through the organisation's gateway | Today |
| Screen lock and password policy | Stage 1 sets screen saver idle time 0 for the prepare's host identity, not a run, and keeps the display awake, and the guest signs in as `agent` on its own. The guest has no console ([Threat model, B14](../../threatmodel/THREAT-MODEL.md#b14--macos-vm-guest-vs-the-mac-that-runs-it)) | Today. macOS ignores these settings when written for the whole system, so a base prepared before 0.10.0-rc.9 needs `wardyn-runnerd macos-vm prepare --new-image`
| Screen lock for a run | At each run's start the guest agent sets screen saver idle time 0 and turns `agent`'s screen lock off, passing the password on stdin only. A failure is logged and the lock stays as it was | Today for new runs, with no re-prepare of an older base. Stage 1 tries it too, and a refusal there does not stop the base |
| Patch policy | Automatic updates are off, so a base stays at the macOS it was built with. Re-prepare to move to a newer one | Today: the caps carry the base's macOS build and when it was prepared. `macos_vm.max_base_age_days` refuses a run on a Mac whose base is older, or whose runner reports no age ([Settings](macos-vms.md#settings)) |
| Device identity for sign-in checks | The guest is an unmanaged device, so a provider that requires a compliant device may refuse sign-ins from its browser | Today, in part: the caps report whether the guest is `enrolled` in MDM and how many profiles it holds. A `profile` item is approved by hand, and MDM-only payloads never take effect from it ([Configuration profiles](macos-vm-base-image.md#configuration-profiles)) |
| Managed application configuration | Wardyn delivers no organisation browser, editor or assistant settings into the guest | Today, if you list them: a `file` item pushes a settings file under `/Library`, `/usr/local` or `/opt`. One at a harness's managed settings path is refused, as are Wardyn's own files ([Customising the base](macos-vm-base-image.md#customising-the-base)) |
| Process-level audit | Wardyn audits the guest's network, credentials and lifecycle. It records no processes inside the guest | Today, in part |

## What the host can see

| On the Mac | Who can use it |
|---|---|
| `macos-vm.noindex/runs/<run>/` (disk, auxiliary storage, `run.json`, helper log) | The logged-in user only (mode `0700`) |
| `/Users/Shared/Wardyn/sock/u<uid>/<run-key>/` (control, agent and desktop sockets) | The logged-in user only (mode `0700`) |
| `macos-vm.noindex/bases/` and `cache/` | The logged-in user only |
| `macos-vm.noindex/prepare.log` | The logged-in user. No password or secret value is logged |
| `macos-vm.noindex/setup-code.json` | The logged-in user only (mode `0600`). It exists only while macOS waits for setup by hand or an `edit-base` session. The window can show its code on a click |
| `macos-vm.noindex/agent-signing/` (`key.pem`, `cert.pem`, `.lock`) | The logged-in user only (directory `0700`, `key.pem` `0600`). This login's guest agent signing identity, which only `wardyn-macvm` reads. A base moved to another Mac or login loses its Full Disk Access grant once |

- The helper runs as the logged-in user, never root, with one entitlement: to use Apple's virtualization. It has no networking entitlement.
- The VM listens on no TCP port on the Mac, and the proxy's credential injection never leaves Docker.

## The run record

- A run records its guest macOS build (`guest_os`) and the Mac's (`host_os`) in `vm_machine`.
- A run's page says whether the Mac removed the run's directory, with the VM's disk and clone, or why it could not. The runner sends fixed codes.
- The migrations are listed in [Upgrades](../OPERATIONS.md#upgrades). A runner older than 0.10.0-rc.3 sends no disk report. Its run's page says "Removing the macOS VM's disk from this Mac…" for 10 minutes after the run ends, then nothing about the disk.
