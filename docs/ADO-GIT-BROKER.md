# Design: never-resident Azure DevOps git egress (proxy-side ADO broker)

Status: **design + boundary** — the security-critical wiring touches the
credential path and cannot be end-to-end validated without a real Azure DevOps PAT
and repository, so it is specified here for a reviewed PR rather than shipped
untested. The current, working ADO support (below) is unchanged.

## What works today: the resident `git_pat` lane

Azure DevOps is reachable from a sandbox **now**, via the `git_pat` grant kind:

- An operator creates a `git_pat` eligible grant whose host is `dev.azure.com`
  (or `*.visualstudio.com`). Dispatch surfaces it to the sandbox as
  `WARDYN_GIT_PAT_GRANTS` and opens the matching egress bundle
  (`internal/api/runs_create.go:230-233`, `adoEgressDomains`).
- Inside the sandbox, `wardyn-git-helper` answers git's credential protocol by
  minting the PAT and emitting it to git, using username `pat` for
  `dev.azure.com` / `*.visualstudio.com` (`internal/broker/scope.go:82-93`).

**The limitation this design addresses:** on this lane the PAT is **resident** —
it reaches the sandbox's git process (streamed over the helper's stdout, gated by
a per-run secret file, wiped after use, but resident nonetheless). GitHub, by
contrast, is never-resident: the proxy-side git broker
(`internal/egress/proxy/git_broker.go`) injects a GitHub **App installation
token** on the outbound request and never returns it to the sandbox. This design
brings ADO to the same never-resident posture.

## The ceiling (state it; do not imply parity)

The GitHub broker's strength is **two** properties: never-resident **and**
repo-scoped, short-lived (a GitHub App installation token is minted per-repo and
expires in ~1h). For ADO, Wardyn can deliver the **first** but not the second
today: there is **no ADO token-minting API integration**. Wardyn holds the
operator-provisioned PAT and can only forward it; it **cannot down-scope it to a
single repo or expire it per-use** — `internal/broker/broker.go:555-564` already
records this ceiling for the `git_pat` kind. So the deliverable is:

> **Never-resident ADO egress** (a real improvement over the resident lane — the
> PAT stays proxy-side, exactly like the GitHub token), scoped by **whatever the
> operator set on the PAT in Azure DevOps** (project/org-level, Wardyn cannot
> narrow it). It is NOT the per-repo, auto-expiring scoping GitHub App tokens give.

A true repo-scoped ADO broker would require Wardyn to integrate an ADO
token-minting flow (e.g. Entra ID service-principal + fine-grained PAT/OAuth
issuance) — out of scope until that API integration exists.

## Extension points (mirror the GitHub broker exactly)

The GitHub broker is the template; ADO is a parallel route, not a new mechanism.

1. **Route + option** — `internal/egress/proxy/config.go` + `proxy.go`: add
   `AdoGrants map[string]uuid.UUID` (key `"<org>/<project>/<repo>"`) alongside
   `GitGrants`, plumbed through `Options` → `Proxy` exactly like `gitGrants`
   (`proxy.go:136-138,190-191,793`). Register `/wardyn/ado/` next to
   `/wardyn/gh/`.

2. **Handler** — `internal/egress/proxy/ado_broker.go`, mirroring
   `handleGitBroker`:
   - `parseAdoBrokerPath` splits `/wardyn/ado/<org>/<project>/<repo>/<rest>` into
     the canonical `"<org>/<project>/<repo>"` allowlist key and the smart-HTTP
     subpath, with the same `gitSegSafe` traversal rejection.
   - allowlist lookup in `p.adoGrants` (403 on miss — repo is the unit of trust);
   - fetch the PAT proxy-side via the existing `mintGitToken` →
     `/api/v1/internal/credentials/mint` path (the `git_pat` mint returns the
     operator PAT; no broker change);
   - re-originate to `https://dev.azure.com/<org>/<project>/_git/<repo>/<rest>`
     built from the MATCHED key (never the raw path);
   - **the never-resident guarantee, unit-testable:** `outReq.Header.Del(
     "Authorization")` then `outReq.SetBasicAuth("pat", token)` (the
     `scope.go:82-93` convention) — the sandbox never receives the PAT, and the
     git response carries no Authorization.
   - `validAdoRest` restricts method+subpath to the git v2 smart-HTTP verbs, like
     `validGitRest`.

3. **Tests** — `ado_broker_test.go`, mirroring the GitHub broker tests: 403 on an
   ungranted repo, path-traversal rejection, sandbox-supplied `Authorization`
   stripped before injection, PAT never in the response to the sandbox, correct
   upstream URL from the matched key. These verify the **security properties**
   with a fake upstream — no real ADO needed.

4. **Dispatch population** — `internal/api/runs_create.go`: where an ADO
   `git_pat` grant is seen (near `:230-233`), ALSO populate `AdoGrants[<org>/<
   project>/<repo>]=grantID` (parallel to the GitHub `gitGrants` population at
   `:227`), and thread it through `runs_dispatch.go:287` like `GitGrants`.

5. **agent-run** — `deploy/images/common/agent-run-lib.sh`: add an `insteadOf`
   for `https://dev.azure.com/<org>/<project>/_git/<repo>` → the
   `wardyn-proxy/wardyn/ado/...` route (mirroring the github insteadOf at
   `:179-183`), gated on an ADO-grant env. **This is the behavior switch** that
   moves ADO from the resident helper to the proxy broker — it must land together
   with a real-ADO end-to-end test (clone + push against a live PAT/repo), which
   is why it is not flipped in this design-only change.

## Acceptance test (for the implementing PR)

With a real ADO PAT + repo: a governed run clones and pushes to the granted ADO
repo through `/wardyn/ado/`; assert the PAT never appears in the sandbox
environment or the git process's view (only proxy-side), a non-granted ADO repo
is 403, and audit shows a `brokered:git` allow row for `dev.azure.com`.
