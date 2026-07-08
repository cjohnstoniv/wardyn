# Wardyn agent images

This directory contains the OCI image definitions for coding-agent sandboxes
governed by Wardyn.  Each subdirectory is one agent image.  All images conform
to the **Wardyn agent image contract** described below.

## Image contract

Every Wardyn agent image MUST satisfy the following invariants.  The docker
driver (`internal/runner/docker/driver.go`) and the e2e suite enforce them.

### 1. No ENTRYPOINT

The image must declare **no ENTRYPOINT**.  The Wardyn docker driver sets only
the container *Cmd* to `["sleep", "infinity"]` at create time so the sandbox
stays alive while the driver calls `Exec` to launch the agent process.  An
ENTRYPOINT would wrap the driver-supplied Cmd (making it an argument to the
entrypoint), and if that entrypoint exits it tears the sandbox down immediately.

### 2. `/usr/local/bin/agent-run "<task>"`

Every image ships an executable shell script at this path.  The driver (or an
operator) calls it to run one task non-interactively:

```
agent-run "<task text>"   # run one task; exit with the CLI's code
agent-run --selftest      # verify binaries + env wiring; no API key needed
```

`--selftest` is used by e2e validation to confirm the image contract before a
live task is scheduled.

### 3. `/usr/local/bin/wardyn-rec`

The session recorder binary, built from `cmd/wardyn-rec`.  It wraps the agent
process and records the PTY session via `asciinema` (GPL subprocess, never
linked) or falls back to a plain `.log` file when asciinema is absent.  The
docker driver calls `wardyn-rec` (via `Exec`) instead of calling `agent-run`
directly when recording is enabled.

### 4. `/usr/local/bin/wardyn-git-helper`

The Git credential helper binary, built from `cmd/wardyn-git-helper`.  It
speaks the [Git credential protocol][cred-proto] and obtains tokens by calling
`POST /wardyn/v1/credentials/mint` on the proxy (see credential flow below).
The system `gitconfig` wires it for `https://github.com`.

### 5. USER agent (uid 1000)

The image creates user `agent` with uid 1000, home `/home/agent`, and
workspace `/home/agent/work`.  All agent activity runs as this user.

### 6. System gitconfig

```ini
[credential "https://github.com"]
    helper = /usr/local/bin/wardyn-git-helper
```

This is set via `git config --system` during the image build so every `git`
invocation inside the sandbox uses the brokered credential helper without any
per-user configuration.

---

## Credential and recording flow

```
┌──────────────────────────────────────────────────────────┐
│  agent sandbox (no default route — L0 structural egress) │
│                                                          │
│  agent-run / claude / codex / git                        │
│    │  ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic
│    │  HTTP_PROXY=http://wardyn-proxy:3128                │
│    │                                                     │
│    ▼                                                     │
│  wardyn-proxy:3128  (only egress path)                  │
│    ├─ /wardyn/v1/credentials/mint  ──► wardynd /api/v1/internal/credentials/mint
│    │     proxy injects run token; response (minted token) passed through
│    ├─ /wardyn/v1/approvals/{id}    ──► wardynd /api/v1/internal/approvals/{id}
│    │     proxy injects run token; response passed through
│    ├─ /wardyn/llm/anthropic/<rest> ──► https://api.anthropic.com/<rest>
│    │     proxy applies brokered api_key InjectionRule (host=api.anthropic.com)
│    │     if no rule is configured: 404 (no LLM credential brokered)
│    └─ all other outbound requests: enforced against the run's egress policy
│                                                          │
│  wardyn-rec (PTY recorder)                              │
│    wraps agent process; delivers cast to shared volume   │
│    or via PUT /api/v1/internal/recordings/{run_id}       │
└──────────────────────────────────────────────────────────┘
```

### Git credential flow

1. `git clone https://github.com/...` triggers the system credential helper.
2. `wardyn-git-helper get` is called; it reads `WARDYN_GITHUB_GRANT_ID` from
   the sandbox env (set by the driver at dispatch — this is the UUID of the
   run's `github_token` CredentialGrant, not a secret).
3. The helper calls `POST /wardyn/v1/credentials/mint` via the proxy (origin-
   form URL — the proxy serves it locally, injecting the run token toward
   `wardynd`).
4. `wardynd` verifies the run token, checks the approval gate, and mints a
   short-lived GitHub installation token (via the GitHub App).
5. The helper emits the token to git's stdout in the credential protocol format.
6. git uses the token for the clone/fetch; the token is never stored to disk
   (git credential store is not configured in the sandbox image).

The run token itself NEVER appears in the sandbox environment.  The proxy holds
it and injects it only when forwarding internal API calls.

### Recording flow

1. The docker driver calls `wardyn-rec -run <uuid> -cast-dir /var/log/wardyn
   [-out-dir /wardyn/recordings] -- <agent argv>`.
2. `wardyn-rec` execs `asciinema rec` (asciinema is GPL; wardyn-rec is Apache-
   2.0; they are never linked — this is the boundary).
3. When `asciinema` is absent (stripped image), `wardyn-rec` tees combined
   stdout/stderr to a `.log` file (degraded but non-blocking).
4. After the agent process exits, `wardyn-rec` copies the finished cast to the
   shared `wardyn-recordings` volume (`-out-dir`), from where `wardynd`'s
   recording store serves it.

---

## Available images

| Directory       | Image tag                    | CLI        |
|-----------------|------------------------------|------------|
| `claude-code/`  | `wardyn/agent-claude-code:demo` | `claude` (`@anthropic-ai/claude-code`) |
| `codex-cli/`    | `wardyn/agent-codex-cli:demo`   | `codex`   (`@openai/codex`) |

---

## Adding a new agent image

1. Create `deploy/images/<name>/Dockerfile` (multi-stage: Go builder + runtime).
2. Copy both binaries from the builder stage:
   ```dockerfile
   COPY --from=builder /out/wardyn-rec        /usr/local/bin/wardyn-rec
   COPY --from=builder /out/wardyn-git-helper /usr/local/bin/wardyn-git-helper
   ```
3. Add `deploy/images/<name>/agent-run` implementing the `--selftest` and task
   modes (see existing scripts for the pattern).
4. Set the system gitconfig credential helper (required for git brokering).
5. Create user `agent` uid 1000, home `/home/agent`, work `/home/agent/work`.
6. Do NOT add an ENTRYPOINT.
7. Add a `profiles: [build-only]` stanza to `deploy/compose/docker-compose.yaml`
   mirroring the `proxy-image` and `agent-claude-code` stanzas.
8. Add the image to the `agent-images` Makefile target.

[cred-proto]: https://git-scm.com/docs/gitcredentials#_custom_helpers
