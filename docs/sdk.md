# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (one non-stdlib dependency,
`github.com/google/uuid`). Every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

> **Redirects.** The default client (`client.New`, or a `Client` with no `HTTPClient`) never
> follows a redirect: a 3xx comes back as an `*APIError`, so a write that an ingress or a
> mistyped base URL redirects fails instead of being replayed (body and bearer included) at
> the `Location`. If you set `Client.HTTPClient`, its redirect policy is yours; set
> `CheckRedirect` to a func that returns `http.ErrUseLastResponse` unless you mean to follow them.

> **Coverage and pagination.** `pkg/client` is a curated SDK over the route families
> external tooling automates, not a 1:1 mirror of wardynd. The exact list of what it
> wraps and what it does not — and the `ListOpts` / `X-Wardyn-Truncated` pagination
> contract — is the package doc on `pkg/client` itself, where your IDE shows it at the
> call site; `TestClientCoversRouteFamilies` pins what it wraps against the real methods, and
> `TestSDKCensusNamesEveryRouteFamily` (internal/api) pins that the not-covered half names every route family the router mounts.

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
is refused `403` `delegation_scope`.

```go
v, err := c.GetRunPolicy(ctx, created.ID)
if err != nil { /* ... */ }
if v.State == client.RunPolicyViewRecorded {
    for _, ch := range v.Changes {
        fmt.Println(ch.Cause, ch.Field, ch.Added, ch.Removed)
    }
    _ = v.Spec // a client.RunPolicySpec, ready for PolicyRequest.Spec
}
```

| Field | Meaning |
|---|---|
| `state` | `recorded`, `not_yet` (the run has not reached sandbox setup) or `never` (it ended before that); `spec` and `recorded_at` are set for `recorded` only |
| `source` | `kind` is `stored`, `inline`, `default`, `profile` or `unknown`; a saved policy carries `policy_id` and its `name` at launch, and `deleted` once it is gone; `preset` and `preset_version` when the run came from one |
| `spec` | the policy the sandbox's proxy enforces, restart denies included; `llm_inspection` secret values are never present |
| `redacted` | true when the reader is below the security admin tier and mount sources or secret names were hidden (`<redacted>`, or dropped); the spec then strict-decodes as a policy but does not validate |
| `changes` | never `null`; each item is a `cause` (`workspace`, `source_control`, `mirror`, `model_access`, `git_broker`, `profile`, `org_disk`, `restart`, `limits`, `launch`), a `field` (a policy JSON name), the `added` and `removed` entries, `profile` for `profile`, `at` for `restart`, and the narrowing sentences in `detail` for `limits`. Grants read `kind:host` or `kind:repo,repo`, mounts by target, repos `repo@ref`: never a hidden value |
| `complete` | false for a run from before Wardyn recorded its starting policy: `changes` then lists only what the launch audit rows state |
| `stored_policy_now` | for a saved-policy source: `same`, `changed`, `updated` (a run from before the record: edited since, possibly a rename only) or `deleted`, with the policy's current `name` |

The CLI is `wardyn run policy <run-id> [--json]`.

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
| `idle_stopped` | the idle reaper stopped the run | |
| `failed` | the run failed | `reason`: `not_started`, `start_failed` or `run_failed` (the phase it failed in) |
| `ended` | always last; the stream then closes | `state`: the terminal run state |

Each event's `id` is monotonic per run; reconnect with `Last-Event-ID` (the SDK
does) to resume without gaps. The server sends a `: keepalive` comment every
15s, ends the stream at the next keepalive once the caller's session is revoked,
and closes a held stream after 5 minutes so the reconnect re-authenticates.
The feed is kept in wardynd's memory: resume works within the daemon's
lifetime. After a restart a live run's stream carries only what happens next,
and a run that had already ended answers `ended` alone. The repository clone
happens inside the sandbox after `ready`, so it is not a separate event.

## Reading a run's output

`GET /api/v1/runs/{id}/output?tail=<bytes>` returns the end of a
non-interactive run's combined stdout/stderr — at most `WARDYN_RUN_OUTPUT_TAIL_BYTES` (64 KiB by default) — from wardynd's memory while the run lives and from Postgres once it has ended. `RunOutput` reads it; pass
`0` for the whole tail:

```go
out, err := c.RunOutput(ctx, created.ID, 0)
fmt.Println(out.Output, out.Truncated, out.Complete, out.Source)
```

`truncated` says the output does not start at the run's first byte; `complete`
says the capture is final; a run that has just finished is not `complete` until
its last bytes are in, so read once more if the end matters. `source` is
`stdout` for a run's own output and `pane_snapshot` for an interactive run's
last screen, which is plain text. `incomplete` says bytes may be missing;
`capture_gap` says none could be captured, so `output` is empty. `mask_scope` is
`run` when the capture was masked against the run's complete manifest and
`globals_only` when it was not (empty when the deployment keeps none).
`captured_at` is when the final row was written, nil while the run is live. The
same `404` as `GET /runs/{id}` answers anyone who may not read the run; the
other refusals carry a `run_output_*` reason (below), `run_output_erased`
among them (the run's output was erased, `404`).

From a shell, `wardyn run output <run-id>` prints the same bytes (below).

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

**Coverage is uniform (#204, #656): every non-2xx body this API writes
carries `reason`, or is one of a small, reviewed, named exception.** The
sweep ran lane by lane — the three credential-injection resolve arms behind
`GET /api/v1/internal/injection/{grantID}` (Azure DevOps, AWS SSO, Bedrock
bearer) first, then #656 slice 1 (`GET/POST /approvals` including its
`/paths` route, the internal sidecar's push-content raise route, `POST /runs`
and `POST /runs/preflight`, `GET /runs`, run kill, workspace delete and the
workspace create/update/admission/env-as-code/providers routes), slice 2
(`/site-config`, `/governance`, `/secrets`, `/people`, `/access`,
`/permissions`, `/admin/delegates`, `/setup/*`, `/sessions/revoke`,
`/ssh-keys`, the user-drive doors), and slice 3 (every remaining `run_*.go`
per-run action, the UI gateway, device federation, and the rest of the route
surface) — but a route reached today answers exactly like one reached at the
start. Run revive and the admin bulk restart send `revive_unsupported` for a
run whose substrate cannot replace its proxy, alongside slice 3's other
per-run reasons. Two repo-wide guards keep it that way:
`TestEveryWriteErrorCallCarriesAReasonOrIsReviewed` fails the build on any new
bare `writeError`/`http.Error` call outside its small, evidence-backed
allowlist, and `TestNoAdHocErrorBodyOrReasonLiteral` catches a direct
`writeJSON(errorBody{...})` construction with no `Reason`, or a hand-typed
string literal passed to `writeErrorReason` instead of a named constant.
Three refusals are DELIBERATELY still bare, each with its own pinning test proving the
absence is intentional (a transient model-provider store failure that is no
door, an unanswered AWS Bedrock SSO renewal that is not a refusal class, and
the drive-mount resolver's own runner-unavailable/caller-cancelled arms) — see
`internal/api/reason_coverage_guard_test.go`'s own allowlist for exactly
which three and why. Every OTHER reason is drawn from one of two sources: the
closed set in `internal/api/reasons.go` and `reasons_routes.go` (a reason two
routes share — the dispatch-time-snapshot family below, the hold-chain
terminal/exhausted pair — always means the same thing regardless of which one
sent it), or `internal/authz/registry.go`'s own registry, which `s.refuse`
writes directly for every Deny/Unprocessable/Conflict-effect authorization
refusal (its two Hidden-effect reasons are never themselves a wire value — see
the authz section below). `TestReasonDocsMatchReasonsGo` checks both sources
against this whole table, so a value missing here is a red build, not a
silent gap:

| Reason | Meaning |
|---|---|
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
| `not_captured` / `dead_credential` / `consent_required` / `interaction_required` / `unavailable` | `ADOEntraFailure`'s own closed enum (`internal/api/ado_entra_store.go`), carried through unchanged when the redemption classifies a renewal failure. Azure DevOps. |
| `invalid_approval_state` / `invalid_run_id_param` / `invalid_view_param` / `invalid_owner_param` / `invalid_status_param` / `status_needs_exclusive` / `status_needs_requires_view` / `invalid_ended_within_param` / `invalid_include_killed_param` / `runs_search_query_too_long` / `invalid_limit_param` / `invalid_offset_param` | A `GET /approvals` or `GET /runs` query parameter is malformed or conflicts with another one. `invalid_view_param` is the same reason on both routes (the same shape); `invalid_limit_param`/`invalid_offset_param` are `parseListPage`'s, shared by every paginated list route in `internal/api` (`GET /audit`, `/policies`, `/secrets`, `/user-drives`, `/api-tokens`, `/permissions/grants`, `/setup/integrations`, `/ssh-keys`, `/runs/policy-history`, …). |
| `listing_unscoped_backend` | The store backend cannot scope this listing to the caller's own runs — the SAME missing capability on `GET /approvals`, `GET /runs` and `GET /runs/policy-history`. |
| `approval_not_found` | The named approval does not exist, or the caller may not see it — `POST /approvals/{id}/{approve,deny}` and every other approval-lookup route, INCLUDING `GET /approvals/{id}/paths`: a foreign approval and a missing one answer byte-identically there, reason and body both, so the reason itself is not an existence oracle. |
| `approval_run_ended` / `approval_already_decided` / `credential_reauth_not_decidable` / `decision_scope_invalid_for_kind` / `invalid_request_body` | `POST /approvals/{id}/{approve,deny}`'s pre-decision refusals: the run ended first, the approval was already decided, a credential-reauth approval takes a sign-in instead, `decision_scope` was sent on a kind that does not accept one, or the body did not decode. |
| `invalid_decision_scope` / `decision_scope_until_needs_expiry` / `decision_expiry_in_past` / `decision_expiry_too_far` / `decision_expiry_without_until` | The `decision_scope`/`decision_expires_at` shape rules (0-3) on a decide call. |
| `decision_scope_always_unavailable` / `decision_always_no_workspace_link` / `decision_always_invalid_host` / `decision_always_workspace_gone` / `decision_always_host_builtin` / `decision_always_host_denied` | The operator-only `decision_scope=always` persistence chain (rules 5-7): no store to persist to, the run names no workspace, the host does not parse, the workspace is gone, or the host is built-in-routed or on the reject list. `decision_scope_always_unavailable` also covers the internal push-content raise route's own "no store" arm (the identical `s.cfg.Store == nil` cause, one route apart). |
| `egress_second_human_local_mode` | `WARDYN_EGRESS_SECOND_HUMAN` (or `WARDYN_CAPABILITY_SECOND_HUMAN`) cannot be enforced in local mode (nobody is authenticated to prove a second human decided). |
| `invalid_push_scope` / `invalid_push_path_list` / `push_content_unattended` / `push_path_list_count_unavailable` / `push_path_list_cap_reached` / `push_not_held` / `push_path_lists_require_postgres` | The internal sidecar's push-content raise route (`POST /api/v1/internal/approvals`, `kind: push_content`) and `GET /approvals/{id}/paths`: a malformed raise, an unattended run's push refused rather than held, a per-run path-list cap, or a route that needs a Postgres-backed capability this backend lacks (`push_path_lists_require_postgres` also covers the raise route's own store-type-assertion arm, the identical cause one status apart). |
| `workspace_seed_store_unavailable` / `workspace_seed_unreadable` / `workspace_seed_source_target_invalid` / `workspace_seed_no_base_image` / `workspace_seed_policy_conflict` | `POST /runs`' `workspace_id` resolution (`seedRequestWorkspace`): no store configured, the workspace could not be read, a stored source's target fails the authored-target deny-list, an exec run named a workspace with no base image and no `--agent`/`--image`, or the seeded sources conflict with the policy's own mounts/repos. |
| `image_devcontainer_exclusive` / `image_builder_unavailable` | `POST /runs`' image/devcontainer build validation: `image` and `devcontainer_repo` were both set (the caller's own request shape), or a custom image was requested but this control plane has no image builder wired (a deployment capability neither field can fix) — two different causes, two reasons. |
| `workspace_sources_store_unavailable` / `workspace_sources_list_unavailable` / `workspace_source_not_onboarded` / `workspace_source_mount_not_allowed` | `POST /runs`' resolved-spec workspace-source checks (`validateWorkspaceSources`, `authorizeSpecWorkspaceSources`): no store, the workspace list could not be read, the member-safe mount gate refuses an onboarded source, or — `workspace_source_not_onboarded` — the source is not onboarded at all. That last one is DELIBERATELY the same reason regardless of which of the two functions answers it or whether the source is a mount or a repo: the message is already byte-identical across all four sites for the cross-member existence-oracle reason `authorizeSpecWorkspaceSources`' own doc comment explains (another member's onboarded source must read exactly like one nobody onboarded), and a distinguishable reason would reopen it on the wire even with the sentence unchanged. |
| `runner_capabilities_unavailable` / `confinement_class_conflict` / `confinement_class_unsupported` / `run_grants_require_spire` | `POST /runs`' confinement-class resolution and the SPIRE-only-grants check (invariant 5). |
| `agent_required` / `agent_not_enabled` / `run_task_reserved` / `confinement_class_unknown` / `task_mode_unknown` / `interactive_start_unknown` / `tool_approvals_unknown` / `tool_approvals_hold_unsupported_agent` / `tool_approvals_hold_interactive_conflict` / `integration_id_retired` / `run_field_too_long` / `run_field_control_char` | `POST /runs`' request-shape validation (`decodeAndValidateCreateRun`): one reason per closed-enum field, plus the agent/text-field checks and the refusal of the retired `integration_id` (Review answers it the same way). |
| `run_kill_already_terminal` / `run_kill_state_changed` | `POST /runs/{id}/kill`: the run was already terminal, or moved to another state between the read and the write. |
| `revive_unsupported` | `POST /runs/{id}/revive` (409), and the same value in each refused run's `reason` in `POST /admin/runs/restart`'s `results`: the run's runner substrate cannot replace its proxy in place (`runner.ErrReviveUnsupported`; on Kubernetes the agent pod pins the proxy pod's IP, so the substrate implements no `runner.ProxyReviver`). Nothing changed. Stop the run and start a new one. |
| `workspace_repo_not_admitted` | The repository is not on this deployment's admitted list (`workspace_admission.go`), at create/update and at launch. |
| `workspace_envcode_no_local_dir` / `workspace_envcode_no_profile` | `GET /workspaces/{id}/env-as-code`: the workspace has no `local_dir` source, or no scanned profile, to emit from. |
| `workspace_providers_invalid` | The operator-only `PUT /api/v1/workspace-providers` (deployment-wide, not a per-workspace route): the submitted block fails validation. Its stale-`If-Match` arm shares `site_config_stale` below — the identical ETag cause, on the same underlying site-config document. |
| `workspace_request_invalid` / `workspace_ssh_sources_not_ready` / `workspace_sources_not_allowed` | `POST/PUT /workspaces`: the request body fails validation, an SSH-remote source names a secret not yet stored, or the caller's own `local_dir` sources fail the member-safe mount gate. |
| `workspace_delete_active_run` | `DELETE /workspaces/{id}`: the workspace is in use by a still-active run. |
| `groups_snapshot_stale` | `PUT/POST /governance/*` and the user-drive resolver: the caller's group-membership snapshot is missing or was truncated at sign-in, so a group-keyed governance profile cannot be resolved. The SAME value as authz's own registered reason (`internal/authz/registry.go`) — a literal in `reasons.go` (the docs⟷reasons.go guard only reads literals), tied to authz's constant by a documented `TestNoAdHocAuthz` exception rather than a reference or a second copy invented for this package. |
| `site_config_request_invalid` / `site_config_artifact_override_invalid` / `site_config_integrations_via_own_route` / `site_config_invalid` / `site_config_stale` | `PUT /site-config`: the body did not decode, a legacy artifact-override field fails validation, integrations were named inline instead of through their own endpoints, the submitted config fails one of the agent/model-provider/default-provider validators, or `If-Match` is stale. `site_config_stale` is shared by every `PUT` that checks `If-Match` against this same document's ETag: `PUT /agent-providers`, `PUT /model-providers` and `PUT /workspace-providers` all answer it too, one reason for one cause regardless of which sub-block the write targeted. |
| `site_config_probe_request_invalid` / `site_config_probe_url_invalid` / `egress_redirect_from_required` / `egress_redirect_not_found` | `POST /site-config/probe-proxy` and the egress-redirect edit routes. |
| `governance_profile_request_invalid` / `governance_ceiling_invalid` / `governance_profile_name_conflict` / `governance_profile_in_use` / `governance_assignment_invalid` / `governance_preview_claims_invalid` | `PUT/POST /governance/profiles` and `/governance/assignments`, and the preview routes — `governance_preview_claims_invalid` is shared with the user-drive naming preview (`user_drives_preview.go`), which feeds the same `normalizeGovernancePreviewClaims` validator. |
| `secret_name_invalid` / `secret_name_reserved` / `secret_owner_param_refused` / `secret_body_invalid` / `secret_value_too_short` / `secret_cap_reached` / `secret_not_found` | `PUT/DELETE /secrets/{name}`: the name fails the secret-name format or is reserved, `?owner=` was sent on a write, the body is malformed, the value is below the mask minimum, the owner already holds the maximum number of secrets, or (the admin-only `?owner=` delete arm) that owner holds no secret by that name. |
| `people_store_unavailable` / `person_principal_invalid` / `person_principal_reserved` / `person_email_invalid` / `person_collision` / `person_email_taken` | `POST /people` (operator/security-tier only). |
| `people_list_param_invalid` | `GET /people` (security tier): `state` is not `active` or `deactivated`, or `cursor` is not one a previous page returned. A malformed `limit` answers `invalid_limit_param`, as every list does. |
| `person_token_mint_removed` | `POST /people/{principal}/tokens` (securityOps, kept mounted): refused for every caller, 403, whether the person exists or not (#1477). No role creates a token that acts as another person; they sign in and create their own (`POST /me/tokens`). The two shared no-store guards below still answer first. |
| `mint_no_human` | `POST /people/{principal}/tokens`'s OWN "needs a signed-in human" sentence — NOT shared with `api_token_no_human` below: same shape, different door, different wording, so a different reason. It answers before `person_token_mint_removed`. |
| `api_token_from_api_token` | Shared, byte-identical, between `POST /people/{principal}/tokens` and `POST /api/v1/tokens` (self-service): an API token may not mint another. |
| `token_lookup_unavailable` | The API-token and delegated-token authentication middlewares' shared shape: the revocation store could not be read to authenticate the bearer. |
| `api_token_no_human` / `api_token_from_delegated_token` / `api_token_member_mode_mint` / `api_token_name_invalid` / `api_token_ttl_invalid` / `api_token_cap_reached` | `POST /api/v1/tokens` (self-service mint) and `DELETE`. |
| `owner_ambiguous` / `owner_unresolved` | The shared owner/principal resolver shape: more than one known principal matches a name, or none does. `secrets.go`'s admin `?owner=` resolution (`ownerRefusalReason`) and `sshkeys_admin.go`'s `resolveSSHKeyOwner` (also reached from `POST /sessions/revoke`'s SSH-key cutoff) both answer from this one vocabulary rather than each inventing its own. |
| `sessions_revoke_param_invalid` | `POST /sessions/revoke`: the body must set exactly one of `sub`/`all`. |
| `sso_not_configured` / `access_role_map_value_invalid` / `access_mapping_target_invalid` / `access_email_mapping_disabled` / `access_lockout` / `access_unknown_user_type` / `access_preview_no_session_claims` | `GET/POST /access` (role-mapping admin, operator-only). |
| `capability_grant_invalid` / `capability_kind_unknown` / `capability_enforcement_stale` / `availability_kind_not_restrictable` / `availability_target_invalid` / `availability_restricted_required` / `availability_only_empty` | `POST /permissions/grants` and the capability-availability routes. |
| `delegation_store_unavailable` / `delegate_name_invalid` / `delegate_client_id_invalid` / `delegate_client_id_is_portal` / `delegate_group_invalid` | `POST /admin/delegates` (operator-only portal delegate registration). |
| `integration_invalid` / `integration_not_found` / `setup_onboarding_store_unavailable` | `PUT/DELETE /setup/integrations/{id}` and `POST /setup/onboarding-complete` (operator-only). |
| `ssh_key_invalid` / `ssh_key_requires_human` / `ssh_key_cap_reached` / `ssh_key_revoked_session` / `ssh_key_registration_refused` / `ssh_key_fingerprint_invalid_encoding` | `POST/DELETE /me/ssh-keys` (self-service). `ssh_key_registration_refused` is DELIBERATELY generic (THREAT-MODEL.md): it never confirms whether the key is already registered, or by whom — a distinguishable reason here would be the same key-squatting reconnaissance oracle the shared sentence already refuses to open. |
| `ssh_key_admin_principal_invalid_encoding` / `ssh_key_admin_principal_required` | `DELETE /people/{principal}/ssh-keys` (security-tier). |
| `user_drive_request_invalid` / `user_drive_allocated_conflict` / `user_drive_slug_conflict` / `user_drive_home_namespace_conflict` / `user_drive_name_conflict` / `user_drive_still_allocated` / `user_drive_grant_invalid` / `user_drive_grant_conflict` | `POST/PUT/DELETE /user-drives` and its allocation-grant sub-route (operator-only). |
| `user_drive_rehome_list_unavailable` / `user_drive_rehome_invalid` | The re-home guard (`PUT /user-drives/{id}`): the drive list could not be read to run it, or it actually refuses an unconfirmed identity-field change on an allocated drive. |
| `user_drive_ceiling_unavailable` / `drives_disabled` / `user_drive_size_refused` | The size-override write's own three causes: the deployment's drive-size ceiling could not be read, the org switch is off (shares `drives_disabled` with the launch-time door below — same cause), or the override exceeds the ceiling. |
| `user_drive_reclaim_invalid` / `user_drive_reclaim_subject_ambiguous` / `user_drive_not_reclaimable` / `user_drive_reclaim_unsupported` / `user_drive_reclaim_conflict` | `POST /user-drives/{id}/reclaim` (operator-only). |
| `user_drive_reclaim_failed` / `user_drive_reclaim_no_directory_name` | The reclaim object resolver's own two causes: a store read failure, or the allocation resolving to no directory name at all. |
| `user_drive_preview_no_claims` / `user_drive_denied_by_profile` | The user-drive naming and bind-failure preview routes (operator-only, mirrors the launch-time resolver's own verdict so an admin reads the SAME sentence a member would). |
| `no_allocation` / `paused` / `runner_cannot_mount` / `backend_elsewhere` / `ceiling_moved` / `home_missing` / `home_unreadable` / `share_unreachable` / `read_only` / `drives_disabled` | `POST /runs`' drive-mount resolution (`seedRequestDrive`, `refuseDrive`) — the SAME closed set (declared in `internal/api/reasons.go`, moved there from beside `refuseDrive` so this guard can see it) already used for the `wardyn_drive_refusals_total` metric and the WARN log line, now also on the wire. Two arms of `driveBindFailure` stay bare on purpose: the runner-unavailable case (a 503 about the deployment, not a class) and the CALLER-cancelled probe (`silent`), which must not be counted OR named as a real refusal either way. |
| `user_type_unknown` / `unmountable` | The user-drive resolver's own closed enum (`internal/api/user_drives_resolve.go`) members that reach `writeDriveError`'s wire body. Two siblings in that same enum, `unavailable` and `governance_unavailable`, never leave `GET /me`'s own field — no route sends them via `writeErrorReason` — so they are deliberately UNDOCUMENTED here rather than given a row that claims a wire presence they don't have; `unavailable` would also collide with `ADOEntraFailure`'s own reason of the same name above. |

#656 slice 3 converts the remaining route families — `run_*.go`'s per-run
actions, the UI gateway, device federation, and every other lane still
sending `error` alone — completing the sweep this issue tracks:

| Reason | Meaning |
|---|---|
| `local_mode_not_owner` / `revive_unsupported_deployment` / `revive_unsupported_runner` / `revive_bulk_cannot_start_agent` / `revive_already_in_progress` / `revive_mint_identity_failed` / `revive_encode_config_failed` / `revive_pull_image_failed` / `revive_claim_failed` / `revive_run_changed` / `revive_proxy_kept_current` / `revive_proxy_replace_failed_lost` / `revive_agent_start_failed_lost` / `revive_recovery_unresolved` / `revive_substrate_unreadable` / `revive_config_not_stored` / `revive_config_unreadable` / `revive_config_does_not_load` / `revive_not_running` / `revive_ended_files_gone` / `revive_past_end` / `revive_ended_task_run` / `revive_reboot_agent_stopped` / `revive_ended_agent_stopped` / `revive_unknown_lost_reason` / `revive_agent_status_unreadable` / `revive_config_run_mismatch` / `revive_ceiling_denies_git_broker` / `revive_owner_authority_unreadable` / `revive_admin_restart_count_invalid` / `revive_proxy_window_store_unavailable` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`): one reason per distinct revive-refusal cause. `local_mode_not_owner` is the same literal the refusal's own audit row already carried; `revive_unsupported_runner` covers three arms that all answer the identical "this runner cannot replace a proxy" fact. `revive_recovery_unresolved` (`503`) is a revive that failed after its claim and then could not write the run's lost mark: the run is not recorded as lost (a bulk restart's `lost_again` is `false`), its proxy is stopped and its broker credentials revoked, and a later sweep recovers it. The `_lost` reasons above say the run was put back to lost: the lost mark was written, or the run was left alone because another writer had already moved it, or it was torn down because it cannot be kept; only when the write itself fails is the answer `revive_recovery_unresolved`. |
| `revive_live_too_soon` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`), `429`: the run is live and this `wardynd` process started replacing its proxy less than a minute ago (a revive that left the proxy untouched, `revive_proxy_kept_current`, does not count). The bound is per process, so each replica allows one a minute. A revive of a lost run is never bounded. |
| `run_capacity_store_unavailable` | `GET /admin/runs/capacity`, `501`: the configured store cannot report fleet capacity. The Postgres store always can; only a test double or an alternative store lacks it. |
| `profile_unreadable` / `profile_gone` / `capability_agent` / `capability_workspace` / `capability_model_provider` / `capability_policy` / `capability_workspace_provider` / `capability_unknown` / `model_credential_erased` / `model_provider_disabled` / `model_provider_gone` / `owner_unverifiable` | `ownerRefusal`'s own reason values (`run_owner_authority.go`): the shared re-check a revive and a run-end-extension both run over the owner's launch-door capabilities, governance profile, model provider and model credential. `model_provider_disabled` covers both an integration that supplies the run's credential and the run's own model provider being turned off; `model_provider_gone` is that provider deleted (or re-created under the same id). The `capability_*` values name the SAME five capability kinds `capabilities.go` enumerates; `capability_unknown` is a defensive fallback outside that closed set. `owner_unverifiable` is `extendRefusal`'s own bucket for three arms that all answer "the owner's authority could not be confirmed right now". |
| `run_end_wait_neither_field` / `run_wait_budget_not_a_number` / `run_end_wait_already_finished` / `run_end_wait_files_gone` / `run_end_wait_store_unavailable` / `run_limits_gate_denied` / `run_end_no_end_not_allowed` / `run_end_must_be_future` / `run_wait_budget_too_small` / `run_end_wait_changed` | `PATCH /runs/{id}`'s end-time and wait-budget door (`run_end_wait.go`). `run_limits_gate_denied` is shared across all four fields the `user_changes_limits` gate can block — the same gate regardless of which field triggered it. |
| `workspace_not_found` / `user_drive_not_found` / `user_drive_allocation_not_found` / `ssh_key_not_found` / `preset_not_found` / `policy_not_found` / `capability_grant_not_found` / `run_not_found` / `governance_assignment_not_found` / `governance_profile_not_found` / `enrolment_token_not_found` / `device_not_found` / `delegate_not_found` / `api_token_not_found` / `role_mapping_not_found` | `notFoundIf`'s (`helpers.go`) per-resource-kind 404s, one for each of its ~27 call sites across the package. `run_not_found` is the same literal `auditRenewDenied` already wrote for a renewal on a run that no longer exists; `preset_not_found` is the admin preset-management route's own 404, distinct from `preset_unknown` (a run naming an unknown preset at launch); `governance_profile_not_found` is `governance.go`'s own by-id GET/PUT/DELETE, distinct from `profile_gone` above (a run's already-captured profile going missing later). |
| `invalid_id_param` / `missing_run_claims` / `run_id_mismatch` / `workspace_store_unavailable` / `scan_upload_run_not_found` / `scan_upload_not_governed` / `scan_upload_wrong_task` / `workspace_requirements_invalid` / `workspace_approved_egress_invalid` / `workspace_approved_egress_dead_host` / `workspace_denied_egress_invalid` / `workspace_llm_cred_invalid` / `request_body_too_large` / `request_body_unreadable` | `helpers.go`'s own shared foundational refusals, reused by dozens of call sites across the package since the cause is identical regardless of which handler hit it: a `{param}` path segment that fails to parse as a UUID, the internal run token's claims failing to read, a scan upload's own 3-arm refusal (`scanresult.go`'s sole caller), `scopedWorkspaceWrite`'s 4 validators (one cause bucket per route; `workspace_approved_egress_dead_host` is its own reason because the remedy differs — use the already-routed door, not fix a malformed domain), and `readCappedBody`'s too-large/unreadable split. |
| `ado_capability_unknown` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, and a launch preset's `inline_policy` (`/api/v1/admin/presets`): the spec's `azure_devops_capabilities` names something the Azure DevOps capability catalogue cannot grant. Its own reason rather than the door's `policy_request_invalid` / `inline_policy_invalid` bucket. |
| `ado_capabilities_none_permitted` | `POST /runs/preflight`: a member's `azure_devops_capabilities` leaves nothing standing on the per-person Azure DevOps lane (none of it is in the provider row's default profile or their governance profile's list). Review's mirror of the same-named refusal launch gives at dispatch. |
| `git_pat_narrowing_needs_broker` | `POST /runs/preflight`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access` or `api` while the PAT broker is off (`WARDYN_GIT_PAT_BROKER`). Only the broker enforces the narrowing; with it off the PAT is resident in the sandbox and nothing narrows it, so the run is refused. |
| `git_pat_narrowing_ssh_conflict` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, a launch preset's `inline_policy` and `POST /runs/preflight`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access` or `api` and the same policy or run holds an `ssh_key` for the same forge (`github.com` and `ssh.github.com` are one forge). SSH is a second push path the broker cannot see, so the narrowing would not bind. |
| `git_pat_narrowing_unsupported_host` | `POST /runs/preflight`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access`, `api` or `forge` for a host another lane serves and that lane does not read those fields: an Azure DevOps host or one the run's Azure DevOps gate covers, or the host of a forge the run is GitHub-brokered for (its PAT is withheld). |
| `policy_request_invalid` / `policy_secret_refs_invalid` / `policy_name_conflict` | `PUT/POST/DELETE /api/v1/policies`: the create/update body fails `decodePolicyRequest`, a secret reference in the spec fails shape validation, or a policy by that name already exists. |
| `model_provider_id_invalid` / `model_provider_not_applicable` / `model_provider_no_block_configured` | `POST /runs`' model-provider choice (`run_model_provider.go`), the three field-validation arms outside `writeProviderRefusal` (which always carries its own reason, either the credential-refusal's audit reason or the generic `model_provider_unavailable`): `model_provider` is not a plain provider id, was set on a run that calls no model, or was named but this deployment has no model providers. `integration_id` is refused earlier, unconditionally (`integration_id_retired`), before this door is reached. |
| `run_title_store_unavailable` | `PATCH /runs/{id}/title` (`run_title.go`): this store cannot rename a run. |
| `run_inspect_no_runner` / `run_inspect_terminal` / `run_inspect_no_sandbox` / `run_inspect_paused` / `run_inspect_exec_stream_unsupported` / `run_resources_read_failed` / `run_files_no_exec_session` | `GET /runs/{id}/resources` and `GET /runs/{id}/files` (`run_resources.go`, `run_files.go`): the two widgets read the identical run-state facts and share a reason per cause rather than each inventing its own synonym. |
| `run_output_tail_invalid` / `run_output_interactive` / `run_output_off` / `run_output_not_kept` / `run_output_expired` / `run_output_erased` | `GET /runs/{id}/output` (`run_output.go`): `?tail=` is not a positive number of bytes (`400`); the run is interactive, and an interactive run keeps no output here (`409`); this deployment keeps none, or refuses stored rows (`WARDYN_EXEC_OUTPUT_TAIL=off`, `409`); no output is kept for the run — a sign-in run, one that finished before output was persisted, or one still being captured, which a read a moment later serves (`409`); the in-memory tail outlived `WARDYN_EXEC_OUTPUT_TAIL_TTL`, or the run ended longer ago than `WARDYN_RUN_OUTPUT_RETENTION_DAYS` and its row was deleted (`410`); the run's output was erased (`404`). |
| `run_resume_not_running` / `run_resume_failed` | `POST /runs/{id}/resume` (`run_pause.go`): the run is not in a resumable state, or thawing it for exec failed. |
| `internal_decision_log_invalid` / `groundtruth_batch_invalid` / `groundtruth_batch_too_large` / `groundtruth_action_not_kernel` / `groundtruth_write_failed` / `internal_approval_request_invalid` / `unsupported_internal_approval_kind` / `missing_requested_scope` / `reserved_scope_key` / `internal_approval_count_unavailable` / `internal_approval_cap_reached` / `broker_not_configured` / `mint_grant_id_required` / `brokered_forge_single_lane` / `brokered_forge_single_lane_unverifiable` / `grant_run_mismatch` / `grant_not_found` / `grant_requires_spire` / `run_renew_store_unavailable` / `run_renew_read_failed` / `run_renew_stamp_failed` / `internal_liveness_read_failed` | `POST /internal/*` (`internal.go`, `internal_live_run.go`): the sidecar/proxy surface, not the member-facing API. Most values are already the exact strings each route's own audit row wrote before #656 slice 3 put them on the wire too; `internal_liveness_read_failed` is the shared `/internal/*` liveness gate every sidecar door runs through. |
| `store_unavailable` / `not_found` / `refused` / `resolve_failed` / `store_refused` | The credential-injection sinks' own closed set (`injection.go`'s `storeReadRefusal` and its callers across `injection_provider_key.go`, `injection_awssso.go`, `provider_subscription.go`, `internal.go`): the credential store did not answer, the named secret is not in the store, the secret exists but the store refused to serve it, resolving a subscription/managed token failed for a reason other than an unreachable store, or (`store_refused`, a person's own model-provider credential specifically) the store refused it. |
| `local_mode_peer_not_loopback` / `local_mode_host_not_loopback` / `admin_token_not_configured` / `missing_bearer_token` / `invalid_admin_token` / `identity_provider_not_configured` / `missing_run_token` / `invalid_run_token` / `missing_sensor_token` / `invalid_sensor_token` | `http.go`'s own auth middlewares — local mode's loopback/CSRF gates, and the admin/run/sensor bearer-token chains. Every value is already the exact string its own `auditAuthFailed(As)` call wrote before #656 slice 3 put it on the wire too; `identity_provider_not_configured` is shared by the run-token and sensor-token chains (the identical cause, no embedded identity provider wired). |
| `ui_gateway_not_found` / `ui_gateway_ticket_query_on_post` / `ui_gateway_invalid_form_body` / `ui_gateway_method_not_allowed` / `ui_gateway_invalid_run_id` / `ui_gateway_wrong_host` / `ui_gateway_ticket_invalid` / `ui_gateway_ticket_lookup_failed` / `ui_gateway_ticket_run_mismatch` / `ui_gateway_not_running` / `ui_gateway_run_kept` / `ui_gateway_policy_lookup_failed` / `ui_gateway_app_not_declared` / `ui_gateway_no_session` / `ui_gateway_session_destination_mismatch` / `ui_gateway_no_runner` / `ui_gateway_resume_failed` / `ui_gateway_conn_cap_reached` / `ui_gateway_exec_failed` / `ui_gateway_launcher_exec_failed` / `ui_gateway_launcher_missing` / `ui_gateway_launcher_not_listening` / `ui_gateway_launcher_probe_failed` / `ui_gateway_connection_closed` / `ui_gateway_bind_missing_ticket` / `bind_not_same_site` / `bind_origin_not_console` | The UI gateway (`uigateway.go`, `uigateway_session.go`, `uigateway_bind.go`): the second, un-authenticated-by-session origin that serves a sandbox's own web app. `ui_gateway_ticket_invalid` is shared by "not bound to this browser" and "invalid/expired/already-used" so a probe cannot tell binding failure from a genuinely bad ticket; `ui_gateway_ticket_run_mismatch` is shared by "the run could not be read" and "the ticket's principal is not this run's owner" — the same existence-oracle-safe parity `getRunAuthorized` uses elsewhere. `bind_not_same_site`/`bind_origin_not_console` are `uiBindRefusal`'s own closed set. |
| `device_store_unavailable` / `missing_device_token` / `invalid_device_token` / `device_lookup_failed` / `device_enrol_rate_limited` / `device_enrolment_unavailable` / `device_enrol_token_required` / `invalid_enrolment_token` / `device_ingest_in_flight` / `invalid_body` / `batch_too_large` / `invalid_row` / `org_run` / `chain_mismatch` / `device_enrolment_token_name_invalid` | Device federation (`devices_auth.go`, `devices.go`): the forwarder's own auth chain and audit-ingest door, plus `/api/v1/admin/devices/enrolment-tokens`. Most values are already the exact strings `auditAuthFailedAs`/`auditEnrolFailure`/`auditIngestFailure` wrote before #656 slice 3 put them on the wire too. |
| `attach_no_runner` / `attach_ticket_not_your_run` / `attach_not_running` / `attach_run_kept` / `attach_no_sandbox` / `attach_resume_failed` / `attach_ticket_mint_failed` / `attach_takeover_no_holder` | `POST /runs/{id}/attach` (`attach.go`): the interactive WebSocket door. |
| `blob_shape` / `field_unsafe` / `field_shape` / `region_mismatch` / `start_url_mismatch` / `account_role_pin_mismatch` / `model_account_mismatch` / `unstamped_scope` / `already_captured` / `stamp_unreadable` / `run_killed` / `provider_changed` / `signin_busy` | The `harness.credential.refuse` row's own FIXED reason vocabulary (`refuseCapture`, `awssso_pin.go`): a closed set on purpose, since the alternative is sandbox-chosen text an incident review cannot group by. `run_killed` is the login run being killed by its own cancel or a superseding sign-in; `provider_changed` is a sign-in whose provider was removed/re-kinded while the login sandbox was open; `signin_busy` is the per-person sign-in lock timing out. |
| `sso_token_no_secret_store` / `sso_token_wrong_run_kind` | `/internal/sso-token` (`ssotoken.go`): this deployment configures no secret store, or the run is not an AWS SSO container-login run. |
| `provider_sign_in_no_block` / `model_provider_not_found` / `provider_sign_in_untyped` / `provider_sign_in_disabled` / `provider_sign_in_config_unreadable` / `provider_sign_in_preview_blocked` / `provider_sign_in_no_portal` / `provider_sign_in_account_ambiguous` / `provider_sign_in_no_image` / `provider_sign_in_capture_body_invalid` / `provider_sign_in_aws_by_helper` / `harness_paste_invalid` / `provider_sign_in_not_your_run` / `provider_sign_in_capture_changed` | `/api/v1/model-providers/{id}/sign-in` (`provider_signin.go`): the console's own sign-in door, distinct from run-create's model-provider choice. `model_provider_not_found` is reused by `model_provider_credentials.go` too (no such provider, or the caller may not see it); `harness_paste_invalid` is `harnessPasteRefusal`'s own bucket, shared with `harnesscred.go`'s identical paste door. |
| `record_ceiling_limit` | `errRecordCeilingLimit`'s own wire reason (`workspace_run_launch.go`): a launch refused by the acting principal's governance profile limits (interactive not allowed, or the concurrent-run cap) — shared by `provider_signin.go`, `harnesscred_launch.go` and `record.go`, since it is the identical cause regardless of which launch door hit it. |
| `injection_grant_not_api_key` / `reserved_secret_name` / `invalid_header_name` | `/internal/injection/{grantID}` (`injection.go`): the proxy's own `api_key` resolve door. Most values are already the exact strings each site's own `secret.read` audit row wrote. |
| `record_session_name_required` / `record_label_collision` / `record_no_runner` / `record_import_step_busy` / `record_promote_no_recording` / `record_promote_rejected` / `record_promote_host_not_promotable` / `record_promote_cap_reached` / `record_promote_conflict` | Record Mode (`record.go`): per-task recording sandboxes and their promotion to durable requirement rows. `record_promote_rejected` is `promotableRecordHosts`' own bucket for several distinct pre-promotion checks that all refuse for the same reason — the entry is not durable-policy material yet. |
| `user_type_request_invalid` / `user_type_conflict` / `user_type_not_found` / `user_type_id_immutable` / `user_type_built_in_no_priority` / `user_type_built_in_immutable` / `user_type_in_use` / `user_type_delete_conflict` | `/api/v1/admin/user-types` (`user_types.go`). |
| `key_domain_request_invalid` / `key_domain_assignment_not_found` / `key_domains_store_unavailable` | `/api/v1/key-domains` (`key_domains.go`): a bad subject type, subject or body; a delete of an assignment that is not there; no Postgres-backed service. |
| `preset_request_invalid` | `/api/v1/admin/presets` (`presets.go`): `validatePresetRequest`'s whole check is one cause bucket, the same grain as `site_config_invalid` — deliberately not split per arm (two arms echo `POST /runs`' own `user_type_*`/`confinement_class_unknown` reasons): an operator-only authoring route, the field is already named in the 400's own message, and `workspace_request_invalid` (slice 1) is the same policy for the same kind of route. |
| `model_provider_credential_no_store` / `model_provider_credential_no_person` / `model_provider_credential_is_sign_in` / `model_provider_credential_body_invalid` / `model_provider_credential_too_short` / `model_provider_credential_store_unavailable` | `/api/v1/model-providers/{id}/credential` (`model_provider_credentials.go`): the console's own key/token storage door, distinct from the sign-in door and run-create's model-provider choice. |
| `lock_unavailable` | Any write door that serializes on a cross-replica lock (a site-configuration or capability-enforcement write, a credential erase, a sign-in capture, a revive of one run, the audit chain verification), `503` with `Retry-After`: the lock is held elsewhere past its wait, the process's lock connections are all in use, or the database could not be asked. Nothing was done and the request is safe to retry; a lock is never skipped. |
| `audit_invalid_run_id` / `audit_export_store_unavailable` / `audit_export_read_failed` / `audit_scope_unavailable` / `audit_chain_verify_store_unavailable` / `audit_chain_verify_busy` / `audit_chain_sweep_failed` / `audit_invalid_timestamp_param` / `audit_invalid_actor_type` / `audit_invalid_origin` / `audit_invalid_export_form` / `audit_partition_not_found` / `audit_partition_open` / `audit_retention_store_unavailable` / `audit_retention_read_failed` / `audit_retention_body_invalid` / `audit_retention_drop_failed` | `/api/v1/admin/audit` (`audit.go`, `audit_partition_export.go`, `audit_retention.go`): the security tier's audit-log query, export, chain-verification and retention doors. `audit_invalid_export_form`, `audit_partition_not_found` and `audit_partition_open` are `GET /audit/export?partition=`'s: a `form` that is neither `readable` nor `raw`, a name that is not a partition of the audit log, and a partition that can still receive rows (it has no digest yet). `audit_retention_store_unavailable` (`501`), `audit_retention_read_failed` (`500`, the status could not be read), `audit_retention_body_invalid` (`400`, `POST /audit/retention/drop` needs `{"partition", "digest"}`) and `audit_retention_drop_failed` (`500`, nothing was dropped) are `GET /audit/retention`'s and `POST /audit/retention/drop`'s; an unknown partition on the drop is `404` `audit_partition_not_found`. |
| `source_not_found` / `source_scan_already_running` / `source_scan_unsupported_kind` / `source_scan_failed` / `source_scan_no_runner` | `POST /api/v1/sources/{id}/scan` and the admin bulk scan (`source_scan.go`). `source_scan_failed` is `scanLocalDirSource`'s own bucket for whatever detail the scan itself failed on. |
| `inline_policy_xor` / `policy_id_not_found` / `inline_policy_invalid` | `POST /runs` and `POST /runs/preflight`'s policy resolution (`inline_policy.go`): `policy_id` and `inline_policy` were both set, the named policy does not exist, or (`inline_policy_invalid`, the whole resolution chain's bucket — grant filtering, domain-count cap, spec/secret-ref validation) the policy fails validation. |
| `ui_layout_invalid_preset` / `ui_layout_too_many_widgets` / `ui_layout_unknown_widget` / `ui_layout_invalid_geometry` / `ui_layout_persistence_unavailable` | `GET/PUT /api/v1/ui-layout` (`ui_layout.go`): the console's own saved-layout door. |
| `ado_sign_in_unconfigured` / `ado_sign_in_foreign_app` / `ado_sign_in_no_session` / `ado_sign_in_scope_invalid` / `ado_sign_in_prompt_invalid` | `/scm/azure-devops/signin` and its callback (`ado_entra.go`): the console's own Azure DevOps per-person sign-in doors, distinct from `ADOEntraFailure`'s own enum above. `DELETE /scm/azure-devops/connection` (`ado_pat_console.go`) answers `ado_sign_in_no_session` for a caller with no sign-in subject and `ado_sign_in_unconfigured` (404) when there is no per-person row the caller may use. `ado_sign_in_prompt_invalid` is `?prompt=` set to anything other than empty or `select_account`. |
| `azure_sign_in_unconfigured` / `azure_sign_in_unknown_row` / `azure_sign_in_no_session` / `azure_callback_cookies_invalid` / `azure_callback_missing_code` | `GET /model-providers-entra/signin?uid=` and the callback it shares with the Azure DevOps sign-in (`azure_foundry_entra.go`): the per-row Azure Foundry capture. No console Entra sign-in is configured (404); the `uid` is not an `azure_foundry` provider (404); the caller has no session subject (403); the one-time nonce or verifier cookie is missing, or the stamped row is malformed (400); the authority redirected back with no code (400). A refusal the person can act on is a redirect to `/?azure_signin_error=<code>` with the Azure DevOps callback's codes plus `row_changed`. |
| `ado_own_pat_unknown_row` / `ado_own_pat_token_invalid` / `ado_own_pat_expiry_invalid` / `ado_own_pat_expiry_too_long` / `ado_own_pat_rejected` / `ado_own_pat_identity_mismatch` / `ado_own_pat_check_unavailable` / `ado_own_pat_request_refused` | `PUT`/`DELETE /me/scm/azure-devops/token` (`ado_own_pat.go`): a person adding or removing their own Azure DevOps token. On PUT, no own-token row the caller may use has that address (a row they may not use answers the same). On DELETE it answers only when no Azure DevOps row on that address holds a token of the caller's, and no usable row exists: a person who holds a token can remove it after their access was withdrawn (#1479); the pasted value is empty, too long or has spaces; `expires_on` is not a date after today, or is past the row's `pat_max_days`; Azure DevOps did not accept the token for the organisation; the token belongs to another account (never named); Azure DevOps could not be asked; or Azure DevOps answered `400`, refusing the request rather than the token (`502`, never a token refusal, and a stored token is never marked refused for it). |
| `ado_callback_cookies_invalid` / `ado_callback_missing_code` / `identity_binding` / `unusable_grant` | The callback half of the same door (`consumeADOCookies`, `handleADOCallback`): browser-reachable, since the identity provider's own redirect lands here directly. `ado_callback_cookies_invalid` buckets all three single-use state/nonce/pkce cookie causes — the remedy is identical for all three (start the sign-in again). `identity_binding` and `unusable_grant` are the SAME strings this callback's own audit row already carried for those two causes. |
| `user_view_no_human` / `user_view_invalid_field` / `user_view_type_invalid` / `user_view_no_session` | `POST /api/v1/me/view` (`user_view.go`): the admin/security-admin user-view toggle. |
| `source_write_invalid` / `source_delete_conflict` / `source_in_use` | `/api/v1/sources` (`sources.go`): the shared source library. `source_write_invalid` is `validateSourceWrite`'s own bucket. |
| `branding_not_branded` / `branding_store_unavailable` / `branding_body_unreadable` | `/api/v1/admin/branding` (`branding.go`). |
| `org_revoked` / `internal_error` | `writeServerError`'s own classified/unclassified split (`writeservererror.go`): the one 5xx chokepoint every otherwise-unclassified server-side failure in this package routes through. `internal_error` is deliberately the single generic fallback — never the driver text the error carries (that stays in the log line, not the wire), just enough for a caller to tell "server-side, not yours" from a specific classified cause. |
| `preflight_rate_limited` | `POST /api/v1/runs/preflight` (`preflight.go`): the person already made `WARDYN_PREFLIGHT_RATE_PER_MIN` checks this minute (burst 5), answered `429` before any gate runs. Not audited, like `run_quota`. The limit is per wardynd replica and never applies to the admin token or to `POST /runs`; `scripts/ci-run.sh` already treats a failed preflight as a warning. |
| `directory_search_query_too_short` / `directory_search_unknown_type` / `directory_search_rate_limited` / `directory_search_failed` | `GET /api/v1/directory/search` (`directory_search.go`). |
| `base_image_write_invalid` / `base_image_in_use` / `base_image_not_found` | `/api/v1/base-images` (`base_images.go`). `base_image_write_invalid` is `validateBaseImageWrite`'s own bucket. |
| `credential_erase_principal_required` / `credential_erase_operator_namespace` / `credential_erase_signin_config_unreadable` | `DELETE /people/{principal}/credentials` (`credential_erase.go`). `credential_erase_signin_config_unreadable` (503): the Azure DevOps sign-in configuration could not be read, so the erase could not take the sign-in's lock and erased nothing; try again. |
| `erasure_scope_unknown` / `erasure_self_refused` / `erasure_operator_namespace` / `erasure_incomplete` | `POST /people/{principal}/erasure` (`person_erasure.go`, security tier). `erasure_scope_unknown` (400): `scopes` is not a non-empty list of `credentials`, `audit_personal_fields`, `run_tasks`, `run_outputs`, `recordings` and `mask_copies`; nothing was erased. `erasure_operator_namespace` (400): the principal names the operator namespace, which is no person's; nothing was erased. `erasure_self_refused` (403): the person named is the caller and a scope other than `credentials` was asked for; nothing was erased (the admin token, which is no person, is never refused). `erasure_incomplete` (500): a scope failed part way; the body's `done` and `remaining` name the scopes, and a retry with the same scopes finishes the rest. The principal also refuses `owner_unresolved` / `owner_ambiguous` (422) as the credential erase does. |
| `explain_principal_invalid` | `GET /permissions/explain` (`capabilities_explain.go`). |
| `credential_inventory_no_meta` | `GET /admin/credentials/inventory` (`credential_inventory.go`). |
| `recording_store_unavailable` / `recording_too_large` / `recording_invalid_part` / `part_limit` | `PUT /internal/recordings/{runID}` and `.../parts/{part}` (`recording.go`). `recording_invalid_part` is `{part}` failing to parse as canonical decimal >= 2; `part_limit` is the ONE name for a part above the limit, both on the wire and in the refusal's own `recording.upload` audit row's nested `reason` detail field. |
| `ado_decision_scope_invalid` / `ado_access_above_ceiling` | The Azure DevOps escalation's decision rule (`injection_ado_capability.go`). |
| `reserved_principal` | The same value as `authFailedReservedPrincipal` (`oidc.DenialReservedPrincipal`): a reserved identity (the admin token, the local-mode operator, a device, a portal delegate, a person's audit subject `subject:<id>`) attempted to authenticate as a human principal — the SSO callback, a session cookie, a `wdn_` token, and a portal's token exchange all refuse it. |
| `scan_facts_invalid` / `scan_upload_superseded` | `/internal/scan-results/{runID}` (`scanresult.go`). |
| `policy_grade_spec_invalid` | `POST /policies/grade` (`policy_grade.go`): a dry-run grading preview, distinct from the real policy CRUD door (`policy_request_invalid`) even though both run the same spec validator. |
| `synthesized_profile_invalid` | The AI Run Composer's profile synthesis (`profile.go`, `POST /runs/{id}/profile`): the synthesized policy spec fails validation after clamping to the operator ceiling. |

#656's FINAL review round found a second source of wire reasons this table had
never covered: `internal/authz/registry.go`'s own closed registry, which
`s.refuse` (`refusal.go`) now writes onto the wire for every Deny/
Unprocessable/Conflict-effect reason (a Hidden-effect reason — `not_owner`,
`attach_ticket_foreign_run` — is deliberately never itself a wire value; a
door refusing one sends its Hidden twin's reason instead, via `.AsIf(...)`, so
those two names never appear here). Several registry values are already rows
above under a *different* refusal that deliberately shares the same string
(`capability_agent`, `capability_workspace`, `capability_workspace_provider`,
`capability_policy`, `capability_model_provider`, `groups_snapshot_stale`,
`run_not_found`, `user_type_unknown`); the rest reach the wire only through
`s.refuse` itself and are new to this table:

| Reason | Meaning |
|---|---|
| `admin_surface` / `security_admin_surface` | The tier gates every admin/security-admin-only route runs through (`isOperator`/`isSecurityOperator`): the caller's stamped role is below the route's own floor. Both answer the byte-identical sentence "requires admin role" — the reason is what tells the two tiers apart. |
| `audit_export_partition_filter` | `GET /audit/export?partition=` carried another filter (`run_id`, `since`, `until`, `action`, `action_prefix`, `actor`, `actor_type`, `outcome` or `origin`): a partition export always covers the whole partition. Answered `400` and not audited. |
| `audit_retention_not_oldest` | `POST /audit/retention/drop` named a partition that is not the oldest retained one. Answered `409` and audited (`authz.denied`, target `audit.retention`, `partition` in the row). |
| `audit_retention_not_closed` | `POST /audit/retention/drop`: the oldest partition can still receive rows. Answered `409` and audited. |
| `audit_retention_inside_window` | `POST /audit/retention/drop`: the partition ended less than the effective retention window ago, or retention is forever. Answered `409` and audited. |
| `audit_retention_live_run` | `POST /audit/retention/drop`: the partition holds audit rows of a run that is still live. Answered `409` and audited. |
| `audit_retention_digest_mismatch` | `POST /audit/retention/drop`: the digest supplied is not the one the database computes for the partition. Answered `409` and audited. |
| `byoi_user` | A Bring-Your-Own-Identity principal reached a route BYOI does not extend to. |
| `capability_egress_host` / `capability_feature` / `capability_secret` | The capability-grant gates outside the five launch-door kinds already covered above: an egress host, a feature flag, or a secret the caller's capability grants do not cover. |
| `delegation_scope` | A portal's delegated token asked for a route outside its own delegation allow-list (#1142). |
| `event_stream_cap` | The caller already holds 32 open `GET /runs/{id}/events` streams, the most one principal may (#1407); close one and retry. Answered `422` and not audited, like `run_quota`. |
| `governance_profile` | The caller's resolved governance profile itself closes the door (distinct from `groups_snapshot_stale`, which is the profile being unresolvable at all). |
| `mask_state_unavailable` | The server cannot prove a run's masking corpus complete, so a door that relays or persists the run's output (recording upload, live attach, SSH shell, live output read) refuses with `503` instead of passing bytes through (ha-l2.0). A run dispatched before 0.8.6 stays refused after a server restart until it ends; any other run clears when the server can read its manifest again. |
| `grant_pairing_not_eligible` | The named capability grant is not eligible to pair with the request it was offered against. |
| `model_provider_unavailable` | `POST /runs`' model-provider choice, and the record door's (`POST /workspaces/{id}/record`) (`writeProviderRefusal`, `run_model_provider.go`): the generic bucket for a non-credential refusal (provider off, not serving this agent, none chosen, no such provider) — the credential-shaped refusal instead sends `model_credential` (below), which the console's sign-in door recognizes. |
| `recording_governed` | `POST /workspaces/{id}/record` by an admin whose runs are governed (`WARDYN_GOVERN_ADMIN_RUNS`): `403`, audited at target `workspaces.record`. The body names the remedy, `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT` set to `recording`. The admin token and local mode are never refused. |
| `run_kept` | The run is kept (ended or lost); its agent is stopped and nothing may act on it as if it were live. |
| `run_owner_only` | Interactive entry (attach-ticket mint or consume, the cookie attach lane, a UI app, take-over) to a run whose owner is a person, asked by a super admin who is not that person (#1476): `403`, with `error` "only the person who started this run can open it interactively". Unlike `not_owner` it is not hidden — the admin can already see the run. A run with no personal owner (operator-owned) stays enterable; kill, approve, policy, grants, revoke, audit, revive and resume are unchanged. |
| `role_stamp_stale` | The `wdn_` API token's role and group stamp is older than the deployment's `WARDYN_ROLE_STAMP_TTL` (0.8.6); answered `401`. The token's owner signs in to the console again, which re-stamps it; mint nothing new. |
| `key_domain_unknown` | A key-domain assignment named a domain the deployment's key domains file does not declare (`PUT /key-domains/assignments/{subject_type}/{subject}`): `422`, audited as `authz.denied`. |
| `key_domain_ambiguous_membership` | A group key-domain assignment would leave people in groups assigned to different domains with no assignment of their own, so their next principal key would be refused: `409`, audited as `authz.denied`, counting them. |
| `run_quota` | The acting principal's governance profile run-count or concurrency limit is at its cap. Not audited on its own (`Audit: false` in the registry): a caller who IS authorized and simply hit a limit should not look like an attacker in the audit trail. |
| `run_terminal` | The run has already reached a terminal state; the requested action no longer applies. |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN`'s own gate (and `WARDYN_CAPABILITY_SECOND_HUMAN`'s, for an Azure DevOps capability escalation): the deciding principal is the same one who raised the approval. |
| `user_view_type_deleted` | An admin viewing through a user type that has since been deleted. `admin_view` (below) is the launch-door row this same cause answers with on `POST /runs`/`/runs/preflight`. |
| `admin_view` | An admin in the user view launched a run after the type the view looks through was deleted — the launch-door twin of `user_view_type_deleted` just above; the underlying cause is audited under that reason, this one is not audited on its own. |
| `user_view_preview` | With `WARDYN_GOVERN_ADMIN_RUNS` on, an admin whose user view looks through a type other than their own sent a write; the view is a read-only preview, so `POST /runs`, `POST /runs/preflight` and every other non-read request answer `409` (sign-out, `POST /me/view` and `POST /policies/grade` still pass). Audited as `authz.denied`. |

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
console depends on for its own sign-in/connect doors (`ui/src/app/lib/api/runs.ts`):

| Reason | Meaning |
|---|---|
| `model_credential` | `POST /runs`' dispatch-time model-credential gate (`runs_dispatch_llm_mechanism.go`): the run was refused specifically over a model credential — the class the console's sign-in door opens on, deliberately not narrowed further (a renewal that merely did not complete grades live and offers no button). |
| `git_credential` | The New Run rail's per-user Azure DevOps connect gate (`scmaccess.go`, the 0.7.7 relaunch path): admitted, but this person has not connected their own git credential yet. |

## Pending approval (governance writes)

A deployment can require a second human to approve governance writes. A covered write is then
stored as a pending change and answered `202` with a `{"pending_change": {...}}` body; nothing is
applied until a different approver approves it. The SDK never reads that body as a saved object:

- Any method that goes through the shared request path returns a `*client.PendingApprovalError`
  (check with `errors.As`) instead of a zero-value result. Only a `202` whose body has a
  `pending_change` key is treated this way; `KillRun`, `RecordWorkspaceTask`, `ScanWorkspace` and
  `ScanSource` also answer `202` and decode as before.
- `ApplyGovernance` keeps its signature. When any write is pending it returns the current document
  and a `*client.PendingApprovalError`, so a caller written before 0.8.6 fails loudly instead of
  carrying on.
- `ApplyGovernanceResult` returns the same outcome as data: the document, the `Pending` changes,
  the `Deferred` assignments and `PruneSkipped`. An assignment that names a profile whose write is
  pending is deferred, never sent; prune does not run after a pending write.
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

The CLI mirrors this. `wardyn governance set` prints the pending and deferred lists on stderr and
exits 0, and `wardyn governance changes list [--state ...]`, `changes approve <id>` and
`changes reject <id> [--reason ...]` act on them.

**Old clients.** A client built before 0.8.6 that writes governance against a deployment with this
switch on decodes the `202` as an empty profile, and its next assignment write is refused with
`profile_id: required` or is itself queued. Nothing applies without approval, but the error is
confusing. Upgrade the CLI and any SDK callers before requiring a second approver.

## Renamed in 0.8

Issue #658: the attach route family had three different sub-resource shapes,
`POST /runs/{id}/profile` was a noun where every sibling POST is a verb, and
one concept spelled itself four ways across the wire, Go, audit and the Helm
chart. Each HTTP route below keeps its OLD path mounted as a chi alias for one
minor (this doc's release plus one); the SDK and CLI already call the NEW
path. The chart key is a clean break, no alias — see
`deploy/helm/wardyn/README.md`'s "User drives" section.

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

It is honored **only when wardynd runs in local (no-auth) mode** — it simulates
different principals on one trusted dev machine. Under admin-token auth the
header is ignored and the action is recorded as `actor_type=system`, principal
`admin-token`; under OIDC the verified `sub` wins. Use OIDC for real per-human
attribution — a shared dev server on an admin token is exactly where this header
stops working.

The override is **attribution only**. It names the run's `created_by`, the
identity's sponsor claim and the audit actor; it does **not** choose which
secret namespace the run resolves credentials from. That namespace comes from
the principal wardynd injected in local mode, so naming another principal in
this header cannot make a run mint that principal's stored `git_pat` or
`ssh_key` — which matters on a database that already carries member-owned
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
`&limit=&offset=` (#1197 L1a): `view` (`user`/`admin`; absent leaves the endpoint's
behaviour exactly as above), `owner` (`me`/`all`; `view=user` forces `me` for
every caller, admin tokens included), repeatable `status`
(`active`/`ended`/`failed`/`killed`/`needs`), `ended_within` (`24h`/`7d`/`30d`/`all`),
`include_killed=1` (a KILLED run older than 24h is hidden by default), `workspace`
(exact match on the run's repo/workspace-path label) and `q` (a case-insensitive
substring search over title/task/repo/created_by). With any of them present the
response is ordered live runs first, then ended runs by end time, and two headers
— `X-Wardyn-Hidden-Older`, `X-Wardyn-Hidden-Killed` — report how many rows the
`ended_within` window and the killed-run default hid. `GET /api/v1/approvals`
accepts the same opt-in `?view=user`, scoping the queue to the caller's own runs'
approvals for every caller.

`view=user`/`view=admin` on `GET /api/v1/runs` also projects `attention:
{kind, by, pending}` onto each LIVE run (#1197) — what it is waiting on
(`approval`/`reauth`/`ado_consent`/`lost`) and who, in the caller's own view,
can clear it (`you`/`owner`/`admin`); `status=needs` (requires `view=`) narrows
the list to runs where `attention.by=="you"`. Every PENDING row on
`GET /api/v1/approvals` now also carries `held` (bool) and, for a hold with a
known end, `held_until` (RFC 3339) — the server-side port of the console's
former client-side hold rule. `GET /api/v1/me/attention?view=user|admin`
returns `{needs_you, pending_approvals}`: `needs_you` is the count of live
runs in that view's own default scope whose `attention.by=="you"`;
`pending_approvals` is the same scoped PENDING count `GET /approvals` gives
today. Absent `?view=`, it defaults to `user`; `view=admin` from a
non-security-operator is coerced to `user`.

Beyond `run_id`, the audit query accepts server-side predicates:
`since`/`until` (RFC 3339), `action` (exact), `action_prefix` (e.g. `egress.`),
`actor` (exact), `actor_type` (`human|agent|system`), `outcome`
(`success|denied|failure`), and `origin` (`device|organisation`: the rows an
enrolled laptop forwarded, each carrying a top-level `device_id`, or the
organisation's own) — they compose, and the CLI mirrors all but `origin` on
`wardyn audit`, whose per-run trail never holds a forwarded row.

The per-run trail is chronological (ASC) and returns up to 1000 events; a longer
trail sets `X-Wardyn-Truncated: true`, so page forward with `&limit=&offset=` to
reach the terminal `run.complete`. `wardyn audit <run-id> --limit=N --offset=N`
mirrors this on the CLI, and prints a `warning: audit trail truncated ...`
line on stderr (never stdout, so `--json` stays a plain array) naming the next
`--offset` — silence means the page you got is the whole trail. The Go client's
`AuditEventsPage` returns the same signal as a `truncated bool` instead of a
header a caller has to remember to check; `scripts/ci-run.sh` loops it so a CI
run's `audit.json` artifact is never a silently-truncated prefix. Everything
else here is one method on the Go client above, or one `wardyn` CLI command.
