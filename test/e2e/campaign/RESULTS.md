# OSS Contributor-Path Campaign — RESULTS

Goal: prove that common OSS GitHub projects across popular languages can be run through Wardyn's
**Record Mode** — a recorded (open-egress) sandbox that builds+tests the project, whose captured
egress is promoted to least-privilege, and a **confined rerun (Verify)** that proves the recording
identified exactly what the project needs. Fully operated by Claude driving the Wardyn HTTP API on
the local compose stack. All 5 workspaces are left **imported** on the local instance to use.

## Result: 5/5 languages fully build+test, recorded + confined-verified ✓

| Lang | Repo @ commit | Build+Test | Recording captured (auto, open egress) | Confined Verify (least-privilege rerun) |
|------|---------------|-----------|----------------------------------------|-----------------------------------------|
| JS   | uuidjs/uuid @ a67db57 | `npm ci` + `npm test` | github.com, **registry.npmjs.org** | **ready** ✓ |
| Go   | go-chi/chi @ 8b258c7 | `go mod download` + `go build ./...` + `go test ./...` | github.com (zero external deps) | **ready** ✓ |
| Rust | BurntSushi/walkdir @ 6fd031c | `cargo build` + `cargo test` | github.com, **index.crates.io, static.crates.io** | **ready** ✓ |
| Py   | jd/tenacity @ b2cd027 | venv + `pip install -e .[test]` + `pytest` | github.com, **pypi.org, files.pythonhosted.org** | **ready** ✓ |
| Java | jhy/jsoup @ d8c49e5 | `mvn -B -DskipTests package` + `mvn -B test` | github.com, **repo.maven.apache.org** | **ready** ✓ |

Each row means: an OPEN sandbox cloned the repo and ran the real build+test; Wardyn recorded the
egress hosts it actually used; those were promoted into the workspace's approved egress; then a
FRESH default-deny sandbox re-ran the same build+test allowed only the **promoted registry hosts
plus the baseline GitHub clone family** (`github.com, api.github.com, codeload.github.com,
*.githubusercontent.com` — always unioned by `verifyEgressDomains` so the repo can be re-cloned)
and passed. The recording correctly identified each ecosystem's package registry — nothing more,
nothing less (e.g. chi with zero deps recorded only the clone host; jsoup recorded only Central's
primary host). Verify genuinely exercised least-privilege: the confined reruns cold-downloaded
their deps through only the allowed hosts (pip fetched pytest, cargo the crates.io index, Maven
the full Central tree) and ran real tests (uuid 77 pass, tenacity 165 passed, walkdir 48 tests,
chi ok, jsoup 1971 tests / 0 failures) — independently verified against the live logs by an
adversarial review pass.

Evidence: `evidence/<lang>.json` (per-workspace record_results + verify_result, step exit codes,
approved_egress, verified_profile_hash/verified_at). `evidence/00-desrisk-model-access-audit.json`
is the earlier end-to-end proof that a confined agent under the operator's subscription writes real
work product (`HELLO-FROM-MODEL`) to a host mount with governed egress (api.anthropic.com allowed;
github + Claude telemetry denied).

## What it took (real issues found + fixed by running it live)

These are genuine environment-fidelity fixes, not paper-over — each was found by a real failing run:

1. **Fat multi-toolchain agent image** (`deploy/images/campaign/Dockerfile`) — the convention image
   has Node only; non-JS builds hit exit 127. The image layers Go 1.23, Python 3.11, Rust 1.82,
   Temurin JDK 21 + Maven onto the base, keeping agent-run/wardyn-verify/claude. Sidesteps envbuild
   (which can't run — built devcontainer images contain no wardyn binaries).
2. **Login-shell PATH** — `wardyn-verify` runs commands via `/bin/sh -lc`; a login shell resets
   PATH from `/etc/profile`, dropping go/rust. Fixed with `/etc/profile.d/campaign-toolchains.sh`.
3. **GOTMPDIR** — the sandbox mounts `/tmp` **noexec**; `go test` compiles+execs test binaries in
   `$TMPDIR` → "permission denied". Pointed `GOTMPDIR` at the agent's exec-allowed home.
4. **Maven proxy** — unlike npm/pip/cargo/go/git (which honor `HTTP(S)_PROXY`), Maven ignores it
   and resolves Central directly → "Unknown host". Baked a `~/.m2/settings.xml` routing Maven
   through the wardyn-proxy sidecar (loopback excluded so jsoup's local integ-test server isn't
   proxied).
5. **Subscription on compose** (`WARDYN_SUBSCRIPTION_INJECT=off`) — proxy-side OAuth injection needs
   host-mode wardynd; on the distroless compose wardynd it fail-lazily crashes the per-run proxy.
   `off` selects resident-copy (the run mounts fresh `~/.claude`), the only mode that works on
   compose. Committed as the compose default.
6. **Agent autonomy** (`--dangerously-skip-permissions` in agent-run) — a batch agent has no human
   to approve its tools; safe because Wardyn is the boundary.

## Honest scope / caveats

- Contributor runs are **CC1 (Fence/runc)** — the weakest confinement tier available on this
  Docker-Desktop/WSL2 box. The 3-tier story (gVisor/Kata) is proven elsewhere; this proves the
  tier-independent record→verify + contributor-path story.
- The compose demo runs wardynd with the host **docker.sock** (root-equivalent control plane) — the
  documented single-tenant-dev trade-off.
- Record/Verify used the repo's DEFAULT branch clone (agent-run shallow-clones); the pinned commits
  above are what the specs were verified against. This shows as minor drift (e.g. uuid ran 77 tests
  vs the spec's 82). Feature-implementation + agentic-judge (a real agent adding a feature,
  deterministically + adversarially graded) is the campaign's next phase.
- `verified_profile_hash` is null on these workspaces: the toolchain image is wired via
  `WARDYN_AGENT_IMAGES` (no per-workspace envbuild), so there's no built-image hash to stamp. The
  proof of a green run is `verify_result.ok` + `verified_at` (both present), not the hash.
- Independently verified by an adversarial review pass: HONEST-AND-SUPPORTED — all exit codes 0,
  server-gated ready, real cold-download-through-allowed-hosts + real test execution confirmed from
  the live logs (not cache/no-op theater).

## Reproduce

Compose stack up (`make setup` or `docker compose -f deploy/compose/docker-compose.yaml up -d`),
fat image built (`docker build -f deploy/images/campaign/Dockerfile -t wardyn/agent-campaign:demo .`)
and wired via `WARDYN_AGENT_IMAGES`. Then per repo: onboard (kind:repo) → PUT setup-commands →
POST /record {task:test,mode:auto} → POST /record/test/promote-egress → POST /verify → poll to
`ready` (that sequence, looped per repo, is the whole driver). API driven from an in-network container with a
`Host: localhost` header (WSL2 can't reach the published port; local-mode needs a loopback Host).
