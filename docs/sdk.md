# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (one non-stdlib dependency,
`github.com/google/uuid`). Every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

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

**Coverage is being phased in lane by lane (#204, #656), not uniform yet.**
The three credential-injection resolve arms behind `GET
/api/v1/internal/injection/{grantID}` — Azure DevOps, AWS SSO and Bedrock
bearer — send a reason on every refusal, and #656 slice 1 has now converted
`GET/POST /approvals` (list, decide, its `/paths` route) and the internal
sidecar's push-content raise route, `POST /runs` and `POST /runs/preflight`
(create validation, the resolved-spec workspace-source checks) and `GET /runs`
(the list filters), run kill, workspace delete, and the workspace
create/update/admission/env-as-code/providers routes. Slice 2 adds
`/site-config` (including its probe and egress-redirect routes), `/governance`
(profiles, assignments, the preview routes), `/secrets`, `/people` (and its
per-person token mint), `/access` (role-mapping admin), `/permissions`
(capability grants and availability), `/admin/delegates`,
`/setup/integrations`, `/setup/onboarding-complete`, `/sessions/revoke`,
`/ssh-keys` (self-service and the admin delete-by-principal door), and the
user-drive doors (`/user-drives`, allocation, reclaim, the naming/bind
previews, and the launch-time drive resolver). Most OTHER routes still send
`error` alone, so `apiErr.Reason == ""`
does not mean "no error", only "this route has not been converted yet". Two
refusals are DELIBERATELY still bare even on a converted route: an AWS
Bedrock SSO renewal AWS never answered (an outage, not a class the console's
sign-in door should open over), and a push-content approval's 404 for "someone
else's approval" (which must stay byte-identical to a genuinely missing one, or
the reason itself becomes an existence oracle). Every lane's reasons are drawn
from the same closed set
(`internal/api/reasons.go`), so a reason two lanes share (the dispatch-time-
snapshot family below, or the hold-chain terminal/exhausted pair) always
means the same thing regardless of which lane sent it:

| Reason | Meaning |
|---|---|
| `missing_scope_snapshot` | The grant names the credential sentinel but carries no dispatch-time snapshot (a hand-authored grant). Azure DevOps, AWS SSO, Bedrock bearer. |
| `owner_not_caller` | The grant's snapshot owner is not the run token's own subject. Azure DevOps. |
| `roster_unreadable` | The site configuration could not be read; nothing is resolved from a failed read. Azure DevOps, AWS SSO, Bedrock bearer; also `POST /runs`' model-provider mechanism resolve. |
| `run_unreadable` | The run row itself could not be read. AWS SSO, Bedrock bearer; also `POST /approvals/{id}/{approve,deny}`'s second-human gate (`WARDYN_EGRESS_SECOND_HUMAN`). |
| `scope_changed` | The live provider row has drifted from the run's dispatch-time snapshot. Azure DevOps, AWS SSO, Bedrock bearer. |
| `store_error` | The credential store read failed. AWS SSO. |
| `token_mode` / `signin_unconfigured` / `signin_unreadable` | The organisation is in token mode, has no sign-in app registration configured, or its sign-in configuration could not be read (`adoEntraConfigFor`). Azure DevOps. |
| `host_not_organisation` | The requested host is outside the snapshot's organisation. Azure DevOps. |
| `sso_host_not_portal` | The requested host is outside the credential's own SSO portal. AWS SSO. |
| `per_user_bearer_absent` / `bearer_absent` | The roster names a per-user bearer this owner has none of, or no `bedrock-api-key` secret is in the store. Bedrock bearer. |
| `capability_not_grantable` / `capability_above_ceiling` / `capability_denied` / `capability_closed` / `capability_always_deny` / `capability_holds_exhausted` / `capability_review` | The capability escalation chain's refusals — see `injection_ado_capability.go`. Azure DevOps. |
| `approval_mismatch` / `approvals_unreadable` / `once_unspendable` | The named approval does not match, could not be read, or was already spent. Azure DevOps; `approvals_unreadable` also AWS SSO's re-auth hold. |
| `signin_closed` / `signin_holds_exhausted` | The hold chain has gone terminal (cancelled, expired, denied) or hit its per-run cap — see `injection_ado_signin.go`'s sign-in hold and `injection_awssso.go`'s re-auth hold, the same shape under two names. |
| `raise_failed` | The approval store itself errored while raising a capability, consent, sign-in or re-auth hold. Azure DevOps, AWS SSO. |
| `preset_unknown` / `preset_field_not_per_launch` / `preset_version_changed` / `preset_version_without_preset` | A `POST /runs` naming a launch preset: no such preset (or not open to the caller's user type, `422`), a field other than `title`/`task` beside `preset` (`400`), a pinned `preset_version` that is no longer current (`409`), or `preset_version` without `preset` (`400`). See OPERATIONS.md's launch presets section. |
| `not_captured` / `dead_credential` / `consent_required` / `interaction_required` / `unavailable` | `ADOEntraFailure`'s own closed enum (`internal/api/ado_entra_store.go`), carried through unchanged when the redemption classifies a renewal failure. Azure DevOps. |
| `invalid_approval_state` / `invalid_run_id_param` / `invalid_view_param` / `invalid_owner_param` / `invalid_status_param` / `status_needs_exclusive` / `status_needs_requires_view` / `invalid_ended_within_param` / `invalid_include_killed_param` / `runs_search_query_too_long` / `invalid_limit_param` / `invalid_offset_param` | A `GET /approvals` or `GET /runs` query parameter is malformed or conflicts with another one. `invalid_view_param` is the same reason on both routes (the same shape); `invalid_limit_param`/`invalid_offset_param` are `parseListPage`'s, shared by every paginated list route in `internal/api` (`GET /audit`, `/policies`, `/secrets`, `/user-drives`, `/api-tokens`, `/permissions/grants`, `/setup/integrations`, `/ssh-keys`, `/runs/policy-history`, …). |
| `listing_unscoped_backend` | The store backend cannot scope this listing to the caller's own runs — the SAME missing capability on `GET /approvals`, `GET /runs` and `GET /runs/policy-history`. |
| `approval_not_found` | The named approval does not exist, or the caller may not see it — `POST /approvals/{id}/{approve,deny}` and every other approval-lookup route EXCEPT `GET /approvals/{id}/paths`, which keeps `notFoundIf`'s bare 404 instead (see the push-content exception above): a foreign approval and a missing one must stay indistinguishable there, on the wire class as much as the body. |
| `approval_run_ended` / `approval_already_decided` / `credential_reauth_not_decidable` / `decision_scope_invalid_for_kind` / `invalid_request_body` | `POST /approvals/{id}/{approve,deny}`'s pre-decision refusals: the run ended first, the approval was already decided, a credential-reauth approval takes a sign-in instead, `decision_scope` was sent on a kind that does not accept one, or the body did not decode. |
| `invalid_decision_scope` / `decision_scope_until_needs_expiry` / `decision_expiry_in_past` / `decision_expiry_too_far` / `decision_expiry_without_until` | The `decision_scope`/`decision_expires_at` shape rules (0-3) on a decide call. |
| `decision_scope_always_unavailable` / `decision_always_no_workspace_link` / `decision_always_invalid_host` / `decision_always_workspace_gone` / `decision_always_host_builtin` / `decision_always_host_denied` | The operator-only `decision_scope=always` persistence chain (rules 5-7): no store to persist to, the run names no workspace, the host does not parse, the workspace is gone, or the host is built-in-routed or on the reject list. `decision_scope_always_unavailable` also covers the internal push-content raise route's own "no store" arm (the identical `s.cfg.Store == nil` cause, one route apart). |
| `egress_second_human_local_mode` | `WARDYN_EGRESS_SECOND_HUMAN` cannot be enforced in local mode (nobody is authenticated to prove a second human decided). |
| `invalid_push_scope` / `invalid_push_path_list` / `push_content_unattended` / `push_path_list_count_unavailable` / `push_path_list_cap_reached` / `push_not_held` / `push_path_lists_require_postgres` | The internal sidecar's push-content raise route (`POST /api/v1/internal/approvals`, `kind: push_content`) and `GET /approvals/{id}/paths`: a malformed raise, an unattended run's push refused rather than held, a per-run path-list cap, or a route that needs a Postgres-backed capability this backend lacks (`push_path_lists_require_postgres` also covers the raise route's own store-type-assertion arm, the identical cause one status apart). |
| `workspace_seed_store_unavailable` / `workspace_seed_unreadable` / `workspace_seed_source_target_invalid` / `workspace_seed_no_base_image` / `workspace_seed_policy_conflict` | `POST /runs`' `workspace_id` resolution (`seedRequestWorkspace`): no store configured, the workspace could not be read, a stored source's target fails the authored-target deny-list, an exec run named a workspace with no base image and no `--agent`/`--image`, or the seeded sources conflict with the policy's own mounts/repos. |
| `image_devcontainer_exclusive` / `image_builder_unavailable` | `POST /runs`' image/devcontainer build validation: `image` and `devcontainer_repo` were both set (the caller's own request shape), or a custom image was requested but this control plane has no image builder wired (a deployment capability neither field can fix) — two different causes, two reasons. |
| `workspace_sources_store_unavailable` / `workspace_sources_list_unavailable` / `workspace_source_not_onboarded` / `workspace_source_mount_not_allowed` | `POST /runs`' resolved-spec workspace-source checks (`validateWorkspaceSources`, `authorizeSpecWorkspaceSources`): no store, the workspace list could not be read, the member-safe mount gate refuses an onboarded source, or — `workspace_source_not_onboarded` — the source is not onboarded at all. That last one is DELIBERATELY the same reason regardless of which of the two functions answers it or whether the source is a mount or a repo: the message is already byte-identical across all four sites for the cross-member existence-oracle reason `authorizeSpecWorkspaceSources`' own doc comment explains (another member's onboarded source must read exactly like one nobody onboarded), and a distinguishable reason would reopen it on the wire even with the sentence unchanged. |
| `runner_capabilities_unavailable` / `confinement_class_conflict` / `confinement_class_unsupported` / `run_grants_require_spire` | `POST /runs`' confinement-class resolution and the SPIRE-only-grants check (invariant 5). |
| `agent_required` / `agent_not_enabled` / `run_task_reserved` / `confinement_class_unknown` / `task_mode_unknown` / `interactive_start_unknown` / `tool_approvals_unknown` / `tool_approvals_hold_unsupported_agent` / `tool_approvals_hold_interactive_conflict` / `integration_not_ai_provider` / `run_field_too_long` / `run_field_control_char` | `POST /runs`' request-shape validation (`decodeAndValidateCreateRun`): one reason per closed-enum field, plus the agent/integration/text-field checks. |
| `run_kill_already_terminal` / `run_kill_state_changed` | `POST /runs/{id}/kill`: the run was already terminal, or moved to another state between the read and the write. |
| `workspace_repo_not_admitted` | The repository is not on this deployment's admitted list (`workspace_admission.go`), at create/update and at launch. |
| `workspace_envcode_no_local_dir` / `workspace_envcode_no_profile` | `GET /workspaces/{id}/env-as-code`: the workspace has no `local_dir` source, or no scanned profile, to emit from. |
| `workspace_providers_invalid` / `workspace_providers_stale` | The operator-only `PUT /api/v1/workspace-providers` (deployment-wide, not a per-workspace route): the submitted block fails validation, or `If-Match` is stale. |
| `workspace_request_invalid` / `workspace_ssh_sources_not_ready` / `workspace_sources_not_allowed` | `POST/PUT /workspaces`: the request body fails validation, an SSH-remote source names a secret not yet stored, or the caller's own `local_dir` sources fail the member-safe mount gate. |
| `workspace_delete_active_run` | `DELETE /workspaces/{id}`: the workspace is in use by a still-active run. |
| `groups_snapshot_stale` | `PUT/POST /governance/*` and the user-drive resolver: the caller's group-membership snapshot is missing or was truncated at sign-in, so a group-keyed governance profile cannot be resolved. Authz's own registered reason (`internal/authz/registry.go`), reused here rather than a second copy of the string. |
| `site_config_request_invalid` / `site_config_artifact_override_invalid` / `site_config_integrations_via_own_route` / `site_config_invalid` / `site_config_stale` | `PUT /site-config`: the body did not decode, a legacy artifact-override field fails validation, integrations were named inline instead of through their own endpoints, the submitted config fails one of the agent/model-provider/default-provider validators, or `If-Match` is stale. |
| `site_config_probe_request_invalid` / `site_config_probe_url_invalid` / `egress_redirect_from_required` / `egress_redirect_not_found` | `POST /site-config/probe-proxy` and the egress-redirect edit routes. |
| `governance_profile_request_invalid` / `governance_ceiling_invalid` / `governance_profile_name_conflict` / `governance_profile_in_use` / `governance_assignment_invalid` / `governance_preview_claims_invalid` | `PUT/POST /governance/profiles` and `/governance/assignments`, and the preview routes — `governance_preview_claims_invalid` is shared with the user-drive naming preview (`user_drives_preview.go`), which feeds the same `normalizeGovernancePreviewClaims` validator. |
| `secret_name_invalid` / `secret_name_reserved` / `secret_owner_param_refused` / `secret_body_invalid` / `secret_value_too_short` / `secret_cap_reached` / `secret_not_found` | `PUT/DELETE /secrets/{name}`: the name fails the secret-name format or is reserved, `?owner=` was sent on a write, the body is malformed, the value is below the mask minimum, the owner already holds the maximum number of secrets, or (the admin-only `?owner=` delete arm) that owner holds no secret by that name. |
| `people_store_unavailable` / `person_principal_invalid` / `person_principal_reserved` / `person_email_invalid` / `person_collision` / `person_email_taken` / `person_not_found` | `POST /people` and `POST /people/{principal}/tokens` (operator/security-tier only). |
| `no_sign_in` / `default_role_unknown_groups` / `elevated_target` | `personMintRefusal`'s own closed set (`POST /people/{principal}/tokens`): the derived role has no sign-in on this deployment, an elevated default role cannot be narrowed until the person signs in once, or minting for an admin/security-admin target needs a super admin. |
| `mint_no_human` | `POST /people/{principal}/tokens`'s OWN "needs a signed-in human" sentence — NOT shared with `api_token_no_human` below: same shape, different door, different wording, so a different reason. |
| `api_token_from_api_token` | Shared, byte-identical, between `POST /people/{principal}/tokens` and `POST /api/v1/tokens` (self-service): an API token may not mint another. |
| `token_lookup_unavailable` | The API-token and delegated-token authentication middlewares' shared shape: the revocation store could not be read to authenticate the bearer. |
| `api_token_no_human` / `api_token_from_delegated_token` / `api_token_member_mode_mint` / `api_token_name_invalid` / `api_token_cap_reached` | `POST /api/v1/tokens` (self-service mint) and `DELETE`. |
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
