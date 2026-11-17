# Other tools over SSH (scripted access)

- Everything in [SSH.md](SSH.md) assumes a human typing `ssh` or pasting a `Host` block by hand.
- An external tool — an IDE or an agent workbench that wants to drive the sandbox itself — needs the same three facts (host, port, username) without a human copying them out of the console.
- Plus one thing the console card never had to solve:
  - knowing when the sandbox is actually ready to open, not merely RUNNING.

## The four commands

- Four commands, each doing one part:

```sh
wardyn ssh-key ensure                                    # once per machine

# --description "external:<tool>" is what makes the run page say tool-managed.
# This command prints the run, including its id.
wardyn run --agent claude-code --interactive --json \
  --description "external:<tool>" \
  --policy-file examples/policies/remote-workspace.yaml
wardyn run wait-ready <id> --json                        # -> {"workspace":{"vcs":"git","path":"..."}}
wardyn run ssh <id> --json                                   # -> {"host","port","username","host_key_fingerprint","command"}
```

- **`wardyn ssh-key ensure`** ([`cmd/wardyn/sshkey.go`](../cmd/wardyn/sshkey.go)) generates an ed25519
  keypair at `~/.wardyn/id_ed25519` the first time it runs (`0600`, plus a
  `0644` `.pub` sibling) and registers the public half via `POST
  /api/v1/me/ssh-keys` unless its fingerprint is already on the account.
- Idempotent, so a script can run it on every launch with no "already done"
  branch to write — a second call neither regenerates the key nor re-registers
  it.
- `--path` picks a different keypair; the default is deliberately **not**
  `~/.ssh/id_ed25519` — a key an external tool dials sandboxes with should be
  revocable from Settings (`/account`) without touching your everyday identity.
- **`wardyn run wait-ready <run-id>`** ([`cmd/wardyn/run_wait_ready.go`](../cmd/wardyn/run_wait_ready.go)) blocks
  past what `run --wait` waits for.
- `--wait` waits for a TERMINAL state (and is refused outright on an interactive run, which never reaches one on its own);
  - `wait-ready` waits for the run to become usable from outside: RUNNING,
    *and* its workspace readable through the same in-sandbox exec channel the
    console's Files-changed widget polls (`GET /runs/{id}/files`,
    [`internal/api/run_files.go`](../internal/api/run_files.go)).
- Those are
  different moments — the workspace clone lands **after** the sandbox comes
  up, so a tool that opens the instant the run turns RUNNING routinely finds an
  empty directory.
- A run that names a repo (`--repo`, or a workspace whose
  sources include a repo) automatically waits for `vcs:"git"`; pass `--expect-git`
  to require that for a workspace-sourced run too.
- A terminal state reached
  before ready fails fast — FAILED exits `1` (with the dispatch failure reason
  when audit carries one, the same lookup `run --wait` uses), any other
  terminal state exits `2`.
- And `--timeout` (default `5m`, must be positive)
  exits `124` rather than hanging a script forever.
- The timeout bounds the
  requests too, and does not stop the run: on a `124` the run keeps running and
  holds its sandbox and credentials until it ends (see
  [ci-jobs-as-runs.md](ci-jobs-as-runs.md), "On `124`, kill the run").
- **`wardyn run ssh <run-id> --json`** ([`cmd/wardyn/ssh.go`](../cmd/wardyn/ssh.go)) is the same `/healthz`
  read `--print`/`--config` use, shaped for a program instead of a terminal:
  `{"host", "port", "username", "host_key_fingerprint", "command"}`.
- `port` is
  always populated — `22` when the gateway's advertised address names none
  — so a caller never has to reimplement ssh's own default the way a bare
  `--print` command's absent `-p` flag implies it.
- `host_key_fingerprint` comes
  straight off `/healthz` (the same `ED25519 SHA256:…` line the console card
  shows) — verify it out-of-band the same way you would any new SSH host;
  `wardyn run ssh` does not do that for you.
- `command` is the literal `ssh …`
  invocation `--print` would emit, so a caller that just wants to shell out
  rather than reimplement the client can.
- **Never-reap and push namespace.** A session like this has no Wardyn-visible
  activity between whatever the human or the tool's own agent does in that
  tool's own UI.
- So set `auto_stop_after_sec: -1` — never `0`: both mean "never
  reaped" to the reaper, but `-1` states the omission was deliberate on a run
  like this, not a policy that simply forgot the field (see
  [`examples/policies/remote-workspace.yaml`](../examples/policies/remote-workspace.yaml)).
- That holds only under a ceiling with no positive `auto_stop_after_sec` (or for an admin). A member under the shipped default ceiling is cut to 3600s unless the operator raises it, as the Channel budget notes below.

- If the tool's own agent commits and pushes under a branch name it picked itself — not the `wardyn/<run-id>/*` namespace `agent-run` sets up —
  - the git-broker's default confinement refuses that push with no way to tell the external tool why.
- `git_push_any_branch: true` is the per-run opt-out (see [docs/POLICIES.md](POLICIES.md)'s `git_push_any_branch` row).
- **Channel budget.** `session` (shell/exec/sftp) and `direct-tcpip` (`-L`
  forwards) draw from the **same** per-run cap,
  [`WARDYN_SSH_MAX_SESSIONS_PER_RUN`](ENV.md#wardyn_ssh_max_sessions_per_run)
  (default `4`), so an editor holding open a shell, an sftp session and two
  `-L` forwards has used the whole budget.
- A fifth channel of any kind is refused, not queued.
- `wardyn-sync` has its own cap of `2`; a `session` channel over the shared cap may only become one.
- **Under SSO, register the key as yourself.** Authorization is owner-only
  ([SSH.md](SSH.md)): the run has to be created by the **same principal** that registered
  the key.
- A key registered with the deployment's admin token can never
  satisfy that for a human's run — `POST /me/ssh-keys` 422s the attempt
  outright once OIDC is configured ([§1](SSH.md#1-register-a-public-key)).
- Point
  `wardyn ssh-key ensure` and `wardyn run` at your **own** API token
  (`WARDYN_TOKEN`) rather than `WARDYN_ADMIN_TOKEN`, or register the key from
  the console instead of the CLI.
