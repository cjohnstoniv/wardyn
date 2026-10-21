# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (one non-stdlib dependency,
`github.com/google/uuid`). Every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

## Before you start

> **Redirects.** The default client (`client.New`, or a `Client` with no `HTTPClient`) never
> follows a redirect.
> A 3xx comes back as an `*APIError`, so a write that an ingress or a
> mistyped base URL redirects fails instead of being replayed (body and bearer included) at
> the `Location`.
> If you set `Client.HTTPClient`, its redirect policy is yours; set
> `CheckRedirect` to a func that returns `http.ErrUseLastResponse` unless you mean to follow them.

> **Coverage and pagination.**
> - `pkg/client` is a curated SDK over the route families
>   external tooling automates, not a 1:1 mirror of wardynd.
> - The exact list of what it
>   wraps and what it does not — and the `ListOpts` / `X-Wardyn-Truncated` pagination
>   contract — is the package doc on `pkg/client` itself, where your IDE shows it at the
>   call site.
> - `TestClientCoversRouteFamilies` pins what it wraps against the real methods, and
>   `TestSDKCensusNamesEveryRouteFamily` (internal/api) pins that the not-covered half names every route family the router mounts.

## Quickstart

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/cjohnstoniv/wardyn/pkg/client"
    "github.com/google/uuid"
)

func main() {
    ctx := context.Background()

    // 1. Create a client. Token is the AdminToken configured in wardynd.
    c := client.New("https://wardyn.example.com", "my-admin-token")

    // 2. Submit a run. Answers as soon as the run row exists, so the result's
    //    client.AgentRun (embedded) reads state PENDING — the image build and
    //    dispatch continue server-side; poll or GetRun for RUNNING/FAILED.
    //    Also carries any ADVISORY warnings — the run is live either way, so
    //    surface them.
    created, err := c.CreateRun(ctx, client.CreateRunRequest{
        Agent: "claude-code",
        Repo:  "org/repo",
        Task:  "fix issue #42",
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("run id:", created.ID, "state:", created.State)
    for _, w := range created.Warnings {
        fmt.Println("warning:", w)
    }

    // 3. Poll or fetch later.
    run, _ := c.GetRun(ctx, created.ID)

    // 4. Approve a pending credential or egress request. The run filter is
    //    uuid.Nil here ("every run"); pass a run id instead — e.g.
    //    c.ListApprovals(ctx, client.ApprovalPending, created.ID) — to get
    //    only that run's approvals (the server's ?run_id= predicate).
    //    Omitting DecisionOpts keeps today's default scope: the grant holds
    //    for the rest of the run.
    pending, _ := c.ListApprovals(ctx, client.ApprovalPending, uuid.Nil)
    for _, ap := range pending {
        approved, err := c.Approve(ctx, ap.ID, "reviewed and safe")
        if err != nil {
            log.Println("approve error:", err)
            continue
        }
        fmt.Println("approved:", approved.ID)
    }

    // 4b. An egress_domain approval can instead be scoped narrower or wider:
    //     ScopeOnce releases a single connection, ScopeUntil holds until a
    //     deadline, and ScopeAlways (operator-only) persists the host onto
    //     the run's workspace so future runs never raise it. Deny takes the
    //     same DecisionOpts.
    if len(pending) > 0 {
        until := time.Now().Add(2 * time.Hour)
        _, _ = c.Approve(ctx, pending[0].ID, "temporary access", client.DecisionOpts{
            Scope: client.ScopeUntil,
            Until: &until,
        })
    }

    // 5. Fetch the append-only audit trail for the run.
    events, _ := c.AuditEvents(ctx, run.ID)
    for _, ev := range events {
        fmt.Printf("%s  %s  %s\n", ev.Time.Format("15:04:05"), ev.Action, ev.Outcome)
    }

    // 6. Kill a run (sandbox teardown + identity + credential revocation).
    resp, _ := c.KillRun(ctx, run.ID)
    fmt.Println("killed, state:", resp.State)
}
```

## Naming server types

The values returned and accepted by `Client` methods are exposed directly on
`pkg/client`, so an SDK consumer never needs `internal/types` (which Go forbids
importing from another module anyway). The aliases are identical types — a
`client.AgentRun` *is* the value wardynd returns.

```go
var (
    r   client.AgentRun        // GetRun / ListRuns (embedded in CreateRunResult)
    g   client.CredentialGrant // ListGrants
    a   client.ApprovalRequest // ListApprovals / Approve / Deny
    p   client.RunPolicy       // ListPolicies / GetPolicy / Create / Update
    ev  client.AuditEvent      // AuditEvents
    sk  client.SSHPublicKey    // ListSSHKeys / AddSSHKey
    rf  client.RunFiles        // RunFiles
    re  client.RunEvent        // RunEvents
    rp  client.RunPolicyView   // GetRunPolicy
)

// Enums and their values are re-exported too:
_ = client.ApprovalPending // also Approved / Denied / Expired / Cancelled (ApprovalState)
_ = client.RunRunning      // also Pending / Completed / Failed / Killed ... (RunState)
_ = client.ScopeOnce       // also Run / Until / Always (ApprovalScope; DecisionOpts.Scope)

// Build a policy spec without touching internal/types:
spec := client.RunPolicySpec{
    // GitHub is reached through the git-broker (a github_token grant below), not
    // via AllowedDomains — list only non-GitHub hosts the task needs.
    AllowedDomains:      []string{"proxy.golang.org"},
    MinConfinementClass: client.CC2,
    EligibleGrants: []client.GrantSpec{
        {Kind: client.GrantGitHubToken, RequiresApproval: true},
    },
}
created, _ := c.CreatePolicy(ctx, client.PolicyRequest{Name: "default", Spec: spec})
_ = created
```

## Reading the policy a run got

`GetRunPolicy` returns `GET /api/v1/runs/{id}/policy`: the resolved policy, where
it started, and what launch changed. It is readable by whoever may read the run
(anyone else gets the same `404` as `GET /runs/{id}`); a portal's delegated token
is refused `403` `delegation_scope`. The policy itself is under `spec`, not at the top level of the response.

```go
v, err := c.GetRunPolicy(ctx, created.ID)
if err != nil { /* ... */ }
if v.State == client.RunPolicyViewRecorded {
    for _, ch := range v.Changes {
        fmt.Println(ch.Cause, ch.Field, ch.Added, ch.Removed)
    }
    _ = v.Spec // a client.RunPolicySpec; read "Reusing the spec" below before sending it as PolicyRequest.Spec
}
```

| Field | Meaning |
|---|---|
| `state` | `recorded`, `not_yet` (the run has not reached sandbox setup) or `never` (it ended before that); `spec` and `recorded_at` are set for `recorded` only |
| `source` | `kind` is `stored`, `inline`, `default`, `profile` or `unknown`; a saved policy carries `policy_id` and its `name` at launch, and `deleted` once it is gone; `preset` and `preset_version` when the run came from one |
| `spec` | the policy the sandbox's proxy enforces, restart denies included; `llm_inspection` secret values are never present |
| `redacted` | true when the reader is below the security admin tier and mount sources or secret names were hidden (`<redacted>`, or dropped); the spec then strict-decodes as a policy but does not validate |
| `changes` | never `null`; each item says why, where and what changed, and never shows a hidden value; the item fields are listed under the table |
| `complete` | false for a run from before Wardyn recorded its starting policy: `changes` then lists only what the launch audit rows state |
| `stored_policy_now` | for a saved-policy source: `same`, `changed`, `updated` (a run from before the record: edited since, possibly a rename only) or `deleted`, with the policy's current `name` |
| `provenance` | per top-level `spec` key, the layer that set it (`client.PolicyProvenance`, [denials.md](operations/denials.md)) |

The CLI is `wardyn run policy <run-id> [--json]`.

#### Reusing the spec

The spec always strict-decodes as a policy. Whether it passes policy validation depends on the reader and the run.

- At the security admin tier it passes, and `wardyn run --policy-file` runs it again as is.
- Below that tier it holds `<redacted>` values and fewer secret names, so it is a starting point.
- A run that used an organisation component's shared secret is a record for every reader. The grant carries `shared: true`, which every authored door refuses, so validation fails.
- For such a run, start the new run with the component named in `CreateRunRequest.Components` ([Custom components](#custom-components)).
- The CLI says so in a `#` comment line above the YAML, and in its help.

#### `changes`

Never `null`. Each item is a `cause` (`workspace`, `source_control`, `component`, `mirror`, `model_access`, `git_broker`, `overlay`, `profile`, `org_disk`, `pool_disk`, `restart`, `limits`, `launch`), a `field` (a policy JSON name), the `added` and `removed` entries, `profile` for `profile`, `at` for `restart`, and the narrowing sentences in `detail` for `limits`.
Grants read `kind:host` or `kind:repo,repo`, mounts by target, repos `repo@ref`: never a hidden value.

## Following a run's lifecycle

`GET /api/v1/runs/{id}/events` is a `text/event-stream` of the run's lifecycle,
readable by whoever may read the run, including a portal's delegated token for
the run's person (anyone else gets the same `404` as `GET /runs/{id}`). One
principal holds at most 32 streams at once; the next is refused `422`
`event_stream_cap` until one closes. `RunEvents` follows it and returns once the
run has ended:

```go
err := c.RunEvents(ctx, created.ID, 0, func(ev client.RunEvent) error {
    fmt.Println(ev.ID, ev.Type, ev.Reason, ev.State)
    return nil
})
```

The vocabulary is closed; each event carries a short machine field and never
log or secret content:

| `type` | When | Field |
|---|---|---|
| `provisioning` | the sandbox is being created (PENDING -> STARTING) | |
| `pulling` | the substrate reported an image pull (at most once) | |
| `ready` | the sandbox is up (STARTING -> RUNNING) | |
| `idle_stopped` | the idle reaper stopped the run (a stop at `WARDYN_RUN_MAX_AGE` ends the stream with `ended` alone) | |
| `failed` | the run failed | `reason`: `not_started`, `start_failed` or `run_failed` (the phase it failed in) |
| `ended` | always last; the stream then closes | `state`: the terminal run state |

- Each event's `id` is monotonic per run on one replica; reconnect with `Last-Event-ID` (the SDK
  does) to resume without gaps on that replica. Ids count within one replica's ring, so with more than one
  wardynd replica a reconnect that lands on another may miss or repeat events.
- The server sends a `: keepalive` comment every
  15s, ends the stream at the next keepalive once the caller's session is revoked,
  and closes a held stream after 5 minutes so the reconnect re-authenticates.
- The feed is kept in wardynd's memory: resume works within the daemon's
  lifetime.
- After a restart a live run's stream carries only what happens next,
  and a run that had already ended answers `ended` alone.
- The repository clone
  happens inside the sandbox after `ready`, so it is not a separate event.

## Reading a run's output

`GET /api/v1/runs/{id}/output?tail=<bytes>` returns the end of a
non-interactive run's combined stdout/stderr — at most `WARDYN_RUN_OUTPUT_TAIL_BYTES` (64 KiB by default) — from wardynd's memory while the run lives and from Postgres once it has ended. `RunOutput` reads it; pass
`0` for the whole tail:

```go
out, err := c.RunOutput(ctx, created.ID, 0)
fmt.Println(out.Output, out.Truncated, out.Complete, out.Source)
```

- `truncated` says the output does not start at the run's first byte.
- `complete`
  says the capture attempt is final.
  A later recording upload may improve a
  finalized recording result.
- `source` is `stdout` for direct command output,
  `pane_snapshot` for an interactive run's last screen, or `recording` for recovered
  asciicast output.
  The runner that served the run decides between `stdout` and `recording`:
  only a recorded run on a Kubernetes runner under the session recorder reads `recording`
  (OPERATIONS.md "Run output").
  The latter two retain the recording's owner-or-operator privacy
  gate, even after recording is disabled.
  A portal's delegated token reads `stdout` output only; for the other two
  it gets the 409 a run with no such row gives, audited `delegation_scope`.
- `incomplete` on `stdout` says bytes may be missing.
  On `recording` it is always true and says the output cannot be vouched byte-complete.
- `capture_gap` says capture or recovery
  was lost: the failure signal.
  Previously persisted masked stdout chunks may remain.
- `mask_scope` is
  `run` when the capture was masked against the run's complete manifest and
  `globals_only` when it was not (empty when the deployment keeps none).
- `captured_at` is when the final row was written, nil while the run is live.
- The
  same `404` as `GET /runs/{id}` answers anyone who may not read the run.
  The
  other refusals carry a `run_output_*` reason (below), `run_output_erased`
  among them (the run's output was erased, `404`).
- An authorized recording reader
  gets `410 recording_erased` when its derived output scope was erased and no
  independent output remains.

From a shell, `wardyn run output <run-id>` prints the same bytes (below).

## Finding a waiting sign-in

`GET /api/v1/runs/{id}/sign-in` finds an AWS sign-in that is waiting for its
device-code approval after the browser tab that started it was lost.

It reads
the sign-in sandbox's terminal once and answers:

- `{"state":"waiting",
  "verification_url":"https://…","user_code":"ABCD-EFGH"}` for the latest
  attempt while an `aws sso login` process is still running in the sandbox,
- or
  `{"state":"not_waiting"}` when no such process is left, that attempt has
  finished or failed, the run captured its sign-in, or the run is not running.

Only the run's owner may
read it: a super admin on a person's run gets `403 run_owner_only`, because
approving the page binds the approver's cloud identity to the owner's session.
`RunSignIn` reads it:

```go
s, err := c.RunSignIn(ctx, runID)
if err == nil && s.State == client.SignInWaiting {
	fmt.Println(s.VerificationURL, s.UserCode)
}
```

From a shell, `wardyn run sign-in <run-id>` prints the same answer.

## Run end

`POST /api/v1/runs` sets when the run ends with `ends_after_sec` or `no_end` on `CreateRunRequest`; `wardyn run` spells them `--ends-in` and `--no-end`.

| Request | Result |
|---|---|
| Neither | The effective default end. An untouched run whose request says `experience: interactive`, with no default from the pool, the profile or the deployment, ends after `24h`. A Background run, or a request that carries no `experience`, has no end in that case |
| `ends_after_sec` (1 to 2147483647) | That end. Past the lowest limit it is cut to it and the `201` carries a warning naming the limit |
| `no_end` | No end, only where the pool and the deployment leave the lifetime unlimited and the profile has `allow_no_end` with `user_changes_limits`; otherwise `403`. When the default is already No end it is `201` without either |
| Both | `400` `run_end_conflicting_fields` |

- The run records `lifetime_cap_at`: the latest end its pool and the deployment (`WARDYN_RUN_MAX_AGE`) allow, counted from launch.
- `PATCH /runs/{id}` is bounded by the earlier of that time and now plus the profile's `max_end_ahead_sec`; its response names the limit in `latest_end_source` (`pool`, `governance` or `deployment`).
- A super admin skips the profile's limits only; the pool and the deployment still bind.
- `GET /runs?no_end=1` lists live runs with no end.

## Policy preview

`POST /api/v1/runs/policy-preview` accepts the same `CreateRunRequest` JSON as create and preflight, with the same member authentication and cookie CSRF checks.

- It resolves an incomplete draft without launching it.
- An empty task stays pending; it is never silently made interactive here.
- An absent model-provider choice is pending; explicit or inherited unavailable choices still refuse without enumerating hidden providers.
- It grades no autonomy: the tool-approval and managed-settings warnings come from `POST /runs/preflight`.

A successful response contains `spec` (authorized, clamped, workspace-folded and redacted), `source` (`kind`: `default`, `profile`, `stored` or `inline`; authorized `name` when available; `policy_id` only for stored), `provisional: true`, `redacted`, `warnings`, `pending`, and `repository_access`, and `components` when the draft names a component or a repository on a Git provider.

- Preview collections are arrays, never null; omitted versus explicitly empty axes inside the policy retain their policy meaning.
- In particular, a PAT scope with `repos: []` still permits no repositories; omitted `repos` remains unset.
- The existing ADO policy choice uses the provider default for both empty and absent lists.
- Repository groups carry `kind` (`github`, `azure_devops`, `other`), normalized `repos`, and the already disclosable ADO `org`, `default_profile` and `capability_ceiling` when applicable.
- They carry no row IDs, configured URL lists or credential-existence facts.
- Mount host paths and grant secret references are redacted for every preview reader.
- Warnings contain only count/structural messages and authorized ADO narrowing text; unsafe clamp messages are omitted.

`ado_lane_declined` is a run-level result: the reason the per-person Azure DevOps credential would not be given to this run's sandbox.

- It is decided by the resolver launch uses, over the run's workspace repositories and `repo`, for the same person. It never covers `devcontainer_repo`, which the server clones.
- When it is absent, no structural decline was reported. That is not a promise the credential is given: inputs that name no Azure DevOps repository omit it, and a repository the credential cannot serve (an SSH address, say) is skipped while another serves it.
- Launch decides again from the provider rows as they are then. The person's connection (`pending`: `credential_liveness`) and the row's capability ceiling are checked only at launch.

- Repository locators retain their transport, meaningful port and full escaped path, with URL userinfo, query strings and fragments omitted.
- Supported SSH aliases and scp forms use a normalized `ssh://` locator; the SSH-over-443 port is implicit.
- A group shares all of its displayed facts: ADO profiles and ceilings are sorted capability sets, and differing sets produce separate groups even when the displayed `org` is identical.

`pending` names unresolved launch work: `task`, `model_provider_selection`, `credential_liveness`, `autonomy`, `tool_approvals`, `runner_confinement`, `drive_readiness`, `dispatch_egress`.

- The drive entry appears only for an enabled selection.
- Preview authorizes drive allocation, profile, org switch, pause and read-only narrowing without checking the runner or share.
- Workspace overrides for unattached workspaces remain inert, as at create; effective attached sources are authorized before facts.

- Preview rejects the same malformed inputs and authorization failures through existing error envelopes and reasons.
- It reads no credential values, renews or mints nothing, persists no run or grant, dispatches nothing, and performs no runner, share, capacity or quota probe.
- After the shared authentication and CSRF middleware, the handler may emit only existing dry-run `authz.denied` and `workspace.provider.admit` events, coalesced with identical preflight denials.
- Authentication failures retain their existing middleware audit behavior.
- Its egress includes the hosts create adds after the mint from the run's own declarations: workspace declarations and clone hosts, an `ssh_key` grant's SSH-over-443 endpoint and a `git_pat` grant's Azure DevOps hosts, each only for a lane create will wire (not one the admitting provider row turns off, an `ssh_key` for Azure DevOps, or an `ssh_key` on `codex-cli`). The site-config SCM hosts, the model provider's hosts and the per-person Azure DevOps lane stay pending (`dispatch_egress`); the configured host list is never shown.
- Custom inline runs with an authorized `github.com` HTTPS/slug clone and no surviving `github_token` also derive `github.com` and `*.githubusercontent.com`, narrowed through the caller's ceiling and personal domain capabilities before union.
- Preflight and create use the same rule; only create audits its additions as `run.egress.add` with `kind: github_direct`.
- Saved/default policy and authored manual-domain semantics are unchanged.

- The response is provisional even for its exact request body.
- Credentials, policies, capabilities and connections may change after it answers; create independently re-resolves every gate.
- Clients should associate results with the request and principal, mark them stale after 60 seconds or relevant auth/config changes, and honor `Retry-After` without replacing a last good preview with an empty result.
- Never use this read output as editable policy input.

### Access

Preflight (`PreflightResult.Access`) and the policy preview answer `access`, one block built by one function from the same fold, so the two agree for any request both admit.

- `rows`: what brings access, inherited first (the harness, each Git provider's repositories, a command), then what the person added (components, secrets by header, allowed hosts). Each has a `status`: `ready`, `needs_setup`, `unavailable` or `refused`.
- `hosts`: the Network total, without the site-config SCM hosts. Each host has a `verdict` against the resolved, clamped policy, never the request: `allowed`, `needs_approval` (a review mode) or `not_allowed` with `reason` `clamped`, `denied_list` or `always_deny`. A deny entry wins over everything.
- `credentials`: the Credentials total, each with its `delivery`, what is left once the run ends (`after_end`) and, for every credential but a model's, a `label` naming its kind and whose it is, never a secret's name. The repository on Wardyn's git broker lists no raw host; its credential is delivered `git_broker`.
- An AWS sign-in follows `WARDYN_AWS_SSO_PROXY_INJECT`. On: the role credentials are in the sandbox, short-lived. Off: the session's access token is in the sandbox too, as a file. Both expire at the lifetime AWS sets.
- `unlisted`: what happens to a host nothing lists: the effective `mode`, `allow_all`, `hold_sec` for `wait_for_review`, and `decider` (`you`, `admin` or `you_or_admin`, from your `egress_host` capability).
- `setup`: what is still needed, as checklist rows with a `fix`: `add_secret`, `sign_in` (`target`, and `id` for a component) or `choose_model_provider` (`tool`). A model-provider sign-in names no provider id; sign in to the provider the draft chose.
- A CLI component's row has kind `cli` and `in_image`: `yes` or `no` for a harness's own image, `unknown` for an image the run names or the operator pinned.
- `harnesses`: one `ToolPolicyFact` per included harness. It carries the lane its calls take, who enforces it, its default, the policy's locked rules, the effects a person may choose, whether rules may name any tool or MCP tools only, and the MCP servers turned on for it.
- `harnesses` answers for the 0.9 run shape. An older-shape request that sets `seed_auto_tools` is still told that an interactive Claude Code run asks before a tool no rule names; the seeded tools run without asking.
- Source kinds add `harness` and `posture` to the provenance set; a client shows no chip for a kind it does not know.

## Listing a repository's branches

`GET /api/v1/workspaces/{id}/branches?repo=<repo>` lists the branches of one of the workspace's repositories, for the New Run branch picker.

| Field | Meaning |
|---|---|
| `listing` | `listed`, or `unavailable`: show a text field instead. A failed listing is still a `200`. |
| `branches` | Never null. The default branch first, then by name, at most `200`. |
| `default` | The default branch, when the forge named one. |
| `truncated` | More branches exist than were returned. |
| `credential` | `ado_signin` (the person's own Azure DevOps sign-in), `ado_own_token` (their own token) or `github_app` (the organisation's GitHub App). |
| `reason` | Why it is `unavailable`: `forge_not_supported`, `no_credential`, `signin_needed`, `forge_refused` or `forge_unreachable`. |

- Only a caller the launch door admits for the workspace may ask; anyone else gets the `404` a missing workspace gets.
- `repo` must be one of the workspace's repository sources (`422` `workspace_repo_unknown`), and each person is rate-limited (`429` `branch_list_rate_limited`).
- A repository given by an SSH address is `forge_not_supported`, since the listing goes over HTTPS: the picker takes a typed branch.
- Azure DevOps is asked at the admitting provider row's own address, never the repository URL's host. A `minted_pat` row lists nothing.
- On GitHub the App mints a one-repository token with `contents: read`, which private repositories need.
- GitHub fixes that token's life at `1` hour and it cannot be shortened. Wardyn revokes it after the read, best effort.
- Each listing writes one `workspace.branches.list` audit row, with no branch names.

## Custom components

A run can carry components that add destinations and secrets ([POLICIES.md](POLICIES.md#custom-components) defines them; [custom-components.md](operations/custom-components.md) is the admin side). The client wraps both stores of them.

| Method | Route | Who |
|---|---|---|
| `MyComponents` | `GET /api/v1/me/components` | any signed-in person |
| `SaveMyComponent` | `POST /api/v1/me/components` | any signed-in person |
| `UpdateMyComponent` | `PUT /api/v1/me/components/{id}` | the owner |
| `DeleteMyComponent` | `DELETE /api/v1/me/components/{id}` | the owner |
| `ListComponents` | `GET /api/v1/components` | admin |
| `PutComponent` | `PUT /api/v1/components/{id}` | admin |
| `DeleteComponent` | `DELETE /api/v1/components/{id}` | admin |

- `MyComponents` answers `may_define`, `resident_delivery_allowed`, `autonomy_cap`, `mine`, `org` and `clis`.
  - `org` holds only the organisation components the caller is granted, as `OrgComponentView`: the admin's `description` when there is one, hosts, delivery modes, config keys, `category` and an optional `cli` or `mcp` view, never a secret name.
  - `clis` lists five CLI cards in a fixed order (`azure`, `github`, `aws`, `gcloud`, `kubectl`), each `{kind, state, rows}` with `state` `available`, `not_granted` or `coming_soon`, built only from rows the caller is granted.
- A save answers `ComponentSaved`: the stored row and `requirements[]`.
  - Each requirement is `{kind: "secret", name, status: "present"|"missing", fix: "add_secret"}`. The list is advisory and never refuses a save.
- `PutComponent` creates an organisation component restricted: nobody may attach it until an admin grants its id under `/api/v1/permissions`.
- A run attaches components in `CreateRunRequest.Components`: `{id}` for a stored one, `{inline, name}` for a definition used by that run alone. At most 16.
- `{builtin: "github", org, repos, access}` reaches the listed repositories of `org` without a workspace, only through Wardyn's git broker. Each repository meets workspace admission and the `workspace_provider` capability; `access` (`read` by default, or `write`) must be within the caller's ceiling for a `github_token` grant. `azure_devops` is refused with `request_field_unavailable`.
- Preflight and the policy preview answer `components`: one fact per component with `status` (`ready`, `needs_input`, `unavailable` or `unknown`), its `requirements`, hosts and each secret's delivery and `shared` flag.
  - A fact never carries the name of a secret the organisation provides.
- Create, preflight and preview refuse with the component reasons in [`Reason`](#reason-branch-on-this-never-on-errors-prose); a run is audited as `run.component.attach` and `run.component.refuse`.

## Run templates

A template is a saved run setup, whole or partial, kept for yourself, published to the organisation or published to one group. It is content: a published template grants nothing.

| Method | Route | Who |
|---|---|---|
| `ListTemplates` | `GET /api/v1/templates` | any signed-in person; each row is one they may read |
| `GetTemplate` | `GET /api/v1/templates/{id}`, `/revisions/{revision}` | a reader of that template |
| `SaveTemplate` | `POST /api/v1/templates`, `PUT /api/v1/templates/{id}` | the owner (personal), an organisation administrator (organisation), the group's template managers (group) |
| `DeleteTemplate` | `DELETE /api/v1/templates/{id}` | as `SaveTemplate` |
| `CopyTemplate` | `POST /api/v1/templates/{id}/copy` | a reader of the source who may write the target |
| `ListTemplateGroupAdmins`, `GrantTemplateGroupAdmin`, `RevokeTemplateGroupAdmin` | `GET`/`PUT`/`DELETE /api/v1/admin/template-group-admins` | admin |

- A template you cannot read, and one that does not exist, answer the same `404` `template_not_found`.
- `ListTemplates` answers `publish`: where you may save one.
  - `org` is true only for an organisation administrator in the Admin view: in the User view an administrator is a member, so publishing to the organisation is a copy made from the Admin view.
  - `groups` lists each group you are a verified member of and manage templates for; `groups_unverified` is true, with no group offered, when your sign-in's group list is missing or incomplete.
- `TemplateSaveRequest.kind` is `template` (the default) or `policy`.
  - A policy holds only `inline_policy` with run-wide network, credential and access-rule keys (`allowed_domains`, `denied_domains`, `allow_all_egress`, `first_use_approval`, `first_use_hold_seconds`, `max_holds`, `allowed_methods`, `eligible_grants`, `llm_inspection`), with `coverage: partial` and no `needs_setup`.
  - Saving one writes no stored policy.
- An update needs `expected_revision`, the revision you read. Another current revision answers `409` `template_revision_conflict` and changes nothing; keep your edits, reload, and apply them again.
  - Every revision stays readable by number; scope and kind never change (copy a template to publish it elsewhere).
- A refused document answers `TemplateSaveRefusal`: the first diagnostic's `error` and `reason`, and every diagnostic with its path.
- A shared template holds references (secret names, workspace and drive ids), never values, and its readers see those names.
- `created_by` and `updated_by` are set only for someone who may write the template.
- A group grant (`TemplateGroupAdmin`) names one group and one person, stored in canonical form; its optional `label` is the group's display name, shown beside its templates, and never authority.
- The CLI covers these: `wardyn template list|show|save|import`, and for administrators `wardyn template group-admin list|grant|revoke` and `wardyn template from-preset <name> [--everyone]`. `from-preset` saves a launch preset to the organisation through `PresetDocument`, which refuses a preset limited to `user_types` unless `--everyone` is passed. A preset's `inline_policy` replaced your default policy, and in a template it is laid onto it.

## Run documents

A `wardyn/v1` document with `kind: Run` is a draft of one run. Its `intent` holds the `CreateRunRequest` fields, and a `kind: RunTemplate` document is read by the same decoder.

- A `Run` document carries `api_version`, `kind` and `intent`. `name`, `coverage` and `needs_setup` are refused as `template_field_unknown`; a run title or description, at the top or in `intent`, as `template_field_excluded`.
- A `Run` document has no coverage rules: completeness is checked when the run is reviewed.
- A `kind` other than Run or RunTemplate is reported at path `kind` and a wrong `api_version` at `api_version`, both as `template_version_unsupported`. When the import request names the `kind` it expects and the document is the other valid kind, `kind` carries a sentence saying so, beside the document's other diagnostics.
- `POST /api/v1/templates/import` (`Client.ImportTemplate`) takes `{format, source, kind?}`, where `format` is `json` or `yaml` and `kind` is `Run`, `RunTemplate` or empty for either. It answers `200` with the normalised `document` when nothing is wrong, and otherwise with `diagnostics`, each `{path, reason, message}`. It stores nothing, grants nothing and needs only a signed-in person.
- A document over 1 MiB is a diagnostic in a `200`; a request over 2 MiB plus 4 KiB is `413` `request_body_too_large`. A client that JSON-escapes `<`, `>` and `&` (Go does, as six bytes each) can push a document under its own cap past the request cap when it is mostly those characters.
- The door holds a document to the same shape and availability checks as create, preflight and the policy preview, so a request they refuse is refused at the door with the same reason, except that a field this server cannot honour yet is `template_field_unavailable` at the door where a run request gets `request_field_unavailable`.
- `wardyn run --file <doc>` (`-` reads stdin) has the door check a `Run` document, then creates the run from its `intent`. A document's `inline_policy` is sent as the request's `policy_overlay`, so it lays onto your default policy and never replaces it. Only `--title`, `--description`, `--wait`, `--timeout`, `--max-wait`, `--json` and `--dry-run` go with it. `wardyn template import --kind Run|RunTemplate` sets the kind the import expects.

`policy_overlay` carries a document's `intent.inline_policy`: the policy keys it names replace your default policy's (your governance ceiling, or the deployment default), and the rest keep the default's values. Only the named keys are bounded by your ceiling, so the default's operator mounts and grant pairings stay. Beside `policy_id` or `inline_policy` it is `400` `policy_overlay_conflict`; beside `preset`, `preset_field_not_per_launch`. A null or empty overlay, an unknown key, a wrongly typed value, an excluded field or a redaction placeholder is `400` `policy_overlay_invalid`, naming the first path. Edits beside a saved policy travel as `overrides`.

`shape` (`agent_task`, `environment` or `application`) names the New Run choice a request was set up from. It is optional and needs `experience`; a value outside the three is `400` `run_mode_invalid`. The server checks it against the run and otherwise does not use it; a request that does not fit is `422` `run_shape_conflict` at create, preflight, preview and the import door.

| `shape` | Requires |
|---|---|
| `application` | `experience: background` and `workload.kind: command`; or, for a server (0.10), `experience: interactive`, `startup.kind: command` and at least one `ui_apps` entry in `inline_policy` or `policy_overlay` |
| `environment` | `experience: interactive` |
| `agent_task` | With `background`, `workload.kind: agent_task`; with `interactive`, a `tools[]` entry of kind `harness` |

### Run again

`POST /runs` keeps each run's setup as a `Run` document for Run again. Only the run's creator reads it.

| Route | Answers |
|---|---|
| `GET /api/v1/me/launch-intents?limit=` | `{intents: [{run_id, title, shape?, created_at}]}`: your own runs that kept one, newest first; `limit` defaults to 20, at most 50 |
| `GET /api/v1/runs/{id}/launch-intent` | `{run_id, created_at, document}`, the document of kind `Run`; `404` `launch_intent_not_found` when the run kept none |
| `POST /api/v1/runs/{id}/desktop-signin` | `201` `{signin_id, expires_at, app}`: the agent is stopped and the person's display is open through the UI gateway as `app` (`_signin`). The run's creator only; `501` `desktop_signins_off` unless `WARDYN_DESKTOP_SIGNINS=on` |
| `POST /api/v1/runs/{id}/desktop-signin/shares` | `{site}` → `{site, cookies}`: that registrable domain's cookies are copied into the agent's browser profile. After a share the agent holds them within its egress |
| `POST /api/v1/runs/{id}/desktop-signin/end` | `204`: the person's display is closed, then the agent resumed |

- Anyone but the run's creator, an administrator included, gets the `404` a missing run gets.
- The document is your request after a preset expands, without the fields chosen at each launch (title, description, `max_wait_sec`, `local_folder`) or the stamps of where it came from (`preset`, `preset_version`, `mode_contract`); `policy_overlay` becomes `intent.inline_policy`. A run that waits in a pool's queue keeps one too.
- A run keeps none when it sets a whole `inline_policy`, when the import door refuses the document (a secret-shaped task, say), or when an erasure of your launch intents is running (`erasure_in_progress`). It still starts; `run.create` says `launch_intent: not_kept` and why.
- Erasure's `run_tasks`, `components` and `run_setups` scopes delete a person's launch intents.
- The SDK has no method for these routes yet.

### Several harnesses in one run

- `tools[]` may include more than one entry of kind `harness` with `interactive`, or with `workload.kind: command`. A background agent task includes only `workload.agent`.
- Each harness names its own `model_provider`, judged as its own choice. The primary harness is the `startup` harness, else the first listed; the run's `agent` and `model_provider` are the primary's.
- The run starts from the image the operator declared for the set, under a `WARDYN_AGENT_IMAGES` key joining the sorted ids with `+`. Without one the request is `422` `harness_set_no_image`; a published install declares none ([build-images.md](operations/build-images.md#the-image-a-runs-harnesses-start-from)).
- An AWS sign-in provider (`bedrock_sso`) serves only the primary harness. On any other it is `422` `request_field_unavailable`, with `tool` naming the harness.

## Model providers

`GetModelProviders` (`GET /api/v1/model-providers`, admin) answers a `ModelProvidersRead`: the provider records, and per provider id how many people hold a credential of their own for it. It returns configuration and counts, never a credential value. Changing providers, signing in and storing a credential stay in the console.

## Error handling

Non-2xx responses are returned as `*client.APIError`:

```go
_, err := c.GetRun(ctx, id)
var apiErr *client.APIError
if errors.As(err, &apiErr) {
    fmt.Println(apiErr.Status, apiErr.Body) // e.g. 404  {"error":"run not found"}
}
```

### `Reason`: branch on this, never on `Error()`'s prose

A route may also send a machine-readable `reason` alongside its `error`
sentence — `apiErr.Reason` carries it, `""` when the route sends none. Match
on `Reason`, never on the human sentence: the sentence is free to reword
without notice, `Reason` is not.

```go
if errors.As(err, &apiErr) && apiErr.Reason == "scope_changed" {
    // relaunch the run — its credential was dispatched with a provider row
    // that has since changed.
}
```

A `400` from `PUT /model-providers` for an Azure provider may also carry `field`, the dotted path of the request
field it is about (for example `azure.endpoint`). It is absent on every other refusal; read it from `apiErr.Body`.

**Coverage is uniform (#204, #656): every non-2xx body this API writes
carries `reason`, or is one of a small, reviewed, named exception.**

The sweep ran lane by lane — the three credential-injection resolve arms behind
`GET /api/v1/internal/injection/{grantID}` (Azure DevOps, AWS SSO, Bedrock
bearer) first, then:

- #656 slice 1 (`GET/POST /approvals` including its
  `/paths` route, the internal sidecar's push-content raise route, `POST /runs`
  and `POST /runs/preflight`, `GET /runs`, run kill, workspace delete and the
  workspace create/update/admission/env-as-code/providers routes),
- slice 2
  (`/site-config`, `/governance`, `/secrets`, `/people`, `/access`,
  `/permissions`, `/admin/delegates`, `/setup/*`, `/sessions/revoke`,
  `/ssh-keys`, the user-drive doors),
- and slice 3 (every remaining `run_*.go`
  per-run action, the UI gateway, device federation, and the rest of the route
  surface).

But a route reached today answers exactly like one reached at the
start.

- Run revive and the admin bulk restart send `revive_unsupported` for a
  run whose substrate cannot replace its proxy, alongside slice 3's other
  per-run reasons.
- Two repo-wide guards keep it that way:
  - `TestEveryWriteErrorCallCarriesAReasonOrIsReviewed` fails the build on any new
    bare `writeError`/`http.Error` call outside its small, evidence-backed
    allowlist,
  - and `TestNoAdHocErrorBodyOrReasonLiteral` catches a direct
    `writeJSON(errorBody{...})` construction with no `Reason`, or a hand-typed
    string literal passed to `writeErrorReason` instead of a named constant.
- Three refusals are DELIBERATELY still bare, each with its own pinning test proving the
  absence is intentional:
  - a transient model-provider store failure that is no
    door,
  - an unanswered AWS Bedrock SSO renewal that is not a refusal class,
  - and the drive-mount resolver's own runner-unavailable/caller-cancelled arms.

  See [`internal/api/reason_coverage_guard_test.go`](../internal/api/reason_coverage_guard_test.go)'s own allowlist for exactly
  which three and why.
- Every OTHER reason is drawn from one of two sources:
  - the
    closed set in [`internal/api/reasons.go`](../internal/api/reasons.go) and `reasons_routes.go` (a reason two
    routes share — the dispatch-time-snapshot family below, the hold-chain
    terminal/exhausted pair — always means the same thing regardless of which one
    sent it),
  - or [`internal/authz/registry.go`](../internal/authz/registry.go)'s own registry, which `s.refuse`
    writes directly for every Deny/Unprocessable/Conflict-effect authorization
    refusal (its two Hidden-effect reasons are never themselves a wire value — see
    the authz section below).

`TestReasonDocsMatchReasonsGo` requires this table to match both sources:

| Reason | Meaning |
|---|---|
| `runners_disabled` / `runners_config_unavailable` | Runner routes require enabled, readable site configuration. |
| `runner_stream_unavailable` / `runner_stream_rate_limited` | Runner stream URL/store unavailable, or admission limits exceeded. |
| `runner_registration_unavailable` | Runner registration requires a configured public org URL and registration store. |
| `runner_registration_rate_limited` | Too many registration attempts; retry later. |
| `runner_token_rate_limited` | `POST /me/runners/tokens` (`runner_registration.go`): a person minted more than 10 runner registration tokens in an hour; retry later (`429` with `Retry-After`). |
| `runner_registration_invalid` / `runner_owner_invalid` | Registration fields or token owner are invalid. |
| `runner_key_already_registered` | This key is already registered; generate a new identity. |
| `runner_not_found` / `runner_token_not_found` | No such runner or unused token for this caller; another person's runner, and an unclaimed runner past its wait, answer as an absent id. |
| `runner_filter_invalid` | `state` is not `active`, `revoked` or `all`. |
| `runner_limits_invalid` | `400`: `limits` is missing or not a valid set (runs at once 1 to 64, at least one CPU, disk in whole GiB, no per-run amount past its total); `422`: a total past what the runner last reported. |
| `runner_trust_invalid` | `PUT /runners/{id}/trust`: `400`, `trust_class` is not `personal`, `managed_device` or `org_cluster`, or `reason` is missing; `409`, the runner is not claimed yet; `422`, a class its owner kind cannot hold (`org_cluster` is an organization runner's class and only its). |
| `run_containment_pending` | `403` on token renewal (`identity.renew`): the run's Kubernetes runner deleted it but cannot yet prove it stopped, so its token is never renewed again. The runner's relay fence is what bounds its egress; this is the second layer. |
| `relay_ip_fenced` | The `reason` on a run's `run.create` failure row, and its failure hint: a Kubernetes runner gave the run's proxy pod an IP it still fences for an earlier run whose termination is unknown, so it stopped this run before it opened. Launch the run again. |
| `node_record_invalid` | `POST /runs/{id}/node-record`, `400`: `attestation` is not `node_powered_off` or `no_prior_holder`, `source` is not `provider_console`, `management_controller` or `other`, `source_id` is missing, longer than 256 characters or holds control characters, or `tuple` is missing. |
| `node_record_refused` | `POST /runs/{id}/node-record`, `409`: the run is not held as containment pending, the runner's record (`tuple`) changed since the form was loaded, `no_prior_holder` was sent after a recorded name reuse or for a run with a causal node association, or another stop for the run already waits for its runner. A held record whose run moved before its approval is refused at the approval the same way. |
| `node_record_human_only` | `POST /runs/{id}/node-record`, `403`: the admin token or an API token made it, or approved a held one (a node record is made and approved from a signed-in session), or the record needs two people and the server runs in local mode, which cannot show who they are. |
| `sign_in_required` | `401` on the public API of a deployment with single sign-on and no admin token, for a request that carries neither a session nor a bearer token: sign in through single sign-on. A deployment with neither single sign-on nor an admin token keeps `admin_token_not_configured`; the `auth.fail` row is the same `admin_token_not_configured` either way. |
| `host_capacity_refused` | `503` with `Retry-After: 30` on every run launch door (new run, preflight, record, harness sign-in, source scan, site-config probe, workspace build): the host is short of memory or over its load ceiling (`WARDYN_HOST_MIN_MEM_AVAILABLE_MIB` / `WARDYN_HOST_MAX_LOAD1`). The body is the one exception to the `error`/`reason` shape: `error` is this code, `reason` names each limit crossed with its measured value, and `message` is the sentence to show. Wait and retry. |
| `runner_limits_required` | `422`: a registration that declares no limits, or limits that are not a valid set. Register with this release's `wardyn-runnerd`. |
| `runner_session_required` | `/me/runners` needs the person's own session; administrative and delegated credentials own no runner. |
| `runners_enabled_required` | `PUT /runners/settings` needs `enabled`: true or false. |
| `runners_org_url_invalid` | Runners cannot be turned on until the organisation and control-plane URLs are HTTPS. |
| `site_config_runners_via_own_route` | Change `runners.enabled` with `PUT /runners/settings`. |
| `runner_token_invalid` | Registration token is unknown, expired, consumed, or bound to another org. |
| `runner_claim_mismatch` | The personal owner, fingerprint, org binding, claim state, or expiry does not match. |
| `runner_not_found` | No such runner to revoke: absent, already revoked, an unclaimed one past its wait, or (on `/me`) another person's. |
| `runner_revoke_reason_invalid` | An owner's revoke names a reason other than `not_mine`. |
| `relay_route_refused` | A runner's relay serves only the closed list of internal routes a local run needs; this route, or a grant not delivered `own`, is not served through it. |
| `relay_runner_mismatch` | A relayed request names a run that is not placed on the relaying runner. |
| `relay_required` | A local run's token was used off its runner's relay; local runs reach the org only through it. |
| `runner_action_pending` | The relaying runner has an action pending for this run; retry once the runner has acted. |
| `delivery_via_org` / `delivery_runner_resident` | Through a relay, a grant delivered `via_org` is used at the org and a `runner_resident` one already lives on the runner, so neither is handed out. Except an Azure DevOps minted token, served to the runner's proxy while policy, owner, trust, posture and opt-in admit it; a refusal revokes it. |
| `delivery_not_via_org` / `via_org_destination_refused` / `via_org_cap_exceeded` | `/internal/via-org/{grantID}/…` serves only this run's grant delivered `via_org`, to the grant's own destination, within 16 open streams and 8 GiB a run per server replica, reset on restart. |
| `missing_scope_snapshot` | The grant names the credential sentinel but carries no dispatch-time snapshot (a hand-authored grant). Azure DevOps, AWS SSO; and every grant naming `bedrock-api-key`, which is no longer a model credential. |
| `owner_not_caller` | The grant's snapshot owner is not the run token's own subject. Azure DevOps. |
| `roster_unreadable` | The site configuration could not be read; nothing is resolved from a failed read. Azure DevOps, AWS SSO. |
| `run_unreadable` | The run row itself could not be read. AWS SSO; also `POST /approvals/{id}/{approve,deny}`'s second-human gate (`WARDYN_EGRESS_SECOND_HUMAN`, `WARDYN_CAPABILITY_SECOND_HUMAN`). |
| `scope_changed` | The live provider row has drifted from the run's dispatch-time snapshot. Azure DevOps, AWS SSO. |
| `store_error` | The credential store operation failed. AWS SSO (a read); also the Azure DevOps sign-in callback's own captured-credential write (`ado_entra.go`) — the identical "a store errored" fact, not a second name for it. |
| `token_mode` / `signin_unconfigured` / `signin_unreadable` | The organisation is in token mode, has no sign-in app registration configured, or its sign-in configuration could not be read (`adoEntraConfigFor`). Azure DevOps. |
| `ado_pat_policy_blocked` | Azure DevOps refused to create a personal access token for the person: the organisation restricts who may create them. The organisation's allow list is the fix. |
| `ado_pat_lifespan_policy` | Azure DevOps refused to create a personal access token because its life is above the organisation's maximum token lifespan. Lower the row's `pat_max_hours`. |
| `ado_pat_consent_needed` | The person's Azure DevOps sign-in cannot create tokens: the app registration lacks the token permissions or consent for them. |
| `ado_pat_mint_refused` | Azure DevOps refused to create a personal access token for another reason. |
| `ado_pat_unavailable` / `ado_pat_run_inactive` | A `minted_pat` run's personal access token can't be created: this deployment has no way to create one, or the run is paused or has ended (resolved again when it resumes). Azure DevOps. |
| `mint_scopes` / `scope_unknown` | S2: the Entra sign-in lane refuses to send a bearer whose granted scopes name a token permission (`vso.pats`, `vso.pats_manage`, `vso.tokens`, `vso.tokenadministration`, `user_impersonation`), or whose authority reported no granted scope at all (`resolveADOInjection`). Azure DevOps. |
| `ado_pat_needs_console_app` | S1: a `minted_pat` row the console cannot redeem with its own client secret — it names another application or tenant, or `WARDYN_OIDC_CLIENT_SECRET` is unset — is unusable: the sign-in door and the organisation check refuse it, and `/me/scm-access` grades it `expired_signin` with this cause. |
| `ado_org_check_unknown_row` / `ado_org_check_organisation` | `POST /workspace-providers/git/{id}/org-check` (`ado_pat_orgcheck.go`): no such row, or not the deployment's `minted_pat` sign-in row (answered identically, D-6); or the row's address names no organisation and `?organisation=` named none it serves. |
| `host_not_organisation` | The requested host is outside the snapshot's organisation. Azure DevOps. |
| `host_not_endpoint` | The requested host is not the provider's own endpoint. `azure_foundry`. |
| `ado_own_pat_not_added` / `ado_own_pat_expired` / `ado_own_pat_other_org` | A `token_mode: own_pat` row's resolve (`resolveADOOwnPATInjection`): the run's owner has added no token of their own, their token has reached the expiry they entered (mid-run this raises the Azure DevOps sign-in hold instead), or it is for a different organisation than the run's. Azure DevOps. |
| `sso_host_not_portal` | The requested host is outside the credential's own SSO portal. AWS SSO. |
| `capability_not_grantable` / `capability_above_ceiling` / `capability_denied` / `capability_closed` / `capability_always_deny` / `capability_holds_exhausted` / `capability_review` | The capability escalation chain's refusals — see `injection_ado_capability.go`. Azure DevOps. |
| `approval_mismatch` / `approvals_unreadable` / `once_unspendable` | The named approval does not match, could not be read, or was already spent. Azure DevOps; `approvals_unreadable` also AWS SSO's re-auth hold. |
| `signin_closed` / `signin_holds_exhausted` | The hold chain has gone terminal (cancelled, expired, denied) or hit its per-run cap — see `injection_ado_signin.go`'s sign-in hold and `injection_awssso.go`'s re-auth hold, the same shape under two names. |
| `raise_failed` | The approval store itself errored while raising a capability, consent, sign-in or re-auth hold. Azure DevOps, AWS SSO. |
| `preset_unknown` / `preset_field_not_per_launch` / `preset_version_changed` / `preset_version_without_preset` | A `POST /runs` naming a launch preset: no such preset (or not open to the caller's user type, `422`), a field other than `title`/`task` beside `preset` (`400`), a pinned `preset_version` that is no longer current (`409`), or `preset_version` without `preset` (`400`). See OPERATIONS.md's launch presets section. |
| `not_captured` / `dead_credential` / `consent_required` / `interaction_required` / `unavailable` | `ADOEntraFailure`'s own closed enum ([`internal/api/ado_entra_store.go`](../internal/api/ado_entra_store.go)), carried through unchanged when the redemption classifies a renewal failure. Azure DevOps. |
| `invalid_approval_state` / `invalid_run_id_param` / `invalid_view_param` / `invalid_owner_param` / `invalid_status_param` / `status_needs_exclusive` / `status_needs_requires_view` / `invalid_ended_within_param` / `invalid_include_killed_param` / `invalid_no_end_param` / `runs_search_query_too_long` / `invalid_limit_param` / `invalid_offset_param` | A `GET /approvals` or `GET /runs` query parameter is malformed or conflicts with another one. `invalid_view_param` is the same reason on both routes (the same shape); `invalid_limit_param`/`invalid_offset_param` are `parseListPage`'s, shared by every paginated list route in `internal/api` (`GET /audit`, `/policies`, `/secrets`, `/user-drives`, `/api-tokens`, `/permissions/grants`, `/setup/integrations`, `/ssh-keys`, `/runs/policy-history`, …). |
| `listing_unscoped_backend` | The store backend cannot scope this listing to the caller's own runs — the SAME missing capability on `GET /approvals`, `GET /runs` and `GET /runs/policy-history`. |
| `approval_not_found` | The named approval does not exist, or the caller may not see it — `POST /approvals/{id}/{approve,deny}` and every other approval-lookup route, INCLUDING `GET /approvals/{id}/paths`: a foreign approval and a missing one answer byte-identically there, reason and body both, so the reason itself is not an existence oracle. |
| `approval_run_ended` / `approval_already_decided` / `credential_reauth_not_decidable` / `decision_scope_invalid_for_kind` / `invalid_request_body` | `POST /approvals/{id}/{approve,deny}`'s pre-decision refusals: the run ended first, the approval was already decided, a credential-reauth approval takes a sign-in instead, `decision_scope` was sent on a kind that does not accept one, the body did not decode, or `reason` is over 512 characters. A body over 16 KiB is `413` `request_body_too_large`. |
| `takeover_not_decidable` / `not_a_takeover_request` / `takeover_not_holder` | "Needs you" (`human_takeover`) requests: `POST /approvals/{id}/{approve,deny}` on one (it is answered with `take-over` or `decline`, for every tier); `take-over`, `hand-back` or `decline` on an approval of another kind; `hand-back` by someone who does not have control. |
| `invalid_decision_scope` / `decision_scope_until_needs_expiry` / `decision_expiry_in_past` / `decision_expiry_too_far` / `decision_expiry_without_until` | The `decision_scope`/`decision_expires_at` shape rules (0-3) on a decide call. |
| `decision_scope_always_unavailable` / `decision_always_no_workspace_link` / `decision_always_invalid_host` / `decision_always_workspace_gone` / `decision_always_host_builtin` / `decision_always_host_denied` | The operator-only `decision_scope=always` persistence chain (rules 5-7): no store to persist to, the run names no workspace, the host does not parse, the workspace is gone, or the host is built-in-routed or on the reject list. `decision_scope_always_unavailable` also covers the internal push-content raise route's own "no store" arm (the identical `s.cfg.Store == nil` cause, one route apart). |
| `egress_second_human_local_mode` | `WARDYN_EGRESS_SECOND_HUMAN` (or `WARDYN_CAPABILITY_SECOND_HUMAN`) cannot be enforced in local mode (nobody is authenticated to prove a second human decided). |
| `invalid_push_scope` / `invalid_push_path_list` / `push_content_unattended` / `push_path_list_count_unavailable` / `push_path_list_cap_reached` / `push_not_held` / `push_path_lists_require_postgres` | The internal sidecar's push-content raise route (`POST /api/v1/internal/approvals`, `kind: push_content`) and `GET /approvals/{id}/paths`: a malformed raise, an unattended run's push refused rather than held, a per-run path-list cap, or a route that needs a Postgres-backed capability this backend lacks (`push_path_lists_require_postgres` also covers the raise route's own store-type-assertion arm, the identical cause one status apart). |
| `workspace_seed_store_unavailable` / `workspace_seed_unreadable` / `workspace_seed_source_target_invalid` / `workspace_seed_no_base_image` / `workspace_seed_policy_conflict` | `POST /runs`' `workspace_id` resolution (`seedRequestWorkspace`): no store configured, the workspace could not be read, a stored source's target fails the authored-target deny-list, an exec run named a workspace with no base image and no `--agent`/`--image`, or the seeded sources conflict with the policy's own mounts/repos. |
| `placement_invalid` / `resources_invalid` / `workspace_target_invalid` / `override_invalid` / `allowed_image_invalid` / `runner_pool_invalid` | `400`: a malformed `placement`, `resources`, workspace `target`, `overrides` entry, `allowed_image` or `runner_pool_id`; `422` for a target on a workspace with several repositories or folders. |
| `workspace_target_overlap` / `workspace_pin_conflict` / `image_conflict` / `workspace_ado_org_conflict` | `422`: the attached workspaces cannot share one run: equal or nested paths (two repositories in one directory included), two model-provider pins, base images or Azure DevOps organisations. |
| `workspace_selection_invalid` / `run_ref_invalid` | `400`: a malformed `workspaces[]` list (an id twice or malformed, over 32, or `workspace_id` not first) or `repos` entry (a repo twice, or a ref git could read as an option). |
| `workspace_repo_unknown` | `422`: a `repos` entry the workspace does not carry, or the `repo` of `GET /workspaces/{id}/branches` is not one of its repository sources. |
| `placement_unavailable` / `placement_credential` / `placement_capability` / `placement_local_path` / `placement_trusted_output` | `422`: a `placement: "local"` run with no user pool of yours, or one carrying a credential, mount, storage, path or trusted-output operation a runner cannot take. A credential is judged at the trust class of each eligible runner, highest first, and the detail names the class it was refused at. A `machine_kind: "linux_vm"` run with `interactive_start: "agent"` is a `placement_capability` too: a Linux VM's terminal opens a shell, not the agent CLI, in this release. |
| `runner_trust_changed` | `409`: a revive of a run on a runner whose trust class is no longer the one the run's credential plan was admitted at (or was never recorded). Launch the run again. |
| `runner_posture_unmet` | `422`: a `runner_resident` credential the runner's reported posture does not meet (the detail names every unmet field and the requirement; an unreported posture, at launch or on a chosen runner, meets no `require_posture`), or a run with a CC3 floor, which a runner-asserted class never meets for a resident credential. |
| `runner_keystore_unavailable` | `422`: the run's runner could not hold a `runner_resident` value: its OS keystore is unreachable or refused the write, or (macOS) the value is too long for the keychain. The run fails at dispatch, before its sandbox is created. |
| `placement_denied` | `403`: your governance profile's `deny_placements` refuses the run's placement, at create, preflight, policy preview and source scans; audited `authz.denied` (target `runs.placement`). At revive the same `403` is audited `run.revive` denied; at dispatch the run fails and is audited `run.dispatch` failure with this `reason`. A managed-harness sign-in is exempt at every door; a site-config probe at create and dispatch. |
| `runner_offline` | `422`: a `placement: "local"` run whose runner is offline or has not reported its capabilities yet, or none of your runners is online. |
| `runner_at_capacity` | `422`: a revive onto a runner at one of its limits (Runs at once, or a total); or a launch that lost the runner's last room to another launch between placement and the insert (launch it again). A launch whose runners are all at their limits waits in the pool's queue instead. |
| `runner_limits_unset` | `422`: no runner of the pool that could take the run has declared its limits. |
| `runner_recording_off` | `422`: the organisation or the pool requires recording (`recording: required`), and no runner of the pool that could take the run records: one started with `--recording-store off`, or one older than per-run recording. |
| `recording_store_off` | `422`: the organisation or the pool requires recording, and the organisation keeps no session recordings. Saving `recording: required` (a pool, or the organisation's defaults) while it keeps none is refused with it too. |
| `override_refused` | `422`: a widening, base-host removal or foreign secret the narrowing table never allows. |
| `request_field_unavailable` / `main_drive_unenforceable` | `422`: a field this server cannot honour yet, refused and never dropped; or a disk size the run's policy sets that the pool's runners cannot cap. |
| `harness_not_installed` / `harness_set_no_image` / `harness_set_custom_image` / `harness_providers_conflict` | `422`: no image here carries a harness, or the harnesses together; two or more harnesses name a custom image; or two harnesses' providers share a host. `tool` names the harness. |
| `drive_selection_conflict` / `drive_invalid` / `drive_not_found` / `drive_not_in_pool` / `drive_target_conflict` / `drive_share_beside_managed` | `drives[]`: `400` `drive` beside `drives`, or a bad id; `422` an unreachable id, an unmountable pool, two drives on one path, a Kubernetes share beside managed. |
| `member_drive_requires_person` / `member_drive_rate_limited` / `member_drive_name_invalid` / `member_drives_disabled` / `member_drives_denied_by_profile` / `member_drive_unsupported` / `member_drive_delete_unavailable` / `member_drive_name_taken` / `member_drive_limit_reached` | `POST /me/drives`, a person's own drive: `403` not a person's session or own token; `429` more than 6 creates and deletes a minute; `400` a bad name; `422` the organisation switch is off, no managed backend or drive size here, or a substrate that could not delete what it would create; `403` a governance profile denies it; `409` a drive you can use has the name; `422` you hold the most drives allowed. |
| `member_drive_in_use` / `member_drive_busy` / `member_drive_delete_failed` | `DELETE /me/drives/{id}` (also `member_drive_requires_person`, `member_drive_rate_limited`, `member_drive_delete_unavailable`): `409` a live or kept run holds the drive, or another delete is in flight; `503` the substrate failed and nothing was removed. A drive that is not yours, an admin drive and an unknown id all answer `404` `drive_not_found` with the same body. |
| `user_drive_member_owned` | `409`: `PUT`/`DELETE /drives/{id}`, `POST /drives/grants` or `POST /drives/{id}/reclaim` named a person's own drive; a super admin deletes it with `DELETE /drives/member/{id}`. |
| `run_mode_required` / `run_mode_invalid` / `start_folder_invalid` | `400`: a new client's mode (or a background task's workload) is unset, and is never inferred (`mode_contract: "0.9"` marks a request as a new client's even when it carries no other run-mode field); a run-mode carrier is malformed; a `start_folder` is malformed, names an attachment or a repository the run lacks, or leaves its mount. |
| `run_mode_conflict` | `422`: run-mode carriers contradict each other, or an older field they replace (`agent`, `task`, `interactive`) rides beside them. The CLI's `--agent` / `--task` (and `--interactive`, `--task-mode`) with no run-mode flag are the deliberate exception: that request carries no `mode_contract` and stays the older-client shape. |
| `policy_overlay_invalid` / `policy_overlay_conflict` | `400`: an unusable `policy_overlay`, or one beside `policy_id` or `inline_policy` (see [Run documents](#run-documents)). |
| `run_shape_conflict` | `422`: `shape` does not fit the run: it needs `experience`, and `application`, `environment` and `agent_task` each require the workload or tool shown under [Run documents](#run-documents). |
| `run_application_ui_apps_denied` | `422`: `POST /runs`, `POST /runs/preflight` and `POST /runs/policy-preview`: `shape` `application` declares `ui_apps` that the governance profile's `deny_ui_apps` strips or its allowed apps lack, and the sentence says which. Other shapes keep the strip and the warning. |
| `run_background_only` | `409`: an interactive door of a background task (terminal, exec, SSH, web gateway, desktop). Logs, audit, status and the lifecycle controls (kill, revive, extend) stay. |
| `template_document_invalid` / `template_field_invalid` / `template_field_unknown` / `template_version_unsupported` | `400`, `422` for the version: a document that is not one bounded document, a value of the wrong shape (null and redaction placeholders included), a key the schema lacks, or an `api_version` or `kind` this server does not read. |
| `template_secret_refused` / `template_run_state_refused` / `template_metadata_in_content` / `template_field_excluded` | `400`: a template never holds a secret value, state of one run (identity, session, claim, admission), an owner, scope, group or revision, or a launch-only (title, description) or retired field. The path is in the diagnostic. |
| `template_field_unavailable` / `template_dependency_missing` / `template_coverage_invalid` / `template_shared_field_refused` | `422`: a field this server cannot honour yet (refused, never dropped), a field whose dependency is neither included nor listed under `needs_setup`, a coverage that does not fit the content, or a field that names one person's own thing in an organisation or group template. |
| `template_scope_forbidden` / `template_group_unverified` / `template_not_found` / `template_revision_conflict` | `403`, `403`, `404`, `409`: publishing to a scope the caller does not administer, group membership that cannot be verified from this sign-in, no such template or not visible to the caller, or an update against a revision that has moved. |
| `templates_unavailable` | `501`: this server's store keeps no templates. Every `/templates` route except `POST /templates/import`, and the `/admin/template-group-admins` routes, answer it; the Postgres store keeps them. |
| `template_name_taken` / `template_limit_reached` | `409`: another template available to the same people already uses the name (compared case-insensitively, per person, per group and for the organisation), or that audience already holds its most templates (200 per person, 200 per group, 500 for the organisation). |
| `launch_intent_not_found` | `404` on `GET /runs/{id}/launch-intent`: the run kept no setup. A run you did not create answers the `404` a missing run does. |
| `desktop_signins_off` / `desktop_signin_runner_unsupported` / `desktop_signin_not_running` / `desktop_signin_no_desktop` | `501`, `409`, `409`, `409` on the `/runs/{id}/desktop-signin` routes: sign-ins are off on this deployment; the run started without sign-ins: its runner is not this deployment's own Docker one (a self-hosted runner, a pool's executor or Kubernetes), its desktop image is not the sign-in image, or it started while sign-ins were off; the run is not running; its policy declares no `novnc` app. |
| `desktop_signin_image_not_granted` | `409` on `POST /runs/{id}/desktop-signin`: the run's image was your own choice and no grant of yours names its sign-in variant (`agent-novnc-signin` at the same registry and tag), so the run kept its image and started without sign-ins. |
| `desktop_signin_open` / `desktop_signin_not_open` / `desktop_signin_not_holder` | `409`, `409`, `403`: a sign-in is already open on the run; none is open (or its end is pending); it was begun by someone else. |
| `desktop_signin_pause_failed` / `desktop_signin_start_failed` / `desktop_signin_end_failed` | `502`: the agent could not be stopped and proven stopped (it was resumed, or, when the resume itself cannot finish, it stays stopped and Wardyn retries); the person's display did not start (the agent was resumed); the display could not be proven closed (the agent stays stopped and Wardyn retries). |
| `desktop_signin_site_invalid` / `desktop_signin_share_failed` | `400`, `502` on `POST .../desktop-signin/shares`: the site is not a host name under a public suffix; the export or import failed, and nothing was shared. |
| `desktop_signin_unavailable` / `desktop_signin_credential_refused` | `503`: the sign-in or the run's policy could not be read, or the sign-in could not be recorded; `500` on the begin when the server could not make the sign-in's credential. `403` from `POST /internal/desktop-signin/check`, the run proxy's question: not the run's open sign-in's credential. |
| `desktop_signin_resume_failed` | The `reason` on a `run.kill` row Wardyn writes: a sign-in that failed before it paused the agent could not continue the agent's stopped main process on 3 sweep passes, so the run was killed rather than left stopped. Start the run again. |
| `template_policy_field_refused` | `422`: a template saved as `kind: "policy"` holds something other than run-wide network, credential and access-rule keys under `inline_policy`. Each refused path is a diagnostic. |
| `runner_pools_unavailable` | `501`: this server's store has no runner pool storage (every pool route answers it), or it has no runner wired (a `POST /runs` that names a `runner_pool_id`). |
| `runner_pool_required` / `runner_pool_default_unavailable` / `runner_pool_run_type_not_allowed` / `runner_pool_barrier_not_allowed` | `422` from `POST /runs`, preflight and the policy preview. `runner_pool_required`: no pool was named and neither you nor the organization has a default. `runner_pool_default_unavailable`: the default that applies is gone or switched off, and no other pool replaces it. `runner_pool_run_type_not_allowed`: the pool's limits do not allow the run's `experience`. `runner_pool_barrier_not_allowed`: the pool's `barriers` do not allow the run's barrier. Each message names the next step: choose a pool, another pool, another run type, or another barrier. |
| `runner_pool_not_found` / `runner_pool_stale` / `runner_pool_unavailable` / `runner_pool_member_mismatch` / `runner_pool_no_eligible_member` | The pool routes: `404` no such pool or one you may not use (the two read alike), `409` a stale revision, `422` a switched-off pool as a default, a runner of the wrong kind (organization pools take remote-hosted and organization runners), a self-hosted runner that is not yours, a remote-hosted runner this server is not set up with or whose "Available to people's pools" policy does not admit you (answered alike), or a use policy on a user pool. A pool carries `scope` (`organization` or `user`); a member `kind` (`self_hosted` or `remote_hosted`) and `runner_id` or `remote_runner_id`; a run `remote_runner_id`. `GET /runner-pools` (`ListRunnerPoolsFor`) gives an unavailable pool's first reason: `runner_pool_unavailable`, `placement_denied`, `placement_unavailable`, `runner_pool_no_eligible_member`, `runner_pool_no_online_runner`, `runner_pool_run_type_not_allowed`, or `runner_pool_not_found` (manage-only); a bad `?run_type=` is `400` `runner_pool_invalid`. A launch that names no run type (an older client's shape) into a pool with limits is `422` `runner_pool_run_type_missing`; an unknown value stays `runner_pool_run_type_not_allowed`. A user pool is its owner's alone (`CreateMyRunnerPool`, `UpdateMyRunnerPool`, `DeleteMyRunnerPool`, `AddMyRunnerToPool`, `AddMyPoolRemoteRunner`); anyone else gets the unknown-id `404`. An eleventh own pool is `422`. `CreateRunnerPool` (`{name, limits}`) makes an organization pool; `AddRunnerPoolRemoteRunner` puts a remote-hosted runner in one, and `AddRunnerPoolRunner` an organization runner. `ListRunnerPoolMembers` (`GET /runner-pools/{id}/members`) lists a pool's runners of both kinds to whoever may read the pool: a user pool's owner, anyone who may use an organization pool, and admins and security admins. The defaults are one pool: `PUT /runner-pool-defaults` (`SetOrgRunnerPoolDefaults`) takes `{pool_id, barrier, recording, revision}`, an organization pool only (a user pool is `400`), and replaces the document, so a PUT that leaves `barrier` out clears it; one that leaves `recording` out keeps it, and an empty `recording` is `preferred`: read first (`GetOrgRunnerPoolDefaults`, which answers the document's `revision`) or use `wardyn runner pool org-default set`. `revision` is optional: a PUT that names any other revision is `409` `runner_pool_stale`, and one that omits it or names `0` writes unconditionally; a person's own default has none and refuses it. A person's own (`SetMyRunnerPoolDefaults`) takes `{pool_id}` and refuses `barrier` (`400`). The retired `hosting_type`, `preferred_hosting`, `remote_provided`, `self_hosted` and `executor_id` fields are refused (`400`). `my_runners` lists the caller's runners in a pool; `owner` names a user pool's owner. |
| `runner_pool_queue_full` / `runner_pool_full` | `422`. `runner_pool_queue_full`: one person already has `100` runs waiting for the pool, so `POST /runs` is refused. `runner_pool_full`: `POST /runs/{id}/revive` into a pool at its run limit, or with others waiting. |
| `remote_runner_not_found` | `404` on the remote-hosted runner routes (`GetRemoteRunner`, `UpdateRemoteRunner`, `SetRemoteRunnerPeoplePolicy`, `ClearRemoteRunnerPeoplePolicy`; admins and security admins): no such runner. `ListMyRemoteRunners` lists the ones a person may add to their own user pools. A people-policy write is held for a second human where the server asks for one (`PendingApprovalError`). |
| `image_devcontainer_exclusive` / `image_builder_unavailable` | `POST /runs`' image/devcontainer build validation: `image` and `devcontainer_repo` were both set (the caller's own request shape), or a custom image was requested but this control plane has no image builder wired (a deployment capability neither field can fix) — two different causes, two reasons. |
| `workspace_sources_store_unavailable` / `workspace_sources_list_unavailable` / `workspace_source_not_onboarded` / `workspace_source_mount_not_allowed` | `POST /runs`' resolved-spec workspace-source checks (`validateWorkspaceSources`, `authorizeSpecWorkspaceSources`): no store, the workspace list could not be read, the member-safe mount gate (which `POST /workspaces/{id}/scan` also runs) refuses an onboarded source, or — `workspace_source_not_onboarded` — the source is not onboarded at all. That last one is DELIBERATELY the same reason regardless of which of the two functions answers it or whether the source is a mount or a repo: the message is already byte-identical across all four sites for the cross-member existence-oracle reason `authorizeSpecWorkspaceSources`' own doc comment explains (another member's onboarded source must read exactly like one nobody onboarded), and a distinguishable reason would reopen it on the wire. |
| `runner_capabilities_unavailable` / `confinement_class_conflict` / `confinement_class_unsupported` / `run_grants_require_spire` | `POST /runs`' confinement-class resolution and the SPIRE-only-grants check (invariant 5). `runner_capabilities_unavailable` (`503`) is also what create, Review and the policy preview answer when no runner in the pool can take the run and a remote-hosted runner's facts could not be read: that runner might have taken it. |
| `agent_required` / `agent_not_enabled` / `run_task_reserved` / `confinement_class_unknown` / `task_mode_unknown` / `machine_kind_unknown` / `interactive_start_unknown` / `tool_approvals_unknown` / `tool_approvals_hold_unsupported_agent` / `tool_approvals_hold_interactive_conflict` / `integration_id_retired` / `run_field_too_long` / `run_field_control_char` | `POST /runs`' request-shape validation (`decodeAndValidateCreateRun`): one reason per closed-enum field, plus the agent/text-field checks and the refusal of the retired `integration_id` (Review answers it the same way). |
| `local_folder_invalid` / `local_folder_unsupported` | `POST /runs`: a malformed `local_folder` (400), or a run that cannot hold its agent for it (422; [SSH.md](SSH.md#hold-the-agent-for-a-local-folder)). |
| `computer_folders_off` | `403` on `GET /me/runners/{id}/folders`, and on `POST /runs` with `local_folder.runner_id`: `WARDYN_COMPUTER_FOLDERS` is off. |
| `computer_runner_offline` / `computer_runner_no_folders` | `409` on the same routes: the runner holding the folder is not connected, or does not share folders (older than 0.10, or not `--substrate folders`). |
| `computer_folder_refused` | `422` on `GET /me/runners/{id}/folders`: the runner refused the path (outside its roots, a symlink, or a credential folder). |
| `desktop_transfer_blocked` / `desktop_transfer_too_large` / `desktop_transfer_bad_request` / `desktop_transfer_unavailable` / `desktop_transfer_not_found` / `desktop_transfer_failed` | The run's Files panel (`/runs/{id}/desktop/files`): `403` the run's `desktop` rules block that direction; `413` an upload over the limit; `400` a name that is not plain or an upload with no `Content-Length`; `409` a run with no UI app; `404` no such file in `~/Transfers`; `502` the in-sandbox half failed. |
| `desktop_rules_unreadable` | `503`: the run's desktop rules could not be read, so the desktop's socket and its Files panel (`/runs/{id}/desktop/files`) refuse rather than fall back to the defaults. Try again. |
| `computer_folder_none` / `computer_folder_copy_in_only` / `computer_folder_not_synced` / `computer_folder_not_running` / `computer_folder_paused` / `computer_folder_agent_unstoppable` | `409` on `POST /runs/{id}/computer-folder/bring-back` (the run's own person only; anyone else gets `404`): no folder from your computer, Copy in only, first sync never landed, the run is not `RUNNING`, it is paused, or its sandbox cannot stop the agent first. |
| `harness_inner_walls_looser` / `harness_inner_walls_unmet` / `harness_sandbox_unavailable` | `422` from `POST /runs` and Review: the policy's `harness_inner_walls` is looser than the run's harness walls, or its harness and lane cannot meet it. `harness_sandbox_unavailable`: a policy write names a harness sandbox mode. |
| `run_max_wait_invalid` | `POST /runs`: `max_wait_sec` is below 1. A wait above 604800 (7 days) is cut to it with a warning, not refused. |
| `run_queued` | `PATCH /runs/{id}`: the run is waiting for a slot in its runner pool (state `QUEUED`), so its end is set when it starts and cannot be changed yet. The `201` that stored a run `QUEUED` reports `resources_applied` as of the launch: the start sizes the run again against the limits as they stand then. |
| `run_kill_already_terminal` / `run_kill_state_changed` | `POST /runs/{id}/kill`: the run was already terminal, or moved to another state between the read and the write. |
| `revive_unsupported` | `POST /runs/{id}/revive` (409), and the same value in each refused run's `reason` in `POST /admin/runs/restart`'s `results`: the run's runner substrate cannot replace its proxy in place (`runner.ErrReviveUnsupported`; on Kubernetes the agent pod pins the proxy pod's IP, so the substrate implements no `runner.ProxyReviver`). Nothing changed. Stop the run and start a new one. |
| `workspace_repo_not_admitted` | The repository is not on this deployment's admitted list (`workspace_admission.go`), at create/update and at launch. |
| `repo_url_credentials` | `400`: an http(s) repository URL carries a user name, password or token before its host, or an `ssh://` URL a password, wherever a repository is entered or a stored one is launched or scanned. Give the plain URL and the token as a `git_pat` grant. `git@host:path`, `ssh://user@` and the Azure DevOps Clone-button form `https://<org>@dev.azure.com/<org>/...` are accepted. Stored runs, workspaces, sources, policies, presets, templates, governance profiles and launch intents are served without it. |
| `workspace_envcode_no_local_dir` / `workspace_envcode_no_profile` | `POST /workspaces/{id}/env-as-code/write` only: no `local_dir` source. That route and `GET /workspaces/{id}/env-as-code`: no scanned profile. |
| `workspace_providers_invalid` | The operator-only `PUT /api/v1/workspace-providers` (deployment-wide, not a per-workspace route): the submitted block fails validation. Its stale-`If-Match` arm shares `site_config_stale` below — the identical ETag cause, on the same underlying site-config document. |
| `workspace_request_invalid` / `workspace_ssh_sources_not_ready` / `workspace_sources_not_allowed` | `POST/PUT /workspaces`: the request body fails validation, an SSH-remote source names a secret not yet stored, or the caller's own `local_dir` sources fail the member-safe mount gate; on `POST /workspaces/{id}/env-as-code/write`, the stored `local_dir` source is, or now resolves to, a host path the mount deny-list refuses (nothing is written). |
| `workspace_delete_active_run` | `DELETE /workspaces/{id}`: the workspace is in use by a still-active run. |
| `workspace_name_taken` | `POST /workspaces`, `PUT /workspaces/{id}`: the workspace's owner already has another workspace of this name (names are unique per owner; operator-owned workspaces are one owner). Pick another, or rename the other one. `POST /workspaces/{id}/reassign`: an operator-owned workspace already has this name. |
| `workspace_approved_egress_denied` | `PUT /workspaces/{id}/approved-egress`, `400`: a host is on the workspace's denied egress list, and a deny beats an approval at the proxy. Remove it with `PUT /workspaces/{id}/denied-egress` first; the approval never removes it for you. |
| `workspace_update_conflict` | `PUT /workspaces/{id}`: the workspace changed (a run claimed it, a recording or a build landed) between the read and the save, so nothing was written. Reload it and apply the edit again. |
| `branch_list_rate_limited` | `GET /workspaces/{id}/branches`: the person passed `30` lists a minute (burst `10`, per wardynd replica), answered `429` before anything is read. Not audited; the admin token is exempt. |
| `groups_snapshot_stale` | `PUT/POST /governance/*` and the user-drive resolver: the caller's group-membership snapshot is missing or was truncated at sign-in, so a group-keyed governance profile cannot be resolved. The SAME value as authz's own registered reason ([`internal/authz/registry.go`](../internal/authz/registry.go)) — a literal in `reasons.go` (the docs⟷reasons.go guard only reads literals), tied to authz's constant by a documented `TestNoAdHocAuthz` exception rather than a reference or a second copy invented for this package. |
| `egress_baseline_invalid` / `egress_baseline_stale` | `PUT /governance/egress-baseline`: a host or suffix is invalid, or `If-Match` is stale. |
| `credential_delivery_invalid` / `credential_delivery_unavailable` / `credential_delivery_stale` | `PUT /governance/credential-delivery`: an unknown or fixed class, a mode outside the class's bounds, or `require_posture` without `runner_resident`; under `by_trust`, an unknown trust class, an override that sets `via_org` or overrides a class delivered `via_org`, or `require_posture` under `org_cluster`; or the store is not Postgres; or `If-Match` is stale. `GET` returns `buildable_modes` beside `policy`: per class, the modes a local run can be delivered under today. A stored mode outside them refuses at every door. |
| `site_config_request_invalid` / `site_config_artifact_override_invalid` / `site_config_integrations_via_own_route` / `site_config_invalid` / `site_config_stale` / `site_config_egress_via_own_route` | `PUT /site-config`: the body did not decode, a legacy artifact-override field fails validation, integrations were named inline instead of through their own endpoints, the submitted config fails one of the agent/model-provider/default-provider validators, `If-Match` is stale, or the body changes the egress baseline (the `egress` block or the baseline-marked `internal_hosts`), which only `PUT /governance/egress-baseline` writes (`PutSiteConfigResult` sends neither, so a captured document restores, and `EgressBaselineIgnored` reports a baseline it did not apply; `PutSiteConfig` sends the document as given and gets this refusal; a restore is still refused when the security tier marked an internal host the older capture does not list: lift the mark first). `site_config_stale` is shared by every `PUT` that checks `If-Match` against this same document's ETag: `PUT /agent-providers`, `PUT /model-providers` and `PUT /workspace-providers` all answer it too, one reason for one cause regardless of which sub-block the write targeted. |
| `expected_principal_mismatch` | `412`: any write carrying the console's `X-Wardyn-Expected-Principal` header whose person is not the one now signed in (another tab signed in as someone else after the draft was composed). Nothing is written; reload and retry. Writes without the header (the CLI, scripts) are not checked. |
| `site_config_probe_request_invalid` / `site_config_probe_url_invalid` / `egress_redirect_from_required` / `egress_redirect_not_found` | `POST /site-config/probe-proxy` and the egress-redirect edit routes. |
| `governance_profile_request_invalid` / `governance_ceiling_invalid` / `governance_profile_name_conflict` / `governance_profile_in_use` / `governance_assignment_invalid` / `governance_preview_claims_invalid` | `PUT/POST /governance/profiles` and `/governance/assignments`, and the preview routes — `governance_preview_claims_invalid` is shared with the user-drive naming preview (`user_drives_preview.go`), which feeds the same `normalizeGovernancePreviewClaims` validator. `governance_profile_in_use` is a `409` when a profile is still assigned or still the base of another profile (the body names those profiles). A composed profile read by an SDK that predates composition shows `ceiling: {}` and `limits: {}` and ignores `effective`; a `PUT` from it omits the composition fields, which are kept, so it cannot flatten the profile. |
| `governance_overlay_invalid` | `PUT/POST /governance/profiles` (400): a composed profile's `overlay` or `overlay_limits` does not decode, names something its base does not permit (a domain outside the base, a method the base excludes, `allow_all_egress` on a base without it, a looser bound, a grant the base does not hold), would mean "everything" by being empty (`allowed_methods: []`), names a base that does not exist, or sits beside a non-empty `ceiling` or `limits` (a composed profile states its policy only in the overlay). |
| `governance_profile_cycle` | `PUT/POST /governance/profiles` (409): the write would make a profile, through its base, its own base. |
| `governance_profile_depth` | `PUT/POST /governance/profiles` (409): the write would put a profile, or one built on it, more than three profiles deep. |
| `governance_overlay_unsatisfiable` | `PUT/POST /governance/profiles` (409): a base change would leave a profile built on it with a policy nothing satisfies (the body names that profile). The SAME value as authz's own registered launch refusal ([`internal/authz/registry.go`](../internal/authz/registry.go), `403` on create, preflight and every live door), a literal in `reasons.go` for the same reason `groups_snapshot_stale` is. |
| `governance_second_human_local_mode` | `WARDYN_GOVERNANCE_SECOND_HUMAN` cannot be enforced in local mode (`503`): nobody is authenticated there, so the proposer and the approver are both client-supplied. Every covered governance write (profile, assignment, grant, enforcement, availability, user-type priority, role mapping, egress baseline) and every `/governance/changes` approve or reject answers it. |
| `governance_change_pending` | A covered write (`/governance/profiles`, `/governance/assignments`, `/permissions/grants`, `/permissions/enforcement`, `/permissions/availability`, `PUT /user-types/{id}`, `/access/mappings`, `PUT /governance/egress-baseline`) with `WARDYN_GOVERNANCE_SECOND_HUMAN` on (409): a live change already waits for approval at this target; the message names it. Approve or reject that one first, or let it expire. |
| `governance_change_stale` | `POST /governance/changes/{id}/approve` (409): the target (or, for an assignment, the profile it points at) or the deployment default changed since the change was proposed, so approving would apply something other than what the reviewer saw. Nothing was applied and the change is now `stale`; propose it again. |
| `governance_change_not_pending` | `POST /governance/changes/{id}/approve` or `/reject` (409): the change was already applied, rejected, expired or marked stale. A change past its expiry is moved to `expired` by the request that finds it. |
| `governance_change_not_found` | `GET /governance/changes/{id}` and the approve and reject routes (404): no such change, or one of a kind the caller may not approve. |
| `governance_change_state_invalid` | `GET /governance/changes?state=` (400): not one of `pending`, `applied`, `rejected`, `expired` or `stale`. |
| `secret_name_invalid` / `secret_name_reserved` / `secret_owner_param_refused` / `secret_body_invalid` / `secret_value_too_short` / `secret_cap_reached` / `secret_not_found` | `PUT/DELETE /secrets/{name}` (and `/me/secrets/{name}`, `/shared-secrets/{name}`): the name fails the secret-name format or is reserved, `?owner=` was sent on a write, the body is malformed, the value is below the mask minimum, the owner already holds the maximum number of secrets, or (the admin-only `?owner=` delete arm) that owner holds no secret by that name. |
| `people_store_unavailable` / `person_principal_invalid` / `person_principal_reserved` / `person_email_invalid` / `person_collision` / `person_email_taken` | `POST /people` (operator/security-tier only). |
| `people_list_param_invalid` | `GET /people` (security tier): `state` is not `active` or `deactivated`, or `cursor` is not one a previous page returned. A malformed `limit` answers `invalid_limit_param`, as every list does. |
| `person_token_mint_removed` | `POST /people/{principal}/tokens` (securityOps, kept mounted): refused for every caller, 403, whether the person exists or not (#1477). No role creates a token that acts as another person; they sign in and create their own (`POST /me/tokens`). The two shared no-store guards below still answer first. |
| `mint_no_human` | `POST /people/{principal}/tokens`'s OWN "needs a signed-in human" sentence — NOT shared with `api_token_no_human` below: same shape, different door, different wording, so a different reason. It answers before `person_token_mint_removed`. |
| `api_token_from_api_token` | Shared, byte-identical, between `POST /people/{principal}/tokens`, `POST /api/v1/tokens` (self-service), `POST /me/invitations/{id}/claim` and the sponsor's `POST /me/service-identities/{name}/tokens`: an API token may not mint another. |
| `cli_sign_in_rate_limited` | `429` from `POST /cli/sign-ins`, `POST /cli/sign-ins/{id}/token` and `POST /cli/sessions/{id}/refresh`: too many from this address, or too many refreshes of one session. |
| `cli_sign_in_invalid_request` | `400` from `POST /cli/sign-ins`: the body is malformed; the message names the field. |
| `cli_paste_sign_in_off` | `403` from `POST /cli/sign-ins`: a paste-back sign-in while `WARDYN_CLI_PASTE_SIGN_IN` is off. |
| `cli_file_store_off` | `403` from `POST /cli/sign-ins`: the CLI reports the file store while `WARDYN_CLI_FILE_STORE` is off. |
| `cli_sign_in_pending_cap` | `429` from `POST /cli/sign-ins`: too many sign-ins wait for approval deployment-wide. |
| `cli_sign_in_not_found` | `404` from `GET /me/cli-sign-ins/{id}`, `/approve` and `/deny`: no such sign-in. |
| `cli_sign_in_used` | `410` from the same routes: the sign-in was already approved, denied or redeemed. |
| `cli_sign_in_expired` | `410` from the same routes: the sign-in is older than 10 minutes. |
| `cli_admin_needs_session` | `409` from `POST /me/cli-sign-ins/{id}/approve`: a `wardyn login --admin` whose computer holds no live CLI sign-in of the approver with that device key. |
| `cli_sign_in_invalid` | `401` from `POST /cli/sign-ins/{id}/token`: the code, the verifier, the device proof or the sign-in is not valid. One answer for all, so it tells a caller nothing. |
| `cli_session_invalid` | `401` from `POST /cli/sessions/{id}/refresh`: the session is unknown, ended, expired or idle, was cut or reused, or the proof is bad. Sign in again. |
| `cli_session_not_found` | `404` from `DELETE /me/cli-sessions/{id}`: no live CLI session of the caller has that id. |
| `token_lookup_unavailable` | The API-token and delegated-token authentication middlewares' shared shape: the revocation store could not be read to authenticate the bearer. |
| `invitation_target_invalid` / `invitation_target_unknown` / `invitation_replaces_invalid` / `invitation_ttl_invalid` / `invitation_cap_reached` | `POST /people/{principal}/invitations`: the target is reserved or unknown, `replaces_token_id` is not their admin-minted token, a lifetime is out of range, or ten are pending. |
| `invitation_list_param_invalid` | `GET /invitations`: `state` is not `pending`, `claimed`, `expired` or `revoked`. |
| `invitation_not_found` / `invitation_not_pending` | `POST /me/invitations/{id}/claim`: someone else's or a missing id (one body); `invitation_not_pending` is the target's own, with `state`. `DELETE /invitations/{id}` answers `invitation_not_found` unless pending. |
| `service_identity_name_invalid` / `service_identity_name_taken` / `service_identity_description_invalid` / `service_identity_user_type_unknown` / `service_identity_patch_empty` | `POST /service-identities` and `PATCH`: the name is not `^[a-z][a-z0-9-]{2,62}$` or a principal-keyed table already holds it (`409`), the description is over 200 characters, the user type does not exist, or the `PATCH` names nothing. |
| `token_move_invalid` | `POST /tokens/{id}/move`: the token is revoked, expired or not minted by an admin for someone else, or `service_identity` is missing, unknown or retired. |
| `service_identity_sponsor_invalid` | The sponsor is not exactly the principal of a bound, non-deactivated person; an email or `entra:` alias is refused. |
| `service_identity_not_found` / `service_identity_inactive` | `/me/service-identities/{name}/tokens`: another person's or a missing name (one body); `service_identity_inactive` is the sponsor's own `409` on a suspended or retired identity, or when its sponsor's session is not live, or with no revocation store, and the answer to a retired service's run renewing (`403`) or reviving (`409`). |
| `needs_sso` | The service-identity routes in local mode, which has no person to sponsor one (`409`). |
| `api_token_no_human` / `api_token_from_delegated_token` / `api_token_member_mode_mint` / `api_token_name_invalid` / `api_token_ttl_invalid` / `api_token_cap_reached` | `POST /api/v1/tokens` (self-service mint) and `DELETE`; the invitation claim and the sponsor's service-token mint answer the mint guards with the same sentences, and `api_token_cap_reached` (`422`) there means `20` live tokens on the identity for the sponsor's mint, but on an invitation claim the cap is the claimer's own live tokens. |
| `owner_ambiguous` / `owner_unresolved` | The shared owner/principal resolver shape: more than one known principal matches a name, or none does. `secrets.go`'s admin `?owner=` resolution (`ownerRefusalReason`) and `sshkeys_admin.go`'s `resolveSSHKeyOwner` (also reached from `POST /sessions/revoke`'s SSH-key cutoff) both answer from this one vocabulary rather than each inventing its own. |
| `sessions_revoke_param_invalid` | `POST /sessions/revoke`: the body must set exactly one of `sub`/`all`, and `sessions_only` (browser sessions end; API tokens and SSH keys keep working) goes with `sub`, not `all`. |
| `identity_not_found` | `POST /admin/identities/{id}/unbind` named no identity row. |
| `identity_not_bound` | `POST /admin/identities/{id}/unbind` found the row with no principal to clear, or bound to another one by the time it was locked. Read it again. |
| `identity_not_rebindable` | `POST /admin/identities/{id}/unbind` named an identity with no object id (not an Entra person keyed by tenant and object id); unbinding it would orphan it. |
| `identity_principal_in_use` | `POST /admin/identities/{id}/unbind` found the principal it would release still holding an API token, SSH key, active run, claimed runner or unspent runner registration token, still sponsoring a service identity, or still named by a pending invitation. Revoke, end or retire them, then retry. |
| `identity_deactivated` | A run, console session, API token, SSH key, stored credential or runner registration token was refused (a session cookie gets `401` "Your access to Wardyn has been turned off") because its owner's identity is deactivated, or was suspended after the caller was admitted. Sign in again; if that is refused too, ask the Wardyn admin. `409` from `POST /admin/identities/{id}/unbind`: the identity to unbind is itself deactivated (or purged), and unbinding it would not let the person back in; the admin calling unbind has nothing to sign in again for. |
| `sso_not_configured` / `access_role_map_value_invalid` / `access_mapping_target_invalid` / `access_email_mapping_disabled` / `access_lockout` / `access_unknown_user_type` / `access_preview_no_session_claims` | `GET/POST /access` (role-mapping admin, operator-only). |
| `capability_grant_invalid` / `capability_kind_unknown` / `capability_enforcement_stale` / `availability_kind_not_restrictable` / `availability_target_invalid` / `availability_restricted_required` / `availability_only_empty` | `POST /permissions/grants` and the capability-availability routes. |
| `delegation_store_unavailable` / `delegate_name_invalid` / `delegate_client_id_invalid` / `delegate_client_id_is_portal` / `delegate_group_invalid` | `POST /admin/delegates` (operator-only portal delegate registration). |
| `integration_invalid` / `integration_not_found` / `setup_onboarding_store_unavailable` | `PUT/DELETE /setup/integrations/{id}` and `POST /setup/onboarding-complete` (operator-only). |
| `component_cap_reached` / `component_name_conflict` / `component_not_found` | `POST/PUT/DELETE /me/components[/{id}]` and `PUT/DELETE /components/{id}`: the owner already holds the maximum number of saved components (32 per person, 256 for the organisation), another component of the same owner already has the name (compared without case), or no such component of the caller's exists (another person's row and an absent id answer alike). These routes also answer `component_definition_invalid` (the definition fails the shared validator or names a secret Wardyn manages), `component_secret_missing` (an organisation component shares a secret the operator has not stored) and `component_store_unavailable`, documented with the run gate below. |
| `ssh_key_invalid` / `ssh_key_requires_human` / `ssh_key_cap_reached` / `ssh_key_revoked_session` / `ssh_key_registration_refused` / `ssh_key_fingerprint_invalid_encoding` | `POST/DELETE /me/ssh-keys` (self-service). `ssh_key_registration_refused` is DELIBERATELY generic (THREAT-MODEL.md): it never confirms whether the key is already registered, or by whom — a distinguishable reason here would be the same key-squatting reconnaissance oracle the shared sentence already refuses to open. |
| `ssh_key_admin_principal_invalid_encoding` / `ssh_key_admin_principal_required` | `DELETE /people/{principal}/ssh-keys` (security-tier). |
| `user_drive_request_invalid` / `user_drive_allocated_conflict` / `user_drive_slug_conflict` / `user_drive_home_namespace_conflict` / `user_drive_name_conflict` / `user_drive_still_allocated` / `user_drive_grant_invalid` / `user_drive_grant_conflict` | `POST/PUT/DELETE /user-drives` and its allocation-grant sub-route (operator-only). |
| `user_drive_rehome_list_unavailable` / `user_drive_rehome_invalid` | The drive list could not be read to run the nesting check (`POST/PUT /drives`) or the re-home guard (`PUT /drives/{id}`), or the guard refuses an unconfirmed identity-field change on an allocated drive. |
| `user_drive_ceiling_unavailable` / `drives_disabled` / `user_drive_size_refused` | `POST/PUT /drives`: the drive-size ceiling could not be read, or a `host_path` share did not answer in time; the org switch is off (shares `drives_disabled` with the launch-time door below); or the size override exceeds the ceiling. |
| `user_drive_reclaim_invalid` / `user_drive_reclaim_subject_ambiguous` / `user_drive_not_reclaimable` / `user_drive_reclaim_unsupported` / `user_drive_reclaim_conflict` | `POST /user-drives/{id}/reclaim` (operator-only). |
| `user_drive_reclaim_failed` / `user_drive_reclaim_no_directory_name` | The reclaim object resolver's own two causes: a store read failure, or the allocation resolving to no directory name at all. |
| `user_drive_preview_no_claims` / `user_drive_denied_by_profile` | The user-drive naming and bind-failure preview routes (operator-only, mirrors the launch-time resolver's own verdict so an admin reads the SAME sentence a member would). |
| `no_allocation` / `paused` / `runner_cannot_mount` / `backend_elsewhere` / `ceiling_moved` / `home_missing` / `home_unreadable` / `share_unreachable` / `read_only` / `drives_disabled` | `POST /runs`' drive-mount refusals (`seedRequestDrives`, `refuseDrive`), the closed set the `wardyn_drive_refusals_total` metric and its WARN line use. The runner-unavailable 503 and a caller-cancelled probe (`silent`) stay unclassed on purpose. |
| `user_type_unknown` / `unmountable` | The user-drive resolver's own closed enum ([`internal/api/user_drives_resolve.go`](../internal/api/user_drives_resolve.go)) members that reach `writeDriveError`'s wire body. Two siblings in that same enum, `unavailable` and `governance_unavailable`, never leave `GET /me`'s own field — no route sends them via `writeErrorReason` — so they are deliberately UNDOCUMENTED here rather than given a row that claims a wire presence they don't have; `unavailable` would also collide with `ADOEntraFailure`'s own reason of the same name above. |
| `drive_subject_unsupported` | `POST /runs`, `POST /drives/preview` (`422`) and the admin reclaim (`400`): the allocation exists, but no directory name is derived from this person's sign-in name under the drive's home template. An admin sets a home override on the allocation. |

#656 slice 3 converts the remaining route families — `run_*.go`'s per-run
actions, the UI gateway, device federation, and every other lane still
sending `error` alone — completing the sweep this issue tracks:

| Reason | Meaning |
|---|---|
| `local_mode_not_owner` / `revive_unsupported_deployment` / `revive_unsupported_runner` / `revive_bulk_cannot_start_agent` / `revive_already_in_progress` / `revive_mint_identity_failed` / `revive_encode_config_failed` / `revive_pull_image_failed` / `revive_claim_failed` / `revive_run_changed` / `revive_proxy_kept_current` / `revive_proxy_replace_failed_lost` / `revive_agent_start_failed_lost` / `revive_recovery_unresolved` / `revive_substrate_unreadable` / `revive_config_not_stored` / `revive_config_unreadable` / `revive_config_does_not_load` / `revive_not_running` / `revive_ended_files_gone` / `revive_past_end` / `revive_ended_task_run` / `revive_reboot_agent_stopped` / `revive_ended_agent_stopped` / `revive_unknown_lost_reason` / `revive_agent_status_unreadable` / `revive_config_run_mismatch` / `revive_ceiling_denies_git_broker` / `revive_ceiling_denies_azure_cli` / `revive_owner_authority_unreadable` / `revive_admin_restart_count_invalid` / `revive_proxy_window_store_unavailable` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`): one reason per distinct revive-refusal cause. `local_mode_not_owner` is the same literal the refusal's own audit row already carried; `revive_unsupported_runner` covers three arms that all answer the identical "this runner cannot replace a proxy" fact. `revive_recovery_unresolved` (`503`) is a revive that failed after its claim and then could not write the run's lost mark: the run is not recorded as lost (a bulk restart's `lost_again` is `false`), its proxy is stopped and its broker credentials revoked, and a later sweep recovers it. The `_lost` reasons above say the run was put back to lost: the lost mark was written, or the run was left alone because another writer had already moved it, or it was torn down because it cannot be kept; only when the write itself fails is the answer `revive_recovery_unresolved`. |
| `revive_live_too_soon` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`), `429`: the run is live and this `wardynd` process started replacing its proxy less than a minute ago (a revive that left the proxy untouched, `revive_proxy_kept_current`, does not count). The bound is per process, so each replica allows one a minute. A revive of a lost run is never bounded. |
| `revive_containment_recovering` | `POST /runs/{id}/revive` and the admin bulk restart, `409`: the run's runner contained it while offline. A recovery is queued; revive the run again once the runner has reconnected. |
| `run_capacity_store_unavailable` | `GET /admin/runs/capacity`, `501`: the configured store cannot report fleet capacity. The Postgres store always can; only a test double or an alternative store lacks it. |
| `profile_unreadable` / `profile_gone` / `capability_agent` / `capability_workspace` / `capability_model_provider` / `capability_policy` / `capability_workspace_provider` / `capability_component` / `capability_unknown` / `model_credential_erased` / `model_provider_disabled` / `model_provider_gone` / `component_gone` / `owner_unverifiable` | `ownerRefusal`'s own reason values (`run_owner_authority.go`): the shared re-check a revive and a run-end-extension both run over the owner's launch-door capabilities, governance profile, model provider and model credential. `model_provider_disabled` covers both an integration that supplies the run's credential and the run's own model provider being turned off; `model_provider_gone` is that provider deleted (or re-created under the same id); `component_gone` is an organisation's component the run was launched with, deleted since (409, for the owner and for an admin). The `capability_*` values name the SAME six capability kinds `capabilities.go` enumerates; `capability_unknown` is a defensive fallback outside that closed set. `owner_unverifiable` is `extendRefusal`'s own bucket for three arms that all answer "the owner's authority could not be confirmed right now". |
| `run_end_wait_neither_field` / `run_wait_budget_not_a_number` / `run_end_wait_already_finished` / `run_end_wait_files_gone` / `run_end_wait_max_lifetime` / `run_end_wait_store_unavailable` / `run_limits_gate_denied` / `run_end_no_end_not_allowed` / `run_end_must_be_future` / `run_wait_budget_too_small` / `run_end_wait_changed` | `PATCH /runs/{id}`'s end-time and wait-budget door (`run_end_wait.go`). It can also answer `lock_unavailable` (`503` with `Retry-After`). `run_limits_gate_denied` is shared across all four fields the `user_changes_limits` gate can block — the same gate regardless of which field triggered it. |
| `run_end_conflicting_fields` / `run_end_after_too_small` / `run_end_after_too_large` | `POST /runs`' create-time end (`run_end_create.go`): `ends_after_sec` and `no_end` together, `ends_after_sec` below 1, or past 2147483647. A `no_end` the pool, the deployment or the person's governance does not allow answers `403` `run_end_no_end_not_allowed` or `run_limits_gate_denied`, the same reasons the end-time `PATCH` gives; an `ends_after_sec` past a limit is cut to it and the `201` warns, never refused. |
| `workspace_not_found` / `user_drive_not_found` / `user_drive_allocation_not_found` / `ssh_key_not_found` / `preset_not_found` / `policy_not_found` / `capability_grant_not_found` / `run_not_found` / `governance_assignment_not_found` / `governance_profile_not_found` / `enrolment_token_not_found` / `device_not_found` / `delegate_not_found` / `api_token_not_found` / `role_mapping_not_found` | `notFoundIf`'s (`helpers.go`) per-resource-kind 404s, one for each of its ~27 call sites across the package. `run_not_found` is the same literal `auditRenewDenied` already wrote for a renewal on a run that no longer exists; `preset_not_found` is the admin preset-management route's own 404, distinct from `preset_unknown` (a run naming an unknown preset at launch); `governance_profile_not_found` is `governance.go`'s own by-id GET/PUT/DELETE, distinct from `profile_gone` above (a run's already-captured profile going missing later). |
| `invalid_id_param` / `missing_run_claims` / `run_id_mismatch` / `workspace_store_unavailable` / `scan_upload_run_not_found` / `scan_upload_not_governed` / `scan_upload_wrong_task` / `workspace_requirements_invalid` / `workspace_approved_egress_invalid` / `workspace_approved_egress_dead_host` / `workspace_denied_egress_invalid` / `workspace_llm_cred_invalid` / `request_body_too_large` / `request_body_unreadable` | `helpers.go`'s own shared foundational refusals, reused by dozens of call sites across the package since the cause is identical regardless of which handler hit it: a `{param}` path segment that fails to parse as a UUID, the internal run token's claims failing to read, a scan upload's own 3-arm refusal (`scanresult.go`'s sole caller), `scopedWorkspaceWrite`'s 4 validators (one cause bucket per route; `workspace_approved_egress_dead_host` is its own reason because the remedy differs — use the already-routed door, not fix a malformed domain), and `readCappedBody`'s too-large/unreadable split. |
| `ado_capability_unknown` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, and a launch preset's `inline_policy` (`/api/v1/presets`): the spec's `azure_devops_capabilities` names something the Azure DevOps capability catalogue cannot grant. Its own reason rather than the door's `policy_request_invalid` / `inline_policy_invalid` bucket. |
| `ado_capabilities_none_permitted` | `POST /runs/preflight` and `POST /runs/policy-preview`: a member's `azure_devops_capabilities` leaves nothing standing on the per-person Azure DevOps lane (none of it is in the provider row's default profile or their governance profile's list). Review's mirror of the same-named refusal launch gives at dispatch. |
| `git_pat_narrowing_needs_broker` | `POST /runs/preflight` and `POST /runs/policy-preview`, the failure of a launched run, and `409` from `POST /runs/{id}/revive` and the admin restart of a run launched while the broker was on: a `git_pat` grant sets `repos`, `access` or `api` while the PAT broker is off (`WARDYN_GIT_PAT_BROKER`). Only the broker enforces the narrowing; with it off the PAT is resident in the sandbox and nothing narrows it, so the run is refused. |
| `git_pat_narrowing_ssh_conflict` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, a launch preset's `inline_policy` and `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access` or `api` and the same policy or run holds an `ssh_key` for the same forge (`github.com` and `ssh.github.com` are one forge). SSH is a second push path the broker cannot see, so the narrowing would not bind. |
| `git_pat_narrowing_unsupported_host` | `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access`, `api` or `forge` for a host another lane serves and that lane does not read those fields: an Azure DevOps host or one the run's Azure DevOps gate covers, or the host of a forge the run is GitHub-brokered for (its PAT is withheld). |
| `git_pat_api_forge_disabled` | `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `api` for `bitbucket_server` while `WARDYN_GIT_PAT_API_BITBUCKET_SERVER` is off on this deployment. Policy and governance writes refuse the same grant with `400`. |
| `git_pat_broker_off_resident` | `POST /runs/preflight` and `POST /runs/policy-preview` (a run no pool places), the failure of a launched run, and `409` from `POST /runs/{id}/revive` and the admin restart: 0.10.0, the PAT broker is off (`WARDYN_GIT_PAT_BROKER`), so a `git_pat` grant's PAT would be minted into the sandbox, and the run is a member's (on revive, one not launched with the admin token or in local mode, unless its owner revives it while their role is an operator's), or `WARDYN_GIT_PAT_BROKER_OFF_REFUSED` is set. A `runner_resident` grant, an Azure DevOps host and a GitHub-brokered forge are not refused. An admin whose runs `WARDYN_GOVERN_ADMIN_RUNS` governs is still refused on revive (`409`). |
| `policy_request_invalid` / `policy_secret_refs_invalid` / `policy_name_conflict` | `PUT/POST/DELETE /api/v1/policies`: the create/update body fails `decodePolicyRequest`, a secret reference in the spec fails shape validation, or a policy by that name already exists. |
| `model_provider_id_invalid` / `model_provider_not_applicable` / `model_provider_no_block_configured` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` model-provider authorization (`run_provider_authorization.go`), the three field-validation arms outside `writeProviderRefusal` (which always carries its own reason, either the credential-refusal's audit reason or the generic `model_provider_unavailable`): `model_provider` is not a plain provider id, was set on a run that calls no model, or was named but this deployment has no model providers. `integration_id` is refused earlier, unconditionally (`integration_id_retired`), before this door is reached. |
| `component_ref_invalid` / `component_store_unavailable` / `component_definition_invalid` / `component_secret_not_owned` / `component_secret_missing` / `component_host_denied` / `component_host_serves_model` / `component_host_collision` / `component_resident_delivery_denied` / `component_builtin_lane_off` / `component_builtin_access_above_ceiling` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` component gate (`components_run.go`), over the request's `components[]`: the list is malformed (400); this store cannot record components (501); or one component breaks a rule (422) — its definition, a secret that is not the caller's own, a secret the organisation provides and has not stored (the preview alone does not refuse this one), a host that is blocked, that serves a model or that already carries a credential on the run, or env/file delivery the organisation turned off; or a built-in GitHub component whose provider has its GitHub App lane off, or whose `access` is above the ceiling. A built-in GitHub component is also refused `400` `component_ref_invalid` when its grant fails the broker's scope-shape rule, or when it would give the run a brokered GitHub grant beside an `ssh_key` or `git_pat` grant for the same forge (a brokered forge is single-lane). Who may attach a component is the authorization reasons `capability_component` and `capability_feature`. |
| `cli_no_github_repo` | `POST /runs` and `/runs/preflight` (`stepGitHubCLIRepo`, `runs_cli_github.go`): a GitHub CLI row is attached and the run has no GitHub repository on the App lane for it to act on (422). The policy preview reports it as the row's `reason` instead. |
| `component_cli_aws_inject_off` / `component_cli_sign_in_required` / `component_cli_aws_bedrock_sign_in` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for an AWS CLI component (422): `WARDYN_AWS_SSO_PROXY_INJECT` is off on this deployment, which would put the person's AWS sign-in in the sandbox, so the row is refused at every door rather than served that way; the caller has not signed in for the row (`POST /components/{id}/sign-in`); or the run's model, or an included harness's, comes through an Amazon Bedrock sign-in, and a sandbox holds one AWS profile. The preview does not refuse the second one: it lists a `sign_in` setup item instead. An Azure CLI component answers `component_cli_sign_in_required` the same way when the caller has not signed in to Azure for it (`GET /components-entra/signin?component=<id>`). |
| `component_cli_azure_no_entra` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for an Azure CLI component (422): this deployment's console does not sign people in with Microsoft Entra, and only that application's sign-in can be bound to a person. The row is refused at every door, since the other way to serve it is a token in the sandbox. |
| `cli_sign_in_not_aws` / `cli_sign_in_preview_blocked` / `cli_sign_in_capture_changed` | `POST /components/{id}/sign-in` (`cli_aws_signin.go`): the component is not an AWS CLI row (422), or an admin previewing a person's view would capture into their own identity (409). The third is the AWS sign-in sandbox's upload (409): the row was removed, or its portal, region, account or role changed, while the sign-in was open. A component the caller may not attach answers as the run doors do, `capability_component`. |
| `cli_sign_in_not_azure` | `GET /components-entra/signin?component=<id>` (`cli_azure_signin.go`), the Azure CLI component's Entra sign-in: the component is not an Azure CLI row (422). The door shares the Azure sign-in doors' other answers: `azure_sign_in_no_session` (403), `azure_sign_in_unconfigured` (404), `cli_sign_in_preview_blocked` (409), and through the shared callback `azure_callback_cookies_invalid`, `azure_callback_missing_code` and a redirect to `/?azure_cli_signin_error=<code>&component=<id>` with the Azure sign-in callback's codes. |
| `credential_host_collision` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for any run (422, `credentialHostRefusal` in `runs_dispatch_components.go`): two of the run's credentials are bound to one host, and a host carries only one — two `api_key` grants, or an `api_key` grant on a host that a token-bearing redirect, the Azure DevOps lane or a `git_pat` API grant also credentials. The body names no host. Also the `reason` on a failed run's `run.create` row when the collision only appears at dispatch; that row carries the `grant_ids` involved and never the host. `component_host_collision` is the same rule for a component's own header. |
| `run_title_store_unavailable` | `PATCH /runs/{id}/title` (`run_title.go`): this store cannot rename a run. |
| `run_inspect_no_runner` / `run_inspect_terminal` / `run_inspect_no_sandbox` / `run_inspect_paused` / `run_inspect_agent_stopped` / `run_inspect_exec_stream_unsupported` / `run_resources_read_failed` / `run_files_no_exec_session` | `GET /runs/{id}/resources` and `GET /runs/{id}/files` (`run_resources.go`, `run_files.go`): the two widgets read the identical run-state facts and share a reason per cause rather than each inventing its own synonym. |
| `run_output_tail_invalid` / `run_output_interactive` / `run_output_not_captured` / `run_output_off` / `run_output_not_kept` / `run_output_expired` / `run_output_erased` | `GET /runs/{id}/output` (`run_output.go`): `?tail=` is not a positive number of bytes (`400`); the run is interactive and has no pane snapshot to serve (`409`): with recording off the sentence says nothing was kept from this session, and with recording on it points at the recording; the runner exposes no direct stdout and no authorized recording-derived output is available (`409`); this deployment keeps none, or refuses stored rows (`WARDYN_EXEC_OUTPUT_TAIL=off`, `409`); no output is kept for the run — one that finished before output was persisted, or one still being captured, which a read a moment later serves (`409`); a sign-in run is interactive and answers `run_output_interactive`; the in-memory tail outlived `WARDYN_EXEC_OUTPUT_TAIL_TTL`, or the run ended longer ago than `WARDYN_RUN_OUTPUT_RETENTION_DAYS` and its row was deleted (`410`); the run's output was erased (`404`). |
| `run_sign_in_not_aws` / `run_sign_in_unreadable` | `GET /runs/{id}/sign-in` (`run_sign_in.go`): the run is not an AWS sign-in run (`409`); the sign-in pane, or the capture's audit row, could not be read in time — a read a moment later may answer (`503`). |
| `run_resume_not_running` / `run_resume_failed` | `POST /runs/{id}/resume` (`run_pause.go`): the run is not in a resumable state, or thawing it for exec failed. |
| `internal_decision_log_invalid` / `groundtruth_batch_invalid` / `groundtruth_batch_too_large` / `groundtruth_action_not_kernel` / `groundtruth_write_failed` / `internal_approval_request_invalid` / `unsupported_internal_approval_kind` / `missing_requested_scope` / `reserved_scope_key` / `internal_approval_count_unavailable` / `internal_approval_cap_reached` / `broker_not_configured` / `mint_grant_id_required` / `brokered_forge_single_lane` / `brokered_forge_single_lane_unverifiable` / `grant_run_mismatch` / `grant_not_found` / `grant_requires_spire` / `grant_run_revoked` / `grant_not_approved` / `grant_ref_ruleset_required` / `grant_kind_unsupported` / `run_renew_store_unavailable` / `run_renew_read_failed` / `run_renew_stamp_failed` / `run_renew_lease_busy` / `internal_liveness_read_failed` | `POST /internal/*` (`internal.go`, `internal_live_run.go`): the sidecar/proxy surface, not the member-facing API. Most values are already the exact strings each route's own audit row wrote before #656 slice 3 put them on the wire too; `internal_liveness_read_failed` is the shared `/internal/*` liveness gate every sidecar door runs through. |
| `store_unavailable` / `not_found` / `refused` / `resolve_failed` / `store_refused` | `POST /runs` also answers `store_unavailable` when the secret store cannot be listed to check a policy's secret references. The credential-injection sinks' own closed set (`injection.go`'s `storeReadRefusal` and its callers across `injection_provider_key.go`, `injection_awssso.go`, `provider_subscription.go`, `internal.go`): the credential store did not answer, the named secret is not in the store, the secret exists but the store refused to serve it, resolving a subscription/managed token failed for a reason other than an unreachable store, or (`store_refused`, a person's own model-provider credential specifically) the store refused it. |
| `local_mode_peer_not_loopback` / `local_mode_host_not_loopback` / `admin_token_not_configured` / `missing_bearer_token` / `invalid_admin_token` / `identity_provider_not_configured` / `missing_run_token` / `invalid_run_token` / `missing_sensor_token` / `invalid_sensor_token` | `http.go`'s own auth middlewares — local mode's loopback/CSRF gates, and the admin/run/sensor bearer-token chains. Every value is already the exact string its own `auditAuthFailed(As)` call wrote before #656 slice 3 put it on the wire too; `identity_provider_not_configured` is shared by the run-token and sensor-token chains (the identical cause, no embedded identity provider wired). |
| `ui_gateway_not_found` / `ui_gateway_ticket_query_on_post` / `ui_gateway_invalid_form_body` / `ui_gateway_method_not_allowed` / `ui_gateway_invalid_run_id` / `ui_gateway_wrong_host` / `ui_gateway_ticket_invalid` / `ui_gateway_ticket_lookup_failed` / `ui_gateway_ticket_run_mismatch` / `ui_gateway_not_running` / `ui_gateway_run_kept` / `ui_gateway_policy_lookup_failed` / `ui_gateway_app_not_declared` / `ui_gateway_no_session` / `ui_gateway_session_destination_mismatch` / `ui_gateway_no_runner` / `ui_gateway_resume_failed` / `ui_gateway_conn_cap_reached` / `ui_gateway_exec_failed` / `ui_gateway_launcher_exec_failed` / `ui_gateway_launcher_missing` / `ui_gateway_launcher_not_listening` / `ui_gateway_launcher_probe_failed` / `ui_gateway_connection_closed` / `ui_gateway_bind_missing_ticket` / `bind_not_same_site` / `bind_origin_not_console` | The UI gateway (`uigateway.go`, `uigateway_session.go`, `uigateway_bind.go`): the second, un-authenticated-by-session origin that serves a sandbox's own web app. `ui_gateway_ticket_invalid` is shared by "not bound to this browser" and "invalid/expired/already-used" so a probe cannot tell binding failure from a genuinely bad ticket; `ui_gateway_ticket_run_mismatch` is shared by "the run could not be read" and "the ticket's principal is not this run's owner" — the same existence-oracle-safe parity `getRunAuthorized` uses elsewhere. `bind_not_same_site`/`bind_origin_not_console` are `uiBindRefusal`'s own closed set. |
| `device_store_unavailable` / `missing_device_token` / `invalid_device_token` / `device_lookup_failed` / `device_enrol_rate_limited` / `device_enrolment_unavailable` / `device_enrol_token_required` / `invalid_enrolment_token` / `device_ingest_in_flight` / `invalid_body` / `batch_too_large` / `invalid_row` / `org_run` / `chain_mismatch` / `device_enrolment_token_name_invalid` | Device federation (`devices_auth.go`, `devices.go`): the forwarder's own auth chain and audit-ingest door, plus `/api/v1/admin/devices/enrolment-tokens`. Most values are already the exact strings `auditAuthFailedAs`/`auditEnrolFailure`/`auditIngestFailure` wrote before #656 slice 3 put them on the wire too. |
| `attach_no_runner` / `attach_ticket_not_your_run` / `attach_not_running` / `attach_run_kept` / `attach_no_sandbox` / `attach_resume_failed` / `attach_ticket_mint_failed` / `attach_takeover_no_holder` / `attach_session_cap_reached` / `attach_local_unavailable` | `POST /runs/{id}/attach` (`attach.go`): the interactive WebSocket door. `attach_local_unavailable` (409, `detail` `flag_off`, `not_your_runner`, `runner_too_old` or `runner_offline`) answers a `transport=local` attach after every other check passed: attach through the organisation instead. |
| `blob_shape` / `field_unsafe` / `field_shape` / `region_mismatch` / `start_url_mismatch` / `account_role_pin_mismatch` / `model_account_mismatch` / `unstamped_scope` / `already_captured` / `stamp_unreadable` / `run_killed` / `provider_changed` / `signin_busy` | The `harness.credential.refuse` row's own FIXED reason vocabulary (`refuseCapture`, `awssso_pin.go`): a closed set on purpose, since the alternative is sandbox-chosen text an incident review cannot group by. `run_killed` is the login run being killed by its own cancel or a superseding sign-in; `provider_changed` is a sign-in whose provider was removed/re-kinded while the login sandbox was open; `signin_busy` is the per-person sign-in lock timing out. |
| `sso_token_no_secret_store` / `sso_token_wrong_run_kind` | `/internal/sso-token` (`ssotoken.go`): this deployment configures no secret store, or the run is not an AWS SSO container-login run. |
| `provider_sign_in_no_block` / `model_provider_not_found` / `provider_sign_in_untyped` / `provider_sign_in_disabled` / `provider_sign_in_config_unreadable` / `provider_sign_in_preview_blocked` / `provider_sign_in_no_portal` / `provider_sign_in_account_ambiguous` / `provider_sign_in_no_image` / `provider_sign_in_capture_body_invalid` / `provider_sign_in_aws_by_helper` / `harness_paste_invalid` / `provider_sign_in_not_your_run` / `provider_sign_in_capture_changed` | `/api/v1/model-providers/{id}/sign-in` (`provider_signin.go`): the console's own sign-in door, distinct from run-create's model-provider choice. `model_provider_not_found` is reused by `model_provider_credentials.go` too (no such provider, or the caller may not see it); `harness_paste_invalid` is `harnessPasteRefusal`'s own bucket, shared with `harnesscred.go`'s identical paste door. |
| `record_ceiling_limit` | `errRecordCeilingLimit`'s own wire reason (`workspace_run_launch.go`): a launch refused by the acting principal's governance profile limits (interactive not allowed, or the concurrent-run cap) — shared by `provider_signin.go`, `harnesscred_launch.go` and `record.go`, since it is the identical cause regardless of which launch door hit it. |
| `injection_grant_not_api_key` / `reserved_secret_name` / `invalid_header_name` | `/internal/injection/{grantID}` (`injection.go`): the proxy's own `api_key` resolve door. Most values are already the exact strings each site's own `secret.read` audit row wrote. |
| `record_session_name_required` / `record_label_collision` / `record_no_runner` / `record_import_step_busy` / `record_promote_no_recording` / `record_promote_rejected` / `record_promote_host_not_promotable` / `record_promote_cap_reached` / `record_promote_conflict` | Record Mode (`record.go`): per-task recording sandboxes and their promotion to durable requirement rows. `record_promote_rejected` is `promotableRecordHosts`' own bucket for several distinct pre-promotion checks that all refuse for the same reason — the entry is not durable-policy material yet. |
| `user_type_request_invalid` / `user_type_conflict` / `user_type_not_found` / `user_type_id_immutable` / `user_type_built_in_no_priority` / `user_type_built_in_immutable` / `user_type_in_use` / `user_type_delete_conflict` | `/api/v1/admin/user-types` (`user_types.go`). |
| `key_domain_request_invalid` / `key_domain_assignment_not_found` / `key_domains_store_unavailable` | `/api/v1/key-domains` (`key_domains.go`): a bad subject type, subject or body; a delete of an assignment that is not there; no Postgres-backed service. |
| `preset_request_invalid` | `/api/v1/presets` (`presets.go`): `validatePresetRequest`'s whole check is one cause bucket, the same grain as `site_config_invalid` — deliberately not split per arm (two arms echo `POST /runs`' own `user_type_*`/`confinement_class_unknown` reasons): an operator-only authoring route, the field is already named in the 400's own message, and `workspace_request_invalid` (slice 1) is the same policy for the same kind of route. |
| `model_provider_credential_no_store` / `model_provider_credential_no_person` / `model_provider_credential_is_sign_in` / `model_provider_credential_body_invalid` / `model_provider_credential_too_short` / `model_provider_credential_store_unavailable` | `/api/v1/model-providers/{id}/credential` (`model_provider_credentials.go`): the console's own key/token storage door, distinct from the sign-in door and run-create's model-provider choice. |
| `lock_unavailable` | Any write door that serializes on a lock, a cross-replica one or a runner's row lock (a site-configuration or capability-enforcement write, a credential erase, a sign-in capture, an edit of a run's end (`PATCH /runs/{id}`), a revive of one run or, for a run on a runner, another revive on a runner of the same owner, a runner revoke (`DELETE /runners/{id}` and `/me/runners/{id}`), the audit chain verification), `503` with `Retry-After`: the lock is held elsewhere past its wait, the process's lock connections are all in use, or the database could not be asked. Nothing was done and the request is safe to retry; a lock is never skipped. |
| `audit_invalid_run_id` / `audit_export_store_unavailable` / `audit_export_read_failed` / `audit_scope_unavailable` / `audit_chain_verify_store_unavailable` / `audit_chain_verify_busy` / `audit_chain_sweep_failed` / `audit_invalid_timestamp_param` / `audit_invalid_actor_type` / `audit_invalid_origin` / `audit_invalid_export_form` / `audit_partition_not_found` / `audit_partition_open` / `audit_retention_store_unavailable` / `audit_retention_read_failed` / `audit_retention_body_invalid` / `audit_retention_drop_failed` | `/api/v1/admin/audit` (`audit.go`, `audit_partition_export.go`, `audit_retention.go`): the security tier's audit-log query, export, chain-verification and retention doors. `audit_invalid_export_form`, `audit_partition_not_found` and `audit_partition_open` are `GET /audit/export?partition=`'s: a `form` that is neither `readable` nor `raw`, a name that is not a partition of the audit log, and a partition that can still receive rows (it has no digest yet). `audit_retention_store_unavailable` (`501`), `audit_retention_read_failed` (`500`, the status could not be read), `audit_retention_body_invalid` (`400`, `POST /audit/retention/drop` needs `{"partition", "digest"}`) and `audit_retention_drop_failed` (`500`, nothing was dropped; `503` under `WARDYN_AUDIT_SEAL=full` while the caller's audit subject cannot be had) are `GET /audit/retention`'s and `POST /audit/retention/drop`'s; an unknown partition on the drop is `404` `audit_partition_not_found`. |
| `source_not_found` / `source_scan_already_running` / `source_scan_unsupported_kind` / `source_scan_failed` / `source_scan_no_runner` | `POST /api/v1/sources/{id}/scan` and the admin bulk scan (`source_scan.go`). `source_scan_failed` is `scanLocalDirSource`'s own bucket for whatever detail the scan itself failed on. |
| `inline_policy_xor` / `policy_id_not_found` / `inline_policy_invalid` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` policy resolution (`run_policy_resolution.go`): `policy_id` and `inline_policy` were both set, the named policy does not exist, or (`inline_policy_invalid`, the whole resolution chain's bucket — grant filtering, domain-count cap, spec/secret-ref validation) the policy fails validation. |
| `ui_layout_invalid_preset` / `ui_layout_too_many_widgets` / `ui_layout_unknown_widget` / `ui_layout_invalid_geometry` / `ui_layout_persistence_unavailable` | `GET/PUT /api/v1/ui-layout` (`ui_layout.go`): the console's own saved-layout door. |
| `ado_sign_in_unconfigured` / `ado_sign_in_foreign_app` / `ado_sign_in_no_session` / `ado_sign_in_scope_invalid` / `ado_sign_in_prompt_invalid` | `/scm/azure-devops/signin` and its callback (`ado_entra.go`): the console's own Azure DevOps per-person sign-in doors, distinct from `ADOEntraFailure`'s own enum above. `DELETE /scm/azure-devops/connection` (`ado_pat_console.go`) answers `ado_sign_in_no_session` for a caller with no sign-in subject and `ado_sign_in_unconfigured` (404) when there is no per-person row the caller may use. `ado_sign_in_prompt_invalid` is `?prompt=` set to anything other than empty or `select_account`. |
| `azure_sign_in_unconfigured` / `azure_sign_in_unknown_row` / `azure_sign_in_no_session` / `azure_callback_cookies_invalid` / `azure_callback_missing_code` | `GET /model-providers-entra/signin?uid=` and the callback it shares with the Azure DevOps sign-in (`azure_foundry_entra.go`): the per-row Azure Foundry capture. No console Entra sign-in is configured (404); the `uid` is not an `azure_foundry` provider (404); the caller has no session subject (403); the one-time nonce or verifier cookie is missing, or the stamped row is malformed (400); the authority redirected back with no code (400). A refusal the person can act on is a redirect to `/?azure_signin_error=<code>` with the Azure DevOps callback's codes plus `row_changed`. |
| `ado_own_pat_unknown_row` / `ado_own_pat_token_invalid` / `ado_own_pat_expiry_invalid` / `ado_own_pat_expiry_too_long` / `ado_own_pat_rejected` / `ado_own_pat_identity_mismatch` / `ado_own_pat_check_unavailable` / `ado_own_pat_request_refused` | `PUT`/`DELETE /me/scm/azure-devops/token` (`ado_own_pat.go`): a person adding or removing their own Azure DevOps token. On PUT, no own-token row the caller may use has that address (a row they may not use answers the same). On DELETE it answers only when no Azure DevOps row on that address holds a token of the caller's, and no usable row exists: a person who holds a token can remove it after their access was withdrawn (#1479); the pasted value is empty, too long or has spaces; `expires_on` is not a date after today, or is past the row's `pat_max_days`; Azure DevOps did not accept the token for the organisation; the token belongs to another account (never named); Azure DevOps could not be asked; or Azure DevOps answered `400`, refusing the request rather than the token (`502`, never a token refusal, and a stored token is never marked refused for it). |
| `github_own_pat_no_session` / `github_own_pat_preview_blocked` / `github_own_pat_unknown_row` / `github_own_pat_token_invalid` / `github_own_pat_expiry_invalid` / `github_own_pat_expiry_too_long` / `github_own_pat_rejected` / `github_own_pat_check_unavailable` / `github_own_pat_account_mismatch` | `PUT`/`DELETE /me/scm/github/token` (`gh_own_pat.go`): a person adding or removing their own GitHub token on a GitHub row with `credential_source: per_user`. The caller has no browser sign-in (`403`) or is in the user view (`409`, both doors); no such row the caller may use has that address (a row they may not use answers the same; DELETE answers it only when they hold nothing on any GitHub row there); the value is not one fine-grained token (`github_pat_…`), is too long or has spaces; `expires_on` is not a date after today, or is more than 30 days out; GitHub did not accept the token (`422`); GitHub could not be asked or answered something that says nothing about the token (`503`); or the token belongs to another GitHub account than the one this row is linked to and `relink` was not set (`409`, the account never named). |
| `github_own_token_required` / `github_own_token_expired` / `github_own_token_undeliverable` / `github_own_token_resident` | A run whose `git_pat` grant for github.com is decided by an own-token GitHub row (`gh_own_pat_run.go`), refused at launch (`422`): the person has added no token of their own, or it has reached the expiry they entered; or it could not be kept out of the sandbox (the PAT broker is off, the run is on the person's own runner) or two such rows claim the run's github.com repositories. `github_own_token_resident` is the mint door (`403`) refusing that grant while the PAT broker is off. |
| `ado_callback_cookies_invalid` / `ado_callback_missing_code` / `identity_binding` / `unusable_grant` | The callback half of the same door (`consumeADOCookies`, `handleADOCallback`): browser-reachable, since the identity provider's own redirect lands here directly. `ado_callback_cookies_invalid` buckets all three single-use state/nonce/pkce cookie causes — the remedy is identical for all three (start the sign-in again). `identity_binding` and `unusable_grant` are the SAME strings this callback's own audit row already carried for those two causes. |
| `user_view_no_human` / `user_view_invalid_field` / `user_view_type_invalid` / `user_view_no_session` | `POST /api/v1/me/view` (`user_view.go`): the admin/security-admin user-view toggle. |
| `source_write_invalid` / `source_delete_conflict` / `source_in_use` | `/api/v1/sources` (`sources.go`): the shared source library. `source_write_invalid` is `validateSourceWrite`'s own bucket. |
| `branding_not_branded` / `branding_store_unavailable` / `branding_body_unreadable` | `/api/v1/admin/branding` (`branding.go`). |
| `org_revoked` / `internal_error` | `writeServerError`'s own classified/unclassified split (`writeservererror.go`): the one 5xx chokepoint every otherwise-unclassified server-side failure in this package routes through. `internal_error` is deliberately the single generic fallback — never the driver text the error carries (that stays in the log line, not the wire), just enough for a caller to tell "server-side, not yours" from a specific classified cause. |
| `namespace_quota_exceeded` | `POST /api/v1/runs` and `POST /api/v1/runs/preflight` (`run_fit.go`): the runs namespace's `ResourceQuota` objects cannot hold this run (both its pods, requests and limits, honouring quota scopes, plus its one Secret and two NetworkPolicies against `secrets`, `count/secrets` and `count/networkpolicies.networking.k8s.io`), `422` before the identity mint, so no run row and no sandbox. The message names the quota and the numbers. Not audited, like `run_quota`. The quota is read as it stands and nothing is reserved, so a concurrent run can still take the room; the quota's own admission stays the authority. Kubernetes only |
| `preflight_rate_limited` | `POST /api/v1/runs/preflight` (`preflight.go`): the person already made `WARDYN_PREFLIGHT_RATE_PER_MIN` checks this minute (burst 5), answered `429` before any gate runs. Not audited, like `run_quota`. The limit is per wardynd replica and never applies to the admin token or to `POST /runs`; [`scripts/ci-run.sh`](../scripts/ci-run.sh) already treats a failed preflight as a warning. |
| `policy_preview_rate_limited` | `POST /api/v1/runs/policy-preview`: this person's independent draft-preview bucket is exhausted (`WARDYN_POLICY_PREVIEW_RATE_PER_MIN`, default 60/minute, burst 15); `429` with `Retry-After` seconds, before gates and without audit. Per replica; admin token exempt. |
| `directory_search_query_too_short` / `directory_search_unknown_type` / `directory_search_rate_limited` / `directory_search_failed` | `GET /api/v1/directory/search` (`directory_search.go`). |
| `base_image_write_invalid` / `base_image_in_use` / `base_image_not_found` | `/api/v1/base-images` (`base_images.go`). `base_image_write_invalid` is `validateBaseImageWrite`'s own bucket. |
| `credential_erase_principal_required` / `credential_erase_operator_namespace` / `credential_erase_signin_config_unreadable` | `DELETE /people/{principal}/credentials` (`credential_erase.go`). `credential_erase_signin_config_unreadable` (503): the Azure DevOps sign-in configuration could not be read, so the erase could not take the sign-in's lock and erased nothing; try again. |
| `secret_personal_needs_person` | `GET/PUT/DELETE /me/secrets` (`secrets_explicit.go`): the caller is no verified person (the admin token, local mode or a device), so it has no personal namespace; `403`, nothing stored. A delegated portal is refused earlier, with `403` `delegation_scope`. Use `PUT /shared-secrets/{name}` for a shared service secret. |
| `credential_erased_during_write` | Any write of a person's stored credential (`PUT /secrets/{name}`, a model-provider key or sign-in, an Azure DevOps own-token paste): the person's credentials were erased after the write began, so `409` and nothing was saved. Retrying starts a new write, which lands. |
| `erasure_scope_unknown` / `erasure_self_refused` / `erasure_operator_namespace` / `erasure_incomplete` | `POST /people/{principal}/erasure` (`person_erasure.go`, security tier). `erasure_scope_unknown` (400): `scopes` is not a non-empty list of `mask_copies`, `run_outputs`, `recordings`, `run_tasks`, `components`, `run_setups`, `runner_pools`, `member_drives`, `audit_personal_fields` and `credentials`; nothing was erased. `erasure_operator_namespace` (400): the principal names the operator namespace, which is no person's; nothing was erased. `erasure_self_refused` (403): the person named is the caller and a scope other than `credentials` was asked for; nothing was erased (the admin token, which is no person, is never refused). `erasure_incomplete` (500): a scope failed part way; the body's `done` and `remaining` name the scopes, and a retry with the same scopes finishes the rest. The principal also refuses `owner_unresolved` / `owner_ambiguous` (422) as the credential erase does. |
| `explain_principal_invalid` | `GET /permissions/explain` (`capabilities_explain.go`). |
| `credential_inventory_no_meta` | `GET /admin/credentials/inventory` (`credential_inventory.go`). |
| `recording_off` / `recording_too_large` / `recording_invalid_part` / `recording_erased` / `part_limit` | `PUT /internal/recordings/{runID}` and `.../parts/{part}` (`recording.go`). `recording_off` (`409`) is an organisation that keeps no recordings, or a run marked `runner_declined` whose runner declined to record it (only a recording runner older than 0.9.3 may still upload, which clears the mark); the routes are served either way, behind the run token and the relay binding. `recording_erased` is a durable per-run erasure fence (`410`), including an upload already streaming when the erase completed; authentication, masking and size refusals retain precedence. `recording_invalid_part` is `{part}` failing to parse as canonical decimal >= 2; `part_limit` is the ONE name for a part above the limit, both on the wire and in the refusal's own `recording.upload` audit row's nested `reason` detail field. |
| `ado_decision_scope_invalid` / `ado_access_above_ceiling` | The Azure DevOps escalation's decision rule (`injection_ado_capability.go`). |
| `reserved_principal` | The same value as `authFailedReservedPrincipal` (`oidc.DenialReservedPrincipal`): a reserved identity (the admin token, the local-mode operator, a device, a portal delegate, a person's audit subject `subject:<id>`) attempted to authenticate as a human principal — the SSO callback, a session cookie, a `wdn_` token, and a portal's token exchange all refuse it. Also `422` on `POST /people/{principal}/erasure` and `DELETE /people/{principal}/credentials` when the principal names one: they erase a person, and these are none. |
| `scan_facts_invalid` / `scan_upload_superseded` | `/internal/scan-results/{runID}` (`scanresult.go`). |
| `policy_grade_spec_invalid` | `POST /policies/grade` (`policy_grade.go`): a dry-run grading preview, distinct from the real policy CRUD door (`policy_request_invalid`) even though both run the same spec validator. |
| `synthesized_profile_invalid` | The AI Run Composer's profile synthesis (`profile.go`, `POST /runs/{id}/profile`): the synthesized policy spec fails validation after clamping to the operator ceiling. |

#656's FINAL review round found a second source of wire reasons this table had
never covered:

- [`internal/authz/registry.go`](../internal/authz/registry.go)'s own closed registry, which
  `s.refuse` (`refusal.go`) now writes onto the wire for every Deny/
  Unprocessable/Conflict-effect reason.
- A Hidden-effect reason — `not_owner`,
  `attach_ticket_foreign_run` — is deliberately never itself a wire value.
- A
  door refusing one sends its Hidden twin's reason instead, via `.AsIf(...)`, so
  those two names never appear here.

Several registry values are already rows
above under a *different* refusal that deliberately shares the same string
(`capability_agent`, `capability_workspace`, `capability_workspace_provider`,
`capability_policy`, `capability_model_provider`, `groups_snapshot_stale`,
`run_not_found`, `user_type_unknown`).
The rest reach the wire only through
`s.refuse` itself and are new to this table:

| Reason | Meaning |
|---|---|
| `admin_surface` / `security_admin_surface` | The tier gates every admin/security-admin-only route runs through (`isOperator`/`isSecurityOperator`): the caller's stamped role is below the route's own floor. Both answer the byte-identical sentence "requires admin role" — the reason is what tells the two tiers apart. |
| `audit_export_partition_filter` | `GET /audit/export?partition=` carried another filter (`run_id`, `since`, `until`, `action`, `action_prefix`, `actor`, `target`, `actor_type`, `outcome` or `origin`): a partition export always covers the whole partition. Answered `400` and not audited. |
| `audit_retention_not_oldest` | `POST /audit/retention/drop` named a partition that is not the oldest retained one. Answered `409` and audited (`authz.denied`, target `audit.retention`, `partition` in the row). |
| `audit_retention_not_closed` | `POST /audit/retention/drop`: the oldest partition can still receive rows. Answered `409` and audited. |
| `audit_retention_inside_window` | `POST /audit/retention/drop`: the partition ended less than the effective retention window ago, or retention is forever. Answered `409` and audited. |
| `audit_retention_live_run` | `POST /audit/retention/drop`: the partition holds audit rows of a run that is still live. Answered `409` and audited. |
| `audit_retention_digest_mismatch` | `POST /audit/retention/drop`: the digest supplied is not the one the database computes for the partition. Answered `409` and audited. |
| `audit_retention_actor_reserved` | `POST /audit/retention/drop`: the caller's actor name is `wardynd`, which is reserved for the retention sweeper. Answered `403` and audited. |
| `byoi_user` | A Bring-Your-Own-Identity principal reached a route BYOI does not extend to. |
| `capability_egress_host` / `capability_feature` / `capability_secret` | The capability-grant gates outside the five launch-door kinds already covered above: an egress host, a feature flag, or a secret the caller's capability grants do not cover. A feature flag also gates the invitation claim and the sponsor's service-token mint (`api_token`) and, as the foreign-run `404`'s audit reason, a sponsor entering a service identity's run (`service_attach`). |
| `delegation_scope` | A portal's delegated token asked for a route outside its own delegation allow-list (#1142). |
| `service_scope` | A service identity's token asked for a route outside its fixed allow-list (#1508): run create, preflight, list, read, events, output, `PATCH` and kill, `GET /me`, `GET /setup/status` and its own model-provider credential. Everything else, audit reads included, answers `403`. |
| `event_stream_cap` | The caller already holds 32 open `GET /runs/{id}/events` streams, the most one principal may (#1407); close one and retry. Answered `422` and not audited, like `run_quota`. |
| `component_autonomy` | The organisation's autonomy cap on runs that use the caller's own custom components closes the door, with or without a governance profile; the body's `policy` names the deployment. |
| `placement_component_self_defined` | A run on your own runner carries a component you defined while your limits omit `local_self_defined_components` (default deny); `403`, target `runs.placement`. |
| `governance_profile` | The caller's resolved governance profile itself closes the door (distinct from `groups_snapshot_stale`, which is the profile being unresolvable at all). |
| `mask_state_unavailable` | The server cannot prove a run's masking corpus complete, so a door that relays or persists the run's output (recording upload, live attach, SSH shell, live output read) refuses with `503` instead of passing bytes through. A run dispatched before 0.8.6 stays refused after a server restart until it ends; any other run clears when the server can read its manifest again. Since the shared masking registry an injection or capture route also answers it when a value could not be committed to the registry: the value is not handed out. |
| `governance_overlay_unsatisfiable` | The caller's governance profile, or the run's own, is composed and nothing satisfies it together with the profile or deployment default it builds on, so the launch and every live door refuse (`403`) rather than widen. The sentence names the person's own profile and never a base. |
| `grant_pairing_not_eligible` | The named capability grant is not eligible to pair with the request it was offered against. |
| `model_provider_unavailable` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` model-provider authorization, and the record door's (`POST /workspaces/{id}/record`) (`writeProviderRefusal`, `run_model_provider.go`): the generic bucket for a non-credential refusal (provider off, not serving this agent, none chosen, no such provider) — the credential-shaped refusal instead sends `model_credential` (below), which the console's sign-in door recognizes. |
| `recording_governed` | `POST /workspaces/{id}/record` by an admin whose runs are governed (`WARDYN_GOVERN_ADMIN_RUNS`): `403`, audited at target `workspaces.record`. The body names the remedy, `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT` set to `recording`. The admin token and local mode are never refused. |
| `run_kept` | The run is kept (ended or lost); its agent is stopped and nothing may act on it as if it were live. |
| `run_owner_only` | Interactive entry (attach-ticket mint or consume, the cookie attach lane, a UI app, take-over) to a run whose owner is a person, asked by a super admin who is not that person (#1476): `403`, with `error` "only the person who started this run can open it interactively". Unlike `not_owner` it is not hidden — the admin can already see the run. A run with no personal owner (operator-owned) stays enterable; kill, approve, policy, grants, revoke, audit, revive and resume are unchanged. Also the SSH gateway's refusal of a service identity's sponsor, whatever grant they hold (#1508). |
| `role_stamp_stale` | The `wdn_` API token's role and group stamp is older than the deployment's `WARDYN_ROLE_STAMP_TTL` (0.8.6); answered `401`. The token's owner signs in to the console again, which re-stamps it; mint nothing new. |
| `key_domain_unknown` | A key-domain assignment named a domain the deployment's key domains file does not declare (`PUT /key-domains/assignments/{subject_type}/{subject}`): `422`, audited as `authz.denied`. |
| `key_domain_ambiguous_membership` | A group key-domain assignment would leave people in groups assigned to different domains with no assignment of their own, or while a person whose last sign-in lost groups (a Microsoft Entra overage) has none, so their next principal key would be refused: `409`, audited as `authz.denied`, counting and naming them. Deactivated or purged people are not counted; deleting such a person's user assignment while a group is assigned is refused the same way, as is deleting the user assignment of a person whose groups are assigned to different domains. |
| `run_quota` | The acting principal's governance profile run-count or concurrency limit is at its cap. Not audited on its own (`Audit: false` in the registry): a caller who IS authorized and simply hit a limit should not look like an attacker in the audit trail. |
| `run_terminal` | The run has already reached a terminal state; the requested action no longer applies. |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN`'s own gate (and `WARDYN_CAPABILITY_SECOND_HUMAN`'s, for an Azure DevOps capability escalation): the deciding principal is the same one who raised the approval. |
| `user_view_type_deleted` | An admin viewing through a user type that has since been deleted. `admin_view` (below) is the launch-door row this same cause answers with on `POST /runs`/`/runs/preflight`/`/runs/policy-preview`. |
| `admin_view` | An admin in the user view launched a run after the type the view looks through was deleted — the launch-door twin of `user_view_type_deleted` just above; the underlying cause is audited under that reason, this one is not audited on its own. |
| `user_view_preview` | A security admin, or a super admin with `WARDYN_GOVERN_ADMIN_RUNS` on, whose user view looks through a type other than their own sent a write; the view is a read-only preview, so `POST /runs`, `POST /runs/preflight` and `POST /runs/policy-preview` and every other non-read request answer `409` (sign-out, `POST /me/view` and `POST /policies/grade` still pass). Audited as `authz.denied`. |

The UI-sandbox relay session's own re-check (`uigateway_session.go`, the
`ui.authorize`/`denied` audit row) refuses a still-open connection with one of
its own closed values, distinct from the connect-time reasons above:

| Reason | Meaning |
|---|---|
| `delegation_ended` / `delegation_unavailable` | The same re-check, and the ticket redemption that opens a session (#1475), for a session a portal opened: the portal was revoked or its grant expired, or the grant store could not be read (fails closed, 503). A redemption refused for `delegation_ended`, `revoked` or a pre-0.8.5 ticket answers the bad-ticket `403`; an unreadable store answers `503`. |
| `not_authorized` / `revoked` / `revocation_unavailable` | An already-open UI relay session's periodic re-check (every 30s): the run is no longer this session's owner's, the session was explicitly revoked, or the revocation store could not be read (fails closed, 503). The re-check's fourth cause, the run itself becoming unreadable, reuses `run_unreadable` (above) rather than a second name for the same fact. |

`PUT /api/v1/branding` (`branding.go`, super-admin only) validates each field
with its own reason:

| Reason | Meaning |
|---|---|
| `invalid_org_name` / `invalid_name_format` | `org_name` is empty or too long, or `name_format` is not `prefix`/`suffix`. |
| `invalid_colour` / `low_contrast` | A colour field is not a valid `#rrggbb` hex value, or the chosen text/fill pair falls below the minimum contrast ratio. |
| `link_not_https` / `invalid_link` | `support_url` does not use `https`, or is not a well-formed web address. |
| `logo_too_large` / `invalid_logo` | The uploaded logo exceeds the size cap, or is not a PNG/SVG Wardyn can use. |
| `logo_from_site_config` | `remove_logo` on a logo the site configuration's `branding.logo_path` delivers; its next apply would put the file back. |

The CSRF guard (`csrf.go`, `http.go`'s local-mode arm, and `attach.go`)
refuses a cross-origin state-changing request with the same reason its
`auth.fail` audit row already carries:

| Reason | Meaning |
|---|---|
| `cross_origin_refused` | A state-changing request's `Origin`/`Referer` does not match this deployment's own origin (or, in local mode, is not loopback). |

Two more pre-existing values, unrelated to each other, round out the set the
console depends on for its own sign-in/connect doors ([`ui/src/app/lib/api/runs.ts`](../ui/src/app/lib/api/runs.ts)):

| Reason | Meaning |
|---|---|
| `model_credential` | `POST /runs`' dispatch-time model-credential gate (`runs_dispatch_llm_mechanism.go`): the run was refused specifically over a model credential — the class the console's sign-in door opens on, deliberately not narrowed further (a renewal that merely did not complete grades live and offers no button). On `/runs/preflight` this 422 also carries `setup_items`: the checklist rows resolved before the refusal, model access aside. |
| `git_credential` | New Run's per-user Azure DevOps connect gate (`scmaccess.go`, the 0.7.7 relaunch path): admitted, but this person has not connected their own git credential yet. The body may also carry `cause` (additive, optional): `token_expired`, `ended`, `consent_needed`, `blocked`, `permissions_missing` or `ado_pat_needs_console_app`; absent means not connected. |

## Pending approval (governance writes)

A deployment can require a second human to approve governance writes. A covered write is then
stored as a pending change and answered `202` with a `{"pending_change": {...}}` body; nothing is
applied until a different approver approves it. The SDK never reads that body as a saved object:

- Any method on the shared request path returns a `*client.PendingApprovalError`
  (check with `errors.As`) instead of a zero-value result. Only a `202` whose body has a
  `pending_change` key is treated this way; `KillRun`, `RecordWorkspaceTask`, `ScanWorkspace`,
  `BuildWorkspace` and `ScanSource` also answer `202` and decode as before.
- `ApplyGovernance` keeps its signature. When any write is pending it returns the current document
  and a `*client.PendingApprovalError`, so a caller written before 0.8.6 fails loudly instead of
  carrying on.
- `ApplyGovernanceResult` returns the same outcome as data: the document, the `Pending` changes,
  the `Deferred` writes and `PruneSkipped`.
  - An assignment that names a profile whose write is
    pending is deferred, never sent.
  - So is a composed profile whose base profile's write is pending
    (a `Deferred` entry with `Base` set), and so, in turn, are its own children and their
    assignments.
  - Prune does not run after a pending write.
- `PlanGovernance(ctx, doc, prune)` returns the creates, updates and deletes `ApplyGovernance` would
  make, as a `GovernancePlan`, and writes nothing: one `GET /api/v1/governance`, no write request.
  Its matching rules are `ApplyGovernance`'s. A plan is a snapshot: the server can change before an
  apply runs.
- `GET /governance` returns every profile and every assignment; a composed profile's own `ceiling` holds only what it stores, so read the resolved one under `effective.ceiling`. `/governance/profiles` and `/governance/profiles/{id}` take writes only, so a `GET` on either answers `405`.
- `GovernanceApplyResult.Steps` lists the writes an apply issued, in order, each `applied`, `pending`
  or `failed`. On a failed write `ApplyGovernanceResult` returns the steps so far with the error.
- `ApplyDrivesResult` is `ApplyDrives` with the same steps, as `DrivesApplyResult.Steps`; `ApplyDrives`
  keeps its signature. Applies are sequential and not atomic: a failed write leaves the earlier ones in place.
- `ListGovernanceChanges(ctx, state)`, `GetGovernanceChange`, `ApproveGovernanceChange` and
  `RejectGovernanceChange(ctx, id, reason)` read and decide the stored changes
  (`/api/v1/governance/changes`).

```go
res, err := c.ApplyGovernanceResult(ctx, doc, false)
if err != nil {
    return err
}
for _, ch := range res.Pending {
    fmt.Println("awaiting approval:", ch.ID, ch.TargetKind, ch.TargetKey)
}
```

The CLI mirrors this. `wardyn governance set` prints the pending and deferred lists (profiles and assignments) on stderr and
exits 0, and `wardyn governance changes list [--state ...]`, `changes approve <id>` and
`changes reject <id> [--reason ...]` act on them.

**Old clients.** A client built before 0.8.6 that writes governance against a deployment with this
switch on decodes the `202` as an empty profile, and its next assignment write is refused with
`profile_id: required` or is itself queued. Nothing applies without approval, but the error is
confusing. Upgrade the CLI and any SDK callers before requiring a second approver.

## Renamed in 0.8

Issue #658: the attach route family had three different sub-resource shapes.
`POST /runs/{id}/profile` was a noun where every sibling POST is a verb.
And one concept spelled itself four ways across the wire, Go, audit and the Helm
chart.

- Each HTTP route below keeps its OLD path mounted as a chi alias for one
  minor (this doc's release plus one); the SDK and CLI already call the NEW
  path.
- The chart key is a clean break, no alias — see
  [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md)'s "User drives" section.

| Old | New | Kind |
|---|---|---|
| `GET /runs/{id}/attach-holder` | `GET /runs/{id}/attach/holder` | HTTP route, aliased for one minor |
| `POST /runs/{id}/attach-ticket` | `POST /runs/{id}/attach/ticket` | HTTP route, aliased for one minor |
| `POST /runs/{id}/profile` | `POST /runs/{id}/profile/synthesize` | HTTP route, aliased for one minor |
| Helm `userDrives.enabled` | Helm `drives.enabled` | Chart value, clean break (no alias); the chart refuses `userDrives.enabled=true` |
| Helm `userDrives.reclaim.enabled` | Helm `drives.reclaim.enabled` | Chart value, clean break (no alias); never released under the old name |
| `approvals list\|get`, `approve`, `deny` | `wardyn approval list\|get\|approve\|deny` | CLI command, clean break (no alias), 0.8.4 |
| `logs <run-id>` | `wardyn run logs <run-id>` | CLI command, clean break (no alias), 0.8.4 |
| `sessions list\|revoke` | `wardyn session list\|revoke` | CLI command, clean break (no alias), 0.8.4 |
| `drive apply`, `governance apply`, `preset apply` | `wardyn drive set`, `wardyn governance set`, `wardyn preset set` | CLI command, clean break (no alias), 0.8.4 |

### Azure DevOps capabilities (#1409)

The Azure DevOps capability ids in `capability_ceiling`, `default_profile` and
`azure_devops_capabilities` split per area in 0.8.2. It is a clean break with no alias: migration
`0101_ado_capability_split` rewrites every stored list, and the server refuses the old `read` id
with a `400` (`ado_capability_unknown` on the policy-shaped doors). The SDK and CLI carry no
capability enum, so no client code changes.

| Old | New | Kind |
|---|---|---|
| `read` | `code_read`, `work_read`, `wiki_read`, `build_read`, `release_read`, `serviceendpoint_read`, `library_read`, `packaging_read`, `test_read`, `project_read`, `identity_read` | Capability id, clean break |
| `work_write` | `work_write`, `work_admin` | Capability id, split |
| `build_execute` | `build_execute`, `release_execute` | Capability id, split |
| `build_admin` | `build_admin`, `release_admin` | Capability id, split |

## Local dev: principal override

`X-Wardyn-Principal` overrides the server-side principal attribution:

```go
c.Principal = "alice@example.com"
```

- It is honored **only when wardynd runs in local (no-auth) mode** — it simulates
  different principals on one trusted dev machine.
- Under admin-token auth the
  header is ignored and the action is recorded as `actor_type=system`, principal
  `admin-token`; under OIDC the verified `sub` wins.
- Use OIDC for real per-human
  attribution — a shared dev server on an admin token is exactly where this header
  stops working.

The override is **attribution only**.

- It names the run's `created_by`, the
  identity's sponsor claim and the audit actor; it does **not** choose which
  secret namespace the run resolves credentials from.
- That namespace comes from
  the principal wardynd injected in local mode, so naming another principal in
  this header cannot make a run mint that principal's stored `git_pat` or
  `ssh_key`.
  - Which matters on a database that already carries member-owned
    secret rows from an SSO-configured era and is later served in local mode.

## Raw HTTP (curl)

The API is **fail-closed behind a bearer token** (it also accepts a valid OIDC
session cookie); the Compose default is `WARDYN_ADMIN_TOKEN=demo-admin-token`,
sent on **every** call (omitting it returns `401`):

```sh
# Create a run. Optional fields: "title" (a name — runs sharing one are grouped
# in the console) and "description"; "image" (bring-your-own container, wrapped +
# governed); "task_mode":"exec" (plain shell command, no agent);
# "interactive_start":"agent" (an interactive run's attach shell opens in the
# image's agent CLI instead of a bare shell); "inline_policy"; "preset" (a launch
# preset's name, which then admits only "title" and "task" beside it).
curl -s -X POST http://localhost:8080/api/v1/runs \
  -H 'Authorization: Bearer demo-admin-token' \
  -H 'Content-Type: application/json' \
  -d '{"agent":"claude-code","repo":"org/repo","title":"Refund flow","task":"fix the flaky test"}'

# The outcome contract: poll until .state is terminal, then read the task's real
# exit code off the run.complete audit event. `wardyn run --wait` wraps this.
curl -s -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>   # .state: COMPLETED | FAILED | ...

curl -s -H 'Authorization: Bearer demo-admin-token' \
  'http://localhost:8080/api/v1/audit?run_id=<id>'   # run.complete -> .data.exit_code

# Or follow the run's lifecycle as it happens (see "Following a run's lifecycle").
curl -sN -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>/events
```

`GET /api/v1/runs` accepts an opt-in, server-side scoping/filtering surface beyond
`&limit=&offset=` (#1197 L1a):

- `view` (`user`/`admin`; absent leaves the endpoint's
  behaviour exactly as above), `owner` (`me`/`all`; `view=user` forces `me` for
  every caller, admin tokens included),
- repeatable `status`
  (`active`/`ended`/`failed`/`killed`/`needs`), `ended_within` (`24h`/`7d`/`30d`/`all`),
- `include_killed=1` (a KILLED run older than 24h is hidden by default), `no_end=1`
  (only live runs with no end), `workspace`
  (exact match on the run's repo/workspace-path label) and `q` (a case-insensitive
  substring search over title/task/repo/created_by).

- With any of them present the
  response is ordered live runs first, then ended runs by end time, and two headers
  — `X-Wardyn-Hidden-Older`, `X-Wardyn-Hidden-Killed` — report how many rows the
  `ended_within` window and the killed-run default hid.
- `POST /api/v1/workspaces/{id}/record/{task}/promote-egress`
  skips, rather than refuses, a requested host that is a model provider row's own host. It names them in the additive
  `X-Wardyn-Record-Skipped-Hosts` header (comma list, absent when none) and as `skipped` in the
  `workspace.egress.approve` audit row.
- `GET /api/v1/approvals`
  accepts the same opt-in `?view=user`, scoping the queue to the caller's own runs'
  approvals for every caller.

- `view=user`/`view=admin` on `GET /api/v1/runs` also projects `attention:
  {kind, by, pending}` onto each LIVE run (#1197) — what it is waiting on
  (`approval`/`reauth`/`ado_consent`/`lost`/`takeover`) and who, in the caller's own view,
  can clear it (`you`/`owner`/`admin`).
- `status=needs` (requires `view=`) narrows
  the list to runs where `attention.by=="you"`.
- Every PENDING row on
  `GET /api/v1/approvals` now also carries `held` (bool) and, for a hold with a
  known end, `held_until` (RFC 3339) — the server-side port of the console's
  former client-side hold rule.
- With `WARDYN_APPROVAL_NOTIFY` routes set, a PENDING row also
  carries `escalation_tier` (the highest notification tier already due; absent at tier 0) and
  `sla_due_at` (RFC 3339, when the next tier is due; absent when none is left), projected at response
  time and never stored.
- `GET /api/v1/approval-notify/status` (security tier) returns
  `{channels: [{id, type, destination_host, last_success_at, last_error, last_error_at,
  failed_last_hour}]}`, the host only and never a URL.
- `GET /api/v1/me/attention?view=user|admin`
  returns `{needs_you, pending_approvals}`: `needs_you` is the count of live
  runs in that view's own default scope whose `attention.by=="you"`;
  `pending_approvals` is the same scoped PENDING count `GET /approvals` gives
  today.
- Absent `?view=`, it defaults to `user`; `view=admin` from a
  non-security-operator is coerced to `user`.
- `GET /api/v1/me/drives?runner_pool_id=<uuid>`: drive catalogue ([user-drives.md](operations/user-drives.md#user-drives-on-docker)). `POST /drives/grants` replaces; `additional: true` adds.
- `POST /api/v1/me/drives` `{name}` creates the caller's own drive (`201`, a catalogue drive with `owned: true`) when the organisation's `storage.user_drive.member_create` switch is on. `DELETE /api/v1/me/drives/{id}` deletes it and its files (`204`). SDK: `CreateMyDrive`, `DeleteMyDrive`.
- `GET /api/v1/drives/member?limit=&offset=` (super admin) lists people's drives with their owner and storage object; `GET /drives` does not list them. `DELETE /api/v1/drives/member/{id}` deletes one and its files and answers `{drive, drive_id, owner, backend, object, outcome}`. SDK: `ListMemberDrives`, `DeleteMemberDrive`; CLI: `wardyn drive member list|delete --yes`.

Beyond `run_id`, the audit query accepts server-side predicates:

- `since`/`until` (RFC 3339), `action` (exact), `action_prefix` (e.g. `egress.`),
  `actor` and `target` (exact; under `WARDYN_AUDIT_SEAL=full` each also matches the subject a sealed row stores for that person),
- `actor_type` (`human|agent|system|service`), `outcome`
  (`success|denied|failure`),
- and `origin` (`device|organisation`: the rows an
  enrolled laptop forwarded, each carrying a top-level `device_id`, or the
  organisation's own).

They compose. The CLI mirrors all but `origin` and `action` on
`wardyn audit`, whose per-run trail never holds a forwarded row.
The Go client's `AuditFilter` has neither: for one action, send `action` over raw HTTP,
or use `action_prefix` (`--action-prefix`).

- The per-run trail is chronological (ASC) and returns up to 1000 events; a longer
  trail sets `X-Wardyn-Truncated: true`, so page forward with `&limit=&offset=` to
  reach the terminal `run.complete`.
  Add `order=desc` to read the newest window first instead (newest event first;
  `limit` and `offset` then count from the newest event).
- `wardyn audit <run-id> --limit=N --offset=N`
  mirrors this on the CLI, and prints a `warning: audit trail truncated ...`
  line on stderr (never stdout, so `--json` stays a plain array) naming the next
  `--offset` — silence means the page you got is the whole trail.
- The Go client's
  `AuditEventsPage` returns the same signal as a `truncated bool` instead of a
  header a caller has to remember to check; [`scripts/ci-run.sh`](../scripts/ci-run.sh) loops it so a CI
  run's `audit.json` artifact is never a silently-truncated prefix.
- Everything
  else here is one method on the Go client above, or one `wardyn` CLI command.

### Composed profile graphs

`ApplyGovernance` reproduces a graph of composed profiles (`base_profile_id`, `overlay`,
`overlay_limits`, `contact`) from `wardyn governance get` on one install to `wardyn governance set`
on another, and a repeat apply is a no-op.

- **Order.** Profiles are written bases first, whatever order the document lists them in; a
  `base_profile_id` cycle in the document is an error before any write. Prune deletes the profiles
  composed on a base before the base, so it never meets the server's `409`.
- **Ids.** Every graph reference, `base_profile_id` and an assignment's `profile_id`, is matched by the
  id the document's own entry for that profile carries and rewritten to the id the profile holds on the
  target. A base the document does not contain is sent as given.
- **Fields.** A profile is rewritten only when its ceiling, limits, contact, base, overlay or
  overlay limits differ from the stored one. The read-only `effective` view is never written back.
- **Pending approval.** A child whose base has a pending write is deferred, not sent; apply again
  once the base is approved.
- **Older clients.** A client built before 0.8.6 drops `base_profile_id`, `overlay` and
  `overlay_limits`.
  - A composed profile it would create is refused with a `400` (`invalid ceiling:
    min_confinement_class is required`), because the exported row carries an empty ceiling and no
    overlay.
  - One it updates keeps the composition the server stored, since an absent member keeps the
    stored value.
  - It also writes in document order, so a child listed before its base fails.
  - Upgrade
    before applying a composed document.
