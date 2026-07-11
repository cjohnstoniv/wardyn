# Wardyn e2e validation results

Executed FOR REAL on this machine (Docker daemon available). Date: 2026-06-12.

- Host: Docker Engine 29.5.2, Docker Compose v5.1.3, Linux 6.6 (WSL2).
- Confinement: only `runc` runtimes registered (no `runsc`/gVisor), so the host
  is **CC1-only**. The compose stack correctly defaults to `examples/policies/demo.json`
  (`min_confinement_class: CC1`); `default.json` demands CC2 and fails closed here
  (invariant 5 — verified live earlier in the integrator pass).
- Driver/identity reported by `/healthz`:
  `{"confinement_classes":["CC1"],"identity_provider":"embedded","runner":"docker","status":"ok","trust_domain":"wardyn.local"}`

Re-runnable harness: `test/e2e/e2e.sh` (guarded by `WARDYN_TEST_DOCKER=1`; no-op
otherwise). Fixture image: `test/e2e/fixtures/` (alpine + curl + iproute2, no
ENTRYPOINT so the driver's `sleep infinity` Cmd holds the sandbox open).

## Build / vet / test checklist (all GREEN, both tag sets)

| Check | Result |
|---|---|
| `go build ./...` | exit 0 |
| `go build -tags docker ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `go vet -tags docker ./...` | exit 0 |
| `go test -count=1 ./...` | all packages `ok` |
| `go test -tags docker -count=1 ./...` | all packages `ok` |
| `WARDYN_TEST_DOCKER=1 go test -tags docker -run TestLifecycle_RealDocker` | **PASS** (was silently broken on Docker 29.x before the driver fix below) |
| `pnpm build` (ui/) | exit 0, 332 kB bundle |
| `gofmt -l` on edited Go files | clean |

## Live e2e assertions (test/e2e/e2e.sh): 12 PASS / 0 FAIL

Full run output ended with `e2e summary: 12 passed, 0 failed`, exit 0, clean teardown.

| # | Assertion | Result | Evidence |
|---|---|---|---|
| a | Sandbox has NO default route (L0) | **PASS** | `ip route` inside agent shows only `172.23.0.0/16 dev eth0 proto kernel scope link` — no `default` route. |
| b | Metadata IP 169.254.169.254 unreachable from sandbox | **PASS** | `curl http://169.254.169.254/...` → `curl: (7) Could not connect ... after 0 ms` (no route to link-local). |
| c | Disallowed domain + metadata IP DENIED by proxy + audit visible via API | **PASS** | Through a configured `wardyn-proxy`: `evil.example.com` → `CONNECT tunnel failed, response 403`; `169.254.169.254` → 403. API `GET /api/v1/audit?run_id=` shows `egress.deny outcome=denied` for `evil.example.com` (`rule_source=policy:default-deny`) and `169.254.169.254`. |
| d | Allowed domain passes via proxy | **PASS** | `github.com` → `http_code=200`; `api.anthropic.com` → `401` (reached Anthropic, unauth). API shows `egress.allow outcome=success rule_source=policy:allowed`. |
| e | Kill cascade: container gone + identity revoked + run.kill audit | **PASS** | Pre-kill internal call with run token = 202; `POST /runs/{id}/kill` = 202 `{"state":"KILLED"}`; agent container removed; **post-kill internal call with the same run token = 401** (run-level revocation cascade); run state `KILLED`; one `run.kill actor_type=human outcome=success` audit event. |
| f | Recording artifact served by GET /runs/{id}/recording | **PASS (store + endpoints)** + documented driver-delivery GAP | `PUT /api/v1/internal/recordings/{runID}` (run-token) = 204; cross-run id = 403; `GET /api/v1/runs/{id}/recording/{runID}` (admin) = 200 `Content-Type: application/x-asciicast` with the exact body; no-auth = 401. **GAP recorded below.** |
| g | OIDC login against Dex; session cookie authenticates GET /runs | **PASS** | `/auth/login` → 302 to Dex with `code_challenge_method=S256` + `nonce` + `state`; Dex authenticates `demo@wardyn.local`/`password` → 303 + code; wardynd callback → 302 `/` + `Set-Cookie: wardyn_session` (HttpOnly, SameSite=Lax, decodes to `email: demo@wardyn.local`); `GET /api/v1/runs` with the **session cookie only (no bearer)** = 200; no-auth = 401. |

Bonus structural evidence: a direct off-host request from the sandbox WITHOUT
the proxy fails at DNS (`Could not resolve host`) — the gatewayless internal
network gives the agent literally no path off its segment except the proxy.

The audit fanout file sink was also confirmed live: `egress.deny`,
`recording.upload`, and `run.kill` events all appear in `/data/audit/audit.log`
inside the `audit` volume while Postgres remains the source of truth (invariant 6).

## Fixes made to make the demo pass (minimal, in-scope)

The assignment permits minimal fixes anywhere except go.mod/go.sum/contracts/
migrations-0001-0002. Three were required; none touch a forbidden file.

### 1. Docker driver: agent L0 attachment (real bug, blocked the whole demo)

`internal/runner/docker/driver.go` created the agent with `NetworkMode: "none"`
and then `NetworkConnect`-ed it to the per-run internal network. Docker 29.x
rejects that transition:

```
docker: connect agent to internal network: Error response from daemon:
container cannot be connected to multiple networks with one of the networks
in private (none) mode
```

So `CreateSandbox` failed at the agent step and **no sandbox ever came up**
(the run landed FAILED). The real-Docker integration test `TestLifecycle_RealDocker`
would have caught this, but it only runs under `WARDYN_TEST_DOCKER=1` and had not
been exercised on Docker 29.x.

Fix: attach the agent to the per-run internal network **at create time** via
`NetworkMode(internalNetName) + NetworkingConfig`, instead of `none`+connect.
That network is `Internal: true` (gatewayless), so the agent still has **no
default route** — L0 is structurally identical, and now Docker 29.x accepts it.
Verified live: `ip route` shows only the on-link internal subnet (assertion a),
and `TestLifecycle_RealDocker` now PASSES.

Touched: `internal/runner/docker/driver.go` (the agent create block) and the
unit assertion in `internal/runner/docker/driver_test.go` (which asserted the
old, broken `NetworkMode == "none"`; it now asserts the correct gatewayless
internal-net attachment and that the agent never joins the control-plane net).

### 2. Dex client secret not env-expanded (broke OIDC token exchange)

The compose stack injected the Dex client secret via `${DEX_CLIENT_SECRET}` in
`dex.yaml`, relying on Dex config env-expansion. On this image (`dex v2.40.0`,
`DEX_EXPAND_ENV` unset) the substitution did NOT happen, so Dex compared the
incoming secret against the literal string `${DEX_CLIENT_SECRET}` and logged
`invalid client_secret on token request, client_id=wardyn`; the wardynd callback
returned `401 token exchange failed`. Enabling expansion was not an option — it
would corrupt the bcrypt password hash (`$2a$/$10$` segments read as undefined
vars, the same gotcha already documented for the hash).

Fix: make the Dex client secret a **literal** in `deploy/compose/dex.yaml`
(matching `WARDYN_OIDC_CLIENT_SECRET`'s default `wardyn-oidc-secret`), exactly
as the password hash already is, and drop the misleading `DEX_CLIENT_SECRET`
injection + comment from `deploy/compose/docker-compose.yaml`. Verified live:
full OIDC code flow now completes and the session cookie authenticates (assertion g).

### 3. Fixture image had no ENTRYPOINT conflict (test artifact, not product)

The first fixture image declared an `ENTRYPOINT`, which wrapped (instead of
replacing) the driver-supplied `Cmd ["sleep","infinity"]`, so the agent exited
immediately. Removed the ENTRYPOINT (documented in the Dockerfile); real agent
images must follow the same contract. This is purely the e2e fixture.

## Documented GAPS (recorded verbatim, NOT papered over)

> **STATUS UPDATE (2026-06-12, post-validation contract fixes):** both gaps
> below are CLOSED. GAP-1: `runner.ProxyConfig` gained a `Policy` field, the
> driver now delivers the full sidecar config (incl. the run's egress policy)
> via `WARDYN_PROXY_CONFIG_JSON`, and `wardyn-proxy` accepts env-var config —
> verified live by e2e assertion (h) (sidecar Running, restarts=0, policy
> loaded) and by the allow/pending/deny probes now flowing through the
> auto-launched sidecar (16/16 pass). The re-validation also surfaced and
> fixed a proxy ordering bug: a literal blocked IP (e.g. 169.254.169.254)
> previously fell into the first-use-approval path instead of the
> unconditional builtin deny (regression test:
> `internal/egress/proxy/ipguard_order_test.go`). GAP-2: the driver mounts
> `Config.RecordingMount` at `/wardyn/recordings` in agent containers and
> passes `-out-dir` to wardyn-rec; compose shares the `wardyn-recordings`
> volume with wardynd's `-recording-dir`. Unit-tested; live exercise awaits
> an agent image that ships wardyn-rec (v0.5 work item). The sections below
> are preserved as the historical record of the gaps as found.

### GAP-1 — runner-launched proxy is not configured (egress sidecar crash-loops) — CLOSED, see status update

The docker driver passes the proxy its run id / run token / control-plane URL /
listen port as **environment variables** (`proxyEnv` in `driver.go`), but the
`wardyn-proxy` binary (`cmd/wardyn-proxy/main.go`) requires `-config <JSON file>`
and reads NOTHING from the environment. The driver writes no config file and
sets no `-config` flag, so the auto-launched sidecar exits immediately:

```
wardyn-proxy: -config is required
```

Confirmed live: `docker logs wardyn-proxy-<run>` → that exact line; container
`Exited (1)`. Worse, even with an env fallback the proxy could not enforce the
run's allowlist, because **`runner.SandboxSpec` carries no egress policy** — the
control plane (`api.dispatch`) never hands the proxy the run's `RunPolicySpec`.
Closing this fully needs either a new field on `SandboxSpec`/`ProxyConfig`
(forbidden: `internal/types`, `internal/runner/runner.go`) or a driver-written
config file fed from the policy the control plane resolved — a design change
beyond a "minimal" fix. It is therefore left as-is and recorded here.

Honest validation path used for assertions (c)/(d): the e2e harness launches the
**same** `wardyn-proxy` binary the way it is designed to run — with a real
`-config` JSON carrying the policy + run token — on the per-run internal network,
then drives allow/deny/metadata from the agent through it. This proves the real
proxy binary + the real control-plane decision-ingest + the real append-only
audit pipeline end to end; only the driver's *automatic* sidecar configuration
is gapped.

### GAP-2 — recording delivery from inside the sandbox is not wired (integrator-documented) — CLOSED, see status update

Recorded verbatim from `cmd/wardynd/runner_docker.go`:

> the docker driver's recorderArgv does not yet pass wardyn-rec's delivery flags
> (-out-dir / -upload-url / -run-token), nor does CreateSandbox bind-mount the
> cast dir to a host volume. Until the driver gains that support, recordings are
> written inside the agent container's CastDir and are NOT yet surfaced to the
> control plane's recording store.

Consequence: wardyn-rec captures the session INSIDE the agent container but does
not upload it to `PUT /api/v1/internal/recordings/{runID}`. The control-plane
recording **store and both HTTP endpoints are fully wired and proven** (assertion
f: a cast uploaded with a valid run token is served back, with the auth gates
enforced). Automatic in-sandbox delivery is the docker driver's open work item;
when it lands, replay lights up with zero control-plane change.

## Note on the metadata-IP deny rule_source

In the CONNECT path the proxy evaluates the policy allowlist BEFORE the builtin
private/link-local IP guard, so `169.254.169.254` (not in the allowlist) is
denied at the allowlist stage with `rule_source=policy:default-deny` rather than
`builtin:private-ip`. Both are correct denies; the unconditional private-IP guard
remains the backstop for an allowlisted-but-DNS-rebinding host and is covered by
`internal/egress/proxy/proxy_test.go`. The client still receives a 403 for the
metadata IP either way (assertion c).

---

# REAL-AGENT milestone: a fully governed agent run, end to end (2026-06-12)

Executed FOR REAL on this machine. The prior sections proved the control-plane
invariants against the synthetic e2e fixture (no agent process, no `wardyn-rec`).
This section adds a REAL governed agent run against the **claude-code** agent
image and proves the brokered credential / LLM / recording paths LIVE. All 16
prior assertions remain intact and passing; the suite now totals **26 PASS / 0
FAIL** (`e2e summary: 26 passed, 0 failed`, exit 0, clean teardown).

- Host: Docker Engine 29.5.2, Docker Compose v5.1.3, Linux 6.6 (WSL2), CC1-only.
- Real agent image: `wardyn/agent-claude-code:demo` (built from
  `deploy/images/claude-code/Dockerfile`; ships `wardyn-rec`, `wardyn-git-helper`,
  `agent-run`, `asciinema`, and the `claude` CLI; `ANTHROPIC_BASE_URL` → the
  proxy LLM route). Resolved by the run via `WARDYN_AGENT_IMAGES`.
- Build / vet / test gate (re-confirmed GREEN, both tag sets): `go build ./...`
  and `go build -tags docker ./...` exit 0; `go vet ./...` and
  `go vet -tags docker ./...` exit 0; `go test -tags docker -race -count=1 ./...`
  all 17 packages `ok`; `gofmt -l` clean on every edited Go file.

## New live assertions (REAL-AGENT section of test/e2e/e2e.sh)

| # | Assertion | Result | Evidence |
|---|---|---|---|
| i | claude-code run → live sandbox RUNNING | **PASS** | `wardyn run --agent claude-code --task ...` → run `RUNNING`; the agent container is the real image (resolved via `WARDYN_AGENT_IMAGES`). |
| i | `run.exec` audited | **PASS** | One `run.exec outcome=success` audit event (driver Exec'd `/usr/local/bin/agent-run "<task>"`, wrapped by `wardyn-rec`). |
| i | recording cast AUTO-DELIVERED (no manual PUT) — **GAP-2 live closure** | **PASS** | `GET /api/v1/runs/{id}/recording/{runID}` → `200 Content-Type: application/x-asciicast`, served from the cast `wardyn-rec` delivered to the shared `wardyn-recordings` volume. The cast captured the real `claude -p` attempt failing fast (`Not logged in · Please run /login`) — exactly the documented "agent-run without an API key fails fast; the recorder still captures + delivers the attempt". |
| ii | brokered git-credential chain, LIVE | **PASS** | From inside the sandbox, `printf 'protocol=https\nhost=github.com\n\n' \| wardyn-git-helper get` → mint via proxy (run token injected proxy-side) → broker `409` raises a **credential `ApprovalRequest`** (visible at `GET /api/v1/approvals?state=PENDING`, `kind=credential`). Approved via `POST /api/v1/approvals/{id}/approve` (200). The helper re-mints and surfaces the documented fail-closed error verbatim: `re-mint after approval: unexpected mint status 500: {"error":"mint: broker: github_token grant but no GitHubMinter configured (fail closed)"}`. No `password=` emitted — no token leaked. Proves the FULL chain sandbox → proxy(token injected) → control plane → broker → approval. |
| iii | LLM route fail-closed | **PASS** | `curl $ANTHROPIC_BASE_URL/v1/messages` from inside the sandbox (no `api_key` grant) → `404 {"wardyn":"no_llm_credential","detail":"no LLM credential is brokered for api.anthropic.com"}`. The proxy holds/injects LLM credentials; none brokered ⇒ fail closed. |
| iv | negative: absolute-URI must NOT reach the local route / get the run token | **PASS** | An absolute-URI forward-proxy request `-x http://wardyn-proxy:3128 POST http://wardynd:8080/api/v1/internal/credentials/mint` is HELD by the proxy's egress allowlist as a first-use approval (`{"wardyn":"approval_pending",...}`) — `wardynd` is not in the demo allowlist. It NEVER reaches the internal mint endpoint and NO run token is injected (token injection + the mint/approval local routes are served ONLY for origin-form, path-only requests). No mint result. |
| ii | `brokered:mint` DecisionLog in audit | **PASS** | `>=1` audit event carrying `rule_source: brokered:mint` (the local-route mint forward emits a DecisionLog that lands in audit via the decisions pipeline). |
| iii | `brokered:llm` DecisionLog in audit | **PASS** | `>=1` audit event carrying `rule_source: brokered:llm` (the LLM route emits a DecisionLog likewise). |

## Two REAL product bugs found by the REAL-AGENT path and fixed (minimal, in-scope)

The assignment permits minimal fixes anywhere except go.mod/go.sum/contracts/
migrations. Both fixes below were REQUIRED to make the real governed run work
and neither touches a forbidden file. They were found by running the suite,
recorded verbatim, fixed, and re-validated — never papered over.

### BUG A — proxy SSRF guard blocked the proxy's OWN control plane (broke the WHOLE brokered chain)

`internal/egress/proxy/local_routes.go` `forwardToControlPlane()` resolved the
operator-configured control-plane URL through `vetURL → VetHost`, which applies
the agent-SSRF private/reserved-IP guard (invariant 3). But the control plane
LEGITIMATELY lives on a private network (`wardynd` → `172.x` on the Docker
internal net), so every brokered mint/approval forward died with:

```
unexpected mint status 502: control plane error: host "wardynd" denied:
blocked address 172.22.0.4: private/reserved v4 172.16.0.0/12
```

This broke the ENTIRE brokered credential chain (assertion ii) in any private-
network deployment — i.e. every real deployment. The SSRF guard exists to stop
the SANDBOX from reaching internal/metadata IPs via the forward-proxy path; it
must NOT apply to the proxy reaching its own TRUSTED control plane (same trust
boundary as the run token the proxy already holds).

Fix: a new `resolveTrustedURL` helper used ONLY for the control-plane forward.
It still resolves + PINS a single IP (TOCTOU / DNS-rebinding guard preserved)
but does NOT apply the private-IP denial. The agent-facing `vetURL` is unchanged
and still SSRF-guards the public LLM upstream (`api.anthropic.com`) and every
forward-proxy target. Verified live: the brokered chain now reaches the broker
and fails closed on the missing GitHubMinter (assertion ii), and the LLM public
upstream is still vetted (assertion iii).

### BUG B — recording auto-delivery EPERM'd for a non-root agent (GAP-2 could not close)

The docker driver wires recording (`Record:true`, `RecordingMount`), but two
directories were not writable by the NON-root agent user (uid 1000 per the image
contract):

1. `wardyn-rec -cast-dir` defaults to `/var/log/wardyn`; `/var/log` is root-owned
   `0755`, so `wardyn-rec`'s `MkdirAll` EPERM'd and it exited BEFORE recording —
   `claude` ran but no cast was ever written.
2. The `RecordingMount` target `/wardyn/recordings` is a fresh Docker named volume
   the daemon creates root-owned `0755`, so even a recorded cast could not be
   delivered there (`copyToDir` → `permission denied`).

Net effect: the cast never reached the recording store and `GET .../recording`
stayed `404` — GAP-2 could not be closed against a real agent image.

Fix (`internal/runner/docker/driver.go`): after the agent container starts (and
only when recording is enabled), a one-shot **root** exec (`User:"0:0"`)
`mkdir -p` + `chmod 0777`s the cast dir AND the recording-mount target, so any
agent uid can both write its in-progress cast and deliver it to the shared mount.
Best-effort (never fatal to sandbox bring-up); deployment-agnostic (works for any
agent uid). Verified live: the real `claude -p` session is recorded and the cast
auto-delivered, served back at `200 application/x-asciicast` with no manual PUT
(assertion i) — GAP-2 is now closed against a real agent image, not just the
control-plane store.

## Notes

- Optional real-LLM path (`WARDYN_E2E_ANTHROPIC_KEY`): the suite documents but
  does not auto-wire it — the full real path needs a boot-time `api_key` grant
  (injection rule host=`api.anthropic.com`) + the secret in wardynd's secret
  store, neither of which is exposed by the running compose stack's REST surface
  (no secret-create or policy-create route). Left OPTIONAL per the assignment.
- The earlier section-(f) NOTE ("auto delivery … is not exercised by THIS
  suite … needs the real agent image (v0.5)") is now superseded by assertion (i)
  above for the claude-code run; it remains accurate for the FIXTURE run, which
  still has no `wardyn-rec` and is used only for the control-plane invariant
  checks. The NOTE is left in place as the historical record for that fixture.

# Test-drive (repo-clone feature) — live validation

Executed FOR REAL on this machine. Date: 2026-06-13.

This pass validates the new repo-cloning feature (a run's `--repo` is now cloned
into the sandbox workspace through the governed egress path) and the guided
governance runner (`scripts/test-drive.sh`) plus the sample-workspace probe
library (`examples/workspaces/probes.sh`). It supersedes the earlier note in the
workspaces catalog that "a future cloning hook will clone it"; the hook is now
live and proven below.

- Host: Docker Engine 29.5.2, Docker Compose v5.1.3, Linux 6.6 (WSL2), CC1-only.
- Default policy in force: `examples/policies/demo.json` (allowlists `github.com`
  + `*.githubusercontent.com`, `first_use_approval: true`).
- Images rebuilt this pass so they carry the feature:
  - `wardyn/agent-claude-code:demo` — rebuilt; `agent-run` now contains the clone
    block (verified: 10 references to `WARDYN_REPO_URL`; the pre-existing image
    had 0). `agent-run --selftest` PASS (binaries present; repo wiring surfaced;
    selftest never clones, confirmed).
  - `wardyn/wardynd:demo` — rebuilt so `dispatch()` surfaces `WARDYN_REPO_URL` /
    `WARDYN_REPO_SLUG` (the integrator's runs.go change).

## Gate (all GREEN, both tag sets) — re-verified this pass

| Check | Result |
|---|---|
| `go build ./...` | exit 0 |
| `go build -tags docker ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `go vet -tags docker ./...` | exit 0 |
| `go test -count=1 ./...` | all packages `ok` |
| `go test -tags docker -count=1 ./internal/runner/docker/... ./internal/api/...` | both `ok` |
| `gofmt -l internal/api/runs.go internal/api/repoclone_test.go` | clean |
| `bash -n scripts/test-drive.sh` / `examples/workspaces/probes.sh` | OK |

## CRITICAL evidence — clone actually populates the workspace

Created a run `--repo octocat/Hello-World` with a non-empty task, waited for
RUNNING, then `docker exec`'d the agent container:

- Sandbox env carried ONLY the non-secret repo wiring (invariant 1 preserved):
  `WARDYN_REPO_SLUG=octocat/Hello-World`,
  `WARDYN_REPO_URL=https://github.com/octocat/Hello-World.git`,
  plus `HTTP(S)_PROXY=http://wardyn-proxy:3128`, `NO_PROXY=wardyn-proxy,...`.
  No token/key/secret values present.
- `ls ~/work/Hello-World` → `.git/  README` (the clone dir is populated).
- `cat ~/work/Hello-World/README` → `Hello World!` (real upstream content, not an
  empty placeholder).
- `git log -1 --oneline` → `7fd1a60 Merge pull request #6 from Spaceghost/patch-1`
  (real shallow clone with upstream history).

This proves the gap is closed: the repo is cloned into the workspace. It was
cloned through the ONLY egress path:

- `/proc/net/route` in the sandbox shows a single on-link subnet route
  (`eth0 000017AC` = the per-run internal /16) and **no default route**
  (`Destination 00000000` absent) → L0 structural egress enforced (invariant 3).
- `curl https://github.com/` via `wardyn-proxy` → `200`; direct (no-proxy)
  `curl https://github.com/` → http_code `000` (couldn't connect) → the only way
  the clone could have succeeded is through `wardyn-proxy`, where `github.com` is
  allowlisted. A public repo over HTTPS clones with NO credentials.

Subtlety recorded: the clone runs inside `agent-run`, which `dispatch()` only
`Exec`s when `run.Task != ""`. A run created with an EMPTY task is a sandbox-only
run (e.g. the probe sandbox) and intentionally does not clone — `~/work` stays
empty there. This is correct and not a regression.

## `scripts/test-drive.sh` — full end-to-end run (no Anthropic key)

Final result: **25 passed, 0 failed** (exit 0). What each section proved:

| § | Proves | Result |
|---|---|---|
| 1 | Clone + governed egress: run cloned `octocat/Hello-World` into `~/work/Hello-World`; L0 enforced (no default route via `/proc/net/route`) | PASS |
| 2 | Egress deny: `evil.example.com` via proxy → `CONNECT tunnel failed, response 403`; `egress.deny`/`pending` audit event visible | PASS |
| 3 | Metadata block: direct `169.254.169.254` unreachable (http `000`, no route); via proxy → `403` builtin deny; `egress.deny` audited | PASS |
| 4 | First-use approval: PENDING `egress_domain` approval raised → approved via API → state `APPROVED` (confirmed via the LIST surface) → audit event | PASS |
| 5 | Brokered git: `wardyn-git-helper get` raised `kind=credential` approval → approved → broker **fails closed** (`github_token grant but no GitHubMinter configured`); no token emitted; no token/key in sandbox env (invariant 1) | PASS |
| 6 | Kill cascade: pre-kill run token valid (202) → kill (202) → agent container gone → run token revoked (401) → state `KILLED` → `run.kill` audit with `actor_type=human` | PASS |
| 7 | Recording: synthetic asciicast uploaded via run token (204) → served by admin endpoint (200) → unauthenticated request rejected (401) | PASS |

Optional real-LLM scenario skipped (`ANTHROPIC_API_KEY` unset), as designed.

## `examples/workspaces/probes.sh` — live sandbox probes

Run against a fresh RUNNING sandbox (`SANDBOX_REF=wardyn-agent-<id>`):

- `l0_isolation` → PASS (reads `/proc/net/route`; no default route).
- `egress_deny` → PASS (`webhook.example.com` via proxy → code `000`, blocked).
- `metadata_deny` → PASS (direct `000`; via proxy `403` builtin `private-ip` guard).
- `git_credential` → PASS (raised `kind=credential` approval for the run; on
  approval the broker ran the mint path and failed closed — the documented stock
  demo PASS; no token leaked).

## Fixes applied this pass (script-side assertion bugs; governance was correct)

The underlying governance behavior was correct throughout; the first run exposed
THREE assertion bugs in the runner/probe scripts that produced misleading
results. All three are fixed and re-verified green.

1. **L0 check used a binary the slim agent image does not ship.** The claude-code
   image (`node:22-bookworm-slim`) has no `ip`/`route`/`ifconfig` binary, so
   `docker exec ... ip route` errored; the old grep for `default` then matched
   nothing and reported a **false PASS** for "no default route" — it would have
   passed even with a default route present, since the probe never ran. Fixed in
   `scripts/test-drive.sh` (§1) and `examples/workspaces/probes.sh`
   (`l0_isolation`) to read `/proc/net/route` directly (always present; no binary
   dependency) and detect a default route as an all-zero (`00000000`) Destination
   row via `awk`.

2. **Section 3 direct-metadata probe mixed curl stderr into the captured code.**
   `curl ... -w '%{http_code}' ... 2>&1 || echo "no-route"` produced the combined
   string `000no-route` plus curl's error line, so the `== "000" || == "no-route"`
   comparisons all failed and reported FAIL even though the direct connection
   correctly failed (curl exit 7, http_code `000`). Fixed to send stderr to
   `/dev/null` and compare the clean http_code.

3. **Section 4 confirmed approval state via a non-existent route.** The admin API
   exposes only `GET /api/v1/approvals?state=` (LIST) plus `POST .../approve|deny`
   — there is **no** `GET /api/v1/approvals/{id}` route, so the single-GET
   returned `404 page not found` and the state read came back empty (false FAIL).
   Fixed to confirm by finding the approval id in the `state=APPROVED` LIST result
   (the supported surface; no API/contract change).

No Go code, no contract files, no `go.mod`/`go.sum`, no migrations, no example
policies, and no `docker-compose.yaml` were touched in this pass. Files edited:
`scripts/test-drive.sh`, `examples/workspaces/probes.sh`, this `RESULTS.md`.

## Teardown

All test runs killed (kill cascade) and their agent/proxy containers + per-run
networks removed; `scripts/test-drive.sh` self-tears-down its runs (no `--keep`).
The compose stack (`postgres`/`dex`/`wardynd`) is brought down with `down -v` at
the end of validation, leaving no residual state.

## Notes

- `examples/workspaces/README.md` described the clone hook as "future" at
  validation time; that prose has since been fixed (the hook is live).

---

# Live TASK / TIER / RECORDING e2e (test/e2e/live)

`test/e2e/e2e.sh` above proves the security INVARIANTS with a fail-fast print
task. The `test/e2e/live` orchestrator (`//go:build docker`, `WARDYN_TEST_DOCKER=1`,
run via `make test-e2e-live`) proves the other half of the question: **did the
agent actually complete the task, and did the sandbox allow/block correctly** —
per confinement tier. It drives an already-running host-mode wardynd through its
API + the host docker daemon, and grades every run with a FRESH-container grader
that inspects final workspace STATE (never a transcript). Executed live on this
box (WSL2) on 2026-07-02 — first CC1-only under Docker Desktop, then **CC1 + CC2
against a native dockerd** (Docker Desktop can't persist custom runtimes; a native
Engine on its own socket carries gVisor). The `test/e2e/live` orchestrator runs
the corpus at EVERY class `/healthz` advertises.

## What "all three tiers" honestly means here

| Tier | Runtime | What was proven | UNVERIFIED |
|---|---|---|---|
| **CC1 / Fence** (runc) | installed | **Full**: tasks complete + graded, egress allow/block, interactive PTY, recording-replay, real Opus agent | — |
| **CC2 / Wall** (gVisor/runsc) | installed (native dockerd) | **Full**: same corpus + confinement re-run under gVisor; a real Opus agent fixed the bug inside a gVisor sandbox | — |
| **CC3 / Vault** (KVM microVM) | **installed — Kata** (`io.containerd.kata.v2`, QEMU + guest kernel 6.18.35 + virtiofsd) as the primary substrate, **krun/libkrun** (crun 1.28 `+LIBKRUN` + libkrun 1.19.3 + libkrunfw 5.5.0) as an alternative — both on native dockerd under WSL2 nested KVM | **FULLY PROVEN**: a real KVM microVM boots (guest kernel ≠ WSL host 6.6.87) under Wardyn's full hardening (CapDrop ALL, seccomp, no-new-privs, `--network none`, tmpfs). **All 5 corpus tasks + the egress allow/block boundary pass at CC3** through full dispatch, AND **a real Opus claude-code agent fixed the pricing bug inside a Kata microVM** (held-out grader, 40s) — the same bar as CC1/CC2. | — (Kata is not dead-ended: the earlier "containerd 2.2 / kata#12284" conclusion was a **misdiagnosis** — Kata is a containerd shim and was mis-registered as a docker OCI-runtime `path`; with `runtimeType: io.containerd.kata.v2` + the shim on PATH it boots fine on Docker 29.6/containerd v2.2.5. Note: the **krun** substrate runs the oracle/deterministic lane fully but NOT the claude-code agent — libkrun's minimal virtiofs hangs a FUSE request in its multi-turn loop; use Kata for interactive agents, krun where a plain OCI-runtime microVM is preferred.) |

**Vault runs on Kata (primary) or krun/libkrun (`CC3 = oci/kata` on `/healthz`).**
CC3 is defined by the *guarantee* — a per-sandbox KVM VM boundary — not one product.
The driver accepts **`kata*` OR `krun`** (probed in that order), rejects bare
`crun`/`runc`/`sysbox`/`runsc` for a CC3 *pin* (known non-VM), and lets an operator
vouch for any other registered OCI microVM (`WARDYN_CONFINEMENT_MAP=CC3=oci:<name>`)
while auto-advertise stays limited to the known-VM allowlist (never overclaim).

**Kata was mis-diagnosed as containerd-2.2-incompatible; it is not.** Kata is a
**containerd shim** (`io.containerd.kata.v2`), but `wardyn setup vault` had registered
it as a docker OCI-runtime **`path`** — so Docker drove the shim with the OCI-runtime
CLI and it exited 2 on the containerd `-info` handshake (wrongly blamed on kata#12284).
Registered correctly — `"kata": {"runtimeType": "io.containerd.kata.v2"}` + the shim
symlinked onto containerd's PATH — Kata boots real VMs on Docker 29.6 / containerd
v2.2.5 and runs the full claude-code agent. `kataScript()` in `cmd/wardyn/setup.go`
is fixed to emit the `runtimeType` form + the PATH symlink.

**Kata vs krun.** Kata (QEMU + a full guest kernel + production virtiofsd) runs
everything including the interactive claude-code agent. krun (crun+libkrun) is a
plain **OCI-runtime-binary** microVM — lighter, needs `/dev/kvm` + the kvm group in
the container, and runs the oracle/deterministic lane fully — but libkrun's minimal
built-in virtiofs hangs a FUSE request in claude-code's multi-turn tool loop, so use
**Kata for real interactive agents** and krun where an OCI-runtime microVM is preferred.

**Three code additions made CC3 real (all unit-tested):**
1. **`/dev/kvm` into the agent container** for a Vault runtime (`hardening.go`) — libkrun
   opens KVM from inside the namespaces; plus the **kvm group** as a supplementary group
   so the non-root agent can open the `0660 root:kvm` device with CapDrop ALL (no cap regained).
2. **Main-process execution mode** (`driver.go`) — libkrun microVMs have **no `docker exec`**
   ("the handler does not support exec"), so for an exec-less runtime the agent workload is
   created as the container's **main process** (recorder-wrapped), and `Wait` blocks on the
   container's exit rather than an exec. Exec-based runtimes (runc/runsc/kata) are untouched.
3. **`HOME=/home/agent`** on the exec-less path — libkrun runs the guest init as **root with
   `HOME=/`** (it ignores the image `USER`; the VM boundary, not the uid, is CC3's isolation),
   which otherwise broke `~`-relative agent paths.

The build is from-source on this Ubuntu/WSL2 box (no apt/prebuilt krun exists): libkrunfw
(guest kernel, patched to a GitHub-fetched `linux-6.12.91` because cdn.kernel.org lacked the
tarball) → libkrun **1.x** (crun `dlopen`s exactly `libkrun.so.1`; `main` is 2.0/`.so.2`) →
crun `--with-libkrun`. On native Linux a packaged krun makes this turnkey.

CC1, CC2, and now CC3 are genuinely confinement-tested end-to-end (real sandboxes under runc,
gVisor, and a libkrun KVM microVM). The remaining CC3 gap is the *agent*, not the tier:
claude-code's interactive loop hits a libkrun I/O race (above); the deterministic oracle
proves the CC3 execution PATH end-to-end.

## Live assertions (all PASS)

| Area | Assertion | Evidence |
|---|---|---|
| Tier matrix | CC1 + CC2 + CC3 all installed and accepted (no tier fails closed on this host) | `TestLive_TierMatrix` PASS (CC1,CC2,CC3) |
| **Per-tier corpus** | every gradeable task runs + grades at CC1 (runc), CC2 (gVisor), AND **CC3 (Kata KVM microVM)** | `TestLive_Tasks/{CC1,CC2,CC3}/oracle/*` PASS |
| Task: build-static-site | oracle builds a valid site; real HTML parse (h1, nav≥3, #menu≥3, CSS rules) | grader PASS |
| Task: fix-failing-test | fail-to-pass **and** pass-to-pass via a **held-out** test overlay (deleting the in-workspace test can't yield a pass) | grader PASS |
| Task: github-issue-fix | slugify matches the issue #42 spec (hidden held-out test) | grader PASS |
| Task: multi-file-feature | cross-file search feature works + add/list intact (held-out) | grader PASS |
| Task: egress-boundary | allowed host reachable via proxy (200); DENIED host blocked (curl rc≠0 / proxy 403); metadata IP no-route | grader PASS |
| Interactive | attach WS-PTY at CC1 AND CC2: `echo` round-trips; allowed host reachable (positive control); **in-session** egress boundary enforced (evil.example.com → 403) | `TestLive_Interactive/{CC1,CC2}` PASS |
| Recording-replay | on native docker the profile is SYNTHESIZED from real egress audit (`allowed_domains=[github.com]`), allow_all forced OFF + first_use_approval ON; relaunch confined to it still reaches github (200) while a never-recorded canary (example.com) is BLOCKED (rc=56) | `TestLive_RecordingReplay` PASS |
| **REAL model** (`WARDYN_E2E_REAL_MODEL=1`) | a real Opus **claude-code** agent BUILDS the site via the AI Composer + subscription mode and FIXES the pricing bug via the manual path at **ALL THREE tiers — CC1 (runc), CC2 (gVisor), and CC3 (Kata KVM microVM)** — held-out grader decides at each | `TestLive_RealModel/manual/{CC1,CC2,CC3}` + composer PASS (CC1 36s, CC2 38s, CC3 40s) |

The real-model runs produced genuine agent work (e.g. semantic HTML with CSS
custom properties; a one-line percentage-discount fix in `pricing.py` with the
tests left untouched) — verified un-gameable because the graders overlay hidden
held-out tests and inspect only final state.

## gVisor (CC2) driver fix found by running it

Under gVisor the sandbox couldn't resolve the `wardyn-proxy` sidecar — gVisor's
netstack does not traverse Docker's embedded DNS (127.0.0.11), so the agent lost
its only egress path (non-network tasks still ran + confined). Fixed in the docker
driver by pinning the proxy's IP in the agent's `/etc/hosts` (works under every
runtime, weakens nothing — the agent still has no default route). With that,
CC2's egress allow/block + interactive lanes pass identically to CC1.

## Environment notes

- **Native dockerd vs Docker Desktop**: Wall/Vault runtimes can't persist in
  Docker Desktop's managed VM, so CC2 was tested against a native Docker Engine on
  its own socket (`/var/run/wardyn-docker.sock`) — Docker Desktop keeps its socket
  (coexist). `run-host.sh` ensures the `wardyn-internal` control-plane network
  exists on whatever daemon wardynd targets.
- **Egress audit callback + recording synthesis**: on **native docker** the proxy's
  decision callback routes to wardynd, so `egress.*` audit events surface and the
  recording profile is synthesized from real observed behavior (above). On Docker
  Desktop/WSL the callback posts to `host.docker.internal` (the Windows host, not
  the WSL wardynd) so those events don't surface — there the allow/block proof
  rests on the graded in-sandbox evidence and the synthesized allowlist is empty
  (logged, never a false green). The audit→allowlist mapping is additionally
  covered by `internal/recordmode` unit tests.
- **Composer analysis backend**: the host `claude` CLI that generates a proposal
  can flake independently of the sandbox (e.g. `max_turns`); the suite SKIPS that
  composer sub-test on a backend flake (a composer-robustness issue, not a
  boundary defect) and the manual real-model lane covers the same task.

## Reproduce

```
# $0 deterministic (oracle) lane at every installed tier + interactive + recording:
WARDYN_TEST_DOCKER=1 make test-e2e-live

# add the real claude-code lane (needs a Claude subscription staged):
scripts/stage-claude-creds.sh
WARDYN_TEST_DOCKER=1 WARDYN_E2E_REAL_MODEL=1 make test-e2e-live
```

To exercise **CC2/Wall** (gVisor) you need a native Docker Engine with the runsc
runtime (Docker Desktop can't persist it). Once a native dockerd is running on
`/var/run/wardyn-docker.sock` with `wardyn setup wall --run` applied, point the
suite at it — the images must live in that daemon and wardynd must target it:

```
export DOCKER_HOST=unix:///var/run/wardyn-docker.sock
docker build -f deploy/compose/Dockerfile.proxy   -t wardyn/wardyn-proxy:demo .
docker build -f deploy/images/oracle/Dockerfile   -t wardyn/agent-oracle:demo .
docker build -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:demo .
WARDYN_TEST_DOCKER=1 make test-e2e-live   # /healthz now advertises CC1+CC2; the corpus runs at both
```

---

# Subscription proxy-side token injection — live proof (2026-07-03, commit 1138705)

Subscription-mode runs previously mounted a frozen COPY of the operator's `~/.claude`
OAuth credential; the access token expires (~hours) and the refresh token ROTATES as the
operator's own host `claude` refreshes, so the copy got locked out mid-run ("invalid
credentials, must login"). Fixed: the sandbox now holds only an inert SENTINEL (refresh
token blanked, expiry pinned, `mcpServers`+`mcpOAuth` stripped, credential backups removed)
and the egress proxy auto-enables TLS-MITM on `api.anthropic.com` and injects the operator's
LIVE, host-refreshed OAuth token per request.

| # | Proves | Result |
|---|--------|--------|
| s1 | **Injection is load-bearing** — a run whose sandbox `.credentials.json` accessToken was replaced with a GARBAGE token still authenticated (claude returned a weekly-rate-limit message, not `401`). The garbage token alone returns `401 Invalid bearer token` (verified before the CA/gate fixes landed). | **PASS** |
| s2 | **Clean-path** — a fresh sentinel run answered "17 + 25" → `42` through the injected token; `13` `scan:mitm` proxy decisions confirm MITM interception; **0** raw Bearer tokens in the decision log (live token never leaked). | **PASS** |
| s3 | **Confinement** — subscription = high-blast-radius → floored to **CC3/Kata** (real microVM); the injected-token session ran there. | **PASS** |
| s4 | **Sentinel hygiene** — the sandbox `.credentials.json` carries `refreshToken` length 0 + far-future `expiresAt`; no `.credentials-backup.json`; no host MCP servers/tokens resident. | **PASS** |

Six real bugs surfaced only by driving it live (all fixed): two scanner-gates blocked
MITM/injection without content inspection; interactive containers never installed the MITM
CA (bypass `agent-run`); a colliding `anthropic-api-key` grant crashed the proxy on direct
runs; `cp -a ~/.claude` broke on agent-owned files under `jobs/`; `.credentials-backup.json`
leaked a refresh token; 15 host MCP servers + `mcpOAuth` tokens were shipped into the sandbox.

Escape hatch: `WARDYN_SUBSCRIPTION_INJECT=off` keeps the legacy resident-copy behavior with
an honest staleness warning.

## Scripted into the live suite (2026-07-05)

Both the attach-walkthrough and the escape hatch are now **automated** lanes in
`test/e2e/live` (build tag `docker`), driven by `scripts/run-e2e-subscription.sh`
(`make test-e2e-subscription`). A single wardynd is in one inject mode, so the driver
RESTARTS wardynd with `WARDYN_SUBSCRIPTION_INJECT` flipped between the two lanes and
restores the safe default (on). The load-bearing discriminator is the wardynd-emitted
`run.llm.subscription_inject` audit event — reliable even on this Docker-Desktop/WSL host
where the proxy→control-plane egress callback may not route. Verified live on the Kata box,
both runs floored to **CC3/Kata**:

| # | Lane | Proves | Result |
|---|------|--------|--------|
| s5 | `TestLive_SubscriptionInject` (inject **on**) | Launching a subscription interactive run authors the proxy-side injection grant + enables TLS-MITM of `api.anthropic.com` (`run.llm.subscription_inject`/success, `tls_mitm=true`); `wardyn attach` reaches a live shell (PTY echo round-trips); an in-PTY curl to `api.anthropic.com` carrying only a GARBAGE sentinel credential traverses the injected+MITM'd path and reaches a real Anthropic HTTP response (not a proxy 403-deny, not an unroutable 000). | **PASS** |
| s6 | `TestLive_SubscriptionEscapeHatch` (inject **off**) | With `WARDYN_SUBSCRIPTION_INJECT=off`, the SAME run authors **no** injection grant (no `run.llm.subscription_inject` audit event, no MITM); the garbage sentinel credential reaches Anthropic over the opaque tunnel unmodified and is rejected **401** — the mirror image of s5, confirming the resident-copy legacy path. | **PASS** |

The optional true end-to-end turn (drive the real `claude` CLI in the attached PTY) rides
behind `WARDYN_E2E_REAL_MODEL=1` + the claude-code image: a rate-limit reply counts as PASS
(auth succeeded — s1's proof), only an explicit auth error fails.

Harness note: the onboarded-workspaces mount gate (`validateWorkspaceSources`) rejects any
local-dir mount source that is not a pre-onboarded workspace, which now covers EVERY live
lane. `harness.seedWorkspace` therefore onboards each seeded dir via the workspaces API
before mounting it (the real operator flow, not a bypass); `TestLive_Interactive` re-verified
green at CC1/CC2/CC3 under the gate.

---

# Onboarded workspaces — live proof (2026-07-04, commits 49c5f4a → 2b68883)

Sandboxes may no longer bind ANY host dir: a user onboards specific local dirs +
repos, each is scanned for its sandbox needs, and only onboarded sources may be
selected (multiple per run, mixing dirs + repos, per-item mount targets). Verified
end to end against a live wardynd on the Kata box via the API.

| # | Proves | Result |
|---|--------|--------|
| w1 | **Local-dir scan** — onboard a dir (go.mod + package.json+pnpm-lock + .devcontainer) → `POST /workspaces/{id}/scan` (host-side) → detected `languages=[Go,JavaScript]`, `package_managers=[go,pnpm]`, `egress_domains=[proxy.golang.org,registry.npmjs.org,sum.golang.org]`, `has_devcontainer=true`, confidence high; profile PERSISTED (status→ready). | **PASS** |
| w2 | **Mount gate (un-bypassable)** — a run with a NON-onboarded inline mount → `422 "mount source … is not an onboarded local directory"`; the ONBOARDED dir → `201 RUNNING`, and the sandbox has it bind-mounted at the requested target. | **PASS** |
| w3 | **Egress union** — the onboarded workspace's detected registries are unioned into the run's allowlist (`run.workspace.egress` audit). | **PASS** |
| w4 | **Repo GOVERNED scan** — onboard `octocat/Hello-World` → `202` + a governed scan run launches (CC1) → clones the repo → runs `wardyn-scan` (no agent/model) → uploads ScanFacts via the run-id-scoped brokered route → control plane derives + persists the profile (`git_remotes.github`, `access_hints=[github_token]`) → workspace flips to `ready` → run `COMPLETED` + auto-torn-down. Audit chain `run.exec → workspace.scan → run.complete`. | **PASS** |
| w5 | **Multi-workspace (mixed, custom targets)** — one run with two onboarded dirs (→ `/home/agent/work/ws1`, `/ws2`, read-only) + a repo (→ `/home/agent/work/hello`): both dirs bind-mounted with their files visible, and `workspace_repos` produced the injection-safe tab-framed `WARDYN_REPOS` clone contract. | **PASS** |

Compose-path multi-workspace and private-repo governed scan are now BOTH done: the
`/runs/compose` API accepts a `workspaces` array and the compose form uses the same
multi-select picker as the wizard (commits dd85d27/90906f8); the repo scan run
carries a read-only github_token grant so a private repo scans when a GitHubMinter
is configured (public repos clone credential-free — regression-verified). The subscription
attach-walkthrough + escape-hatch e2e lanes (prior feature) are now scripted too — see s5/s6
above.
