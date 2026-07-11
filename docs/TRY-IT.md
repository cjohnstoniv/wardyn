# Try Wardyn in 10 minutes

Everything below runs on a single machine with Docker. Three levels, easiest
first: the **fastest start** (just the UI + Getting-started — no keys, no login),
the **governance demo** (a scripted governed run, no API keys), and the **real
agent run** (bring an Anthropic API key).

## Level 0 — fastest start: the UI + Getting-started (no keys, no login)

The recommended first step. One command detects your host, sets up **host mode**
(sandbox agents on this machine with your Claude login), launches with no SSO
and no token, and opens the **Getting-started page** in your browser the moment
the UI is live:

```sh
make setup    # host-mode installer → launch → open http://localhost:8080
              # wardynd runs in the background: log/PID in ~/.wardyn/, stop with `make stop-host`
```

> `make setup` asks which of the two supported single-user setups you want
> (Enter = host): **host mode** (above) or **containerized** (the compose
> stack; `wardynd` runs in a container so workspace Verify/Record work on
> Docker Desktop + WSL2 NAT, and model access comes from an API key/Bedrock
> instead of your host login). Headless runs default to host; scripts pick
> with `WARDYN_SETUP_MODE=local|container`. **Team mode** — that same compose
> control plane as a shared *multi-user* service (SSO logins, per-user
> identity/RBAC) — is **coming soon**.

The Getting-started page detects this host's real capabilities — which confinement
tiers are available (Fence = CC1 hardened runc, Wall = CC2 gVisor, Vault = CC3
Kata microVM; whichever are missing show a copy-paste
`wardyn setup wall` / `wardyn setup vault` command tailored to your OS and Docker
setup), whether an LLM path exists, and secret-store durability — then links
straight into your first run. This is all you need to look around. On WSL, run it
inside your WSL distro's shell; it opens the UI in your Windows browser
automatically.

> **Inside a corporate network?** The enterprise-engineer path is built in:
> the Getting Started wizard's **Corporate network** steps chain the sandbox
> proxy through your corporate HTTP proxy and redirect npm/pip/cargo/maven/
> go/nuget to an Artifactory/Nexus mirror; the **SCM Provider** step covers
> GitHub Enterprise / Azure DevOps (PAT or SSH-over-443 through the credential
> broker); **AWS Bedrock** (below) gives Claude access with no direct Anthropic
> egress, billed through AWS; and `make setup`'s build retries with
> `GOTOOLCHAIN=local` when a proxy blocks the public Go module proxy.

## Level 1 — governance demo (no keys)

```sh
make agent-images        # build wardyn/agent-claude-code:local (+codex)
make demo                # compose up: postgres + dex + wardynd; creates a demo run
```

Then:

- **UI**: http://localhost:8080 — use the CLI or admin token. (Human SSO login
  via the UI is **coming soon** — the "Sign in with SSO" button is disabled in
  this version, though the `/auth/login` flow still works server-side.)
- **CLI** (inside or outside the container):

```sh
export WARDYN_URL=http://localhost:8080 WARDYN_ADMIN_TOKEN=demo-admin-token
wardyn run --agent claude-code --repo octocat/hello-world --task "explain this repo"
wardyn runs list       # watch state
wardyn audit --run <id>
```

What you can verify live, even without keys:

| What | How |
|---|---|
| L0 isolation | `docker exec wardyn-agent-<id> ip route` → no default route |
| Egress policy | from the sandbox, curl an unlisted domain via the proxy → 403 + a pending approval in the UI |
| Approval queue | the Approvals tab; approve/deny and watch the audit trail |
| Attributed audit | Audit tab: every event carries `actor_type` human/agent/system |
| Terminal replay | Runs tab → Replay (the recorder captures even failed agent starts) |
| Kill switch | `wardyn kill <id>` → container gone, run token revoked (401), audit `run.kill` |
| Brokered credentials | `docker exec wardyn-agent-<id> sh -c 'printf "protocol=https\nhost=github.com\n\n" \| wardyn-git-helper get'` → raises a credential approval; approving it hits the fail-closed mint (no GitHub App configured) — the whole chain is visible in audit |

## Level 2 — real Claude Code run (bring an Anthropic API key)

```sh
# 1. Store the key (write-only; no API path ever returns it):
echo "$ANTHROPIC_API_KEY" | wardyn secret set anthropic-api-key

# 2. Switch the default policy to the LLM-enabled one and restart wardynd:
WARDYN_DEFAULT_POLICY=/examples/policies/claude-llm.json docker compose \
  -f deploy/compose/docker-compose.yaml up -d wardynd

# 3. Create a real run:
wardyn run --agent claude-code --repo octocat/hello-world \
  --task "Read the repository and write a SUMMARY.md describing it"
```

What happens: the run's policy carries an auto-mintable `api_key` grant for
`api.anthropic.com`; the proxy resolves the key **at startup, into proxy
memory only** (the sandbox never sees it — check: `docker exec
wardyn-agent-<id> env | grep -i key` is empty); Claude Code talks to
`ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic`, where the
proxy injects `x-api-key` and logs every model call as a `brokered:llm`
decision in the audit trail. Watch the session live in the Replay tab.

To also enable real GitHub pushes: create a GitHub App (contents+PR write),
then `wardyn secret set github-app-id` and `wardyn secret set github-app-key`
(PEM), restart, and approve the credential request the agent raises — the
minted installation token is 1h, repo-scoped, and permission-clamped to
`contents:write` + `pull_requests:write`. Branch-namespace confinement
(`wardyn/<run-id>/*`) is recorded in the token metadata but is
**advisory-only today** — the token can push to any branch (including the
default) within its granted repos; real branch-namespace enforcement is
**[v0.5 — planned]** (see `threatmodel/THREAT-MODEL.md` asset #4).

### Model auth: three ways to give Claude Code its LLM access

Wardyn credentials a Claude run one of three ways (precedence: subscription →
Bedrock → api-key). All keep the real credential out of the sandbox *except* the
Bedrock access-key path (see below):

- **API key** (Level 2 above) — `wardyn secret set anthropic-api-key`. The proxy
  injects `x-api-key` at startup; **never resident**.
- **Subscription (OAuth)** — mount your host `~/.claude` into the run (New Run →
  Access → Subscription). The proxy injects your **live** OAuth token as
  `Authorization: Bearer`; the sandbox holds only an inert sentinel. Host mode
  stages this during `make setup`; if you skipped the prompt (or ran headless),
  `make stage-claude` stages it later and restarts wardynd. On compose,
  stage creds with `WARDYN_SUBSCRIPTION_INJECT=off scripts/stage-claude-creds.sh`.
- **AWS Bedrock** — operator-configured (not a per-run choice). Set
  `WARDYN_BEDROCK_REGION` + `WARDYN_BEDROCK_MODEL` (a cross-region *inference-profile*
  id, not a bare model id) and add credentials to the secret store:
  - `bedrock-api-key` (a Bedrock **bearer** token) → proxy-injected as
    `Authorization: Bearer` into `bedrock-runtime.*`, **never resident** (preferred).
  - or `aws-access-key-id` + `aws-secret-access-key` (+ optional `aws-session-token`)
    → AWS SigV4 signs in-process, so these are **resident** in the sandbox env
    (masked + withheld from verify/scan runs; scope IAM tightly — see
    `threatmodel/THREAT-MODEL.md` "Bedrock credential residency").

  Configured Claude runs then use Bedrock automatically.

## Level 2.5 — record a session, rerun it as a governed profile

The primary way to onboard your own work: in a workspace, **record** a named
interactive session (with model access), then rerun it governed — the New Run
dialog's Basics step offers the workspace's recorded sessions as **profiles**;
picking one fast-tracks you to Review with the recording's observed egress
already loaded into the allowlist. **Verify** launches a fresh CONFINED session
for a recording you pick — default-deny egress limited to the approved set,
live approvals surfaced next to the attached terminal — so you re-run the same
steps under least privilege and prove the profile works before relying on it.
(It's a live re-run under the tighter policy, not a byte-for-byte replay of the
captured session. The workspace *import* flow has its own Verify step with
different semantics: it executes the operator-approved setup commands in a
governed sandbox to prove the environment builds.)

## Level 3 — enable the AI Composer (describe a task, get a proposed run)

The **AI Run Composer** turns a plain-English task into a *proposed* confined run
(agent, repo, confinement, egress, grants) that Wardyn grades for you to review before
launch. It's off by default, and in this release the Describe surface is
additionally hidden in the UI: enabling it needs a backend via
`WARDYN_COMPOSER_CONFIG` **and** the `COMPOSER_UI_ENABLED` flag flipped to
`true` in `ui/src/app/lib/features.ts` (a UI rebuild).

No API key — deterministic demo:

```sh
echo 'WARDYN_COMPOSER_CONFIG={"default":"dev","backends":[{"name":"dev","wire":"fake","model":"demo"}]}' >> deploy/compose/.env
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

Real prompt-driven proposals — Anthropic API + Opus:

```sh
wardyn secret set anthropic-api-key   # paste your key (write-only; no API path returns it)
echo 'WARDYN_COMPOSER_CONFIG={"default":"claude","backends":[{"name":"claude","wire":"anthropic","transport":"api","model":"claude-opus-4-8","api_key_secret":"anthropic-api-key"}]}' >> deploy/compose/.env
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

`wardynd` logs `AI Run Composer enabled (backends=[...] default="...")` on boot. With
`COMPOSER_UI_ENABLED=true` built into the UI, the New Run dialog offers
**Describe your task**: type a task and review the proposal — the provider/model is
shown, every choice is risk-graded, and you can pick **Interactive** (attach and
drive) vs **Autonomous**. More templates (incl. the Claude CLI via your
subscription, and OpenAI) are in [`examples/composer-configs/`](../examples/composer-configs/).

## Stop / Reset

```sh
make stop-host           # Local (host) mode: stop the background wardynd (PID in ~/.wardyn/)
make compose-down        # compose stack: stop it — KEEPS your data (runs + audit)
make reset               # start over from an empty Runs list: wipes Postgres + recordings volumes
                         # (confirms first — default No; WARDYN_FORCE_RESET=1 headless)
```

`make reset` operates on the **compose** stack: it wipes those volumes and
brings up a *containerized* wardynd. It does not touch a host-mode daemon — to
reset host mode, `make stop-host && make setup`.

Honest limits of this demo deployment (see `threatmodel/`): single host,
CC1/CC2 only unless a Kata runtime is registered (CC3/Vault is experimental —
needs /dev/kvm + Kata; not available on Docker Desktop), wardynd holds the host Docker
socket (daemon-trust tradeoff, loudly documented in the compose file), and
the model-API channel is a logged-but-open data path by design.
