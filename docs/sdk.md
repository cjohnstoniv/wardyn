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

    // 2. Submit a run. Returns client.CreateRunResult: the created
    //    client.AgentRun (state PENDING or RUNNING), embedded, plus any
    //    ADVISORY warnings — the run is live either way, so surface them.
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
Today the three credential-injection resolve arms behind `GET
/api/v1/internal/injection/{grantID}` — Azure DevOps, AWS SSO and Bedrock
bearer — send a reason on every refusal; most other routes, including
deciding an Azure DevOps approval, still send `error` alone, so
`apiErr.Reason == ""` does not mean "no error", only "this route has not been
converted yet". Every lane's reasons are drawn from the same closed set
(`internal/api/reasons.go`), so a reason two lanes share (the dispatch-time-
snapshot family below, or the hold-chain terminal/exhausted pair) always
means the same thing regardless of which lane sent it:

| Reason | Meaning |
|---|---|
| `missing_scope_snapshot` | The grant names the credential sentinel but carries no dispatch-time snapshot (a hand-authored grant). Azure DevOps, AWS SSO, Bedrock bearer. |
| `owner_not_caller` | The grant's snapshot owner is not the run token's own subject. Azure DevOps. |
| `roster_unreadable` | The site configuration could not be read; nothing is resolved from a failed read. Azure DevOps, AWS SSO, Bedrock bearer. |
| `run_unreadable` | The run row itself could not be read. AWS SSO, Bedrock bearer. |
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
| `not_captured` / `dead_credential` / `consent_required` / `interaction_required` / `unavailable` | `ADOEntraFailure`'s own closed enum (`internal/api/ado_entra_store.go`), carried through unchanged when the redemption classifies a renewal failure. Azure DevOps. |

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
| Helm `userDrives.enabled` | Helm `drives.enabled` | Chart value, clean break (no alias) |

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
# image's agent CLI instead of a bare shell); "inline_policy".
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

Beyond `run_id`, the audit query accepts server-side predicates:
`since`/`until` (RFC 3339), `action` (exact), `action_prefix` (e.g. `egress.`),
`actor` (exact), `actor_type` (`human|agent|system`), and `outcome`
(`success|denied|failure`) — they compose, and the CLI mirrors them on
`wardyn audit`.

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
