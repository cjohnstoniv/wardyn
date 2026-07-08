# Wardyn Go SDK — Quickstart

Import `github.com/cjohnstoniv/wardyn/pkg/client` (zero non-stdlib deps). The
package is self-sufficient: every type its methods return or accept is named
through `client.*` (e.g. `client.AgentRun`, `client.ApprovalPending`), so you
never import `internal/types`.

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
    AllowedDomains:      []string{"api.github.com"},
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
