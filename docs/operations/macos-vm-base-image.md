> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Preparing and customising a macOS VM base image

- This page is for the operator of the Mac: building the base image once, setting up the guest by hand on macOS 26, editing the base, and listing what goes into it.
- Requirements and switches are in [macOS VM sandboxes on a Mac](macos-vms.md).

## Preparing the base image

`wardyn-runnerd macos-vm prepare` builds the Mac's base image. It runs in the background and takes hours. On macOS 27 it is unattended; on macOS 26 it stops once for [setup by hand](#setup-by-hand). It runs once per Mac, and a newer macOS needs a re-prepare (see Newer macOS below).

- It sizes its VM at the runner's per-run limits and never raises them. A runner that has connected to an organisation that sets them has them saved, and `prepare` uses them. Before that, if the organisation sets none, or to use others, pass `--per-run-cpu` (CPUs) and `--per-run-memory` (GB); a flag wins over the saved limit.
- `--keep-ipsw` keeps Apple's image after the base is ready, and `--new-image` is the re-prepare.
- `--ipsw <file> --sha256 <hex>` builds from a restore image you downloaded, for a network where Apple's download fails (see Network in [Before you start](macos-vms.md#before-you-start)). `--sha256` is required; take the SHA-256 from a source you trust. It never asks Apple which image is newest.
- `--ipsw-url <https URL> --sha256 <hex>` downloads from your own mirror instead, in the same resumable ranges. The mirror must answer HTTP Range requests, else prepare says so. With the flag, the hash is `--sha256` alone, never the profile's. It never falls back to Apple; an image with another hash is deleted and refused.
  - A managed Mac sets the mirror in the profile (`MacOSVMImageURL`, `MacOSVMImageHash`) or `runnerd.json` (the profile wins), and setup's prepare uses it. The Mac trusts the mirror's certificate through its own keychain.
- Prepare copies the file into its cache, never linking or moving it, and uses the copy only when its hash matches, before anything loads or installs it. A mismatch is refused with both hashes and the copy removed. Your file is never changed or deleted.
- With a base ready, prepare does nothing and does not use the file; `--new-image` builds the new base from it. The copy is deleted once the base is ready, unless `--keep-ipsw` is set.
- A restore image in the cache is reused only when a previous prepare's download recorded its SHA-256 (a `.sha256` file beside it) and the file still matches. The record is written only for an image Wardyn downloaded over the pinned connection, resumed only from its own partial file, or copied with `--ipsw` after its hash matched.
- Any other file there (placed by hand, or no longer matching its record) stops prepare with an error naming it. Prepare neither uses nor deletes it: move it out and pass it with `--ipsw` and `--sha256`, or delete it. The record guards against hand-placed or damaged files, not against someone with the runner user's write access.
- `wardyn-runnerd setup` starts the same preparing itself with the limits it just registered, on a first registration unless the base is ready. Given `--keep-ipsw`, setup passes it on to that prepare. A rerun prints the prepare command instead.

| Step | What happens |
|---|---|
| 1. Check | The host, Docker, free disk ([Disk](macos-vm-host.md#disk)) and AC power. A failed check stops with the fix |
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
  - The UI gateway and the Mac's helper each drop keys, pointer moves and paste, so that input never reaches the guest.
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
- The window needs the Mac's desktop. On macOS 26, `prepare` and the prepare that `setup` starts refuse at once, before any download, when started over SSH, outside the signed-in desktop, or with sudo.

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

## Updating the tools

`wardyn-runnerd macos-vm update-tools` starts a tools update of the ready base now: the update a stale guest agent or a changed install list starts by itself.

- It runs in the background; see `tools-update.log` and `status`. It updates even when the base looks current.
- Run it in Terminal on the Mac with the runner serving. It refuses without a ready base, and while a prepare, a tools update or edit-base runs.
- A tools update starts only on AC power. While it waits, `status` and `doctor` say "tools update waiting for AC power".

## Editing a base

`wardyn-runnerd macos-vm edit-base` boots the ready base once, in a window, for an attended session. Use it to turn on Screen Sharing with control when `status` says `view only` (on a macOS 26 base the desktop stays view only, [#3901](https://github.com/cjohnstoniv/wardyn/issues/3901)), or to change the guest by hand. Runs keep using the current base until the edited one passes its checks.

1. Run `wardyn-runnerd macos-vm edit-base` in Terminal on the Mac, signed in at its desktop. Not over SSH, and not with sudo.
2. Wardyn copies the base and, in a boot with no window, changes the password of `wardyn-admin` from the one the base keeps to a fresh one-time code. The code goes to `setup-code.json`, readable by you only. The window types it or shows it on a click.
3. The command's first line says it edits a copy of the VM base, not this Mac. It then prints that the copy takes about 2 minutes to start, and the copy boots in a window.
   - It says the code is `wardyn-admin`'s password. Click Type code in the window to type it at a prompt, or Show code to read it.
   - The copy is meant to have no internet during the session.
   - **Who you are signed in as.** The base turns on auto-login for `agent`, so the copy may open at `agent`'s desktop, not at a login window. As `agent` you have no `sudo` and cannot read `/Library/Wardyn/stage`.
   - **Administrator steps.** Click Open admin Terminal in the window. It opens Terminal in the guest at `su - wardyn-admin`: click Type code, then press Return.
     - By hand, open Terminal (Spotlight may not answer in the guest; use Finder > Go > Utilities), run `su wardyn-admin` and give the one-time code as the password.
   - **If the window opens on macOS's setup pages.** Continue without a network, as in setup by hand. The recheck refuses any account Wardyn did not make, so make none.
4. Make your changes. For desktop control, open System Settings > General > Sharing and turn on Screen Sharing. Under its (i) button, turn on "VNC viewers may control screen with password".
   - For folder shares, click Full Disk Access in the window. It opens System Settings at Full Disk Access in the guest and shows the guest agent in Finder: drag `/usr/local/libexec/wardyn-guest-agent` into the list and turn it on.
     - By hand, click +, press Command-Shift-G, type that path, and turn it on.
   - The grant is meant to stay in the base and reach every run's guest, where Full Disk Access lifts macOS's privacy prompts for every process the run starts.
   - Grant it once. Wardyn signs the guest agent on this Mac, so the grant can last through tools updates. The first tools update after an upgrade changes the signature: grant it once more.
   - The base trusts the signing certificate for code signing only. When the base could not trust it (macOS 26 can refuse), the Terminal where you started `edit-base` lists the "guest agent signing certificate" with a `-p codeSign` command, and Trust the list's CAs includes it.
   - A certificate that still reads `not_trusted` is listed in the Terminal where you started `edit-base`, with its name, subject, SHA-256 and the exact `sudo security add-trusted-cert` command.
     - Click Trust the list's CAs in the window. It opens Terminal in the guest with one such command per certificate, and each asks for the code.
     - Check each subject and SHA-256 and confirm macOS's prompt. With nothing to trust the button does nothing and says so.
   - **Rosetta is off unless you press Install Rosetta**, for Intel-only tools. It runs Apple's installer in the guest through the prep proxy ([The prep proxy](#the-prep-proxy)).
     - A TLS-inspecting proxy must exempt Apple's hosts (Apple support article 101555).
     - From macOS 26.4, an Intel app's first launch shows an end-of-Rosetta notice. macOS 27 is the last release with full Rosetta.
   - **FileVault stays off.** The window and the recheck warn when it reads on. Turn it off in System Settings > Privacy & Security. The base is kept either way.
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
  - `status` also says how the base's guest agent is signed: on this Mac, or ad hoc, which loses Full Disk Access at each tools update. A signing certificate the base does not trust yet is named, with the instruction to run `edit-base`.
  - After a tools update, `status` can say Full Disk Access is unknown. Recheck it in `edit-base`.
  - `doctor` shows a `file-vault` line when the base last read FileVault as on. It is never a failure.
  - `status` reads the base's `tools.json`: `agent_signature` (`this_mac` or `ad_hoc`), `agent_signing_cert` (`trusted` or `not_trusted`), `agent_signer` (`wardyn`, `codesign` or `ad_hoc`), `agent_sign_errors` (path-free) and `guest.full_disk_access`. `doctor`'s folder-shares stage reads the first and the last.

## The prep proxy

- Stage 2 runs on its own Docker network, made for the job. Its only way out is a prep-only `wardyn-proxy` that allows Apple's software-update hosts on port 443 and nothing else: `swscan.apple.com`, `swcdn.apple.com`, `swdist.apple.com`, `swdownload.apple.com`, `updates.cdn-apple.com`, `mesu.apple.com`, `gdmf.apple.com` and `configuration.apple.com`.
- The proxy holds no credential and does not inspect TLS. The tunnel is opaque, so what is enforced is the host name and port 443. A first use of any other host is denied.
- The network, proxy and relay are removed when the job's helper exits.
- **A crash leaves none behind.** At start, before any job gets a network, the runner removes the prep containers and networks that a crash left for its own runner and deployment. A job still in the ledger keeps its own.
  - It logs how many it removed. A sweep that cannot run is logged as a warning and does not stop the runner.
  - At start the runner also deletes the staging copy that a crashed tools update or re-prepare left, unless that update is still running.

## Customising the base

An operator can have prepare install packages, apps, files, configuration profiles and a CA into the base, as admin.

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

The `ca` item trusts your organisation's root in the guest at prepare. It is separate from the per-run proxy CA ([Credentials and sign-ins](macos-vm-host.md#credentials-and-sign-ins)).

The file must be an X.509 v3 CA certificate: its `basicConstraints` say `CA:TRUE`. Wardyn reads each `ca` file when it reads the list, and a file that is not one stops `prepare` and a tools update before anything downloads, saying why. A v1 certificate has no `basicConstraints`, so it is refused. macOS's own `openssl req -x509` writes a v1 certificate unless you give it a v3 extensions section that sets `basicConstraints = CA:TRUE`.

1. The guest agent runs `security add-trusted-cert -d -r trustRoot -p ssl` on the certificate as root: SSL only, never every policy. It does not change the `com.apple.trust-settings.admin` authorization rule; macOS 26 refuses root that change.
2. `security verify-cert -p ssl` reads the trust back. `tools.json` records `trusted` or `not_trusted` with the certificate's SHA-256 and SHA-1 fingerprints.
3. Stage 2 goes on either way: a CA that reads `not_trusted` does not stop preparing.

- `not_trusted` means macOS refused a trust without a person. On macOS 26 expect this: Apple asks a person to approve a new admin trust. Trust it by hand in `edit-base`: in Terminal, run `sudo security add-trusted-cert -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain /Library/Wardyn/stage/<name>/item.cer` and answer macOS's prompt.
  - `edit-base` prints this command for each such item, with its subject and SHA-256, and runs nothing itself. The Trust the list's CAs button in its window opens a Terminal that runs them, as the admin.
  - A certificate trusted by hand reaches hosts that need only the system trust store. A host marked Inspected still needs the run's proxy certificate.
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

### MDM templates

`deploy/macos-guest/mdm/` holds two profile templates for organisations with MDM. Copy one and replace each `__PLACEHOLDER__`.

| Template | Does | How it installs |
|---|---|---|
| `setup-assistant.mobileconfig.template` | Skips the macOS 26 Setup Assistant panes (`SkipSetupItems`) | By hand or MDM. As a `profile` item, one approval in `edit-base` installs it, and the base keeps it |
| `full-disk-access.mobileconfig.template` | Grants the guest agent Full Disk Access (a TCC payload) | MDM only |

- `__AGENT_PATH__` is `/usr/local/libexec/wardyn-guest-agent`.
- `__CODE_REQUIREMENT__` is the agent's designated requirement, `identifier "com.wardyn.guest-agent" and certificate root = H"<SHA-1>"`, where the SHA-1 is that of the signing certificate the runner made on the Mac. `codesign -d -r- /usr/local/libexec/wardyn-guest-agent` in `edit-base`'s admin Terminal prints it after `designated => `. Each Mac has its own.
- `profiles install` can't add either profile since Big Sur. Without MDM, use System Settings.

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

A Mac can hold other bases beside its first, Standard. Each is derived from Standard and adds its own install list.

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

## What Wardyn sets in the base

Stage 1 writes these when it prepares a base. Every tools update writes them again, so a base prepared earlier gets them, and the stamps follow a guest macOS update. In stage 1 every row is skipped when it fails; the route skips any failed row. Skipped rows are listed in `tools.json` as `base_settings_skipped` and as a WARN by `wardyn-runnerd doctor`.

| Setting | Value | Account | Optional | Stage |
|---|---|---|---|---|
| `com.apple.SetupAssistant` `DidSee*` for 16 panes (Accessibility, ActivationLock, AppStore, AppearanceSetup, ApplePaySetup, CloudSetup, LockdownMode, Privacy, ScreenTime, SetupSequence, SiriSetup, SyncSetup, SyncSetup2, TermsOfAddress, TouchIDSetup, iCloudLoginForStorageServices) | bool true | agent | the six not seen on a real guest: Accessibility, AppearanceSetup, Privacy, ScreenTime, SetupSequence, iCloudLoginForStorageServices | 1 and 2 |
| `DidSeeAppleIntelligence` | bool true | agent | yes | 1 and 2 |
| `LastSeenCloudProductVersion` | string, `sw_vers -productVersion` | agent | no | 1 and 2 |
| `LastPreLoginTasksPerformedVersion`, `LastSeenDiagnosticsProductVersion`, `LastSeenAgeRangeSelectionProductVersion` | string, product version | agent | yes | 1 and 2 |
| `LastPreLoginTasksPerformedBuild`, `LastSeenBuddyBuildVersion` | string, `sw_vers -buildVersion` | agent | yes | 1 and 2 |
| `MiniBuddyLaunchReason` | int 0 | agent | yes | 1 and 2 |
| `MiniBuddyShouldLaunchToResumeSetup`, `SkipFirstLoginOptimization`, `SkipExpressSettingsUpdating` | bool false, true, true | agent | yes | 1 and 2 |
| `com.apple.HIToolbox` `AppleFnUsageType` (the Fn key does nothing, so no Dictation prompt) | int 0 | agent | yes | 1 and 2 |
| `NSQuitAlwaysKeepsWindows` (global) | bool false | agent | no | 1 and 2 |
| `com.apple.loginwindow` `TALLogoutSavesState` (per host) | bool false | agent | yes | 1 and 2 |
| `mdutil -a -i off` (Spotlight indexing) | off | root | yes | 1 and 2 |
| `DiagnosticMessagesHistory.plist` `AutoSubmit`, `ThirdPartyDataSubmit`, with mode 644 and group admin | bool false | root | yes | 1 and 2 |
| Setup Assistant and Terminal ended, Terminal's saved state removed | by signal, then `rm -rf` as the agent | agent | no | 2, and edit-base's finish for Terminal |
| Safari `WarnAboutFraudulentWebsites` (Safe Browsing) | bool false, read back once | agent | yes | 2 |

- The version and build values come from the guest's `sw_vers`. A value outside `^\d+\.\d+(\.\d+)?$` or `^[0-9]{2}[A-Z][0-9]{1,4}[a-z]?$` skips the whole table.
- The agent's rows run in the console session as the agent. With nobody at the console, or another account there, they are skipped as "nobody at the console" or "the console is not the agent account's", and the next update applies them.
- The first update after an upgrade is answered by the base's old guest agent, which has no route for this: `tools.json` says "agent predates this setting; run `wardyn-runnerd macos-vm update-tools` again". Run that command twice; it updates even when the base looks current.
- Rows are read back from disk with `plutil -p`. A key that did not land is skipped as "not found when read back". Key names other than the `DidSee*` panes a real guest listed and the documented global and `mdutil` rows come from community references and are unverified on macOS 26.

## Apple's boot-time hosts

- A macOS guest calls a few Apple hosts in the background at boot. Each would otherwise wait for an approval nobody can usefully give.
- A run denies them quietly: no approval is raised, and nothing on the list is ever allowed.

| Host | What the guest uses it for |
|---|---|
| `gdmf.apple.com` | Software-update catalogue checks |
| `mesu.apple.com` | Software-update asset metadata |
| `gateway.icloud.com` | iCloud background services |
| `init-kt.apple.com` | Key transparency lookups |
| `mask.icloud.com`, `mask-h2.icloud.com` | iCloud Private Relay |
| `proxy.safebrowsing.apple` | Safari Safe Browsing |

- The list holds exact names, never wildcards. Push courier hosts, Apple News hosts and Apple addresses reached by IP are not on it: a run records a denial for each.
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
