# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (one non-stdlib dependency,
`github.com/google/uuid`). Every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

> **Coverage (read this first).** `pkg/client` is a curated SDK over the route
> families external tooling automates, **not** a 1:1 mirror of every wardynd
> route. It wraps: **run** (create/get/list/grants/kill/profile), **approval**
> (list/approve/deny), **policy** (CRUD), **workspace** (CRUD + scan/verify/
> record), **audit** (per-run + global feed), **secret** (list/set/delete),
> **site-config** (get/put), **setup** (status), **identity** (`Me`), and
> **health** (`Healthz`). It does **not** wrap the AI Run Composer
> (`/runs/compose*`), preflight, attach (the interactive terminal WebSocket) /
> attach-ticket, the harness-login device flow, or the agent-facing
> `/internal/*` mint & decision endpoints — drive those with the `wardyn` CLI /
> UI or raw HTTP. `TestClientCoversRouteFamilies` pins that every family named
> here has a real method, so this list cannot silently drift from the code (the
> same honesty rule as [`docs/PLUGGABILITY.md`](PLUGGABILITY.md)).
>
> **Pagination.** The list methods and `AuditEvents`/`RecentAuditEvents` take an
> optional `client.ListOpts{Limit, Offset}` (variadic — existing zero-arg calls
> are unchanged) that sends `?limit=&offset=`. wardynd defaults to 200 rows
> (audit keeps its historical 1000/500) and hard-caps at 1000; a truncated page
> sets the `X-Wardyn-Truncated` response header, and you page forward with
> `Offset += len(page)`. Because the per-run audit trail stays chronological
> (ASC), paging forward is how you reach the terminal `run.complete` event on a
> trail longer than one page.

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/cjohnstoniv/wardyn/pkg/client"
)

func main() {
    ctx := context.Background()

    // 1. Create a client. Token is the AdminToken configured in wardynd.
    c := client.New("https://wardyn.example.com", "my-admin-token")

    // 2. Submit a run. Returns client.AgentRun (state PENDING or RUNNING).
    var run client.AgentRun
    run, err := c.CreateRun(ctx, client.CreateRunRequest{
        Agent: "claude-code",
        Repo:  "org/repo",
        Task:  "fix issue #42",
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("run id:", run.ID, "state:", run.State)

    // 3. Poll or fetch later.
    run, _ = c.GetRun(ctx, run.ID)

    // 4. Approve a pending credential or egress request.
    pending, _ := c.ListApprovals(ctx, client.ApprovalPending)
    for _, ap := range pending {
        approved, err := c.Approve(ctx, ap.ID, "reviewed and safe")
        if err != nil {
            log.Println("approve error:", err)
            continue
        }
        fmt.Println("approved:", approved.ID)
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
    r   client.AgentRun        // CreateRun / GetRun / ListRuns
    g   client.CredentialGrant // ListGrants
    a   client.ApprovalRequest // ListApprovals / Approve / Deny
    p   client.RunPolicy       // ListPolicies / GetPolicy / Create / Update
    ev  client.AuditEvent      // AuditEvents
)

// Enums and their values are re-exported too:
_ = client.ApprovalPending // also Approved / Denied / Expired (ApprovalState)
_ = client.RunRunning      // also Pending / Completed / Failed / Killed ... (RunState)

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

## Multi-user dev: principal override

`X-Wardyn-Principal` overrides the server-side principal attribution (dev only):

```go
c.Principal = "alice@example.com"
```

## Raw HTTP (curl)

The API is **fail-closed behind a bearer token** (it also accepts a valid OIDC
session cookie); the Compose default is `WARDYN_ADMIN_TOKEN=demo-admin-token`,
sent as a bearer header on **every** call (omitting it returns `401`):

```sh
# Create a run
curl -s -X POST http://localhost:8080/api/v1/runs \
  -H 'Authorization: Bearer demo-admin-token' \
  -H 'Content-Type: application/json' \
  -d '{"agent":"claude-code","repo":"org/repo","task":"fix the flaky test"}'
# Optional create fields: "image" (bring-your-own-container, wrapped + governed),
# "task_mode":"exec" (run the task as a plain shell command — no agent),
# "inline_policy" (full RunPolicySpec instead of policy_id).

# Wait for the outcome (poll until terminal; the run.complete audit event
# carries the task's real exit code). The CLI wraps this: `wardyn run --wait`.
curl -s -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>          # .state: COMPLETED | FAILED | ...
curl -s -H 'Authorization: Bearer demo-admin-token' \
  'http://localhost:8080/api/v1/audit?run_id=<id>' # run.complete -> .data.exit_code
# The per-run trail is chronological and returns up to 1000 events by default.
# On a longer trail the response sets `X-Wardyn-Truncated: true`; page forward
# with &limit=&offset= (offset += page size) to reach the terminal run.complete.

# List pending approvals
curl -s -H 'Authorization: Bearer demo-admin-token' \
  'http://localhost:8080/api/v1/approvals?state=PENDING'

# Approve
curl -s -X POST http://localhost:8080/api/v1/approvals/<id>/approve \
  -H 'Authorization: Bearer demo-admin-token' \
  -H 'Content-Type: application/json' \
  -d '{"reason":"reviewed scope, looks correct"}'
```
