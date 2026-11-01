> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Check a release candidate's macOS VMs on your Mac

- [`scripts/macos-vm-check.sh`](../../scripts/macos-vm-check.sh) installs a release candidate's runner on an Apple silicon Mac, runs the macOS VM checks and writes `report.md`.
- **Not yet run on a real Mac.** The script is tested only against stand-ins for every command. This notice is removed after its first real run.
- It changes the Mac a lot, so use one that is not in daily use. It uploads nothing.
- The report is evidence from your Mac. It is not proof of a security row unless we watched that run.

## What it changes

| Change | Undone by `--cleanup` |
|---|---|
| Installs the release package with `sudo` and registers a runner | The runner is revoked. macOS has no uninstaller for a package, so its files stay |
| Restarts the runner as the tests need | Not applicable |
| Fills one run's disk to its limit and wedges one guest | The runs end with the tests |
| Writes a root-owned posture file that says this Mac is managed, and marks the runner `managed_device` | The file is removed if the script wrote it |
| Waits for a macOS base image that you start yourself | Removed if this run prepared it |
| Starts a local stack of containers (local mode only) | Removed with its volumes |

## Before you start

| Need | Detail |
|---|---|
| The Mac | Apple silicon, macOS 26 or later, on AC power, 110 GiB free, 16 GiB of memory or more |
| A login | Someone logged in at the Mac's desktop. Over SSH, use Screen Sharing for that session |
| Tools | Docker running, Go, the Xcode command line tools, `git`, `curl`, `python3`. Node and `pnpm` for `--console` |
| A checkout | This repository at the release's tag, with no local changes. The suite is built from it on your Mac |
| An organisation | `WARDYN_MACOS_VMS=1` set, runners enabled, and your own `WARDYN_TOKEN` in the environment (`--server`) |
| Fixtures | `WARDYN_MACVM_ALLOWED_URL`, `WARDYN_MACVM_DENIED_HOST`, `WARDYN_MACVM_GIT_URL` and `WARDYN_MACVM_1G_URL`. A row that needs one you leave unset is NOT_RUN |

## Run it

1. Check without changing anything: `scripts/macos-vm-check.sh --preflight --version <release>`. It names each need and what is missing.
2. Run it with the consent flag. Without the flag it prints what it would change and stops.
   `scripts/macos-vm-check.sh --version <release> --out ~/wardyn-check --server <url> --i-understand-this-changes-this-mac`
3. When it prints the prepare command, paste it into Terminal on the Mac's own desktop. Prepare refuses over SSH and with `sudo`.
4. On macOS 26, do [setup by hand](macos-vms.md#setup-by-hand) when the status asks. On macOS 27 nothing is needed.
5. If a second admin is needed for the trust change, they run the approve command the script prints.
6. Read `report.md` in the out directory, then file it ([below](#file-the-result)).
7. Clean up: `scripts/macos-vm-check.sh --version <release> --out ~/wardyn-check --cleanup --i-understand-this-changes-this-mac`.

A crashed run resumes: run the same command with the same `--out` and the steps already recorded in `steps.tsv` are skipped.

## Options

| Option | Effect |
|---|---|
| `--version V` | The release, like `0.10.0-rc.10`. Required. The checkout must be at its tag |
| `--out DIR` | Where steps, the suite output and `report.md` go. Default `~/wardyn-check` |
| `--full` | Every row. The default |
| `--quick` | A shorter set with shorter time budgets. It omits BLOCK rows, so it can never read complete |
| `--server URL` | Use this organisation. Without it the script starts a local stack |
| `--allow-org-writes` | With `--server`, also run P20, which changes governance state the script cannot undo |
| `--base-ready` | Reuse the macOS base already on this Mac. Without it, an existing base is refused |
| `--console` | Also run the console checks against one live run |
| `--prompts` | Let rows that need a person at the Mac ask. Without it they are NOT_RUN |
| `--stop-at TIME` | Start no row after this UTC time, like `2026-11-02T14:00:00Z` |
| `--allow-existing-runner` | Use a runner already registered here. Without it the script refuses |
| `--preflight` | Check only |
| `--cleanup` | Remove what this script's state file records |

## What needs a person

| Row or step | Who |
|---|---|
| The base prepare | You, in Terminal at the desktop |
| Setup by hand on macOS 26 | You, about 10 minutes at the Mac |
| Trust under four-eyes, P20 and the audit read | Two admin sessions. Without them the rows are NOT_RUN |
| Rows that ask you to look, such as the local window or Safari | You, only with `--prompts` |
| Rows that need a fixture | A fixture you provide |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every row passed or was NOT_RUN on a non-security row, at least one passed, and the summary says `result: complete` |
| 1 | A row failed, or the code-shape check failed |
| 2 | A BLOCK row failed, or nothing could run |
| 4 | Incomplete: nothing passed, a BLOCK row was not run, or the run was cut off |

Only 0 is green. The report leads with the counts and says INCOMPLETE in its first line when the code is 4.

## What the report holds

- It is built from a fixed list of fields: the release, the commit, the package checksum, the macOS version, the chip, the memory, the step durations and each row's outcome, duration and reason.
- A second scan then refuses it if it holds a URL, a host or user name, an address, a token, a key, a cloud id, a run id or a home path.
- On a hit it writes no report and says which kind of thing it found. Read `suite/summary.tsv` yourself before you share any of it.
- It never collects the setup code file, the runner's configuration file or the posture file.

## `--server`

- It registers one runner and its single-use registration token in that organisation, and marks the runner `managed_device`.
- The tests create runs there, with their files and output. A run document you set for the suite is applied there.
- `--cleanup` revokes the runner. The audit entries and run records stay in the organisation.
- It never reads a credential from disk: the `WARDYN_TOKEN` it uses is the one in your environment.

## The local stack

- The local stack uses the release's published images, so it works only for a published candidate.
- It does not create the suite's network fixtures. Rows that need them are NOT_RUN unless you set them.
- Its generated admin secret is in a mode 600 file in the out directory. It is never printed or put on a command line.

## File the result

1. Open a new issue from the "macOS VM check" template.
2. Paste `report.md`. Do not attach other files from the out directory.

The report is evidence from your Mac for that release. We treat a security row as proven only when we watched a run of it.
