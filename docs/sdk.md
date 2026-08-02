# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (one non-stdlib dependency,
`github.com/google/uuid`). Every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

> **Coverage and pagination.** `pkg/client` is a curated SDK over the route families
> external tooling automates, not a 1:1 mirror of wardynd. The exact list of what it
> wraps and what it does not — and the `ListOpts` / `X-Wardyn-Truncated` pagination
> contract — is the package doc on `pkg/client` itself, where your IDE shows it at the
> call site; `TestClientCoversRouteFamilies` pins it against the real methods.

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
    r   client.AgentRun        // GetRun / ListRuns (embedded in CreateRunResult)
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

## Raw HTTP (curl)

The API is **fail-closed behind a bearer token** (it also accepts a valid OIDC
session cookie); the Compose default is `WARDYN_ADMIN_TOKEN=demo-admin-token`,
sent on **every** call (omitting it returns `401`):

```sh
# Create a run. Optional fields: "image" (bring-your-own container, wrapped +
# governed), "task_mode":"exec" (plain shell command, no agent), "inline_policy".
curl -s -X POST http://localhost:8080/api/v1/runs \
  -H 'Authorization: Bearer demo-admin-token' \
  -H 'Content-Type: application/json' \
  -d '{"agent":"claude-code","repo":"org/repo","task":"fix the flaky test"}'

# The outcome contract: poll until .state is terminal, then read the task's real
# exit code off the run.complete audit event. `wardyn run --wait` wraps this.
curl -s -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>   # .state: COMPLETED | FAILED | ...

curl -s -H 'Authorization: Bearer demo-admin-token' \
  'http://localhost:8080/api/v1/audit?run_id=<id>'   # run.complete -> .data.exit_code
```

The per-run trail is chronological (ASC) and returns up to 1000 events; a longer
trail sets `X-Wardyn-Truncated: true`, so page forward with `&limit=&offset=` to
reach the terminal `run.complete`. Everything else here is one method on the Go
client above, or one `wardyn` CLI command.
