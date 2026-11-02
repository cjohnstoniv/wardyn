> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# macOS VM sandboxes on a Mac

- A macOS VM sandbox (`machine_kind: macos_vm` on a run) is a macOS guest under Apple's Virtualization framework, started by a registered `wardyn-runnerd` on a Mac.
- It is a preview and off by default, in two places: the organisation and the Mac.
- Wardyn does not ship macOS. Each Mac downloads Apple's restore image and builds its own base image once.
- This page is for the person or admin who runs the Mac. The console's New run page offers it (choose a virtual machine, then macOS), `POST /api/v1/runs` takes it too, and `wardyn run --machine-kind macos_vm` asks for it (`--macos-base <name>` picks a base other than Standard). A Run document for `--file` cannot carry either; the flags stand beside it.
- The Linux counterpart is [Linux VM sandboxes](linux-vm-sandboxes.md).

## What a run gets

| Part | What it is |
|---|---|
| The machine | A macOS guest with the CPUs and memory the run was given, never more than the runner's per-run limits. It runs as the standard user `agent` |
| The disk | A copy-on-write clone of the Mac's base image, made for the run and deleted when the run ends |
| The agent | The Wardyn guest agent in the guest serves terminals and starts the run through the launcher, `/usr/local/bin/agent-run` ([What starts in a guest](#what-starts-in-a-guest)). It dials out to the runner over a virtual socket; nothing connects in |
| Tools | Apple's Command Line Tools (git, clang, python3) and the pinned harness CLIs, installed once into the base. A Mac offers only the harnesses its base can run |
| Network | One virtual NIC, wired to the run's own network relay and proxy in Docker. The guest has no other way out |
| Proxy variables | `HTTPS_PROXY`, `HTTP_PROXY` and `WARDYN_PROXY_URL` name the run's proxy. The relay carries a connection to the proxy itself unchanged, and the proxy checks what is sent on it as it checks a container's |
| Open connections | The relay holds at most 1,024 per run on granted ports other than 80. One past that is refused and reported as `tunnel-limit`; the cap does not evict existing connections; lower process or container resource limits can bind first |
| Desktop | The guest's own Screen Sharing, reached only through the runner and the organisation's gateway. The clipboard rules apply as for a Linux VM desktop. The desktop is not recorded in this release |
| Confinement | The VM boundary, which the runner treats as the `CC3` family. The run's recorded `confinement_class` stays the one it asked for |
| Sign-ins | Kept on the run's virtual disk, not in memory ([Credentials and sign-ins](#credentials-and-sign-ins)) |

- Not in this release: Linux VMs on a Mac and reviving a stopped VM. Each is refused with a reason.
- Recording is not in this release either. A macOS VM run starts unrecorded, with a notice at Review. It is refused at create when recording is required, the organisation's level is locked above off, or the governance ceiling sets a level.
- macOS VMs need a pool where every runner offers them; a mixed pool refuses them. This matches Linux VMs.
- A Kubernetes runner refuses the kind.

## What starts in a guest

- The launcher, `/usr/local/bin/agent-run`, is the guest agent's own binary, which stage 2 installs into the base. It keeps the contract the Linux images keep, so a run's workload starts the same way on every machine.
- It runs as `agent`, after the run's files are in place: the harness's configuration, the run's repos (cloned through the run's proxy) and the starting folder.
- Built and unit-tested; not yet run on a real Mac.

| Run | What starts |
|---|---|
| Agent task | The run's harness on the task: Claude Code (`claude -p`) or Codex CLI (`codex exec`) |
| Command | The command, as `/bin/sh -c`, with no harness |
| Self-test | `agent-run --selftest` checks the guest can start the harness or a command, and writes nothing |
| Refused | Tasks for several agents, held tool approvals, a service, a workspace scan, and an interactive agent session. The launcher exits 64 with the reason |

- **Managed files.** The runner moves each file a run's policy manages to where macOS reads it: Claude Code's managed settings and MCP servers to `/Library/Application Support/ClaudeCode`, Codex's requirements to `/etc/codex`. Paths under `/home/agent` move to `/Users/agent`. A managed file with no macOS location stops the run's start with a reason.
- **Before the run starts.** The launcher checks each managed file is in place: root's at its mode, in a folder only root can change. A file that is not stops the run's start with a reason.

## Before you start

| Requirement | Detail |
|---|---|
| The Mac | Apple silicon, macOS 26 or later. On macOS 26 each prepare, `--new-image` too, needs [setup by hand](#setup-by-hand) once per base, about 10 minutes at the Mac. macOS 27 sets the guest up unattended |
| Docker | A running engine on the Mac. The guest's network relay and proxy run in it, and the runner needs it to start ([Docker on the Mac](#docker-on-the-mac)) |
| Network | An uninspected path to `updates.cdn-apple.com`, which Apple pins: a TLS-inspecting proxy fails the image download at once, and the error prints the image URL and names `--ipsw`. The same exemption covers [stage 2](#the-prep-proxy)'s opaque tunnels |
| Disk | About 81 GiB free to prepare, then about 107 GiB free to start a second VM ([Disk](#disk)) |
| Power | On AC power to start preparing ([Preparing the base image](#preparing-the-base-image)) |
| A login | Someone logged in on the Mac ([Logins, FileVault and restarts](#logins-filevault-and-restarts)) |
| Company-managed | The Mac's trust class is `managed_device` and its posture file says `mdm_managed` ([Which Macs qualify](#which-macs-qualify)) |
| The package | `wardyn-runnerd.pkg` from this release, which installs the VM helper and the shared ledger ([Installing with a macOS package and MDM](hybrid-laptops.md#installing-with-a-macos-package-and-mdm)) |

## Turn it on

1. In the organisation, set `WARDYN_MACOS_VMS=1` ([`WARDYN_MACOS_VMS`](../ENV.md#wardyn_macos_vms)) and restart `wardynd`. With it off, create refuses the kind.
2. Install the runner package on the Mac and register the Mac as a runner ([Claiming a runner](hybrid-laptops.md#claiming-a-runner)). Set its per-run limits for a macOS guest ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)).
3. Turn the Mac's own switch on: `WARDYN_RUNNER_MACOS_VM=1` in the runner's environment, or the managed preference `MacOSVM` from your MDM, or answer yes when `wardyn-runnerd setup` asks.
4. Have a security operator mark the Mac `managed_device` ([Which Macs qualify](#which-macs-qualify)) and have your MDM attest posture: the `MDMManaged` key in the profile, or the posture file.
5. Prepare the base image: `wardyn-runnerd macos-vm prepare`, or answer yes to the setup question and let `setup` start it ([Preparing the base image](#preparing-the-base-image)).
6. Check it with `wardyn-runnerd macos-vm status`. A run is placed on the Mac only when the base is ready.

- The two switches are separate gates. The organisation's decides whether a run may ask: with it off, create refuses `macos_vm`. The Mac's decides what its runner offers: with it off, the Mac offers no macOS VMs.

## Docker on the Mac

- The runner uses the engine that `DOCKER_HOST` names, else the selected Docker context, as the `docker` command does. It follows only a context that is a local socket, and refuses any other with the `DOCKER_HOST` to set.
- Docker Desktop is the engine this page is written for. Rancher Desktop, Colima and other engines are untested with macOS VMs.
- Docker's memory comes off the Mac's budget for macOS guests ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)).

## Settings

| Setting | Where | Meaning |
|---|---|---|
| `WARDYN_MACOS_VMS` | The organisation | `1` lets a run ask for `macos_vm`. Off by default |
| `WARDYN_RUNNER_MACOS_VM` | The Mac's runner | `1` offers macOS VMs from this Mac once its checks pass, else logs the fix. Off by default |
| `MacOSVM` | The managed preferences profile | The same switch, from your MDM. Only a macOS profile sets it ([Managed preferences](hybrid-laptops.md#managed-preferences)) |
| `macos_vm_base_file` | The Mac's runner, or `MacOSVMBaseFile` in the profile | The base's install list, a JSON file on the Mac. A changed list starts a tools update ([Customising the base](#customising-the-base)) |
| `macos_vm_bases_dir` | The Mac's runner, or `MacOSVMBasesDir` in the profile | A directory of the Mac's other bases, one install list each ([Several bases](#several-bases)) |
| `MDMManaged`, `DiskEncrypted` | The managed preferences profile | The posture, from your MDM, when no posture file exists |
| `macos_vm.max_base_age_days` | The organisation's site config | The most days since a Mac's base was prepared that a run is accepted. Empty or 0: no maximum; a negative number is refused. A runner that reports no base age is refused while one is set |
| `macos_vm.allow_background_watch` | The organisation's site config (Settings, macOS virtual machines) | Whether a person may watch a background macOS run's desktop, view only. Absent or `true`: allowed. `false`: no one may. A governance profile's `deny_background_desktop_watch` denies it for that profile's runs. |

- **A write that leaves the switch out keeps it.** A site config write that leaves out `allow_background_watch`, whether or not it names `macos_vm`, keeps the stored value. A stored `false` stays off.

## Which Macs qualify

- A macOS VM runs only on a Mac that both of these hold for:
  - its trust class is `managed_device`, which only a security operator sets (`wardyn runner trust <runner-id> managed_device --reason "<why>"`), held for a second person under four-eyes;
  - its posture says `mdm_managed`, with the MDM's root-owned posture file (`/Library/Application Support/Wardyn/posture.json`) or the profile's `MDMManaged` key as the evidence.
- Both are assertions. The Mac's MDM device certificate is not checked in this release.
- A Mac not marked `managed_device` is refused `placement_capability`. A Mac whose posture is missing or unmet is refused `runner_posture_unmet`.
- Posture is self-reported and does not constrain a root developer ([Resident credentials and posture](hybrid-laptops.md#resident-credentials-and-posture)).

## Apple's licence and the two-VM limit

- Apple's licence allows up to two additional copies or instances of macOS on a Mac for development, testing, macOS Server or personal non-commercial use. The framework refuses a third. Wardyn counts first.
- One ledger on the Mac counts every macOS VM, whichever login or purpose: a run, preparing the base, and a tools update. Preparing holds one of the two slots.
- A third VM is refused with the reason. Docker's own Linux VM does not count.
- A macOS VM that ended without reporting its stop frees its slot once no Virtualization or helper process of its user created after its helper remains, other than another macOS VM's own helper. Docker's Linux VM holds that slot only if Docker started after that helper.
  - Which came first is read from the time since the Mac started, which a change of the clock does not move. A process whose creation time cannot be read keeps the slot.
  - A slot recorded by an earlier release frees once no macOS VM, helper or Docker engine of that user runs. Two kinds wait for their owner instead ([The shared ledger](#the-shared-ledger)).
- Nothing in a Wardyn release contains macOS. The restore image comes from Apple to the Mac that uses it, and it is never uploaded, exported or stored in a registry.

## Preparing the base image

`wardyn-runnerd macos-vm prepare` builds the Mac's base image. It is unattended, continues in the background, and takes a while beyond the download. It runs once per Mac. A newer macOS needs a re-prepare (see Newer macOS below).

- It sizes its VM at the runner's per-run limits and never raises them. A runner that has connected to an organisation that sets them has them saved, and `prepare` uses them. Before that, if the organisation sets none, or to use others, pass `--per-run-cpu` (CPUs) and `--per-run-memory` (GB); a flag wins over the saved limit.
- `--keep-ipsw` keeps Apple's image after the base is ready, and `--new-image` is the re-prepare.
- `--ipsw <file> --sha256 <hex>` builds from a restore image you downloaded, for a network where Apple's download fails (see Network above). `--sha256` is required; take the SHA-256 from a source you trust. It never asks Apple which image is newest.
- `--ipsw-url <https URL> --sha256 <hex>` downloads from your own mirror instead, in the same resumable ranges. The mirror must answer HTTP Range requests, else prepare says so. With the flag, the hash is `--sha256` alone, never the profile's. It never falls back to Apple; an image with another hash is deleted and refused.
  - A managed Mac sets the mirror in the profile (`MacOSVMImageURL`, `MacOSVMImageHash`) or `runnerd.json` (the profile wins), and setup's prepare uses it. The Mac trusts the mirror's certificate through its own keychain.
- Prepare copies the file into its cache, never linking or moving it, and uses the copy only when its hash matches, before anything loads or installs it. A mismatch is refused with both hashes and the copy removed. Your file is never changed or deleted.
- With a base ready, prepare does nothing and does not use the file; `--new-image` builds the new base from it. The copy is deleted once the base is ready, unless `--keep-ipsw` is set.
- A restore image in the cache is reused only when a previous prepare's download recorded its SHA-256 (a `.sha256` file beside it) and the file still matches. The record is written only for an image Wardyn downloaded over the pinned connection, resumed only from its own partial file, or copied with `--ipsw` after its hash matched.
- Any other file there (placed by hand, or no longer matching its record) stops prepare with an error naming it. Prepare neither uses nor deletes it: move it out and pass it with `--ipsw` and `--sha256`, or delete it. The record guards against hand-placed or damaged files, not against someone with the runner user's write access.
- `wardyn-runnerd setup` starts the same preparing itself with the limits it just registered, on a first registration unless the base is ready. Given `--keep-ipsw`, setup passes it on to that prepare. A rerun prints the prepare command instead.

| Step | What happens |
|---|---|
| 1. Check | The host, Docker, free disk (the 81 GiB above) and AC power. A failed check stops with the fix |
| 2. Download | Apple's current restore image, about 25 GB, in resumable 64 MiB ranges. The SHA-256 is recorded |
| 3. Validate | The image is loaded and checked against what this Mac supports. A bad image is deleted and fetched once more |
| 4. Install | macOS is installed into a sparse 50 GB disk |
| 5. First boot | On macOS 27, Apple's unattended setup creates the guest's setup account, `wardyn-admin`, with a random password made for this prepare, and turns on Remote Login. On macOS 26 the Mac's person does it once ([Setup by hand](#setup-by-hand)) |
| 6. Stage 1 | Over a network with no egress, Wardyn signs in over SSH. It installs the guest agent, creates the standard user `agent` and sets up Screen Sharing, sleep, lock and updates. Last it turns Remote Login off and proves it (below) |
| 7. Stage 2 | Through a prep-only proxy ([The prep proxy](#the-prep-proxy)), the guest agent installs the Command Line Tools. The pinned harness CLIs and the helper's guest agent build, also the run launcher, are pushed in; the guest needs no egress for them |
| 8. Self-test | The launcher's self-test runs as `agent`, for a command and for each harness. A harness that fails it is withheld. If the command self-test fails, no base is published |
| 9. Finish | The manifest is written and the base is marked ready. The downloaded image is deleted unless `--keep-ipsw` is set |

- **Power.**
  - Preparing starts only on AC power.
  - The download pauses on battery at the end of its current range and continues on power.
  - Every later step starts only on power and is never paused, because interrupting an install has undefined results.
  - If a later step is interrupted anyway, by a power loss, a restart or a crashed helper, Wardyn discards the partial base and restarts from the install once the Mac is on power. The downloaded image is kept.
- **Remote Login is proved off.** Stage 1 does not take the command's word for it.
  - Just before, the guest must answer Screen Sharing's RFB banner on port 5900 and answer on port 22. Stage 1 notes the guest's boot, counted by the DHCP requests the helper's own network has seen.
  - Just after, a new connection to port 22 must be refused outright. A timeout, or a network that cannot reach the guest, proves nothing: stage 1 keeps trying for 30 seconds, then fails.
  - A refusal counts only from a guest shown live on the same boot: port 5900 still sends its banner and no new DHCP request arrived. A stopped, rebooted or reset guest also refuses port 22, so that case stops stage 1 at once.
  - Any failure stops preparing and no base is published.
- **The desktop.** Apple has said Screen Sharing turned on from the command line may let a viewer only watch. Preparing ends with a control check, and `status` reports whether the desktop is `control`, `view only` or `none`. With `view only`, a run's desktop can be watched from the run page (Watch desktop), not acted in.
  - The UI gateway and the Mac's helper each drop keys, pointer moves and paste, so that input never reaches the guest. Built and unit-tested; not yet run on a real Mac.
  - View only covers keys, pointer moves and paste. Uploads from the run's Files panel still go to `~/Transfers` in the guest unless policy blocks them.
  - `status` says how, and the prepare log says why.
  - To turn control on, run `wardyn-runnerd macos-vm edit-base`, turn on Screen Sharing with "VNC viewers may control screen with password" in the guest, then close the window. On a macOS 26 base the desktop stays view only even so ([#3901](https://github.com/cjohnstoniv/wardyn/issues/3901)).
- **Newer macOS.** Wardyn does not tell you that Apple has a newer image, but `status` shows when the base was prepared and this Mac's macOS beside it. To move to a newer one, run `wardyn-runnerd macos-vm prepare --new-image` with the per-run flags; a plain `prepare` keeps a ready base and says there is nothing to prepare.
  - The new base is built beside the old one, which runs keep using until the new one is ready. Then the old one is replaced and deleted.
  - A re-prepare holds one of the two slots while it runs.
- **Stopping stage 2.** Stopping a stage-2 boot, preparing's or a tools update's, through its control socket ends it at any point, even partway through an install.
  - Calls in flight to the guest are cancelled, the VM is stopped and its socket folder is removed. The stop returns only after that.
  - Stage 2 then fails, and no base is published from that boot.

## Setup by hand

- macOS 26 has no unattended setup of a guest; Apple added it in macOS 27. So on a macOS 26 Mac, preparing boots the installed guest into macOS's Setup Assistant and waits for a person, once per base.
- On macOS 27 this happens only when the guest's Remote Login has not answered 10 minutes after the first boot.
- Before you start, `wardyn-runnerd macos-vm status` on a macOS 26 Mac says that its prepare needs setup by hand.
- When the wait begins, preparing writes a fresh 8-character one-time code to `~/Library/Application Support/Wardyn/macos-vm.noindex/setup-code.json`, readable by you only. `status` and `doctor` name the file and never print the code. No log or event carries it.
- It opens a window on the Mac, "Wardyn: set up this macOS VM", with the guest's screen in it and the three steps below its title.
  - The steps: create the account `wardyn-admin` with the one-time code as its password, turn on Screen Sharing with control, and turn on Remote Login.
  - Every prepare writes a new code. Type the code shown for this prepare: an earlier prepare's code, a mistyped character or a lost Shift key makes stage 1 stop within seconds, saying the account's password does not match the one-time code. Delete the VM and prepare again.
  - The window comes to the front, over other apps and on the Space you are using. It has no close button and closes by itself when the wait ends. The code is hidden until you click Show code, and Hide code hides it again.
- Preparing waits up to an hour for the guest's Remote Login, then stops and publishes no base. Run `prepare` again to retry.
- The window needs the Mac's desktop. On macOS 26, `prepare`, and the prepare that `setup` starts, refuse at once, before any download, when started over SSH, outside the signed-in desktop, or with sudo. They say: run prepare in Terminal on the Mac, signed in at its desktop, not over SSH and not with sudo.

1. Run `wardyn-runnerd macos-vm prepare`, or `wardyn-runnerd setup`, in Terminal on the Mac, signed in at its desktop. Not over SSH, and not with sudo.
2. Wait for the download and install. The window has no Dock icon. When it opens, click Show code in the window and read the code. The account is `wardyn-admin`.
3. In the window, choose a country and language. The guest has no internet while preparing: when asked for a network, continue without one, and skip Migration Assistant and Apple Account. Setup Assistant's Screen Time screen may take a minute to give up waiting for the network: wait, then continue.
4. Create the account: any full name, account name exactly `wardyn-admin`, and the code as its password. Do not turn on FileVault in the guest. Skip the remaining screens.
   Create no other account and add no SSH keys; only `wardyn-admin`. Prepare refuses a base that holds another account, and names it.
5. At the guest's desktop, open System Settings > General > Sharing.
   - Turn on Screen Sharing. Under its (i) button, turn on "VNC viewers may control screen with password" and give it any password; Wardyn replaces it. Turned on from the command line, Screen Sharing may let a viewer only watch, so turn it on here.
   - Turn on Remote Login.
6. Leave the window. Within seconds of Remote Login answering, the window closes and preparing continues alone, signing in over SSH.

- Stage 1's first step replaces the code with a random password, signs in again with that, and only then deletes the file. Preparing also deletes it when it discards the base or ends.

## Editing a base

`wardyn-runnerd macos-vm edit-base` boots the ready base once, in a window, for an attended session. Use it to turn on Screen Sharing with control when `status` says `view only` (on a macOS 26 base the desktop stays view only, [#3901](https://github.com/cjohnstoniv/wardyn/issues/3901)), or to change the guest by hand. Runs keep using the current base until the edited one passes its checks.

1. Run `wardyn-runnerd macos-vm edit-base` in Terminal on the Mac, signed in at its desktop. Not over SSH, and not with sudo.
2. Wardyn copies the base and, in a boot with no window, changes the password of `wardyn-admin` from the one the base keeps to a fresh one-time code. The command names the file that holds the code, `setup-code.json`, as setup by hand does.
3. The command's first line says it edits a copy of the VM base, not this Mac. It then prints that the copy takes about 2 minutes to start, and the copy boots in a window. Click Show code in the window.
   - The copy is meant to have no internet during the session; that has not been seen on a real Mac.
   - **Who you are signed in as.** The base turns on auto-login for `agent`, so the copy may open at `agent`'s desktop, not at a login window. One Mac did, with `agent` unlocked. As `agent` you have no `sudo` and cannot read `/Library/Wardyn/stage`.
   - **Administrator steps.** Open Terminal. Spotlight may not answer in the guest (one Mac showed "Indexing Paused"), so use Finder > Go > Utilities. Run `su wardyn-admin` and give the one-time code as the password.
   - **If the window opens on macOS's setup pages.** One Mac showed "How Do You Connect?" on a base that had finished setup by hand. Which answer to give has not been witnessed. The steps of setup by hand continue without a network, and the recheck refuses any account Wardyn did not make, so make none.
   - Seen on one Mac: what was clicked on those pages stayed in `agent`'s settings and reached later runs. Wardyn has not tested it.
4. Make your changes. For desktop control, open System Settings > General > Sharing and turn on Screen Sharing. Under its (i) button, turn on "VNC viewers may control screen with password".
   - For folder shares, open System Settings > Privacy & Security > Full Disk Access, click +, press Command-Shift-G, type `/usr/local/libexec/wardyn-guest-agent`, and turn it on.
   - The grant is meant to stay in the base and reach every run's guest, where Full Disk Access lifts macOS's privacy prompts for every process the run starts. That it survives the recheck and reaches each run is not yet witnessed. Wardyn gives no button for it.
   - A certificate that still reads `not_trusted` is listed in the Terminal where you started `edit-base`, with its name, subject, SHA-256 and the exact `sudo security add-trusted-cert` command. Nothing runs it for you: run each in the admin Terminal, check the subject and SHA-256, and confirm macOS's prompt.
5. Leave Remote Login off and add no accounts. When you are done, close the window: Wardyn asks the guest to shut down. The window closes once the guest has shut down.

- **The recheck.** The copy boots once more with no window. First the code is replaced with a fresh random password, which the copy keeps for the next edit, and the code file is deleted.
  - Then, in order: the control check, the accounts, Remote Login, and the launcher's self-test for a command and for each harness the base offers.
    - The control check discards the copy only when the base had desktop control and the copy lost it. A view-only base stays view only after an edit.
  - Only a copy that passes is swapped in, by the same atomic exchange as a tools update. A crash at any point leaves the old base ready.
  - A failure discards the copy and says which check failed. Runs keep using the base from its last date.
  - Keep `wardyn-admin`'s password as the code during the session. If you change it, the recheck cannot replace it and discards the copy, saying the guest did not take the one-time code.
- **Accounts.** The recheck allows `wardyn-admin`, `agent` and macOS's own accounts: below uid 500, and named with a leading `_`, or `root`, `daemon` or `nobody`. Any other account, a Guest User included, discards the copy.
- **A tripwire, not prevention.** The recheck of accounts and Remote Login is a tripwire, not prevention, because the Mac's person is root on the host. They can change a base's disk without `edit-base`.
- **Refusals.** `edit-base` refuses while a prepare or tools update runs, without a ready base, without the Mac's desktop, and when both VM slots are in use. It also refuses a base prepared before Wardyn kept its admin password: run `wardyn-runnerd macos-vm prepare --new-image` first. The edit holds one slot for all its boots.
- **What the organisation sees.** The runner reports when its base was last edited and the last recheck's result. Each run records when the base it started from was last edited, as `base_edited_at` on its machine.
- **What `status` shows.** After a passing edit, `wardyn-runnerd macos-vm status` prints what the recheck read in its boot, and when.
  - Each `ca` item reads trusted, not trusted or could not be read. Full Disk Access for the guest agent reads allowed, refused or could not be read.
  - The edit prints the same lines, with the desktop mode, when the recheck finishes.
  - They record that boot. They are not live, and a read that failed says it could not be read, never trusted or allowed.
  - The Full Disk Access read is one open of macOS's protected TCC database by the guest agent. It reads nothing from the file and writes nothing.
- Built and unit-tested; not yet run on a real Mac.

## The prep proxy

- Stage 2 runs on its own Docker network, made for the job. Its only way out is a prep-only `wardyn-proxy` that allows Apple's software-update hosts on port 443 and nothing else: `swscan.apple.com`, `swcdn.apple.com`, `swdist.apple.com`, `swdownload.apple.com`, `updates.cdn-apple.com`, `mesu.apple.com` and `gdmf.apple.com`.
- The proxy holds no credential and does not inspect TLS. The tunnel is opaque, so what is enforced is the host name and port 443. A first use of any other host is denied.
- The network, proxy and relay are removed when the job's helper exits.
- **A crash leaves none behind.** At start, before any job gets a network, the runner removes the prep containers and networks that a crash left for its own runner and deployment. A job still in the ledger keeps its own.
  - It logs how many it removed. A sweep that cannot run is logged as a warning and does not stop the runner.
  - At start the runner also deletes the staging copy that a crashed tools update or re-prepare left, unless that update is still running.

## Customising the base

An operator can have prepare install packages, apps, files, configuration profiles and a CA into the base, as admin. Built and unit-tested; not yet run on a real Mac.

- **The list.** The runner setting `macos_vm_base_file` (in `runnerd.json`, or `MacOSVMBaseFile` from your MDM) names a JSON file on the Mac. No flag or environment variable sets it, so an MDM value always wins.
- **When.** Prepare installs the list, and so does every tools update. A list whose contents change starts a tools update; the old base serves runs until the new one is swapped in.
- **Pinned.** Each file is checked against its `sha256` before it is pushed, through the same handle. A mismatch stops the install.
- **Staged as root.** Each item is staged in `/Library/Wardyn/stage/<name>`, root's and mode 0700, and installed with fixed commands. There is no shell, and the list cannot add a flag.
- **Fails closed.** An item that fails stops stage 2. Prepare publishes no base, and a tools update keeps the old one. A command's output is not kept, since an installer may echo what it was given.

```json
{"version": 1, "items": [
  {"kind": "pkg", "name": "edr", "path": "/Library/Org/edr.pkg", "sha256": "<64 hex>",
   "secret_files": [{"path": "/Library/Org/edr-token", "name": "com.vendor.registration-token", "sha256": "<64 hex>"}]},
  {"kind": "ca", "name": "org-ca", "path": "/Library/Org/root.pem", "sha256": "<64 hex>"},
  {"kind": "profile", "name": "wifi", "path": "/Library/Org/wifi.mobileconfig", "sha256": "<64 hex>", "identifier": "com.example.wifi"}
]}
```

| Kind | File | Installed with | Also takes |
|---|---|---|---|
| `pkg` | `.pkg` | `installer -pkg … -target /` | `secret_files` |
| `app` | `.zip` of the bundle, or `.dmg` | `ditto` into `/Applications`; a `.dmg` is attached read-only first | `app`: the bundle in the `.dmg` (`Name.app`) |
| `file` | any | pushed to `dest`, root's, mode 0644 | `dest`: under `/Library`, `/usr/local` or `/opt` |
| `profile` | `.mobileconfig` | staged for approval in `edit-base` | `identifier`: its `PayloadIdentifier` |
| `ca` | one X.509 v3 CA certificate (`CA:TRUE`), `.pem`, `.cer`, `.crt` or `.der` | trusted in the System keychain for SSL | none |

What Wardyn automates, and what the operator does by hand in `wardyn-runnerd macos-vm edit-base`:

| Item kind | Done by |
|---|---|
| `pkg`, `app`, `file`, `secret_files` | Automated at prepare and tools update |
| `ca` | Automated (add-trusted-cert, verified). By hand in edit when it reads `not_trusted` |
| `profile` | Staged automatically. Approved by hand in edit. Wardyn reads back `installed` |
| System-extension or helper approval (EDR) | By hand in edit (or by MDM, outside Wardyn) |
| Screen Sharing control | Setup by hand at prepare; otherwise in edit |
| Folder shares | By hand in edit: Full Disk Access for the guest agent |

- **Refused destinations.** A `file` item may not write a harness's managed settings (`/Library/Application Support/ClaudeCode`, `/etc/codex`), because a run writes those per run. Your own agent rules go in your policy instead.
  - Nor may it write Wardyn's own files, the stage directory, `/var/run/wardyn` or `/Library/Keychains`. Paths compare without case, as APFS does.
- **Secret files.** A `pkg` item's `secret_files`, such as an EDR licence, are each a mode 0600 file on the Mac. A file others can read is refused.
  - Each is pushed root's and 0600 beside the package, for its installer to read, and removed with the stage directory after the install, on every path.
  - Its value is never in a command line, a log, `tools.json` or the caps. The list's digest covers its pin only, so to rotate a licence, change its `sha256`.

### The organisation CA

The `ca` item trusts your organisation's root in the guest at prepare. It is separate from the per-run proxy CA ([Credentials and sign-ins](#credentials-and-sign-ins)).

The file must be an X.509 v3 CA certificate: its `basicConstraints` say `CA:TRUE`. Wardyn reads each `ca` file when it reads the list, and a file that is not one stops `prepare` and a tools update before anything downloads, saying why. A v1 certificate has no `basicConstraints`, so it is refused. macOS's own `openssl req -x509` writes a v1 certificate unless you give it a v3 extensions section that sets `basicConstraints = CA:TRUE`.

1. The guest agent runs `security add-trusted-cert -d -r trustRoot -p ssl` on the certificate as root: SSL only, never every policy. It does not change the `com.apple.trust-settings.admin` authorization rule; macOS 26 refuses root that change.
2. `security verify-cert -p ssl` reads the trust back. `tools.json` records `trusted` or `not_trusted` with the certificate's SHA-256 and SHA-1 fingerprints.
3. Stage 2 goes on either way: a CA that reads `not_trusted` does not stop preparing.

- `not_trusted` means macOS refused a trust without a person. On macOS 26 expect this: Apple asks a person to approve a new admin trust. Trust it by hand in `edit-base`: in Terminal, run `sudo security add-trusted-cert -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain /Library/Wardyn/stage/<name>/item.cer` and answer macOS's prompt.
  - `edit-base` prints this command for each such item, with its subject and SHA-256, and runs nothing itself.
  - Seen on one macOS 26.7 Mac: the command took effect with no prompt, the recheck passed, `tools.json` moved the item to `trusted`, and the next run's clone trusted it. Wardyn's own checks have not run this on a Mac.
  - On that Mac a certificate trusted by hand reached hosts that need only the system trust store. A host marked Inspected still needs the run's proxy certificate.
  - The list is a file on the Mac ([Customising the base](#customising-the-base)). Whoever can write that file decides which certificates `edit-base` offers; the console does not edit it.
- A tools update removes a CA the list no longer pins, with its trust settings, by its SHA-1.
- On macOS 26, expect removing or rotating a CA to stop the tools update: Apple asks a person to remove an admin trust. The old base stays ready and still trusts the old CA. To drop or replace a CA, change the list and run `wardyn-runnerd macos-vm prepare --new-image`: the new base trusts only the CAs the list holds.

### Configuration profiles

- macOS installs a profile only with a person's approval, or from MDM. Prepare stages each `profile` item and reads `profiles list` back: `staged` until it is approved, then `installed`.
- Before its window opens, `edit-base` stages each profile still `staged` again from its file in the list. It copies each to `/Users/wardyn-admin/Wardyn/<name>.mobileconfig`, readable by wardyn-admin only, and prints that path. `edit-base` owns that folder: it empties it before each window and removes it after.
- A profile whose file is not on the Mac, or that the list now pins differently, is not offered. `edit-base` logs the second by its path.
- While signed in as wardyn-admin, open the copy, then review and install the profile in System Settings.
- The recheck removes the copies, then the staged files, and checks each is gone before it reads the profiles back. A removal that fails discards the edit.
- Some payloads take effect only from MDM, whatever a person approves, so a guest never gets them from a staged profile.
  - These include Privacy Preferences Policy Control (TCC), system extension and kernel extension policies, and managed login items.
  - Approve a system extension by hand in `edit-base` instead.

### Guest posture

Stage 2 and the `edit-base` recheck read the guest's posture. The caps carry it as the base's `guest`, beside the Mac's own posture and never in it.

| Field | Read from | Values |
|---|---|---|
| `os_version`, `os_build` | `sw_vers` | e.g. `26.6.2`, `25G83` |
| `sip` | `csrutil status` | `on`, `off` |
| `gatekeeper` | `spctl --status` | `on`, `off` |
| `profiles` | `profiles list` | how many are installed |
| `mdm` | `profiles status -type enrollment` | `enrolled`, `not_enrolled` |
| `org_ca` | the `ca` items | `trusted` when every one is, else `not_trusted`; absent without one |

- The posture is read when the base is prepared, updated or edited. It is not live.
- Each read is tolerant: output Wardyn does not recognise leaves its field absent and fails nothing.
- A guest is a clone. To sign-ins that require a compliant device, it is an unmanaged device, whatever it reports here.

### Several bases

A Mac can hold other bases beside its first, Standard. Each is derived from Standard and adds its own install list. Built and unit-tested; not yet run on a real Mac.

- **The lists.** The runner setting `macos_vm_bases_dir` (in `runnerd.json`, or `MacOSVMBasesDir` from your MDM) names a directory on the Mac. Each `<name>.json` in it is the install list of the base `<name>`, in `macos_vm_base_file`'s form.
  - A name is lowercase letters, digits, `.`, `_` and `-`. A list cannot hold a `profile` item, since `edit-base` edits Standard only.
  - A list runnerd can't read (for example one without `"version": 1`) shows its base as `failed` with the reason, in `wardyn-runnerd macos-vm status` and on the runner's page; the other bases are unaffected and the failed one is not offered.
- **Derived.** A base is an APFS clone of Standard, booted once to install its own list. It keeps Standard's tools and Standard's prepare date, so the maximum base age reads the same date.
- **Rebuilt.** When Standard changes (prepare, tools update or edit) or a list changes, each base is rebuilt from it.
  - One at a time, in one of the 2 VM slots, and only while no prepare, tools update or edit runs.
  - Never while a run on that base lives. The output is `derive-base.log` in the VM directory.
  - A rebuild that fails is not retried until Standard or the list changes again. The base is not offered meanwhile.
- **Picking one.** A run picks a base with `macos_base` on create: `Standard` (the default) or a base's name. The run's `vm_machine` records it.
- A run on another base records Standard's last edit.

| Refusal | When |
|---|---|
| `placement_capability`, field `macos_base`: "No Mac in this pool has the macOS base <base>. Choose another base or another pool." | No eligible Mac has the base ready |
| `placement_denied`, field `macos_base`: "<profile> doesn't allow the macOS base <base>. Choose another base." | The governance profile's `allowed_macos_bases` leaves the base out |

- **Governance.** A profile's `allowed_macos_bases` lists the bases its people may pick, Standard included. Empty allows any base a Mac offers. A restriction narrows it the same way.
- **The pool list.** A pool's `macos_vm.bases` lists the other bases: `ready` on every Mac, `rebuilding` with the least progress, or `missing` on at least one.
- A directory that cannot be read, or a list that is refused, leaves Standard alone offered, and the runner logs why.
- Removing a base's list does not remove its files: delete `variants/<name>` in the VM directory by hand when no run is on it.

## Apple's boot-time hosts

- A macOS guest calls a few Apple hosts in the background at boot. Each would otherwise wait for an approval nobody can usefully give.
- A run denies them quietly: no approval is raised, and nothing on the list is ever allowed.

| Host | What the guest uses it for |
|---|---|
| `gdmf.apple.com` | Software-update catalogue checks |
| `mesu.apple.com` | Software-update asset metadata |
| `gateway.icloud.com` | iCloud background services |
| `init-kt.apple.com` | Key transparency lookups |

- The list is `deploy/macos-guest/boot-hosts.json`. It grows only from hosts a real Mac pass records.
- A run that allows all egress is left as it is.
- To reach one of these hosts, allow it: an exact name or a wildcard in the run's policy. That host is then handled as any allowed host.
- Apple ID sign-in, the App Store, developer downloads and certificate checks are not on the list. They keep first-use approval.
- **Apple developer services.** To allow those hosts once for everyone, add a custom component with this definition and attach it to the runs that need it:

```json
{
  "description": "Lets a macOS VM reach Apple ID sign-in, the App Store, Xcode and simulator downloads, and code signing and notarisation.",
  "hosts": ["gsa.apple.com", "idmsa.apple.com", "appleid.apple.com", "buy.itunes.apple.com", "bag.itunes.apple.com", "developer.apple.com", "ocsp.apple.com"]
}
```

- Name the component `Apple developer services`. The host list is a starting point, to be checked on a real Mac.

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
  - Nothing says which VM a process belongs to, so it errs toward keeping the entry ([Apple's licence and the two-VM limit](#apples-licence-and-the-two-vm-limit)).
  - Then the Virtualization framework's own cap of two is the backstop.
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
| Trust in your organisation's certificate authority | Node, Python, git and curl are pointed at the run proxy's certificate. On macOS 26 the run's System keychain does not trust it ([System trust store](#apps-that-use-only-the-system-trust-store)) | Today for your own CA: a `ca` item is trusted in the guest's System keychain for SSL, on macOS 26 by hand in `edit-base`, and the caps report `org_ca` as `trusted` or `not_trusted` ([The organisation CA](#the-organisation-ca)) |
| Endpoint security agent | None is installed in the base. Stage 2 installs the Command Line Tools, the harness CLIs and the guest agent only | Today, in part: a `pkg` item installs your agent's package into the base as admin, with its licence as a `secret_files` entry. A system-extension or helper approval is by hand in `edit-base`, or from your MDM ([Customising the base](#customising-the-base)) |
| Privilege management | `agent` is a standard user. Stage 1 refuses to publish a base that holds any other account than it and the setup account, and only the Mac's user can read the setup account's password, kept for `edit-base` | Today |
| Disk encryption | The guest's disk is a file on the Mac's volume, so the Mac's disk encryption covers it when the volume has any. The run's clone is deleted at the end, which is not secure erasure | Today |
| OS integrity (SIP, Gatekeeper) | Stage 1 changes neither, so the guest keeps macOS's defaults | Today: the runner reports the base's `os_version`, `os_build`, `sip` and `gatekeeper` in the caps, as read at prepare, update or edit. They are not live ([Guest posture](#guest-posture)) |
| Firewall | Nothing connects in. The guest agent dials out over a virtual socket, and the desktop is reached only through the organisation's gateway | Today |
| Screen lock and password policy | Stage 1 sets screen saver idle time 0 for the prepare's host identity, not a run, and keeps the display awake, and the guest signs in as `agent` on its own. The guest has no console ([Threat model, B14](../../threatmodel/THREAT-MODEL.md#b14--macos-vm-guest-vs-the-mac-that-runs-it)) | Today. macOS ignores these settings when written for the whole system, so a base prepared before 0.10.0-rc.9 needs `wardyn-runnerd macos-vm prepare --new-image`
| Patch policy | Automatic updates are off, so a base stays at the macOS it was built with. Re-prepare to move to a newer one | Today: the caps carry the base's macOS build and when it was prepared. `macos_vm.max_base_age_days` refuses a run on a Mac whose base is older, or whose runner reports no age ([Settings](#settings)) |
| Device identity for sign-in checks | The guest is an unmanaged device, so a provider that requires a compliant device may refuse sign-ins from its browser | Today, in part: the caps report whether the guest is `enrolled` in MDM and how many profiles it holds. A `profile` item is approved by hand, and MDM-only payloads never take effect from it ([Configuration profiles](#configuration-profiles)) |
| Managed application configuration | Wardyn delivers no organisation browser, editor or assistant settings into the guest | Today, if you list them: a `file` item pushes a settings file under `/Library`, `/usr/local` or `/opt`. One at a harness's managed settings path is refused, as are Wardyn's own files ([Customising the base](#customising-the-base)) |
| Process-level audit | Wardyn audits the guest's network, credentials and lifecycle. It records no processes inside the guest | Today, in part |

## Watching a background run's desktop

A background run has no terminal, SSH or web door. This is the one exception: the person who started a background macOS VM run can watch its desktop from the run page, view only. Built and unit-tested; not yet run on a real Mac.

- **What a watch can do.** It shows the picture and nothing else. Keys, pointer moves, paste and clipboard copy are dropped by three separate things: the helper for that boot, the Mac's desktop mode and the gateway's own filter.
  - A watch does not wake a paused run, mark the run as in use or keep it alive.
  - It opens through `POST /runs/{id}/desktop/watch-ticket`. A declared noVNC app, the desktop's files, a sign-in and a terminal stay refused ([Errors](../sdk.md#reason-branch-on-this-never-on-errors-prose)).
- **The exposure.** A background run has no terminal, so what a person sees of it is its logs and audit trail. A screen can show what those keep out: a secret in a GUI app, a document, anything the run puts on screen.
  - Turn the site switch off, or set the profile limit, where that is not acceptable.
  - On macOS 26 the viewer may show the guest's login window, not its apps ([#3890](https://github.com/cjohnstoniv/wardyn/issues/3890)). Whether what it shows is useful is not yet witnessed.
- **Recording.** A watch is recorded exactly when the run's desktop is: the run's recording level is Screen and actions and the organisation keeps recordings. A watch goes through the same recorder as any desktop view, and nothing about a watch changes that. No test pins a recorded watch.
  - The audit log shows `ui.open` and `ui.close` with `view_only` and `background`, never the picture.

| A watch is refused when | What you see |
|---|---|
| The caller is a member, a sponsor or a security admin, and did not start the run | `404`, as for any run they cannot see |
| The caller is a super admin | `403` `run_owner_only`, on another's run and on a run with no personal owner |
| The run is not running, or has ended | `409` `attach_not_running` or `attach_run_kept` |
| The run is not a background run | `409` `run_background_watch_not_background` |
| The run is a background run that is not a macOS VM | `409` `run_background_only` |
| `macos_vm.allow_background_watch` is `false` | `403`, "Watching background desktops is turned off for this install. An admin can turn it on." |
| The run's profile sets `deny_background_desktop_watch`, `deny_ui_apps` or `deny_interactive` | `403` `governance_unit`, target `runs.desktop_watch` ([Denials](denials.md)). No role is exempt |
| The run's profile cannot be resolved because its overlay cannot be satisfied | `403` `governance_overlay_unsatisfiable`, target `runs.desktop_watch`, with the overlay's own sentence |
| The agent's sign-ins in a desktop are "Not allowed" | `403` `desktop_sign_ins_off`, "Your admin doesn't allow sign-ins in a desktop while `<agent>` runs, and a macOS desktop can't block them yet." |
| The run began before the upgrade, or its helper has not reported that this boot's desktop is forced view only | `409`. A run started before the upgrade has no watch |
| The Mac's runner is offline, or its desktop mode is not `observe` (view only), including a Mac that offers control | `409`. Wardyn checks again when the runner reconnects |
| The run is paused | `409`, "This run is paused. You can watch it once it is running again." |

- **An open watch is re-checked every 30 seconds.** Each check reads the site switch, the profile and the Mac again, and ends the view when any of them now refuses. A read that fails ends it too. An open view ends within a minute of a change; closing it does not stop the run.
- **Where it shows.** `GET /runs/{id}` carries `background_watch`, beside `macos`, on a running background macOS VM run. Its `state` is `open`, `site_off`, `governance`, `sign_ins_off`, `old_boot`, `unconfirmed`, `offline`, `control`, `paused` or `not_run_owner`.
  - The run's owner also gets the refusal's sentence. Anyone else who can read the run gets `not_run_owner` and no sentence.
  - The run page shows its Watch button only when the state is `open`.

## Limits in this release

| Limit | Detail |
|---|---|
| Proxy certificate | On macOS 26 the run's System keychain does not trust it, so apps that use only the system trust store fail on inspected hosts ([System trust store](#apps-that-use-only-the-system-trust-store)). Node, Python, git and curl are pointed at it |
| Harnesses | Claude Code, Codex CLI and "Your own tools", depending on what the base installed and what passed the launcher's self-test. If Apple's Command Line Tools cannot install, only "Your own tools" is offered |
| Folders | Shared into the guest; see [Folders](#folders) |
| Desktop | One fixed 1920 x 1080 display. A `view only` result (see above) can be watched, not controlled |
| Clipboard | Paste in is allowed and copy out is blocked by default, as for other desktops, if Apple's Screen Sharing carries the standard clipboard messages. Otherwise nothing is copied either way |
| Heat and battery | New VMs are refused at serious or critical thermal state. Battery use and lid-close sleep on MacBooks were not measured |
| Guest patching | The guest's automatic updates are off, so a run's OS is predictable. Re-prepare to move to a newer macOS. `macos_vm.max_base_age_days` in the site config refuses runs on a Mac whose base is older |

### Apps that use only the system trust store

Before a run starts, Wardyn asks the guest to trust the run's proxy certificate in its System keychain, for TLS only. macOS 26 refuses this without a person ([#3923](https://github.com/cjohnstoniv/wardyn/issues/3923)). The run goes on, and its `run.dispatch` audit row says `keychain_trust` `not_trusted`, reason `refused`.

| App or tool | Host marked Inspected | Host marked Tunnel only | Tunnel only, behind a TLS-inspecting proxy |
|---|---|---|---|
| Uses only the system trust store: Safari, Electron apps, and Go or Swift tools with no CA file of their own (`gh` and `kubectl` not yet run on a real Mac) | Fails TLS | Works | Fails TLS until your organisation's root is trusted |
| Reads a CA variable: Node, Python, git, curl and the AWS CLI | Works | Works | Works, with your organisation's root in the [runner's CA bundle](hybrid-laptops.md#a-corporate-ca-on-a-laptop-runner) |

- **The CA variables.** `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `AWS_CA_BUNDLE` and `CURL_CA_BUNDLE` are set for every process a run starts.
- **Your organisation's root.** Trust it by hand in `edit-base` ([The organisation CA](#the-organisation-ca)). That reaches a Tunnel only host.
- **The run's proxy certificate.** Trusting it by hand can't help: the certificate is new for each run.
- **Which hosts are which.** The run's Policy tab and `wardyn run policy` mark each host.
- **Testing with `nscurl`.** Reported on one Mac: `nscurl` printed a TLS failure (`-9802`) against a Tunnel only host behind a TLS-inspecting proxy and still exited 0. Read its output, not its exit code. `curl` with the CA variables set answered 200 on the same hosts.
- **`kubectl`.** A kubeconfig that names a CA may not use the system trust store. The one a kubectl component writes names the run's proxy certificate.

### Folders

- Shared into the guest at `/Users/agent/…` (a workspace's `/home/agent/…` path), with the same rules and read-only or writable choice as a container.
- A run reads a shared folder only once the guest agent has Full Disk Access in the base. macOS asks a privacy question for each network volume, and nobody can answer it inside a run. Grant it once in `edit-base` ([Editing a base](#editing-a-base)).
  - Without it, a run with a shared folder stops before it starts, within 10 seconds, and names this fix.
  - A guest agent signed ad hoc, as in a package built without a Developer ID, loses the grant at each tools update, so grant it again in `edit-base`. One signed with a Developer ID keeps it. Wardyn's released package is currently built without one.
  - Full Disk Access lifts macOS's privacy prompts for every process a run starts in the guest. POSIX permissions still apply, and each run's guest is its own clone.
- A folder macOS protects (Desktop, Documents, Downloads, iCloud Drive, removable volumes) needs the runner allowed under Privacy & Security → Files and Folders first.
- A whole home folder, /Users or /private/var can't be shared, because each holds files Wardyn protects; share a project folder inside it.
- A workspace folder on this Mac is bound to its runner ([A workspace folder on your runner](hybrid-laptops.md#a-workspace-folder-on-your-runner)). The runner shares only a folder bound to it, inside its `--drive-root`, and a writable one only inside the writable roots.
  - **Who sets which root.** On a managed Mac, whoever runs the MDM sets the runner's drive roots with `DriveRoot`. With `lock_security_settings` in that profile they outrank a flag, an environment variable and `runnerd.json`, so the Mac's person cannot override them through the runner.
  - On a Mac with no managed profile, the person sets them: `wardyn-runnerd setup --drive-root <dir>`.
  - The operator of the organisation's server sets the member workspace roots (`WARDYN_USER_WORKSPACE_ROOTS`, or a member's entry in `WARDYN_USER_WORKSPACE_ROOTS_MAP`) and the writable roots (`WARDYN_USER_WRITABLE_ROOTS`, less `WARDYN_USER_WRITABLE_DENY`, where deny wins). A folder refused for being outside the workspace roots names the first setting.
  - A member's folder has to pass both: the server's roots when the run is made, and the runner's own at the bind.

### Drives

- An admin or team drive in a folder on the Mac (a `host_path` share, or a second place bound to this runner) is shared into the guest as a folder. It mounts at the container's names: `/Users/agent/drive`, `/Users/agent/drives/<name>` and `/Users/agent/shared/<name>`. A read-only drive stays read-only.
- It is held to its own folder and to the runner's drive roots, as in a container: a link out of either, or a folder macOS protects, is refused before the run starts. Give the Mac's runner its roots with `wardyn-runnerd setup --drive-root <dir>` (repeatable), or the `DriveRoot` key in the profile; see [Drive roots](hybrid-laptops.md#drive-roots-on-a-laptop-runner).
- A runner that predates drives refuses the run at create: update the runner, or run without drives. A Docker or cluster volume is refused too, because a macOS VM can't open it.
- A person's own drive can be a disk drive kept on one of their Macs (backend `mac_disk`): `POST /api/v1/me/drives` with `runner_id`, the Mac's runner id, whose runner must be online and up to date. It is a sparse disk image in `macos-vm.noindex/drives/`, 64 GiB unless the organisation sets a drive size, and takes room only as it fills.
- Only a macOS VM run on that Mac attaches it, as a second disk at the drive's path (`/Users/agent/drive` or `/Users/agent/drives/<name>`). Its first run formats it as Mac OS Extended (journaled): a later run's VM did not mount an APFS one. A run on another runner, or of another sandbox kind, is refused naming the Mac.
- Turn on FileVault on a Mac that keeps disk drives: the guest does not encrypt them.
- A disk drive is in one run at a time. A second run on the Mac that asks for it while another holds it is refused when that run starts, not at Review.
- A run's VM holds its disk drive for up to about 80 seconds after the run shows as ended, usually half a minute. A run that asks for the drive meanwhile waits for it on the Mac.
  - The runner tries every 3 seconds for at most 90 seconds in all, and the run goes on as soon as the drive is free.
  - A drive held by a live run of the same runner is refused at once. Anything else holding it is waited for: a run that just ended, another login's VM, or a `drive open` ([Opening a drive on the Mac](#opening-a-drive-on-the-mac)).
  - After 90 seconds the run fails with "Waited 90 seconds for the drive to be released." and the sentence below.
  - The sentence: "`<drive>` is in another macOS VM on this Mac. If it is open there with drive open, close it there, then start the run again. Otherwise a run that just ended may still be releasing it: a disk drive is in one run at a time, so try again in a minute, or when that run ends."
- The guest finds a disk drive by its size, apart from the boot disk, because a macOS guest does not show the disk's identifier. It refuses two disks of one size.
- A macOS VM run attaches one disk drive, and a kept work folder counts as one. A run that names two is refused at Review and at create (`drive_one_disk`), whatever their sizes.
- Keeping the work folder of a macOS VM run keeps `/Users/agent/work` as a disk drive on the Mac the run used. The run's pool must hold one Mac; with several, it is refused. A run its Mac refused when it started removes its kept work folder drive.
- Deleting a disk drive asks its Mac to delete the image. While the Mac is offline the drive stays as deleting, and the image goes when the Mac reconnects. If the Mac is no longer registered, the image stays on it and only the record goes. A drive in a run is refused until that run ends.
- For up to 90 seconds after a person's last macOS VM run ends, deleting their disk drive is refused as a drive still being released: try again in two minutes.
- Wardyn does not encrypt the image: the runner's macOS login and root on the Mac can read it. It stays until the drive is deleted, and deleting it is not secure erasure (see [Backups, Spotlight and removal](#backups-spotlight-and-removal)).

### Opening a drive on the Mac

An agent wrote a disk drive's files, so treat them as untrusted. `wardyn-runnerd drive open <name>` opens a drive through a throwaway VM. It finds only a drive that a run on this Mac has attached: the runner keeps the drive's name beside its image when a run attaches it.

The Mac never mounts, checks or repairs the volume. It hands the file to the VM as a raw disk and reads a tar stream back, writing plain files. Opening the image yourself, by double-click in Finder or with `hdiutil`, makes this Mac's own system read a volume an agent could have crafted. Do neither with a drive an agent wrote.

1. Run `wardyn-runnerd drive open <name>` in Terminal as the Mac's user. A name the Mac does not know is refused. A name two drives share is refused with the object names to use instead.
2. Wait about a minute. The VM starts from the ready base with the drive and nothing else: no environment, secret, file or CA, and no agent.
3. The files are copied to `~/Wardyn Drives/<name>`. The command prints how many files and bytes it copied, and what it skipped.
4. Change the files. Press Return to write the changes back, or Ctrl-C to discard them. A second Ctrl-C stops at once.
5. The next run that uses the drive sees the written-back files, owned by `agent`.

- **Network.** The VM's network device is wired to one end of a socket pair that carries nothing, so the guest has nothing to reach by design. The command says the VM has no internet. That has not been seen on a real Mac.
- **What reaches the Mac.** Regular files and folders only. Links of both kinds, devices, pipes and set-id files are skipped and never followed. The closing line counts them, for example "Skipped 3: 2 links, 1 device."
  - Every file is written with mode 0644, so none can execute.
  - Wardyn sets the `com.apple.quarantine` attribute on every file and folder, which is what macOS reads to treat a file as downloaded. What macOS then does with it is not yet witnessed.
- **Limits on a copy.** At most 250,000 files and folders, and no more bytes than the Mac has free less 5 GiB. A name part over 255 bytes, a path over 1,024 bytes, an absolute name or a `..` ends the copy and removes it.
- **The folder.** A copy goes only into an empty folder, which the command makes private (mode 0700) under your home. A link in its place is refused. The folder stays after the command ends.
  - A lock file, `.<name>.lock`, beside the folder keeps a second `drive open` of the same name out while the first runs. The lock goes with the command that holds it, even if that command is killed.
- **A partial copy.** If the VM's `tar` could not read some items, the copy is kept and the command says how many are missing and what the VM said. Write back then replaces nothing, and `--delete` removes nothing, that the copy does not hold.
  - A damaged listing, a limit passed or Ctrl-C before the copy ends removes the copy and writes nothing.
- **Writing back.** New and changed files go back through the VM as `agent`. A name the copy never held is never overwritten. `--delete` also removes from the drive the copied files you deleted, and no other file. Links you made in the folder are skipped.
- **A slot and the lock.** The VM takes one of the Mac's VM slots and the drive's lock while it is open. A run that wants the drive waits, then fails as above. A run the Mac then has no VM room for is refused, not queued: "has no room for another macOS VM right now".
- **Record.** A drive open leaves no row in the organisation's audit log. The command logs when a drive opens and closes, with its result, in `drive-open-cli.log` in `macos-vm.noindex/`, beside the helper's `drive-open.log`.
- Built and unit-tested; not yet run on a real Mac. The first runs must still show that the drive mounts in the VM, that the copy arrives, that `xattr` reads the quarantine attribute back, and that the VM's slot frees when it ends.

## Not yet tested on a real Mac

- Unit tests and a fake engine cover these. None has run against a real macOS guest, and the test summary lists them as untested, never as passed.
- A hosted Mac in CI builds, vets and tests the helper and the guest agent and signs the helper, but it boots no macOS guest.

| Not yet witnessed | What depends on it |
|---|---|
| Apple's Screen Sharing answers port 5900 with an RFB banner on the provisioning boot, before any VNC password is set | Stage 1's proof that the guest is live. Without the banner stage 1 fails closed |
| Port 22 is refused with a reset, not a dropped connection, once Remote Login is off | The proof that Remote Login is off. A silent drop reads as not refused and fails closed |
| Each stage-1 command and setting name, and the auto-login password file format, on macOS 26 and 27 guests | Stage 1 as a whole. A wrong name fails the step and publishes no base |
| `defaults -currentHost write com.apple.screensaver idleTime` run as `agent` through sudo, before `agent` has ever signed in | The run desktop without the lock screen. Written for the prepare's host identity, not a run. Whether this alone ends the lock screen is not yet known. A refusal fails stage 1 and publishes no base |
| The setup window opens on the Mac, comes to the front over other apps and shows its three steps, shows the guest and takes its keyboard and mouse, on macOS 26 and 27 | Setup by hand. Without it nobody can finish the setup, and preparing stops after its hour |
| Prepare refused over SSH while someone is signed in at the Mac's desktop (the runner's launchctl session check) | The refusal before the download. Without it, a prepare over SSH reaches setup by hand and cannot show the window |
| The helper's desktop check (CoreGraphics' session) refuses on a Mac with nobody signed in at its desktop | The helper's own refusal before AppKit starts. Over SSH while someone is signed in, it likely passes |
| A prepare run with sudo from Terminal reaching the signed-in desktop's window | Nothing today: setup by hand refuses root. Allowing root would need this |
| A macOS 26 Mac boots its installed guest into Setup Assistant with no start options | The macOS 26 prepare |
| `systemsetup -setremotelogin off` runs over SSH without Full Disk Access | Stage 1's Remote Login step. A refusal fails it and publishes no base |
| A macOS 26 guest's agent reaches the runner over the virtual socket | Every run on a macOS 26 base |
| A rebooted or reset guest sends a new DHCP request | The same-boot half of the Remote Login proof |
| macOS's auto-login copy of the setup password is overwritten by stage 1 | Whether the guest holds that password in `/etc/kcpassword` |
| The Command Line Tools install through the seven allowed hosts and no other | Stage 2's tools. Without them the Mac offers only the user's own tools |
| A real stop of a stage-2 boot partway through an install | That the VM ends, its socket folder goes and no base is published |
| A socket bind updates the socket folder's modification time on APFS | The one-minute grace before a socket folder can be swept as a gone run's |
| A real `wardyn-proxy` that is attached through standard input only and watched for an early exit | The prep proxy that stage 2 depends on |
| The VM has exactly one network device of the expected kind | The guest's only route being its own relay |
| A VM shut down from inside the guest frees its slot | The two-VM count |
| A full network buffer on the guest's side drops only the frames it refuses, and the guest's network stays up | The guest's network under a large download, such as the Command Line Tools. TCP resends what was dropped |
| The guest's `mount -t virtiofs` accepts `nosuid`, `nodev`, `noowners` and `rdonly` | Every shared folder. A refused option fails the run before it starts |
| With `noowners`, the agent can read and write files on a shared folder that belong to the Mac's login | Writable folders, and reading a folder whose files are not world-readable |
| A folder shared read-only refuses writes from the guest | Read-only folders |
| Without Full Disk Access for the guest agent, its listing of a share as `agent` waits as a run's read of a file does, and with it the listing answers | Folder shares: the refusal that names the fix. If the listing does not wait, a run can still hang on its first read |
| A Full Disk Access grant made in `edit-base` survives the recheck and reaches each run's clone | Folder shares on every run |
| `gh` and `kubectl` against a host marked Inspected, in a macOS VM run | Whether they fail TLS as [the table](#apps-that-use-only-the-system-trust-store) says |
| A `ca` item trusted by hand in `edit-base` stays trusted in each run's clone | Apps that use only the system trust store, on Tunnel only hosts behind a TLS-inspecting proxy |
| The runner refuses a share of a whole home folder, /Users or /private/var on a real Mac, before the VM starts | Folders: what a run may share |
| The VM's share is allowed into a folder macOS protects once the runner is allowed into it | Protected folders. The runner's own check passes, and the share could still read empty |
| Removable volumes and iCloud Drive are allowed in the same Files and Folders pane | The fix the refusal names |
| A link inside a shared folder is followed in the guest, not on the Mac | What a shared folder can reach |
| A slow or evicted volume answers the runner's check within ten seconds | A slow volume is otherwise refused with the privacy sentence, whose advice then does not apply |
| A guest that goes away is noticed by the helper's network relay | The relay ending instead of waiting; on Linux it ends at its next send |
| A release's guest agent carries the same version stamp as its helper | The helper's check that the guest runs its own agent. Both are built in one step from one stamp, but nothing reads the agent's stamp back |
| `wardyn-macvm probe` reporting the guest agent as `ok` on a release build | That the agent beside the helper is the release's own. Probe compares the two builds' commit and module version; a release build records no stamp to compare, so `ok` there means same commit, not same release stamp |
| A macOS VM run's Inspected and Tunnel only marks match what its proxy does, checked host by host from inside the guest | The run's Policy tab and `wardyn run policy` on a macOS VM run |
| A run's VM boots the installed guest, and two VMs run on one Mac at once | Running a macOS VM, and the two-VM count |
| A throwaway VM mounts a drive an agent wrote, its `tar` copies the files out, and its slot frees when it ends | `drive open <name>` |
| The attribute `drive open` sets (`com.apple.quarantine`) reads back with `xattr`, and macOS treats the file as downloaded | Whether a file copied from a drive is flagged on the Mac |
| The throwaway VM reaches no network, and neither does the `edit-base` copy | The "no internet" lines of `drive open` and `edit-base` |
| A background run's helper drops all input in a boot started with the forced view, on a real macOS guest | Watching a background run's desktop |
| The Full Disk Access probe answers allowed and refused correctly, and `su wardyn-admin` works from the auto-login session in the edit window | `edit-base`'s Full Disk Access read, and its administrator steps |

- **Every other check that needs a real Mac** has also not run:
  - the signed helper running from the package;
  - cold prepare and its timings, and an interrupted download or install;
  - stage-2 tools, and the hosts the prep proxy allowed and refused;
  - warm start, and the agent session and terminal over the virtual socket;
  - the network, allowed and refused, and its throughput;
  - the desktop's control, recording and clipboard;
  - the cleanup boundary and the proxy certificate matrix;
  - the cap, slot release and two logins;
  - a runner restart and a package upgrade with VMs live;
  - a wedged guest, the disk bound, heat and power, and teardown;
  - the managed-Mac gate, the guest clock and distinct machine identities.

## Upgrades

- An upgrade of the runner package does not repeat the macOS install.
- A runner set up with `wardyn-runnerd setup` keeps its own copies of `wardyn-macvm` and `wardyn-guest-agent` beside it. `wardyn runner upgrade` replaces only `wardyn-runnerd` and says so; to update all three, install the new `wardyn-runnerd.pkg` and run `/usr/local/wardyn/bin/wardyn-runnerd setup` again with the same flags. `install.sh --uninstall` removes all three.
  - A LaunchAgent of your own that runs `/usr/local/wardyn/bin/wardyn-runnerd` needs no rerun. The package installs the helper and guest agent in that folder, and the runner looks beside its own binary.
  - If that LaunchAgent's plist sets `WARDYN_PROXY_IMAGE`, reload it with `launchctl bootout` and then `launchctl bootstrap` after you change the value. Reported on one Mac: `launchctl kickstart -k` keeps launchd's cached environment, so the run proxy stays on the old image.
- When the guest agent in the base differs from the runner's, the runner runs a tools update: it clones the base, boots it, applies the new agent and pins, and keeps the result. It takes one slot like any VM, for minutes.
- A base the new runner cannot accept is marked "update needed", and the Mac offers no macOS VMs until it is re-prepared (`prepare --new-image`).

## Remove it

1. Stop the Mac's runs, or wait for them to end.
2. Run `wardyn-runnerd macos-vm remove`. It deletes the base and the downloaded image under `macos-vm.noindex/`. It does not touch the ledger files or the Time Machine exclusion.
3. Unset `WARDYN_RUNNER_MACOS_VM` (or the `MacOSVM` preference) and restart the runner.

## When a Mac stops offering macOS VMs

| What you see | Cause and fix |
|---|---|
| Not offered; the runner says macOS 26 is needed | The Mac runs macOS 15 or older. Upgrade it to macOS 26 or later |
| No Mac in the pool offers macOS VMs | No runner in the pool has a ready base, or the pool mixes runners that do not. Run `status` on each Mac |
| Not offered after a restart | Nobody has logged in since the restart, and FileVault is on |
| The runner says the ledger is missing or damaged | Reinstall the runner package |
| The runner says it has no macOS VM helper | It was set up with `wardyn-runnerd setup` from an older package, or the package is missing. Install the runner package, then run `wardyn-runnerd setup` again from it. `prepare` refuses with the same words until then |
| The runner refuses macOS VMs and names a socket folder | Something other than the login's own `0700` folder is at that name under `/Users/Shared/Wardyn/sock/`. Remove it as root ([The shared ledger](#the-shared-ledger)) |
| Installing the package failed and macOS VMs stay off | A login held the admission lock for over 10 seconds. Reinstall the package |
| The runner says the ledger is busy | Another account holds the lock, or the ledger is crowded with entries. See [The shared ledger](#the-shared-ledger) |
| A third VM refused | The Mac already runs two, or preparing holds one |
| `max` is 0 | Docker's memory leaves no budget, the Mac has too little free disk, or a per-run limit is below the base's minimum ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)) |
| A run ended because the Mac ran low on disk | Free space fell below 5 GiB, possibly held by snapshots ([Disk](#disk)) |
| The runner says the Mac needs a restart | The framework still holds a slot. Restart the Mac |
| A newer macOS is out | Run `wardyn-runnerd macos-vm prepare --new-image` ([Preparing the base image](#preparing-the-base-image)) |
| Preparing waits | For AC power, or up to 2 minutes for Docker to start; then preparing stops. Run `wardyn-runnerd macos-vm prepare` again once Docker is running. A tools update's wait for Docker is not reported in 0.10.0 |
| Preparing is waiting for setup by hand | Finish it once in the window "Wardyn: set up this macOS VM" ([Setup by hand](#setup-by-hand)). Preparing waits up to an hour |
| `prepare` says setup by hand needs this Mac's desktop | It ran over SSH, outside the signed-in desktop, or with sudo. Run it in Terminal on the Mac, signed in at its desktop |
| Preparing stopped at the Remote Login check | Port 22 did not refuse, or the guest was not shown live on the same boot. No base was published; run `prepare` again |
| `edit-base` says the base was prepared before Wardyn kept its admin password | The base comes from an older release, and macOS will not let root change its setup account's password without the old one. Run `wardyn-runnerd macos-vm prepare --new-image`, then `edit-base` again |
| A tools update stops removing a CA, saying macOS 26 can refuse that | The update stops, and the old base stays ready and still trusts the old CA. Change the list as you want it and run `prepare --reprepare`, which builds a base from it |
| `edit-base` says the guest did not take the one-time code | The setup account's password in the guest is not the one Wardyn keeps, or was changed in the session. Runs keep the old base. If it fails again before the window opens, run `prepare --new-image` |
| The runner logs that it offers none and names `--per-run-memory` or `--per-run-cpu` | A per-run limit is below the base's minimum. Raise it ([How many VMs a Mac offers](#how-many-vms-a-mac-offers)) |

The runner drawer shows most of these from fixed codes and numbers the runner reports. Whether someone is logged in goes only as yes or no, and no user or account name is sent. In an error, the runner's home folder shows as `~`.

## What the host can see

| On the Mac | Who can use it |
|---|---|
| `macos-vm.noindex/runs/<run>/` (disk, auxiliary storage, `run.json`, helper log) | The logged-in user only (mode `0700`) |
| `/Users/Shared/Wardyn/sock/u<uid>/<run-key>/` (control, agent and desktop sockets) | The logged-in user only (mode `0700`) |
| `macos-vm.noindex/bases/` and `cache/` | The logged-in user only |
| `macos-vm.noindex/prepare.log` | The logged-in user. No password or secret value is logged |
| `macos-vm.noindex/setup-code.json` | The logged-in user only (mode `0600`). It exists only while macOS waits for setup by hand or an `edit-base` session. The window can show its code on a click |

- The helper runs as the logged-in user, never root, with one entitlement: to use Apple's virtualization. It has no networking entitlement.
- The VM listens on no TCP port on the Mac, and the proxy's credential injection never leaves Docker.

## The run record

- Migration `0234_macos_vm_kind` widens `agent_runs.machine_kind` to allow `macos_vm`, and the desktop sign-in `store` column to allow `run_disk`. It changes no row.
- A run records its guest macOS build (`guest_os`) and the Mac's (`host_os`) in `vm_machine`.
- A run's page says whether the Mac removed the run's directory, with the VM's disk and clone, or why it could not. The runner sends fixed codes, which migration `0236_macos_vm_disks` stores.
- A runner older than this release sends no such report. Its run's page says "Removing the macOS VM's disk from this Mac…" for 10 minutes after the run ends, then nothing about the disk.
