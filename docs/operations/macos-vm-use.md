> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Using a macOS VM sandbox

- This page is for the person who works in a macOS VM sandbox: the first run, keeping the machine, a window on the Mac, watching, recording, folders and drives.
- The Mac must be set up first: see [macOS VM sandboxes on a Mac](macos-vms.md).

## Quickstart: an interactive developer sandbox

An interactive macOS VM run is a sandbox you work in: a terminal, and a window on the VM on your own Mac. This is the path from a prepared Mac to a first run.

Needed first:

- The Mac turned on and registered as your own runner ([Turn it on](macos-vms.md#turn-it-on)). A window opens only on a Mac whose runner you registered.
- A ready base ([Preparing the base image](macos-vm-base-image.md#preparing-the-base-image)).
- Someone signed in at the Mac, with its screen unlocked ([Logins, FileVault and restarts](macos-vm-host.md#logins-filevault-and-restarts)).
- `macos_vm.allow_attended_window` on, for the window ([The site switch](#the-site-switch)).

1. Check the Mac: `wardyn-runnerd macos-vm status`. A run is placed on it only when the base is ready.
2. Start the run: `wardyn run --experience interactive --machine-kind macos_vm --window`. In the console, choose a virtual machine, then macOS, in New Run. `--macos-base <name>` picks a base other than Standard.
3. Wait until the run is `RUNNING` and its preparation is done. With `--window` the window opens once preparation finishes. If it does not, run `wardyn window <run>`.
4. Work in the window. Closing it leaves the run going ([Closing a window](#closing-a-window)).
5. For a terminal, run `wardyn attach <run>`, or `wardyn ssh <run>` to use SSH tools.
6. Ask whether a window can open, and why the last one closed: `wardyn window <run> --status`.

## Keeping the machine

A run's VM is deleted when the run ends. To keep it on your own Mac, an admin turns on `macos_vm.allow_kept_machines`, which is off by default. An idle machine is deleted after `macos_vm.kept_machine_idle_days` (30 by default, 1 to 90). `macos_vm.max_kept_machines_per_person` caps how many one person keeps (1 by default, 1 to 4).

| To | Run |
|---|---|
| Keep this run's machine when it stops | `wardyn run --experience interactive --machine-kind macos_vm --keep-machine`, with `--machine-name <name>` to name it |
| Start a run from a machine you kept | `wardyn run --experience interactive --machine-kind macos_vm --resume-machine <id\|name>` |
| Start from it again, from the Mac's current base | The same, with `--fresh`. This deletes what is on the machine |
| List your kept machines | `wardyn machines` (`--json` for JSON) |
| Delete one, with its files, tools and sign-ins | `wardyn machines delete <id\|name>`. It asks first; without a terminal, add `--yes` |
| List, update or delete one from the Mac itself | `wardyn-runnerd macos-vm machines`, with `update <id>` or `delete <id>` |

- In the console, New run and Review offer keeping the machine or resuming a kept one. The run page's Machine card shows the machine as it is now, and Your settings lists your kept machines with their idle days.
- A resumed machine is kept again when the run stops. A machine a run holds is in use and cannot be deleted.
- A run that keeps its machine is interactive: its startup command runs when someone attaches, and until then the run records "awaiting attach" (`run.interactive.start`). On a macOS VM the repo clone and harness setup still run at start.
- `--keep-machine` and `--resume-machine` do not go together, `--machine-name` needs `--keep-machine`, and `--fresh` needs `--resume-machine`. None of them works with `--file`.
- Kept machines are folders in `macos-vm.noindex/kept/` on the Mac. A resume of a machine whose base is older than `macos_vm.max_base_age_days` is refused (`kept_machine_base_too_old`).

### What stays on a kept disk

A kept machine's disk is a folder in the `kept` folder of `macos-vm.noindex`, in the runner login's account on your Mac. While the machine is stopped it holds:

| What | Contents |
|---|---|
| Everything you made in it | Files, your work folder and its repositories, the tools and apps you installed, and their settings |
| Your sign-ins | The `agent` account's keychain, browser profiles and cookies, and app sign-ins. A Claude or Codex command-line sign-in you made yourself stays too, until a run starts with your organisation's credential for that tool, which replaces it |
| What every copy of your Mac's base holds | The guest agent, the harness CLIs, the base's pairing key, and the `agent` account's password in `/etc/kcpassword` |

- **Who can read your sign-ins.** The password in `/etc/kcpassword` opens the keychain, so anyone who can read the disk file can read your sign-ins. Your Mac's file permissions and FileVault protect it. Nothing else does.
- **No secret Wardyn delivers.** A kept machine refuses environment and file secrets, SSH keys, a git token held in the machine, and other credentials that live inside it. Use credentials the proxy adds.
- **What a stop removes.** When a run stops normally, Wardyn removes the run's managed settings, proxy certificate, credential placeholders, tool routes, git routes and desktop password. It asks macOS to remove the certificate's trust. It does all of this again each time the machine starts, before the run's setup.
- **If macOS refuses.** When macOS will not remove a trust without a person, the run's record says so and lists the certificate's fingerprint.
- **After a crash.** After a crash, a restart of the Mac or a kill, these stay until the machine next starts. None of them is a live credential.
- **AWS sign-ins.** Wardyn removes the AWS command-line sign-in caches (`~/.aws/sso/cache` and `~/.aws/cli/cache`) as folders. Sign in again.
- **AWS config.** The profile and SSO session named `wardyn` belong to Wardyn. The scrub removes the `[profile wardyn]` and `[sso-session wardyn]` sections from `~/.aws/config`, including any you wrote under those names. It keeps your other sections byte for byte.
  - It deletes the file only when nothing but blank lines and comments is left. Give your own profiles other names.
  - A config the scrub cannot read as sections is left whole. If it names `wardyn`, the scrub reports a failure.
- **Deleted files.** Deleting a file does not erase its blocks. Copies may remain in the disk file until the machine is deleted, and in the Mac's local snapshots and backups after that.

### The life of a kept machine

| Event | What happens |
|---|---|
| Keep | `--keep-machine` makes the machine when the run starts, on your own Mac, from its base. The run must be an interactive macOS VM run you start yourself |
| Stop | A normal stop scrubs the disk (up to 60 seconds) and keeps the machine. Its expiry date is the stop time plus the idle days the run was admitted with, as the Mac recorded them |
| A stop without a scrub | A kill by you, a restart of the Mac, lost network, a runner upgrade or a low disk keeps the machine too. The scrub did not run, so the machine shows `scrubbed: false` and is scrubbed when it next starts |
| A kill by someone else | A kill by an admin or a service identity, who is not the run's owner, deletes the machine |
| Resume | `--resume-machine` starts a new run from the disk. Before it boots, the Mac brings the machine's tools up to date if they are behind ([The tools update](#the-tools-update-of-a-kept-machine)) |
| Fresh start | `--resume-machine <id\|name> --fresh` deletes what is on the machine and starts again from the Mac's current base, under the same name and base |
| Idle expiry | A machine unused past its date is deleted. A resume of one past it is refused `410` and starts its delete. The Mac enforces the date too, offline included. A machine a run holds does not expire |

- **Each machine has its own date.** Changing `macos_vm.kept_machine_idle_days` does not move the date of a machine already kept. `wardyn machines` shows each machine's date.
- **Older runners.** This needs the Mac's runner from 0.10.0-rc.12. A Mac still on an older runner dates the machine from the site's setting at the stop, as before. Update the Mac's runner.
- **Resume ends the old run.** A macOS run cannot be revived. If you resume a machine while its last run is still in its ended-run grace, that run ends at once. While the machine is still marked in use, the resume is refused (`kept_machine_in_use`). Stop the last run first.

### Deleting a kept machine

- `wardyn machines delete <id|name>` asks first. Without a terminal it refuses, and `--yes` deletes without asking. An admin deletes someone's machine with `DELETE /machines/kept/{id}` ([sdk.md](../sdk.md#kept-machines)).
- **In use.** A machine a run holds is refused (`409`, `kept_machine_in_use`) until the Mac records the run's stop, or saves it at its next sweep when it could not at the stop.
- **The Mac is offline.** The delete is accepted (`202`) and waits. It finishes when the Mac is back, and `wardyn machines` shows the machine as deleting. If the Mac is no longer registered, Wardyn only records the delete (`record_only`).
- **Unconfirmed.** If the Mac's answer is lost, Wardyn cannot tell whether the machine is gone (`503`, `kept_machine_delete_unconfirmed`). The delete stays pending and is retried until the Mac confirms. You cannot resume the machine meanwhile.
- **Refused by the Mac.** If the Mac refuses before removing anything (`503`, `kept_machine_delete_failed`), the machine is kept. Run the delete again.

### When a limit is 0

- **The Mac's disk limit is `0`.** `macos_vm_kept_disk_limit_gb` of `0` turns kept machines off on that Mac. A keep is refused, and so is a resume: the Mac answers "This Mac doesn't keep machines." Machines already there are still listed and can still be deleted, and they still expire.
- **The per-person limit.** `macos_vm.max_kept_machines_per_person` is 1 to 4, and 0 or an unset value reads 1. At the limit, a keep is refused (`kept_machine_person_limit`) and a resume still works.
- **The site switch.** With `macos_vm.allow_kept_machines` off, a keep and a resume are both refused (`kept_machine_site_off`).

### The tools update of a kept machine

A kept disk keeps the guest agent and harness tools it was made with. When they are behind the Mac's, a resume updates them before the run boots. `wardyn-runnerd macos-vm machines update <id> --force` runs the update on demand.

- The update boots your disk outside any run, on the prep network. That network reaches only Apple's software-update hosts, on port 443, with no credential ([The prep proxy](macos-vm-base-image.md#the-prep-proxy)).
- **The window.** It lasts from the start of the update job until its helper exits. The network is removed shortly after. The run's proxy does not apply: no run exists yet.
- If the update fails, the machine does not start. Start it fresh, or try again.

## Watching a background run's desktop

A background run has no terminal, SSH or web door. This is the one exception: the person who started a background macOS VM run can watch its desktop from the run page, view only.

- **What a watch can do.** It shows the picture and nothing else. Keys, pointer moves, paste and clipboard copy are dropped by three separate things: the helper for that boot, the Mac's desktop mode and the gateway's own filter.
  - A watch does not wake a paused run, mark the run as in use or keep it alive.
  - It opens through `POST /runs/{id}/desktop/watch-ticket`. A declared noVNC app, the desktop's files, a sign-in and a terminal stay refused ([Errors](../sdk.md#reason-branch-on-this-never-on-errors-prose)).
- **The exposure.** A background run has no terminal, so what a person sees of it is its logs and audit trail. A screen can show what those keep out: a secret in a GUI app, a document, anything the run puts on screen.
  - Turn the site switch off, or set the profile limit, where that is not acceptable.
- **macOS 27.** Its prepare flow creates only the setup account, and Wardyn's own prepare steps set the run user's first-login settings on 27 as on 26. Pane names change by version, so nothing beyond macOS 26 is promised as tested.
- **Recording.** A watch is recorded when the run is at Screen and actions ([Recording](#recording)).
  - The audit log shows `ui.open` and `ui.close` with `view_only` and `background`, never the picture.

| A watch is refused when | Status and reason | What you see |
|---|---|---|
| The caller is a member, a sponsor or a security admin, and did not start the run | `404` | As for any run they cannot see |
| The caller is a super admin | `403` `run_owner_only` | On another's run and on a run with no personal owner |
| The run is not running, or has ended | `409` `attach_not_running` or `attach_run_kept` | |
| The run is not a background run | `409` `run_background_watch_not_background` | |
| The run is a background run that is not a macOS VM | `409` `run_background_only` | |
| `macos_vm.allow_background_watch` is `false` | `403` `run_background_watch_site_off` | "Watching background desktops is turned off for this install. An admin can turn it on." |
| The run's profile sets `deny_background_desktop_watch`, `deny_ui_apps` or `deny_interactive` | `403` `governance_unit`, target `runs.desktop_watch` | See [Denials](denials.md). No role is exempt |
| The run's profile cannot be resolved because its overlay cannot be satisfied | `403` `governance_overlay_unsatisfiable`, target `runs.desktop_watch` | The overlay's own sentence |
| The agent's sign-ins in a desktop are "Not allowed" | `403` `desktop_sign_ins_off` | "Your admin doesn't allow sign-ins in a desktop while `<agent>` runs, and a macOS desktop can't block them yet." |
| The run began before the upgrade | `409` `run_background_watch_old_boot` | A run started before the upgrade has no watch |
| Its helper has not reported that this boot's desktop is forced view only, or the Mac's desktop mode is not `observe` | `409` `run_background_watch_unconfirmed` | Wardyn checks again when the runner reconnects |
| The Mac's runner is offline | `409` `run_background_watch_offline` | Wardyn checks again when the runner reconnects |
| The Mac offers desktop control | `409` `run_background_watch_control` | |
| The run is paused | `409` `run_background_watch_paused` | "This run is paused. You can watch it once it is running again." |

- **An open watch is re-checked every 30 seconds.** Each check reads the site switch, the profile and the Mac again, and ends the view when any of them now refuses. A read that fails ends it too. An open view ends within a minute of a change; closing it does not stop the run.
- **Where it shows.** `GET /runs/{id}` carries `background_watch`, beside `macos`, on a running background macOS VM run. Its `state` is `open`, `site_off`, `governance`, `sign_ins_off`, `old_boot`, `unconfirmed`, `offline`, `control`, `paused` or `not_run_owner`.
  - The run's owner also gets the refusal's sentence. Anyone else who can read the run gets `not_run_owner` and no sentence.
  - The run page shows its Watch button only when the state is `open`.

## Recording

A macOS VM run is recorded the way a Linux VM run is, at the run's recording level. The recording belongs to the organisation: the runner keeps none of its own.

| Level | What is kept for a macOS VM run |
|---|---|
| Off | Nothing. Review shows no macOS notice |
| Terminal | Console and SSH terminal sessions, masked, and a task run's agent output, masked. The desktop is not recorded |
| Screen and actions | The above, and the desktop screen while someone watches it from the run page |

- **Screen frames are not masked.** Masking runs on terminal bytes. A frame is kept as the guest drew it, so a secret the desktop shows is in the recording. Review says so before the run starts. The desktop's clipboard is left out.
- **Only a watched screen is kept.** A run nobody watches has no desktop recording. A takeover or a sign-in pause shows as a gap in the recording.
- **The runner is trusted for frames.** The gateway records what the person's own runner relays, so a runner that alters the picture alters the recording ([Threat model](../../threatmodel/THREAT-MODEL.md)).

### The agent output cast

A task run's agent output is masked into an asciinema cast and saved when the agent's output copy ends. The run page lists it as "Agent output". An interactive run has no agent exec and no cast; its terminal sessions are recorded as any run's are.

| Part | What happens |
|---|---|
| Saved | The cast is stored under the run with a `session.recording.write` row (`session` `agent`). The row is accepted before the run counts as recorded |
| Nothing printed | An agent that printed nothing leaves nothing to record. The run stays recorded, with no cast |
| Declined | The run is marked `runner_declined` and a **failure** row says why in `reason`. A decline after the run starts never stops it. One at dispatch can (below) |
| Recovery | After a restart, or on another replica, the runner's copy is read again and saved as `agent-recovered`. The two never replace each other. A sweep settles every finished run that still owes a cast |
| Erasure | Erasing a run's recordings erases its casts. A run erased in the middle of a save or a re-read owes nothing and gets no cast |
| Required | Under a recording requirement, a task run whose cast cannot open at start fails: "Recording is required, and this macOS VM run's agent output can't be recorded on this deployment, so the run didn't start." |

| Decline `reason` | Meaning |
|---|---|
| `mask_gap` | The masker dropped bytes it could not vouch for, and nothing else was kept |
| `mask_uncovered` | The run's masking was not covered, so the output could not be checked for secrets |
| `save_failed` | The recording store refused the cast |
| `no_tail` | The run has no output to re-read from |
| `read_failed` | The re-read kept failing until the sweep gave up |
| `erased` | The run's recordings were erased |
| `unrecoverable`, `sandbox_gone`, `runner_offline` | The re-read found nothing: the runner cannot recover it, the VM is gone, or the runner is not connected |

A decline has three timings:

- **At dispatch** (`no_tail`: no cast could open on the run's output; `save_failed`: its obligation could not be stored). The run is marked `runner_declined` and a failure row is durably accepted before it starts.
  - If the audit recorder will not accept the row, the run does not start: "Wardyn could not record why this run's agent output isn't recorded, so the run didn't start. Try again."
  - Its `run.create` failure row has `reason` `decline_not_recorded`.
  - Under a recording requirement it fails with the required sentence above instead.
- **Later** (a lost or unreadable copy, a failed save, a restart). The run keeps running. A rejected row keeps the obligation, and the sweep writes it again.
- **Partial or total mask drop.** Some output kept and some dropped: the cast is saved, `incomplete` and `mask_gap`. Everything dropped: nothing is saved and the run is declined as `mask_gap`.

A saved cast says how complete it is, on its row and in its header. A row carries these only when they apply:

| Field | Meaning |
|---|---|
| `incomplete` | The cast is not the whole output. Always set with `read_error` or `mask_gap`, and with `completeness` `unknown` |
| `read_error` | The runner's copy ended in an error. The cast ends with a marker line. The error's text goes only to the log |
| `mask_gap` | Some output was left out because Wardyn could not check it for secrets. The gap is marked, never filled |
| `truncated`, `limit_bytes` | Only the first `limit_bytes` were kept |
| `gap_bytes` | The start of the output is missing: the runner had already discarded that many bytes |
| `completeness` `unknown` | A swept row whose cast header could not be read. It is never called complete on no evidence |

## Opening a window on your Mac

A window is a keyboard-and-pointer view of your own macOS VM run, drawn on your own Mac. The run's own helper (`wardyn-macvm run`) draws it with Apple's virtual machine view. Wardyn's server decides each open and tells the run's runner to draw it. Nothing rests on the Mac between opens.

- The window shows the VM's screen and takes your keys and pointer.
- If Setup Assistant opens in a run, the guest agent ends it; a run's disk is thrown away, so nothing it asks is kept.
- A macOS guest's own background requests keep a run active only when your policy allows them.
- Its Apple boot hosts never do, allowed or refused. On a Mac runner this holds once the runner is upgraded.
- It is not recorded, because it is drawn on your Mac and never crosses the gateway. A run at Screen and actions can't open one.
- While it is open, the run's browser desktop is view only. A browser desktop that had control is ended first, and the open waits up to 5 seconds for it.

| Way to open it | What it does |
|---|---|
| `wardyn window <run>` | Asks the server to open a window on the Mac the run is on. It prints "The window is opening on `<computer>`." or the refusal's sentence, and exits non-zero on a refusal |
| `wardyn window <run> --status` | Prints `window:` and a state, the state's sentence, and `last window closed:` with the reason and time. It opens nothing |
| `wardyn run ... --window` | Asks at create for a window once the VM is up; an interactive run's window waits for its preparation to finish. A create the window would be refused for is refused with that sentence, and the run is not launched |
| The run page | The run's Mac window section has an Open Mac window button while you can open one. `GET /runs/{id}` carries `mac_window` on a running macOS VM run; its `state` is `open` when you can open a window |

- The CLI route is `POST /runs/{id}/desktop/window`. Its body may carry `trigger`, either `cli` or `run_page`.
- The run's owner also gets the refusal's sentence in `mac_window`. Anyone else who can read the run gets `not_run_owner` and no sentence.

### The site switch

| Setting | Where | Meaning |
|---|---|---|
| `macos_vm.allow_attended_window` | The organisation's site config | `true` lets a person open a window. Absent or `false`: no window. Off by default |

- **A write that leaves the switch out keeps it.** A save that omits it keeps the stored value, so an older client cannot turn it on or off by silence.
- A governance profile that sets `deny_ui_apps` or `deny_interactive` also refuses the window. No role is exempt.

### When a window is refused

Wardyn asks these in order, and the first that refuses answers. Every refusal writes a denied `takeover.begin` row.

| Refused when | Status and reason | What you see |
|---|---|---|
| The site switch is off | `403` `mac_window_site_off` | "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config." |
| The run is not a macOS VM run | `409` `mac_window_not_macos_vm` | "Only a macOS VM run has a window." |
| The run is not running, or has ended | `409` `attach_not_running` or `attach_run_kept` | "run is not RUNNING; cannot open its window", with the state, or "run has ended; cannot open its window" |
| You did not register the run's Mac, did not start the run, or came through a delegated token | `403` `mac_window_not_your_mac` | "Wardyn opens a window only on a Mac whose runner you registered. This run is on `<computer>`, so Wardyn can't tell who is at its keyboard. Watch its desktop from the run page instead." |
| The run's profile sets `deny_ui_apps` or `deny_interactive` | `403` `governance_unit`, target `runs.desktop_window` | "Your organisation's governance profile doesn't allow an interactive desktop on this run." |
| The run's recording level is Screen and actions | `403` `mac_window_unrecorded` | "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead." |
| The agent's sign-ins in a desktop are "Not allowed" | `403` `desktop_sign_ins_off` | "Your admin doesn't allow sign-ins in a desktop while `<agent>` runs, and a macOS desktop can't block them yet." |
| The Mac's runner is offline | `409` `mac_window_offline` | "This Mac's runner is offline, so Wardyn can't open a window on it." |
| The runner is connected through another server | `409` `mac_window_not_here` | "Wardyn couldn't reach this Mac's runner from here. Try again in a moment." |
| The runner cannot open a window | `409` `mac_window_runner_too_old` | "This Mac's runner can't open a window. Upgrade it to this server's version." |
| The run is paused | `409` `mac_window_paused` | "This run is paused. You can open its window once it is running again." |
| The run is still being prepared | `409` `mac_window_preparing` | "This run is still being prepared; the window opens when preparation finishes." |
| The run's desktop settings are still being applied | `409` `mac_window_preparing` | "This run's desktop settings are still being applied; the window opens when they are." |
| The run already has a window | `409` `mac_window_open` | "This run's window is already open on `<computer>`." |
| Your session was cut off | `403` `mac_window_revoked` | "This session is no longer authorized, so Wardyn won't open a window. Sign in again." |
| A browser desktop with control did not close in time | `409` `mac_window_desktop_control` | "A browser desktop that has control of this run didn't close in time, so Wardyn can't open the window. Close it and open the window again." |
| You are not signed in at the Mac's screen, or it is locked | `409` `mac_window_not_at_mac` | "The window didn't open: you aren't signed in at `<computer>`'s screen, or it is locked. Unlock it and open the window again." |
| The Mac can't tell whether its screen is locked | `409` `mac_window_lock_unknown` | "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed." |
| The runner could not draw the window | `502` `mac_window_failed` | "Wardyn couldn't open the window on the run's Mac. The run goes on; try again." |

- A runner older than 0.10.0-rc.12 does not report the window, so it reads as "can't open a window".
- A read of the site config or the run that fails refuses the open.
- `mac_window.state` is `open`, `window_open`, `site_off`, `governance`, `sign_ins_off`, `not_your_mac`, `unrecorded`, `offline`, `runner_too_old`, `not_here`, `paused`, `preparing`, `unconfirmed` or `not_run_owner`. Only `open` shows a button.

### The run's settings in the window

- Terminal and apps opened from the Dock or Finder in the window get the run's non-secret settings: the proxy, the CA bundle, the provider's sign-in files and placeholders, and the git identity.
  - `env_secret` values are not in the window's Terminal or desktop. They are in `wardyn attach`.
  - Shell and loader startup variables (`ZDOTDIR`, `ENV`, `BASH_ENV`, `FPATH`, `PS4`, `PROMPT_COMMAND`, `DYLD_*`, `LD_*`) are never passed.
- The guest applies them once the agent account is signed in at the console, waiting up to 10 minutes. An app opened before the window shows ready, or a login item that auto-login started first, may need a relaunch.
- Only zsh reads them from a file: one line in `/etc/zshenv` that the agent account's zsh sources. A bash or sh Terminal profile gets nothing from it.
- **While they are pending.** `wardyn window <run> --status` says `preparing` with the sentence above. A window asked for at create waits for them, up to about 10 minutes, and the run's start is not held.
- **If they fail.** The window opens, and the status says why: "the agent account was not signed in", "the desktop session did not take them" or "they were not confirmed in time". Run `agent-run --session-env-retry` in the window's Terminal to retry.
- **After a logout inside a run.** The window's apps lose the proxy settings until you run that retry command. Nothing applies them again by itself.
- In the guest, `cat /Library/Wardyn/run/session-env.status` reads `session_env=pending`, `session_env=set vars=<n> via=<asuser|user|retry|none>` or `session_env=failed reason=<not_signed_in|setenv_failed>`.

### Closing a window

- Closing the window detaches the view. The VM, its one network card and its proxy are untouched, so the run goes on as before.
- The window also ends when the run ends, the runner goes offline, or the link to the runner is lost.
- It closes as soon as the Mac locks, or as soon as the Mac can no longer tell whether its screen is locked.
- **An open window is re-checked every 30 seconds.** Each check reads the site switch, the profile, the run and your session again, and closes the window when any of them now refuses.
- `GET /runs/{id}` carries `mac_window.last_close` with a reason of `person`, `host_locked`, `run_ended` or `failed`, and the time. `wardyn window <run> --status` prints it.

### Audit rows

| Row | When | What it carries |
|---|---|---|
| `takeover.begin`, success | A window opens | `surface` `mac_window`, `trigger` (`launch`, `cli` or `run_page`), `background`, `recorded` false, `runner_id` and `grant_id` |
| `takeover.begin`, denied | Any refusal above | The same fields and `reason`, the refusal's reason. A refusal at create has no run to name |
| `takeover.close` | A window ends | The same fields, `held_ms` (how long it was open) and `reason`. `refused_by` names the check that ended it on a re-check |

- A `takeover.close` reason is `closed_by_person`, `host_locked`, `refused_on_recheck`, `revoked`, `run_ended`, `runner_offline`, `link_lost` or `helper_failed`.
- The rows carry no screen contents and no keys.

## Limits in this release

| Limit | Detail |
|---|---|
| Harnesses | Claude Code, Codex CLI and "Your own tools", depending on what the base installed and what passed the launcher's self-test. If Apple's Command Line Tools cannot install, only "Your own tools" is offered |
| Desktop | One fixed 1920 x 1080 display. A `view only` desktop ([Preparing the base image](macos-vm-base-image.md#preparing-the-base-image)) can be watched, not controlled |
| Clipboard | Paste in is allowed and copy out is blocked by default, as for other desktops, if Apple's Screen Sharing carries the standard clipboard messages. Otherwise nothing is copied either way |
| Heat and battery | New VMs are refused at serious or critical thermal state. Battery use and lid-close sleep on MacBooks were not measured |

### Apps that use only the system trust store

Before a run starts, Wardyn asks the guest to trust the run's proxy certificate in its System keychain, for TLS only. macOS 26 refuses this without a person ([#3923](https://github.com/cjohnstoniv/wardyn/issues/3923)). The run goes on, and its `run.dispatch` audit row says `keychain_trust` `not_trusted`, reason `refused`. The table says which apps that affects.

| App or tool | Host marked Inspected | Host marked Tunnel only | Tunnel only, behind a TLS-inspecting proxy |
|---|---|---|---|
| Uses only the system trust store: Safari, Electron apps, and Go or Swift tools with no CA file of their own | Fails TLS | Works | Fails TLS until your organisation's root is trusted |
| Reads a CA variable: Node, Python, git, curl and the AWS CLI | Works | Works | Works, with your organisation's root in the [runner's CA bundle](hybrid-laptops.md#a-corporate-ca-on-a-laptop-runner) |

- **The CA variables.** `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `AWS_CA_BUNDLE` and `CURL_CA_BUNDLE` are set for every process a run starts.
- **Your organisation's root.** Trust it by hand in `edit-base` ([The organisation CA](macos-vm-base-image.md#the-organisation-ca)). That reaches a Tunnel only host.
- **The run's proxy certificate.** Trusting it by hand can't help: the certificate is new for each run.
- **Which hosts are which.** The run's Policy tab and `wardyn run policy` mark each host.
- **Testing a host.** In the guest, run `/usr/local/libexec/wardyn-guest-agent tls-check <host>`. It checks the way the system-trust apps do and honours `HTTPS_PROXY` and `NO_PROXY`. It exits 0 when the host is trusted, 1 when TLS failed, and 2 for a usage or network error.
  - `nscurl` can print a TLS failure (`-9802`) and still exit 0, so don't trust its exit code.
  - For a tool that reads a CA variable, check with `curl` and the CA variables set.
- **`kubectl`.** A kubeconfig that names a CA may not use the system trust store. The one a kubectl component writes names the run's proxy certificate.

### Folders

A shared folder needs three settings, in three places:

| Setting | Where | Who sets it |
|---|---|---|
| The workspace roots, and the writable roots for a writable share | The organisation's server: `WARDYN_USER_WORKSPACE_ROOTS` (or a member's entry in `WARDYN_USER_WORKSPACE_ROOTS_MAP`), `WARDYN_USER_WRITABLE_ROOTS` | The server's operator |
| The runner's drive roots | The Mac: `wardyn-runnerd setup --drive-root <dir>`, or `DriveRoot` in a managed profile | The Mac's person, or the MDM |
| Full Disk Access for the guest agent | The base image, once, in `edit-base` (its window has a Full Disk Access button) | Whoever edits the base |

A folder has to pass all three. The rules and the failures are below.

- Shared into the guest at `/Users/agent/…` (a workspace's `/home/agent/…` path), with the same rules and read-only or writable choice as a container.
- A run reads a shared folder only once the guest agent has Full Disk Access in the base. macOS asks a privacy question for each network volume, and nobody can answer it inside a run. Grant it once in `edit-base` ([Editing a base](macos-vm-base-image.md#editing-a-base)).
  - Without it, a run with a shared folder stops before it starts, within 10 seconds, and names this fix.
  - The runner signs each guest agent it pushes with a signing identity it makes once on that Mac, so the grant can last through tools updates. The first tools update after an upgrade changes the signature: grant it once more.
  - It signs in its own process first, then with `codesign`. Each copy must pass `codesign --verify --strict` before it is pushed.
  - If signing fails, the agent is pushed signed ad hoc and loses the grant at each tools update. `doctor`'s guest-agent stage fails, and `wardyn-runnerd macos-vm status` says why.
  - A base moved to another Mac, or another login on the same Mac, loses the grant once: the identity is per Mac and per login.
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
  - Every holder is waited for, up to 90 seconds: a live run, a run that just ended, another login's VM, or a `drive open` ([Opening a drive on the Mac](#opening-a-drive-on-the-mac)). The failure after the wait states the wait.
  - After 90 seconds the run fails with "Waited 90 seconds for the drive to be released." and the sentence below.
  - The sentence: "`<drive>` is in another macOS VM on this Mac. If it is open there with drive open, close it there, then start the run again. Otherwise a run that just ended may still be releasing it: a disk drive is in one run at a time, so try again in a minute, or when that run ends."
- The guest finds a disk drive by its size, apart from the boot disk, because a macOS guest does not show the disk's identifier. It refuses two disks of one size.
- A macOS VM run attaches one disk drive, and a kept work folder counts as one. A run that names two is refused at Review and at create (`drive_one_disk`), whatever their sizes.
- Keeping the work folder of a macOS VM run keeps `/Users/agent/work` as a disk drive on the Mac the run used. The run's pool must hold one Mac; with several, it is refused. A run its Mac refused when it started removes its kept work folder drive.
- Deleting a disk drive asks its Mac to delete the image. While the Mac is offline the drive stays as deleting, and the image goes when the Mac reconnects. If the Mac is no longer registered, the image stays on it and only the record goes. A drive in a run is refused until that run ends.
- For up to 90 seconds after a person's last macOS VM run ends, deleting their disk drive is refused as a drive still being released: try again in two minutes.
- Wardyn does not encrypt the image: the runner's macOS login and root on the Mac can read it. It stays until the drive is deleted, and deleting it is not secure erasure (see [Backups, Spotlight and removal](macos-vm-host.md#backups-spotlight-and-removal)).

### Opening a drive on the Mac

An agent wrote a disk drive's files, so treat them as untrusted. `wardyn-runnerd drive open <name>` opens a drive through a throwaway VM. It finds only a drive that a run on this Mac has attached: the runner keeps the drive's name beside its image when a run attaches it.

The Mac never mounts, checks or repairs the volume. It hands the file to the VM as a raw disk and reads a tar stream back, writing plain files. Opening the image yourself, by double-click in Finder or with `hdiutil`, makes this Mac's own system read a volume an agent could have crafted. Do neither with a drive an agent wrote.

1. Run `wardyn-runnerd drive open <name>` in Terminal as the Mac's user. A name the Mac does not know is refused. A name two drives share is refused with the object names to use instead.
2. Wait about a minute. The VM starts from the ready base with the drive and nothing else: no environment, secret, file or CA, and no agent.
3. The files are copied to `~/Wardyn Drives/<name>`. The command prints how many files and bytes it copied, and what it skipped. The volume's own folders (`.Trashes`, `.Spotlight-V100`, `.fseventsd`, `.TemporaryItems`) and extended attributes are left out of the copy and are not counted as unread.
4. Change the files. Press Return to write the changes back, or Ctrl-C to discard them. A second Ctrl-C stops at once.
5. The command then removes the folder and says so: "The folder ~/Wardyn Drives/<name> was removed." Ctrl-C does the same after "Discarded: nothing was written back."
6. The next run that uses the drive sees the written-back files, owned by `agent`.

- **Network.** The VM's network device is wired to one end of a socket pair that carries nothing, so the guest has nothing to reach by design. The command says the VM has no internet.
- **What reaches the Mac.** Regular files and folders only. Links of both kinds, devices, pipes and set-id files are skipped and never followed. The closing line counts them, for example "Skipped 3: 2 links, 1 device."
  - Every file is written with mode 0644, so none can execute.
  - Wardyn sets the `com.apple.quarantine` attribute on every file and folder, which is what macOS reads to treat a file as downloaded.
- **Limits on a copy.** At most 250,000 files and folders, and no more bytes than the Mac has free less 5 GiB. A name part over 255 bytes, a path over 1,024 bytes, an absolute name or a `..` ends the copy and removes it.
- **The folder.** A copy goes only into an empty folder, which the command makes private (mode 0700) under your home. A link in its place is refused.
  - After a discard, or a write-back that sent everything, the command removes what it copied, and the folder too if it made it. A folder that was already there stays, empty, and the closing line says so.
  - The folder is kept when a write-back failed, or when some items (links, devices, pipes, set-id files) could not be written back, because it then holds the only copy of those changes. The closing line says which. Delete it yourself before the next `drive open` of that name.
  - A lock file, `.<name>.lock`, beside the folder keeps a second `drive open` of the same name out while the first runs. The lock goes with the command that holds it, even if that command is killed.
- **A partial copy.** If the VM's `tar` could not read some items, the copy is kept and the command says how many are missing and what the VM said. Write back then replaces nothing, and `--delete` removes nothing, that the copy does not hold.
  - A damaged listing, a limit passed or Ctrl-C before the copy ends removes the copy and writes nothing.
- **Writing back.** New and changed files go back through the VM as `agent`. A name the copy never held is never overwritten. `--delete` also removes from the drive the copied files you deleted, and no other file. Links you made in the folder are skipped.
- **A slot and the lock.** The VM takes one of the Mac's VM slots and the drive's lock while it is open. A run that wants the drive waits, then fails as above. A run the Mac then has no VM room for is refused, not queued: "has no room for another macOS VM right now".
- **Record.** A drive open leaves no row in the organisation's audit log. The command logs when a drive opens and closes, with its result, in `drive-open-cli.log` in `macos-vm.noindex/`, beside the helper's `drive-open.log`.
