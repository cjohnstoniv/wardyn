> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Troubleshooting macOS VM sandboxes

- This page is for whoever is paged when a Mac stops offering macOS VMs or a run fails: where the logs are, what each symptom means, and what has not run on a real Mac.
- Requirements and switches are in [macOS VM sandboxes on a Mac](macos-vms.md).

## Where to look when it fails

- **The guest agent has no log file.** Its launchd job, `com.wardyn.guest-agent`, names no output path, and the agent writes its messages with Go's `slog` to standard error. Wardyn sets up no place for them on the guest's disk. The lines it writes, such as `screen lock left as it was`, are not kept in a file Wardyn names.
- **The Mac-side record.** `helper.log` is in `~/Library/Application Support/Wardyn/macos-vm.noindex/runs/<run-key>/` on the Mac.
- **First checks.** `wardyn-runnerd macos-vm status` on the Mac, then `wardyn window <run> --status`.

## When a Mac stops offering macOS VMs

| What you see | Cause and fix |
|---|---|
| Not offered; the runner says macOS 26 is needed | The Mac runs macOS 15 or older. Upgrade it to macOS 26 or later |
| No Mac in the pool offers macOS VMs | No runner in the pool has a ready base, or the pool mixes runners that do not. Run `status` on each Mac |
| Not offered after a restart | Nobody has logged in since the restart, and FileVault is on |
| The runner says the ledger is missing or damaged | Reinstall the runner package |
| The runner says it has no macOS VM helper | It was set up with `wardyn-runnerd setup` from an older package, or the package is missing. Install the runner package, then run `wardyn-runnerd setup` again from it. `prepare` refuses with the same words until then |
| The runner refuses macOS VMs and names a socket folder | Something other than the login's own `0700` folder is at that name under `/Users/Shared/Wardyn/sock/`. Remove it as root ([The shared ledger](macos-vm-host.md#the-shared-ledger)) |
| Installing the package failed and macOS VMs stay off | A login held the admission lock for over 10 seconds. Reinstall the package |
| The runner says the ledger is busy | Another account holds the lock, or the ledger is crowded with entries. See [The shared ledger](macos-vm-host.md#the-shared-ledger) |
| A third VM refused | The Mac already runs two, or preparing holds one |
| Both VM slots stay taken after an upgrade or a restart loop | At start the runner ends old `update-tools` and `prepare` helpers that never claimed a slot (a live VM's is kept) and logs "macOS VM: ended a helper a previous runner left". One it can't read stays: end it by hand |
| `max` is 0 | Docker's memory leaves no budget, the Mac has too little free disk, or a per-run limit is below the base's minimum ([How many VMs a Mac offers](macos-vm-host.md#how-many-vms-a-mac-offers)) |
| A run ended because the Mac ran low on disk | Free space fell below 5 GiB, possibly held by snapshots ([Disk](macos-vm-host.md#disk)) |
| The runner says the Mac needs a restart | The framework still holds a slot. Restart the Mac |
| A newer macOS is out | Run `wardyn-runnerd macos-vm prepare --new-image` ([Preparing the base image](macos-vm-base-image.md#preparing-the-base-image)) |
| Preparing waits | For AC power, or up to 2 minutes for Docker to start; then preparing stops. Run `wardyn-runnerd macos-vm prepare` again once Docker is running. A tools update's wait for Docker is not reported in 0.10.0 |
| Preparing is waiting for setup by hand | Finish it once in the window "Wardyn: set up this macOS VM" ([Setup by hand](macos-vm-base-image.md#setup-by-hand)). Preparing waits up to an hour |
| `prepare` says setup by hand needs this Mac's desktop | It ran over SSH, outside the signed-in desktop, or with sudo. Run it in Terminal on the Mac, signed in at its desktop |
| Preparing stopped at the Remote Login check | Port 22 did not refuse, or the guest was not shown live on the same boot. No base was published; run `prepare` again |
| `edit-base` says the base was prepared before Wardyn kept its admin password | The base comes from an older release, and macOS will not let root change its setup account's password without the old one. Run `wardyn-runnerd macos-vm prepare --new-image`, then `edit-base` again |
| A tools update says "A tools update of this macOS base is already running." | Another tools update or a prepare holds the base's update lock. The second update now fails at once instead of waiting. Wait for the running one to finish, then run it again |
| A tools update stops removing a CA, saying macOS 26 can refuse that | The update stops, and the old base stays ready and still trusts the old CA. Change the list as you want it and run `wardyn-runnerd macos-vm prepare --new-image`, which builds a new base from it |
| `edit-base` says the guest did not take the one-time code | The setup account's password in the guest is not the one Wardyn keeps, or was changed in the session. Runs keep the old base. If it fails again before the window opens, run `prepare --new-image` |
| After an upgrade, `wardyn-runnerd doctor` says "waiting for the proxy image; next try at <time>" | The proxy image could not be pulled (not published, no access, or network); the log says. It retries after about 15 s, 30 s, 1 and 2 minutes, then every 5 |
| The runner logs that it offers none and names `--per-run-memory` or `--per-run-cpu` | A per-run limit is below the base's minimum. Raise it ([How many VMs a Mac offers](macos-vm-host.md#how-many-vms-a-mac-offers)) |
| A macOS VM run stops at the login window: "the Mac guest never reached the agent's desktop: still at the login window" | The setup-assistant-quit switch signs the agent user out on macOS 26.6.2. Run `sudo rm -f /Library/Wardyn/setup-assistant-quit` in `edit-base`'s admin Terminal. This release's guest agent ignores the switch; a base with tools older than this release still acts on it. |
| A macOS VM run on a base with `/Library/Wardyn/carry-before-login` fails with "the macOS VM stopped while booting" | The hold's restart did not bring the guest back. On a test base, remove the two items from its list. On Standard, `edit-base` and the tools update can stop the same way; if they do, prepare Standard again |

The runner drawer shows most of these from fixed codes and numbers the runner reports. Whether someone is logged in goes only as yes or no, and no user or account name is sent. In an error, the runner's home folder shows as `~`.

## Not yet tested on a real Mac

- Unit tests and a fake engine cover these. No check that needs a real macOS guest has run, and the test summary lists them as untested, never as passed.
- A hosted Mac in CI builds, vets and tests the helper and the guest agent and signs the helper, but it boots no macOS guest.

| Not yet witnessed | What depends on it |
|---|---|
| Apple's Screen Sharing answers port 5900 with an RFB banner on the provisioning boot, before any VNC password is set | Stage 1's proof that the guest is live. Without the banner stage 1 fails closed |
| Port 22 is refused with a reset, not a dropped connection, once Remote Login is off | The proof that Remote Login is off. A silent drop reads as not refused and fails closed |
| Each stage-1 command and setting name, and the auto-login password file format, on macOS 26 and 27 guests | Stage 1 as a whole. A wrong name fails the step and publishes no base |
| `defaults -currentHost write com.apple.screensaver idleTime` run as `agent` through sudo, before `agent` has ever signed in | The run desktop without the lock screen. Written for the prepare's host identity, not a run. Whether this alone ends the lock screen is not yet known. A refusal fails stage 1 and publishes no base |
| `sysadminctl -screenLock off -password -` reading the password on stdin, run as `agent` through sudo in stage 1 and through `launchctl asuser` by the guest agent at Init | The run desktop without the lock screen. Whether it works outside `agent`'s own login is not yet known. A refusal, in stage 1 or at Init, is only logged |
| The setup window opens on the Mac, comes to the front over other apps and shows its three steps, shows the guest and takes its keyboard and mouse, on macOS 26 and 27 | Setup by hand. Without it nobody can finish the setup, and preparing stops after its hour |
| Prepare refused over SSH while someone is signed in at the Mac's desktop (the runner's launchctl session check) | The refusal before the download. Without it, a prepare over SSH reaches setup by hand and cannot show the window |
| The helper's desktop check (CoreGraphics' session) refuses on a Mac with nobody signed in at its desktop | The helper's own refusal before AppKit starts. Over SSH while someone is signed in, it likely passes |
| A prepare run with sudo from Terminal reaching the signed-in desktop's window | Nothing today: setup by hand refuses root. Allowing root would need this |
| A macOS 26 Mac boots its installed guest into Setup Assistant with no start options | The macOS 26 prepare |
| Whether per-host settings files copied into a run's new machine identity at boot, when the guest agent starts, keep Setup Assistant's Apple Account pane closed | The setup-assistant-carry switch, off by default. Without it the pane can stay up in a run's window |
| With no `autoLoginUser`, a macOS guest stays at the login window, and loginwindow reads it only as the boot starts | The sign-in hold's order: the carry runs before `agent` is signed in. Whether the carried files keep the pane closed is the row above |
| A restart started inside a macOS guest (`shutdown -r now`) restarts the VM in place, without ending the run or the `edit-base` window | The sign-in hold, off by default. If the VM stops instead, each run on a base with `/Library/Wardyn/carry-before-login` fails as stopped while booting |
| `defaults write` of `autoLoginUser` reaches the disk before a restart started right after it | The sign-in hold. A restart that loses it is retried, at most 3 in a row |
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
| A guest agent signed on this Mac keeps its Full Disk Access through a tools update | Folder shares after an update. `macos-vm status` shows what the base last read |
| The run's settings reach the window's apps (`launchctl setenv` as the user), and zsh reads the line in `/etc/zshenv` | [The run's settings in the window](macos-vm-use.md#the-runs-settings-in-the-window) |
| `gh` and `kubectl` against a host marked Inspected, in a macOS VM run | Whether they fail TLS as [the table](macos-vm-use.md#apps-that-use-only-the-system-trust-store) says |
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
