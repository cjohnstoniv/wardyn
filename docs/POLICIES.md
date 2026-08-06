# Policy reference (`RunPolicySpec`)

Every governed run resolves to one `RunPolicySpec` — the whole configuration
surface. This is the field list; [`examples/policies/`](../examples/policies/) is
the worked set, and `wardyn policy render -f <file>` converts YAML→JSON and
rejects a misspelled field before you launch.

The spec is the same object everywhere: `--policy-file`, `policy create/update -f`,
an inline policy on a create-run request, and `WARDYN_DEFAULT_POLICY`. Unknown
fields are refused (`DisallowUnknownFields`), so JSON carries no comments — use
YAML if you want them.

Source of truth: `types.RunPolicySpec` (`internal/types/types.go`); the legal
values below are what `validatePolicySpec` (`internal/api/policy.go`) enforces at
write time. A guard test fails if a field here drifts from the struct.

## Top level

| Field | Type | Default | What it does |
|---|---|---|---|
| `allowed_domains` | `[]string` | `[]` (deny all) | L2 egress allowlist: exact hosts or `*.` wildcards. Empty under default-deny means the sandbox reaches nothing. Each entry must be a shape the proxy's matcher can match — a mid-label wildcard, a URL, or a bad `:port` is rejected at write time rather than shipped as a rule that silently matches nothing. |
| `denied_domains` | `[]string` | `[]` | Always wins over `allowed_domains`, in both egress modes. Same entry-shape validation. **Dispatch appends its own** (four GitHub HTTPS hosts plus the forge's SSH endpoint) for any run carrying a `github_token` grant with repos — see the note below the table. |
| `allow_all_egress` | `bool` | `false` | Switches egress from allowlist-only to deny-list-only: any non-denied **public** host is allowed. The SSRF/private-IP guard is unaffected (metadata, loopback, link-local and private ranges stay denied unconditionally), and credential injection still requires an exact `allowed_domains` entry — allow-all never widens where a secret may go. `first_use_approval` is inert under it. It does **not** re-open the GitHub hosts a brokered run loses — the four HTTPS names plus that forge's SSH endpoint — a deny beats allow-all too. |
| `first_use_approval` | `string` | `always_deny` | How an unknown domain is handled. See the three modes below. A legacy boolean still decodes (`true`→`deny_with_review`, `false`→`always_deny`). |
| `allowed_methods` | `[]string` | `[]` (all) | Optional HTTP method restriction. |
| `min_confinement_class` | `string` | — (**required**) | `CC1` (hardened runc), `CC2` (gVisor), or `CC3` (Kata microVM). The run refuses to launch below it; an unrecognised value is rejected at write time and would otherwise rank below CC1. |
| `eligible_grants` | `[]GrantSpec` | `[]` | The ceiling of credential scopes this run may request. Eligibility is not issuance — the broker still mints. |
| `auto_stop_after_sec` | `int` | `0` | Idle auto-stop. `> 0` = stop after that many seconds of wall-clock idleness plus a fixed 30s activity-debounce slack (egress-driven clock resets are coalesced to one per 30s, so the slack guarantees an active run is never read as idle; the `run.autostop` audit event's `threshold_sec` records the effective value, configured + 30); `0` = never reaped; `< 0` = never reaped, stated explicitly (what an interactive run should set, so the reaper does not stop it the moment it looks idle). Idleness is `updated_at` age — an attach or an egress call resets it, local CPU/disk work does not (`internal/lifecycle`). |
| `workspace_mounts` | `[]WorkspaceMount` | `[]` | Operator-authored host bind mounts. Never agent-chosen. |
| `workspace_repos` | `[]WorkspaceRepo` | `[]` | Additional git repos cloned into the run — the clone counterpart of `workspace_mounts`. |
| `llm_inspection` | `LLMInspectionSpec` | omitted = **off** | Outbound content inspection on brokered LLM routes. |
| `resources` | `ResourceLimits` | omitted = platform defaults | Sandbox CPU/memory/PID/disk caps. |

### Brokered GitHub: the denies you did not write

A run whose `github_token` grant covers at least one repo (from the grant's
`scope.repos`, or from the run's `--repo` / `workspace_repos` clone set) is
**brokered**: git traffic goes through the proxy's `/wardyn/gh/<org>/<repo>`
route, where the installation token is minted server-side and never enters the
sandbox. To make that the only route to those host **names**, dispatch
subtracts **and denies** `github.com`, `api.github.com`, `codeload.github.com`,
`*.githubusercontent.com` — and the forge's `ssh.<forge>` SSH-over-443 endpoint
(`ssh.github.com` in v1; the broker is github.com-only) — for that run
(`confineGitBrokerEgress`, `internal/api/runs_dispatch_gitbroker.go`). All of these are
added on **every** brokered run, whether or not it holds an `ssh_key` grant at
all — see "The `ssh_key` and `git_pat` lanes are closed too" below for what
that buys.

**Nothing in the policy re-opens those names.** The confinement runs last,
after every widening phase, and a deny beats `allowed_domains`, beats
`allow_all_egress`, beats a promoted `ApprovedEgress` entry, and beats a runtime
`first_use_approval` — the proxy returns on the deny verdict before it ever
considers an approval, on every port, not just 443 (the SSH deny in particular
is spelled as the bare host: that covers port 22 under `allow_all_egress`,
where a `:443`-only spelling would not, and it beats a surviving `*.github.com`
wildcard allow the same way any deny beats any allow — a policy that never
subtracted that wildcard, because it isn't one of the exact broker-managed
names, still can't use it to reach `ssh.github.com`). So a brokered run that
dials any of those names cannot `curl` the GitHub API, `gh pr create`, fetch a
release tarball or a raw `githubusercontent.com` file, or clone/push over SSH
to the same forge. The narrowed envelope is disclosed in the
`run.policy.effective` audit event.

**It is a name deny, and that is the whole of its reach.** The proxy keys the
verdict on the host string the sandbox asked for (`evalHost`), so a `CONNECT` to
a raw GitHub IP is a different key and none of these denies see it. Under
the default posture that changes nothing — an unlisted host is `always_deny` —
but under `allow_all_egress` a literal public IP is allowed (only private,
loopback, link-local and metadata ranges are denied unconditionally), and under
`deny_with_review` / `wait_for_review` it becomes an approvable unknown. If you
run a brokered policy with `allow_all_egress`, the broker route is the only
*convenient* route, not the only one — it is what git itself uses, since a
clone/push URL carries a name, never a bare IP.

### The `ssh_key` and `git_pat` lanes are closed too

An earlier pass over this doc described a real gap here: the GitHub denies
were exact HTTPS hosts, so a run that also carried an `ssh_key` grant for the
same forge kept `ssh.<forge>:443` allowlisted — a second, unparseable push path
beside the brokered one. That gap is now closed, at write time and at dispatch:

- **Write time.** `validateGrantLaneExclusivity` (`internal/api/policy.go`, run
  from `validatePolicySpec` on every policy write — stored `POST`/`PUT
  /policies`, an inline run policy, `WARDYN_DEFAULT_POLICY`, and the
  composer/profile clamps) refuses a policy that declares both a `github_token`
  grant and an `ssh_key` **or** `git_pat` grant for the same forge, with a `400`
  naming the choice: brokered and branch-confined, or operator-supplied and
  unbound — not both for one forge. It fires on the DECLARATION, not on whether
  a run ends up brokered — a `github_token` grant with `"repos": []` still
  counts, since policy-write cannot know what a later run will `--repo` into.
- **Dispatch.** For a policy stored before this rule existed,
  `confineGitBrokerEgress` denies the forge's `ssh.<forge>` endpoint alongside
  the HTTPS hosts above, and `dropBrokeredGrants` withholds that forge's
  `ssh_key` **and** `git_pat` grants from the sandbox env entirely — the
  credential is never minted, not merely unable to reach its forge — logging an
  `slog` warning and a `run.ssh.brokered_forge` / `run.git_pat.brokered_forge`
  audit event so neither withholding is ever silent.
- **Mint.** `POST /api/v1/internal/credentials/mint` refuses either kind for a
  brokered forge before it opens the broker transaction
  (`brokeredForgeMintKind`, `internal/api/internal.go`). This is the seam that
  matters most for `git_pat`: the proxy's own mint refusal
  (`isBrokeredGitGrant`) matches `github_token` grant ids only, so a caller that
  POSTs the mint route directly — rather than going through
  `wardyn-git-helper`, which every GitHub host on a brokered run already refuses
  — used to be answered with the PAT.

| Lane | Route to the forge | What binds a push |
|---|---|---|
| `github_token` with repos (brokered) | `/wardyn/gh/<org>/<repo>` only — every name above is denied | Per-repo allowlist **plus** the receive-pack pkt-line parser (`internal/egress/proxy/git_broker.go`), which reads the refs being pushed and refuses any outside `refs/heads/wardyn/<run-id>/`. Default-on; `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` opts out, and a push forwarded unparsed carries the distinct decision-log `rule_source` `brokered:git:branch-ns-off` so the posture is provable per push |
| An `ssh_key` grant for the **same** forge | None | Refused at write (`400`) going forward; for anything already stored, the grant is withheld from the sandbox at dispatch and refused at mint — there is no credential left to push with |
| A `git_pat` grant for the **same** forge (`github.com` or a `*.github.com` host) | None | Same three seams. A GitHub `git_pat` is typically a *user* PAT — broader than the repo-scoped installation token beside it, and bound by no branch namespace |

An `ssh_key` or `git_pat` grant for a **different** host (`dev.azure.com`,
`gitlab.com` or a GHES host, say, alongside a `github.com` `github_token`) is
untouched by any of it — every mechanism keys on the same forge, and that
non-GitHub lane is what `git_pat` is for. There the old shape still applies for
`ssh_key`: the key is resident for the
clone only — `agent-run` mints it, writes it `0400`, clones, then shreds it and
unsets `GIT_SSH_COMMAND` before exec'ing the agent — a real narrowing and not a
confinement, since the grant id rides the sandbox env (`WARDYN_SSH_GRANTS`), an
auto-mintable grant is re-mintable by design (`MintForGrant` — only
`requires_approval: true` makes it single-use), and the proxy's mint route
refuses only *brokered GitHub* grant ids (`isBrokeredGitGrant`). That forge's
key is bounded by the operator who supplied it, not by Wardyn
(`threatmodel/THREAT-MODEL.md` §5.1a) — the honest scope for a non-brokered
credential, unchanged by any of the above.

**If the run needs direct GitHub fetches, drop the `github_token` grant** (and
allowlist the hosts you need). A run with no git grants keeps whatever GitHub
egress its policy grants — the confinement is no-op without one.

### Bound the token itself: a GitHub ruleset

Everything above bounds the **route**. None of it bounds the **token**. A GitHub
App installation token cannot self-restrict to a ref prefix — the installation-token
request carries repository names, repository ids and a permission map, and no ref
or branch field, because the API accepts none — so `refs/heads/wardyn/<run-id>/`
is enforced by Wardyn's receive-pack parser on the proxy, and a token that
escaped the proxy would be unbounded on GitHub's side.

A **repository ruleset** changes that. Target every branch, exclude the run
namespace, restrict creation, update and deletion: the App can then write only
inside `refs/heads/wardyn/**/*`, and that holds for a leaked token, a
misconfigured proxy, or any path Wardyn is not on.

**Mind the `/*`.** The exclude pattern is `refs/heads/wardyn/**/*`, not
`refs/heads/wardyn/**`. GitHub matches these with fnmatch semantics where `*`
does not cross `/`, and a *trailing* `**` behaves the same as `*` — only `**/`
is recursive. A run pushes `wardyn/<run-id>/work`, two segments below `wardyn/`,
so `refs/heads/wardyn/**` excludes nothing a run actually pushes and the ruleset
refuses every governed push. Measured against GitHub's live evaluator, read-only,
on public repos that already carry such rulesets: with `refs/heads/**` as the
*include* (`nodejs/node` #714753), `zzz` gets the rule and `zzz/foo` does not;
with `refs/heads/dependabot/**/*` as the include (`angular/angular` #9964176),
`dependabot/foo` and `dependabot/a/b/c/d/e` both get it and bare `dependabot`
does not; with `refs/heads/copilot/**/*` as the *exclude*
(`open-telemetry/opentelemetry-collector` #10457348), `copilot/foo` and
`copilot/a/b/c` are excluded and bare `copilot` is not. GitHub's docs agree: "the
`*` wildcard does not match directory separators (`/`)" and "you can include any
number of slashes after `qa` with `qa/**/*`".

Rulesets are what Wardyn can *read back* and therefore the only form it verifies.
Classic branch protection ("restrict who can push") also bounds a token, but it
is a different API and does **not** appear in the rules endpoint below — measured:
repos with heavily protected default branches return `[]` there. A repo protected
that way is genuinely protected and will still be graded "unconfined" by the check.

`target: "branch"` bounds **branches only**. The target enum is
`branch|tag|push`, so this ruleset leaves `refs/tags/*` open: a leaked
`contents:write` token can still create, move or delete any tag. Wardyn's own
receive-pack parser refuses tags, but the ruleset exists precisely for the case
where that parser is bypassed. Add a second `target: "tag"` ruleset if tags
matter to you; Wardyn does not verify one.

Create it (needs **admin on the repo** — Wardyn never has that, which is why this
is an operator step and not something the control plane can do for you):

```sh
OWNER=acme REPO=widgets

# bypass actor: a User id (gh api /users/<login> --jq .id) or a Team id
# (gh api /orgs/$OWNER/teams/<slug> --jq .id). See the note below on roles.
HUMAN_ID=$(gh api /users/your-login --jq .id)

jq -n --argjson human "$HUMAN_ID" '{
  name: "wardyn-agent-ref-confinement",
  target: "branch",
  enforcement: "active",
  conditions: { ref_name: { include: ["~ALL"], exclude: ["refs/heads/wardyn/**/*"] } },
  rules: [
    { type: "creation" },
    { type: "update", parameters: { update_allows_fetch_and_merge: false } },
    { type: "deletion" },
    { type: "non_fast_forward" }
  ],
  bypass_actors: [
    { actor_id: $human, actor_type: "User", bypass_mode: "always" }
  ]
}' | gh api --method POST "/repos/$OWNER/$REPO/rulesets" --input -
```

Field names, enum values and which fields are required are as published in
GitHub's OpenAPI description (`github/rest-api-description`,
`descriptions/api.github.com/api.github.com.json`): `name` and `enforcement` are
the only required properties; `target` ∈ `branch|tag|push`; `enforcement` ∈
`disabled|active|evaluate`; `ref_name.include` accepts `~ALL` (all branches) and
`~DEFAULT_BRANCH`; `bypass_actors[].actor_type` ∈
`Integration|OrganizationAdmin|RepositoryRole|Team|DeployKey|User` with
`bypass_mode` ∈ `always|pull_request|exempt`, and `actor_id` is required for
`Integration`, `RepositoryRole`, `Team` and `User`.

**On `RepositoryRole`.** Bypassing by role ("everyone with write") is a supported
`actor_type`, but it needs a numeric `actor_id` and **GitHub's published schema
does not document what the numbers are** — so this recipe uses a `User`/`Team` id
you can resolve with a command. If you want the role form, create one ruleset in
the web UI with the bypass you want and read the number back:
`gh api /repos/$OWNER/$REPO/rulesets/<id> --jq .bypass_actors`.

**What it costs.** Only listed bypass actors can create, update, delete or
force-push any branch outside `refs/heads/wardyn/**/*` — including the default
branch, including you, including CI. Grant bypass to the humans and automation
that need it *before* you set `enforcement: "active"`, or use `"evaluate"` first
(GitHub Enterprise only) to see what would break. The Wardyn App is deliberately
**not** a bypass actor; that is the entire point.

**Verify it took on your own repo** — the same reads Wardyn makes, and the same
reason for each. Outside the namespace `creation`, `update` and `deletion` must
all appear (each is a write: without `deletion` a leaked token still runs
`git push --delete origin main`); inside it, `creation` and `update` must not (or
the ruleset would refuse the run's own pushes). Then each ruleset behind those
rules must report that you cannot bypass it. The branch does not have to exist —
GitHub answers for the name — so these create nothing:

```sh
gh api "/repos/$OWNER/$REPO/rules/branches/probe-wardyn-ref-confinement" --jq '[.[].type]'
# → a list containing at least "creation", "update" and "deletion"

gh api "/repos/$OWNER/$REPO/rules/branches/wardyn/00000000-0000-0000-0000-000000000000/probe" --jq '[.[].type]'
# → []   ← this is the line that catches a wrong exclude pattern

# for each distinct ruleset_id in the first response:
gh api "/repos/$OWNER/$REPO/rules/branches/probe-wardyn-ref-confinement" --jq '[.[].ruleset_id]|unique|.[]' |
  while read -r id; do gh api "/repos/$OWNER/$REPO/rulesets/$id" --jq '.name + " " + (.current_user_can_bypass // "ABSENT")'; done
# → "never" for each. Anything else means that ruleset does not bind you, and
#   ABSENT means it could not be determined — Wardyn treats both as not-confined.
```

The second read is the one that catches `refs/heads/wardyn/**`: with the wrong
pattern it returns the same rule list as the first, because nothing two segments
deep was ever excluded.

GitHub's App-permissions reference lists **both** reads under **Metadata: read** —
`GET /repos/{owner}/{repo}/rules/branches/{branch}` and
`GET /repos/{owner}/{repo}/rulesets/{ruleset_id}` are both rows in "Repository
permissions for Metadata"; the "Administration" rows for that second path are the
`PUT`, `DELETE` and `/history` variants, not the plain `GET`. Metadata: read is
the one permission every GitHub App holds mandatorily, so reading a ruleset back
should cost no new permission grant. **Unverified against a live installation**:
that is the docs' claim, not a measurement, which is why every read failure
grades *unknown* instead of *unconfined*. The rules endpoint is readable with no
authentication at all on a public repo (measured); `current_user_can_bypass`,
however, is omitted from an unauthenticated read (also measured).

Wardyn grades exactly this on the setup checklist (`github_ref_ruleset`), for the
first repo the default policy or a stored policy names in a `github_token` grant
scope — one repo, not all of them; the row says which. The row is omitted
entirely when no GitHub App is configured or when no policy names a concrete repo
— shipped example policies carry `"repos": []` because `eligible_grants` are
templates the run fills in — so a fresh install makes no outbound call at all.

What the check does and does not settle:

- **Bypass is checked**, not assumed away. For every ruleset backing the
  creation/update/deletion rules, Wardyn reads `current_user_can_bypass` and
  requires `"never"`; `always`, `pull_requests_only` or `exempt` grade
  **unconfined**, because a bypassing App writes anywhere despite a perfect rule
  list. Measured with a plain non-admin user token on 11 rulesets across 7
  public repos: the field comes back (`"never"`) even though `bypass_actors` is
  withheld, which GitHub returns only to a caller with write access to the
  ruleset (and documents that). **Two things
  remain unknown** and are not asserted: whether GitHub computes that field
  meaningfully for a GitHub App *installation* token, and what App permission the
  ruleset read needs. An *unauthenticated* read omits the field entirely
  (measured) — so an absent field is treated as unknown, never as a pass.
- **Branches only.** The verified ruleset targets branches, so `refs/tags/*` is
  not bounded by it and the check does not claim otherwise.
- **Any error grades unknown** — timeout, rate limit, permission refused, a
  bypass mode that could not be read — never "unconfined".
- **`api.github.com` only**, so this does not cover GitHub Enterprise Server
  (neither does minting — the broker has no GHES base-URL setting).

To make it mandatory rather than advisory, set
`WARDYN_GITHUB_REQUIRE_REF_RULESET=true` on `wardynd`: the broker runs the same
verification before every `github_token` mint, for **every** repo in the grant,
and refuses the mint if any of them is unconfined *or* cannot be verified. It is
**off by default** — defaulting it on would break every existing deployment on
its next mint, since the ruleset has to be created per repo by hand. Turn it on
only once the reads above return what they should on every repo you broker:
`refs/heads/wardyn/**` instead of `refs/heads/wardyn/**/*` will fail the gate and
block every governed push at GitHub as well.

### `first_use_approval` modes

| Value | Behavior |
|---|---|
| `always_deny` | Hard-deny the unknown domain and log it. No approval is ever raised. |
| `deny_with_review` | Raise a pending approval **and** deny the in-flight request. Once approved, a retry passes. The connection is never held. |
| `wait_for_review` | Raise a pending approval and **hold the connection** until it is decided or the proxy's hold deadline passes. Approved in time, the same in-flight request completes; on deadline it fails closed (403) with the approval still pending. |

Empty or unrecognised normalises to `always_deny` at runtime (fail closed), but
an unrecognised literal is rejected at write time.

`wait_for_review`'s one built-in user today is a workspace's confined record
replay (`docs/TRY-IT.md` "record a session, rerun it as a governed profile"):
`launchRecordRun` sets it for exactly that session, never for a learning
(open-egress) one. Approving a hold there does more than release the parked
connection — the decision funnels through the same chokepoint every approval
does (`decide`, `internal/api/approvals.go`), which — only for an
`egress_domain` approval raised during a `workspace record` run — writes a
required `egress:<host>` row into that workspace's own requirements contract
(`learnVerifyEgress`). The next confined replay of the SAME workspace folds
required `egress:` rows into its allowlist (`confinedEgressDomains`), so it
does not hold on that host again; a plain run's hold widens only its own run
and writes nothing durable.

Both write-backs land on the workspace's own overlay row, never a shared
source: a source can be attached to many workspaces (see
[OPERATIONS.md](OPERATIONS.md) → "Workspaces: three tiers"), and approving a
host for one aggregate must not leak the approval into every other
workspace attaching the same repo. The SAME destination is where **Promote
to approved egress** now writes too (`handlePromoteRecordEgress`,
`internal/api/record.go`) — an operator reviewing an OPEN recording's
observed-and-allowed hosts and promoting some or all of them, outside any
approval flow. The legacy `approved_egress` list is not read-only. The
hold-approval write-back and Promote no longer write here — both now land
on the requirements contract instead, one destination for what used to be
two — but the record pane's own per-host **Approve** button, on a blocked
or pending-approval host under "Off-policy attempts caught," still calls
`PUT /workspaces/{id}/approved-egress` (`handleSetApprovedEgress`,
`internal/api/workspaces.go`) directly and appends the new host — a live
write path this migration left untouched. Whatever `approved_egress`
already holds, from that button or from before this migration, is still
unioned into a confined replay's allowlist.

## `eligible_grants[]` — `GrantSpec`

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `string` | — (required) | `github_token`, `cloud_sts`, `api_key`, `git_pat`, or `ssh_key`. Anything else is rejected. |
| `scope` | object | — | Kind-specific; see the table below. |
| `ttl_seconds` | `int` | `3600` | TTL of the minted credential. 1h is both the default and the maximum. Negative is rejected. |
| `requires_approval` | `bool` | `false` | Force a human approval before the broker will mint, instead of auto-minting on policy. |

| `kind` | `scope` shape | Write-time rules |
|---|---|---|
| `github_token` | `{"repos":[…],"permissions":{…}}` | Validated with the broker's own mint-time predicate, so a malformed permission is a 400 at policy write, not a mint failure mid-run. **Once any repo is covered the run is brokered and unconditionally loses `github.com`, `api.github.com`, `codeload.github.com`, `*.githubusercontent.com`, and the forge's `ssh.<forge>` endpoint** — see "Brokered GitHub" above. Also refuses a co-declared `ssh_key` or `git_pat` grant for the same forge at write time (see "The `ssh_key` and `git_pat` lanes are closed too"). Drop the grant if the run needs direct GitHub fetches. |
| `cloud_sts` | `{}` | Must decode as a JSON object if present. Hard-requires the SPIRE identity provider, which does not ship — it mints nothing today. |
| `api_key` | `{"host":"…","header":"…"}` | Proxy-side injection only; the value never enters the sandbox. Referencing a reserved platform secret (`wardyn-signing-key`, `wardyn-session-key`) is refused. |
| `git_pat` | `{"host":"…","secret_name":"…","username":"…"}` | `host` + `secret_name` required; reserved secret names refused. The stored PAT **value** is handed to the git credential helper (ADO/GitLab have no injectable seam), so it is resident for the git operation. `username` defaults by convention (ADO `pat`, GitLab `oauth2`). A GitHub `host` (`github.com` or a `*.github.com` host) may not be combined with a `github_token` grant — refused at write, withheld at dispatch, refused at mint (see "The `ssh_key` and `git_pat` lanes are closed too"); every other host is unaffected. |
| `ssh_key` | `{"host":"…","key_secret_ref":"…","username":"…","known_hosts_secret_ref":"…"}` | `host` + `key_secret_ref` required; reserved secret names refused for either ref. `host` must be an SSH-over-443 provider Wardyn supports (`github.com`, `dev.azure.com`). A **documented exception** to the no-resident-secret rule: the key lands as a 0400 file for the clone and is wiped right after — except for the same forge as a co-declared `github_token` grant, which this kind may not be combined with (see "The `ssh_key` and `git_pat` lanes are closed too"). |

## `workspace_mounts[]` — `WorkspaceMount`

| Field | Type | Default | What it does |
|---|---|---|---|
| `source` | `string` | — (required) | Host path. Must be absolute and cleaned, and not under a denied host location — the same deny-list the docker driver enforces, checked here so a bad mount is a 400 at write time too. |
| `target` | `string` | — (required) | In-container path, under an allowed prefix (`/home/agent`, `/work`, `/workspace`). Must be unique across all `workspace_mounts` **and** `workspace_repos` targets, so a clone can never land on a bind target. |
| `read_only` | `bool` | `true` when omitted | Omitting it means read-only — the safe direction. Read-write requires an explicit `"read_only": false`. |

## `workspace_repos[]` — `WorkspaceRepo`

| Field | Type | Default | What it does |
|---|---|---|---|
| `repo` | `string` | — (required) | Repo slug or URL, validated like a run's `--repo`. |
| `target` | `string` | (unset) | Optional clone destination; validated and collision-checked against every other target when set. Unset defers to the `~/work/<name>` convention. |

## `llm_inspection` — `LLMInspectionSpec`

A guardrail and visibility layer, **not** exfiltration prevention (see
`threatmodel/THREAT-MODEL.md` §5.1). Omitting the block, or `mode: "off"`, is off.

| Field | Type | Default | What it does |
|---|---|---|---|
| `mode` | `string` | `off` | `off`, `alert` (scan + audit, forward unchanged), or `block` (a qualifying finding refuses the request). When not off, at least one detector — a `detect_*`, a sidecar URL, or `classified_markers` — must be enabled. |
| `workspace_secret_values` | `[]string` | `[]` | Operator-declared known secret **values** the run must not leak into a prompt. The v1 detection corpus. Never logged; values below the masking floor are ignored. |
| `detect_secrets` | `bool` | `false` | Exact match against `workspace_secret_values`. |
| `detect_secret_patterns` | `bool` | `false` | Regex catalog of well-known secret *formats* (AWS/GitHub/Slack/Google keys, PEM, JWTs, Stripe). Higher precision than entropy; false-positives on example keys in code. |
| `detect_entropy` | `bool` | `false` | Shannon-entropy detector. High false-positive rate in code; emits medium severity so a strict `block_min_severity` can exclude it. |
| `detect_pii` | `bool` | `false` | Regex/Luhn PII detector. Best-effort visibility signal, never a control. |
| `detector_sidecar_url` | `string` | (unset) | Out-of-process detector the proxy POSTs each span to (e.g. Presidio, LLM-Guard). Must be an `http(s)://` URL. A sidecar error respects `on_scanner_error` like any other scanner error. |
| `classified_markers` | `[]string` | `[]` | Literal markers (`INTERNAL ONLY`, `CONFIDENTIAL//NOFORN`) whose presence flags a classified-content leak. Case-insensitive substring match. |
| `scan_attachments` | `bool` | `false` | Decode and scan base64 image/document attachment bytes. Off by default: binary, large, high-FP. |
| `inspect_forward_egress` | `bool` | `false` | Extend inspection from the LLM routes to the generic plaintext-HTTP forward path. HTTPS connectors tunnel via opaque CONNECT and stay uninspected unless MITM'd. |
| `max_scan_bytes` | `int` | `1048576` (1 MiB) | Cap on a single scanned span. A larger span is skipped fail-open and recorded. Negative is rejected. |
| `on_scanner_error` | `string` | `pass` | `pass` (fail-open, the request still flows) or `block` (fail-closed in block mode). |
| `require_inspectable_llm` | `bool` | `false` | Refuse to schedule a run whose resolved LLM transport is opaque (subscription-OAuth / Bedrock CONNECT) and therefore uninspectable. Requires `intercept_tls` — only TLS-MITM gives a runtime guarantee. Default only warns. |
| `intercept_tls` | `bool` | `false` | Opt the run into TLS-MITM of opaque CONNECT tunnels to known LLM hosts, making the subscription-OAuth path inspectable. The control plane provisions a per-run CA: the private key goes only to the proxy sidecar, the sandbox trusts only the public cert. Adds a CA trust dependency inside the sandbox (threatmodel §5.1a). |
| `block_min_severity` | `string` | `low` | Minimum finding severity that blocks in `block` mode: `low`, `medium`, `high`, `critical`. |

## `resources` — `ResourceLimits`

A zero or omitted field means "platform default". Dispatch fills conservative
defaults so **every** run is capped even under a policy that sets nothing.

| Field | Type | Default | What it does |
|---|---|---|---|
| `cpu_millis` | `int` | `2000` (2 vCPU) | Milli-CPU cap. |
| `memory_mib` | `int` | `4096` | Hard memory cap, MiB. |
| `pids_limit` | `int` | `512` | Max processes/threads — the fork-bomb guard. |
| `disk_mib` | `int` | (storage-driver default) | Writable-storage cap, MiB. Best-effort: it needs a storage driver that supports a per-container quota, and warns/fails closed when a cap is demanded but unsupported. |
