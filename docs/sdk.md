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

    "github.com/cjohnstoniv/wardyn/pkg/client"
)

func main() {
    ctx := context.Background()

    // 1. Create a client. Token is the AdminToken configured in wardynd.
    c := client.New("https://wardyn.example.com", "my-admin-token")

    // 2. Submit a run. Answers as soon as the run row exists, so the result's
    //    client.AgentRun (embedded) reads state PENDING — the image build and
    //    dispatch continue server-side; follow RunEvents (below) or GetRun for RUNNING/FAILED.
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

    // 4. Approve this run's pending credential or egress requests. created.ID
    //    asks for only this run's approvals (the server's ?run_id= predicate);
    //    uuid.Nil would list every run's. Omitting DecisionOpts keeps the
    //    default scope: the grant holds for the rest of the run.
    pending, err := c.ListApprovals(ctx, client.ApprovalPending, created.ID)
    if err != nil {
        log.Fatal(err)
    }
    for _, ap := range pending {
        approved, err := c.Approve(ctx, ap.ID, "reviewed and safe")
        if err != nil {
            log.Println("approve error:", err)
            continue
        }
        fmt.Println("approved:", approved.ID)
    }

    // 4b. Instead of that default, an egress_domain approval can be scoped
    //     narrower or wider: ScopeOnce releases a single connection, ScopeUntil
    //     holds until a deadline, and ScopeAlways (operator-only) persists the
    //     host onto the run's workspace so future runs never raise it. Deny
    //     takes the same DecisionOpts. In place of the Approve call above:
    //
    //         until := time.Now().Add(2 * time.Hour)
    //         approved, err := c.Approve(ctx, ap.ID, "temporary access", client.DecisionOpts{
    //             Scope: client.ScopeUntil,
    //             Until: &until,
    //         })

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

The recording fields of an `AgentRun` are derived, not stored:

- `GetRun` (`GET /api/v1/runs/{id}`) always sets `HasRecording`, `RecordingBytes` and `RecordingDurationSec`.
- `ListRuns` sets them only with `?include=recording_meta`; otherwise `HasRecording` is `nil` and the key is absent, so a listed run never reads as "no recording".
- `RecordingBytes` and `RecordingDurationSec` are omitted at zero; `HasRecording` is the only "no recording" signal.

A `QUEUED` run held back by its runners' limits carries `queue.limit_sentence`.

- It is on a single-run `GET /runs/{id}` and in the `warnings` of the create answer, for the run's owner or an admin. A list never has it, and it is absent when it cannot be worked out. The sentence from `queue.reason` then stands alone.
- It names the limit as the runners stand now, for the size the start would hold: "`<runner>` is at a limit: Runs at once: 2 running of 2. This run starts when it has room." A total reads "memory total: 16 GB held of 16 GB, this run needs 8 GB"; CPU and disk read alike.
- With several runners it gives one as an example, the runner holding the fewest runs (the first by id on a tie). That is not claimed to be the closest to having room.

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
| `redacted` | true when the reader is below the super admin tier and mount sources were hidden (`<redacted>`), or below the security admin tier and secret names were dropped; the spec then strict-decodes as a policy but does not validate |
| `changes` | never `null`; each item says why, where and what changed, and never shows a hidden value; the item fields are listed under the table |
| `complete` | false for a run from before Wardyn recorded its starting policy: `changes` then lists only what the launch audit rows state |
| `stored_policy_now` | for a saved-policy source: `same`, `changed`, `updated` (a run from before the record: edited since, possibly a rename only) or `deleted`, with the policy's current `name` |
| `provenance` | per top-level `spec` key, the layer that set it (`client.PolicyProvenance`, [denials.md](operations/denials.md)) |
| `host_marks` | `inspected` the allowed hosts Wardyn opens and `terminated` every host it terminates once admitted, as `host` or `host:port`, and `outside` those opened on a port no entry names (all, under allow-all), from the run's proxy config (`client.RunPolicyHostMarks`); other allowed hosts are tunnels. Absent for a run recorded before marks or restarted |
| `wall_preset` | the run's wall preset (`id`, `name`, `scope`, `group_id`, `version`) and what it `narrowed`, `removed` and `added`; only `scope` for a personal preset read by anyone but its owner |

The CLI is `wardyn run policy <run-id> [--json]`.

`GET /api/v1/runs/{id}` also returns `policy_readback`, the run's newest proxy start as its policy readback stands: `state` (`pending`, `enforced`, `unconfirmed`, `failed`, `abandoned` or `superseded`), `generation` and `confirmed_at`. There is no key for a run with no recorded start. See [run lifetime](operations/run-lifetime.md#policy-readback).

`PATCH /api/v1/runs/{id}/egress` (`client.EditRunEgress`) narrows a live run's egress: `{"remove_hosts": [...], "deny_hosts": [...]}`, never a host added. It answers `200 {"policy_generation": N}` only after the new proxy's readback for that start is `enforced`, Any `4xx` is a refusal before anything changed. Otherwise the new proxy is stopped: `503 egress_edit_unconfirmed` once that stop is confirmed, `503 egress_edit_containment_unresolved` while it is not. `503 egress_edit_superseded` means another revive replaced the edit's start, and nothing was stopped for the edit ([run lifetime](operations/run-lifetime.md#live-egress-edit)).

`GET /api/v1/runs/{id}` also returns `policy_digest`: the SHA-256 of the run's effective policy as `run.policy.resolve` records it, or no key before the policy is recorded. It identifies the policy and enforces nothing. Audit rows of the run carry the same value; see [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md).

#### Reusing the spec

The spec always strict-decodes as a policy. Whether it passes policy validation depends on the reader and the run.

- As a super admin it passes, and `wardyn run --policy-file` runs it again as is.
- Below that tier it holds `<redacted>` mount sources (and, below the security admin tier, fewer secret names), so it is a starting point.
- A run that used an organisation component's shared secret is a record for every reader. The grant carries `shared: true`, which every authored door refuses, so validation fails.
- For such a run, start the new run with the component named in `CreateRunRequest.Components` ([Custom components](#custom-components)).
- The CLI says so in a `#` comment line above the YAML, and in its help.

#### `changes`

Never `null`. Each item is a `cause` (`workspace`, `source_control`, `component`, `mirror`, `model_access`, `git_broker`, `overlay`, `unit`, `walls`, `wall_preset`, `org_disk`, `pool_disk`, `restart`, `limits`, `launch`), a `field` (a policy JSON name), the `added` and `removed` entries, `unit` (the governance unit's name) for `unit`, `at` for `restart`, and the narrowing sentences in `detail` for `limits`.
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

## Running a command in a run

An automation can create, execute, inspect and destroy a run without a terminal.
`CreateRun` starts it, `ExecRun` runs commands in it, `RunFiles` and `RunOutput` read what it did, and `KillRun` ends it:

```go
res, err := c.ExecRun(ctx, created.ID, client.ExecRequest{Command: "go test ./...", TimeoutSec: 120})
fmt.Println(res.ExitCode, res.Stdout, res.Stderr, res.Truncated, res.TimedOut)
```

`POST /api/v1/runs/{id}/exec` takes `{"command": "...", "timeout_sec": 60}` and answers `200 {"exit_code", "stdout", "stderr", "truncated", "timed_out"}` once the command ends.

- It is the SSH gateway's exec, over HTTP: the command runs through `/bin/sh -c` from the sandbox user's home directory, with no TTY and no added environment.
- It is on whether or not the SSH gateway is (`WARDYN_SSH_LISTEN`), and the per-run controls below apply either way: `deny_interactive`, the `data_out` rule and background-only runs.
- As with SSH exec, the output is neither masked nor recorded.
- Only the person who may enter the run may call it, as for the terminal; a profile's `deny_interactive` closes it too.
- A run whose `data_out` rule is `review` or `blocked`, or whose rule cannot be read, answers `409 data_out_blocked`.
  The command never reaches the sandbox, and a `run.data_out.deny` row with `path` `exec` is written, as for an SSH command.
- A background run (`experience: background`) answers `409 run_exec_unavailable`; create the run with `experience: interactive` to use exec. A run that cannot take a command now (not running, ended, or paused and not resumed) answers it too.
- The command may be up to 128 KiB, the most Linux execs as one argument; a longer one answers `400 run_exec_invalid`.
- `timeout_sec` defaults to 60 and may be 1 to 300.
  When it passes, the command's stream is closed, and the reply says `timed_out` with `exit_code` `-1`.
  The command itself may keep running in the sandbox until it ends or the run ends; a retry can run a second copy.
- Each stream keeps its first 1 MiB; `truncated` says one had more.
  The rest is read and dropped, so a command writing more never blocks.
- The command is never recorded.
  Its `run.command.exec` audit row names the command (its first 4 KiB) and the exit code, never output.
  A caller who leaves before the command ends gets a `failure` row saying so.
- A command holds one of the run's SSH channels while it runs (`WARDYN_SSH_MAX_SESSIONS_PER_RUN`, shared with SSH); with all in use the answer is `429 run_exec_busy`.
  While it runs, the run does not count as idle.
- The call waits for the command, so an `http.Client` with a `Timeout` needs a longer one.

## Finding a waiting sign-in

`GET /api/v1/runs/{id}/sign-in` finds an AWS sign-in waiting for its
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

## Approving one MCP call

A tool rule with effect `approve_each` raises one `tool_call` approval for each
call of that remote MCP tool. Its `requested_scope`:

```json
{"tool": "mcp__jira__delete_issue", "mcp_server": "jira", "mcp_host": "jira.example.com",
 "arguments": {"id": 1}, "call_sha256": "9f2c…"}
```

- `mcp_tool` is the server's own tool name, present only when it differs from
  `tool`. `extra` holds the call's other params, when it has any. `harness` and
  `hold_seconds` appear only when they apply.
- `arguments` and `extra` are whole and canonical, up to 32 KiB. `call_sha256`
  hashes them with the server and the tool name; Wardyn refuses a raise whose
  hash does not match.
- The run's owner or an administrator decides it, with no `decision_scope` or
  `once`. Any other scope is `400 decision_scope_invalid_for_kind`. With
  `WARDYN_CAPABILITY_SECOND_HUMAN` set, the run's creator cannot decide it. An
  approval rule never does.
- An approval sends that exact call once. A different call asks again.
- Each listed row of an MCP call carries `call_text`: `arguments` and, when the call has them, `extra` as the
  canonical JSON, indented, in text, and on a held kubectl write, its body as text (`""` when it has none). Show and
  copy it as it is. Reading `requested_scope` as JSON in a client that parses numbers as floating point rounds an
  integer past 2^53 and shortens a long decimal, while the approval binds the exact value. `wardyn approval get`
  prints the same text, with the tool, the server's own tool name and the server.

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

## Scheduled runs

- `correlation_id` on `CreateRunRequest` (`wardyn run --correlation-id`) is your own reference for the run, kept and returned on it. Every event of the run that reaches an audit sink carries it as `data.correlation_id` while the run's masking is held complete (a row labelled `mask_scope: globals_only` carries none); the stored trail and its hash chain do not, and neither does the acknowledged-delivery webhook. A template carries none. Erasing a person's `run_tasks` clears it; sinks drop it within 30 seconds, and exported copies follow the sink's retention.
- `start_at` and `start_time_zone` on `CreateRunRequest` start a run once, later: at most 6 days ahead for a background task (6 days 23 hours interactive), so it keeps its wait before the 7-day cap, when the server sets `WARDYN_SCHEDULED_RUNS` ([Scheduled runs](operations/runner-pools.md#scheduled-runs)).
- The run is `QUEUED` with `queue.reason` `scheduled`, `queue.start_at` and `queue.time_zone`; cancel it with `KillRun`.
- `GET /setup/status` carries `features.scheduled_runs`, so a client offers scheduling only when it is on.

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
- `credentials`: the Credentials total, each with its `delivery`, what is left once the run ends (`after_end`) and, for every credential but a model's, a `label` naming its kind and whose it is, never a secret's name. The repository on Wardyn's git broker lists no raw host; its credential is delivered `git_broker`. A cluster account (`kube_api`) also names its `namespaces` and its `access` (`read_only`, `restart`, `read_write`), and `approve_each: true` when a person approves each write.
- An AWS sign-in puts short-lived role credentials in the sandbox; the session's access token stays at the proxy. They expire at the lifetime AWS sets.
- `unlisted`: what happens to a host nothing lists: the effective `mode`, `allow_all`, `hold_sec` for `wait_for_review`, and `decider` (`you`, `admin` or `you_or_admin`, from your `egress_host` capability).
- `setup`: what is still needed, as checklist rows with a `fix`: `add_secret`, `sign_in` (`target`, and `id` for a component) or `choose_model_provider` (`tool`). A model-provider sign-in names no provider id; sign in to the provider the draft chose.
- A CLI component's row has kind `cli` and `in_image`: `yes` or `no` for a harness's own image, `unknown` for an image the run names or the operator pinned.
- `harnesses`: one `ToolPolicyFact` per included harness. It carries the lane its calls take, who enforces it, its default, the policy's locked rules, the effects a person may choose (`allow`, `hold`, `approve_each`, `deny`; `approve_each` is offered for rules only, and greyed with `approve_each_default` or `approve_each_no_remote_mcp`), whether rules may name any tool or MCP tools only, and the MCP servers turned on for it.
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

## Your walls

A person's own walls are limits they set on every run they start; they only narrow ([POLICIES.md](POLICIES.md#your-walls) lists the knobs). The client wraps the three routes.

| Method | Route | Answers |
|---|---|---|
| `GetMyWalls` | `GET /api/v1/me/walls` | `200` `{spec, updated_at?}`; `{spec: {}}` when none are set |
| `PutMyWalls` | `PUT /api/v1/me/walls` | `{spec}` replaces the whole document. `200` `{spec, updated_at}`; an empty spec is no walls |
| `ClearMyWalls` | `DELETE /api/v1/me/walls` | `204`; the stored document becomes `{}` |

- The routes act for the signed-in person only. A personal API token, a CLI session or a delegation acts as that person; no principal is read from the path or the body.
- The admin token, local mode and a service identity are refused `400` `walls_need_person`. A deployment whose store keeps no personal settings answers `501` `walls_unavailable`.
- A body that names a wider value, a site that is not a DNS name or `*.` wildcard, or a list over its cap is `400` `walls_invalid`, naming the first path. A body with an unknown key is refused by the strict decode.
- A write is audited as `person.walls.set` or `person.walls.delete`: the knobs named, the length of each list and the kinds kept out, never the lists.
- `wardyn walls` prints your walls as YAML, `wardyn walls set --file walls.yaml` (`-` reads stdin) replaces them and `wardyn walls clear` removes them. The file holds the knobs at the top level, and one that names no limit is refused: use `clear`.
- A run that your walls narrowed records `policy_source.personal_walls` on its `run.create` row, read back by `GET /runs/{id}/policy` as the cause `walls`.

### Narrowing one run

`overrides.egress.allow_all_egress: false` turns open egress off for one run, on any base: the default policy, a stored `policy_id`, a preset, a template or a run document.

- The run then reaches the base's listed hosts, the hosts in `overrides.hosts.add_allowed` and the hosts its own setup needs. The base's own hosts stay read-only.
- It is a narrowing, so it is always allowed and the ceiling has nothing to bound. On a base that already has open egress off it changes nothing.
- Add `overrides.egress.first_use_approval: always_deny` for default-deny. `true` still requests every other public host and is bounded by the ceiling.
- The client's `RunEgressOverrides.AllowAllEgress` is a `*bool`, and `Items()` reports `false` as the operation `narrow` and `true` as `set`.

```yaml
overrides:
  egress:
    allow_all_egress: false
    first_use_approval: always_deny
  hosts:
    add_allowed: [api.example.com, pypi.org]
```

## Wall presets

A wall preset is a named set of walls that only narrows, picked for one run at launch. Open, Read-only, Sealed and No network are built in; a person keeps their own, a group's template managers keep the group's, and an organisation administrator keeps the organisation's.

| Method | Route | Who |
|---|---|---|
| `ListWallPresets` | `GET /api/v1/wall-presets` | anyone signed in, the admin token and a service identity: the built-ins, then each preset they may read, and `publish` |
| `GetWallPreset` | `GET /api/v1/wall-presets/{id}` | a reader of that preset; `{id}` is a uuid or a built-in's name (`open`, `read-only`, `sealed`, `no-network`) |
| `SaveWallPreset` | `POST /api/v1/wall-presets`, `PUT /api/v1/wall-presets/{id}` | the owner (personal), an organisation administrator in the Admin view (organisation), the group's template managers (group) |
| `DeleteWallPreset` | `DELETE /api/v1/wall-presets/{id}` | as `SaveWallPreset` |

- The rules are the [run templates'](#run-templates): the same scopes, the same `publish`, and the same template managers. A preset you cannot read, and one that does not exist, answer the same `404` `wall_preset_not_found`.
- `WallPresetSaveRequest` on `POST` names `scope` (`person`, `group` or `org`), `group_id` for a group, `name`, `description` and `spec`. On `PUT` it names `name`, `description`, `spec` and `expected_version`, the version you read; the scope never changes.
- `spec` is the [Your walls](#your-walls) document and is checked the same way, as `422` `wall_preset_invalid`. A spec that names no limit is refused too: Open is the built-in for no preset.
- A built-in is never written (`403` `wall_preset_builtin`), and no preset may take a built-in's name in any case (`409` `wall_preset_name_taken`).
- Writes need a signed-in person (`400` `walls_need_person`). Who saved a shared preset last is answered as `updated_by`; for a personal preset it never is.
- A write is audited as `wall_preset.create`, `wall_preset.update` or `wall_preset.delete`. A shared preset's row records its whole spec; a personal one's records only the knobs and list sizes, as `person.walls.set` does.

## Run templates

A template is a saved run setup, whole or partial, kept for yourself, published to the organisation or published to one group. It is content: a published template grants nothing.

| Method | Route | Who |
|---|---|---|
| `ListTemplates` | `GET /api/v1/templates` | any signed-in person; each row is one they may read |
| `GetTemplate` | `GET /api/v1/templates/{id}`, `/revisions/{revision}` | a reader of that template |
| `SaveTemplate` | `POST /api/v1/templates`, `PUT /api/v1/templates/{id}` | the owner (personal), an organisation administrator (organisation), the group's template managers (group) |
| `DeleteTemplate` | `DELETE /api/v1/templates/{id}` | as `SaveTemplate` |
| `CopyTemplate` | `POST /api/v1/templates/{id}/copy` | a reader of the source who may write the target |
| `ListTemplateGroupAdmins`, `GrantTemplateGroupAdmin`, `RevokeTemplateGroupAdmin` | `GET`/`PUT`/`DELETE /api/v1/admin/template-group-admins` | admin; a grant makes one person a template manager of one group, for its templates and its [wall presets](#wall-presets) |

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
- A worked `coverage: partial` example for an Azure DevOps repository is in [AZURE-DEVOPS.md](AZURE-DEVOPS.md#a-template-for-contributing-to-a-repository).
- `wardyn run --file <doc>` (`-` reads stdin) has the door check a `Run` document, then creates the run from its `intent`. A document's `inline_policy` is sent as `policy_overlay`, so it lays onto your default policy and never replaces it. Only `--title`, `--description`, `--wall-preset`, `--wait`, `--timeout`, `--max-wait`, `--start-at`, `--time-zone`, `--machine-kind`, `--macos-base`, `--correlation-id`, `--json` and `--dry-run` go with it. `wardyn template import --kind Run|RunTemplate` sets the kind the import expects.

`policy_overlay` carries a document's `intent.inline_policy`: the keys it names replace your default policy's (your governance ceiling, or the deployment default); the rest keep the default's values. Only the named keys are bounded by your ceiling: the default's operator mounts and grant pairings stay. Beside `policy_id` or `inline_policy` it is `400` `policy_overlay_conflict`; beside `preset`, `preset_field_not_per_launch`. A null or empty overlay, an unknown key, a wrongly typed value, an excluded field or a redaction placeholder is `400` `policy_overlay_invalid`, naming the first path. Edits beside a saved policy travel as `overrides`.

`shape` (`agent_task`, `environment` or `application`) names the New Run choice a request was set up from. It is optional and needs `experience`; a value outside the three is `400` `run_mode_invalid`. The server checks it against the run and keeps it on the run's `shape`, which is otherwise the one shape the run fits (empty when several fit); a request that does not fit is `422` `run_shape_conflict` at create, preflight, preview and the import door.

| `shape` | Requires |
|---|---|
| `application` | `experience: background` and `workload.kind: command`; or, for a server (0.10), `experience: interactive`, `startup.kind: command` and `ui_apps` in `inline_policy`, `policy_overlay` or the `policy_id` policy |
| `environment` | `experience: interactive` |
| `agent_task` | With `background`, `workload.kind: agent_task`; with `interactive`, a `tools[]` entry of kind `harness` |

### Run again

`POST /runs` keeps each run's setup as a `Run` document for Run again. Only the run's creator reads it.

| Route | Answers |
|---|---|
| `GET /api/v1/me/launch-intents?limit=` | `{intents: [{run_id, title, shape?, created_at}]}`: your own runs that kept one, newest first; `limit` defaults to 20, at most 50 |
| `GET /api/v1/runs/{id}/launch-intent` | `{run_id, created_at, document}`, the document of kind `Run`; `404` `launch_intent_not_found` when the run kept none |
| `POST /api/v1/runs/{id}/desktop-signin` | `201` `{signin_id, expires_at, app}`: the agent is stopped and the person's display is open through the UI gateway as `app` (`_signin`). The run's creator only; `501` `desktop_signins_off` unless `WARDYN_DESKTOP_SIGNINS=on`; `403` under `deny_interactive` or `deny_ui_apps`, and the agent keeps running |
| `POST /api/v1/runs/{id}/desktop-signin/shares` | `{site}` → `{site, cookies}`: that registrable domain's cookies are copied into the agent's browser profile. After a share the agent holds them within its egress; `403` under `deny_interactive` or `deny_ui_apps` |
| `POST /api/v1/runs/{id}/desktop-signin/end` | `204`: the person's display is closed, then the agent resumed |

- Anyone but the run's creator, an administrator included, gets the `404` a missing run gets.
- The document is your request after a preset expands, without the fields chosen at each launch (title, description, `max_wait_sec`, `local_folder`) or the stamps of where it came from (`preset`, `preset_version`, `mode_contract`); `policy_overlay` becomes `intent.inline_policy`. A run that waits in a pool's queue keeps one too.
- A run keeps none when it sets a whole `inline_policy`, when the import door refuses the document (a secret-shaped task, say), or when an erasure of your launch intents is running (`erasure_in_progress`). It still starts; `run.create` says `launch_intent: not_kept` and why.
- Erasure's `run_tasks`, `components` and `run_setups` scopes delete a person's launch intents.
- The SDK has no method for these routes yet.

### Several harnesses in one run

- `tools[]` may include more than one entry of kind `harness` with `interactive`, or with `workload.kind: command`. A background agent task includes only `workload.agent`, unless it runs several agents (below).
- Each harness names its own `model_provider`, judged as its own choice. The primary harness is the `startup` harness, else the first listed; the run's `agent` and `model_provider` are the primary's.
- The run starts from the image the operator declared for the set, under a `WARDYN_AGENT_IMAGES` key joining the sorted ids with `+`. Without one the request is `422` `harness_set_no_image`; a published install declares none ([build-images.md](operations/build-images.md#the-image-a-runs-harnesses-start-from)).
- An AWS sign-in provider (`bedrock_sso`) serves only the primary harness. On any other it is `422` `request_field_unavailable`, with `tool` naming the harness.

### Several agents in one background run

0.10.0, behind `WARDYN_MULTI_AGENT_TASKS` (off by default), which `GET /setup/status` reports as `features.multi_agent_tasks`. Off, a request carrying `tools[].task` is `422` `multi_agent_tasks_off`; every other request is unchanged.

- A background agent task includes `workload.agent` and one or more other harnesses. `workload.agent`'s prompt is `workload.task`; every other harness carries its own `tools[].task`. Anything else is `422` `run_mode_conflict`: `tools[].task` on `workload.agent`, a harness without one, `workload.agent` not in `tools[]`, or `tools[].task` outside a background agent task.
- The agents share one sandbox: one policy, one proxy, the same files. They share the person's model sign-ins, as an interactive run with several harnesses does: one agent can reach another's provider host and be served that person's credential. Nothing crosses between people.
- Every agent's rules are met, strictest wins: each tool any agent names gets the strictest effect any agent gives it, for every agent. An agent that cannot enforce the met rules is refused, with `tool` naming it, as its own run with those rules would be (Codex CLI cannot hold a call). When any agent's rules need Wardyn's approval lane, the whole run is on it.
- Every agent meets the doors its own background run would: a governance profile that holds or denies refuses Codex CLI, and autonomy level L1 refuses an agent with no approval lane.
- `GET /runs/{id}` adds `agents`: each agent with its own `task`, the run's agent first. It is absent on every other run.
- The run ends when the last agent exits. It succeeds only if every agent exited 0, and fails if any agent failed.
- `GET /runs/{id}/output` adds `agents`: each agent's `state`, `running` or `exited` with its `exit_code`, as the last status line in the kept output reads. It is absent when no status line is in that output. `?agent=<id>` returns that agent's lines alone. Every line an agent prints is prefixed with its id in the combined output. The prefix and `?agent=` are for display only: an agent can print lines that look like another's.
- A held tool call's approval names the agent that asked in `requested_scope.harness`; an egress approval names none, since the agents share one proxy route: the agent whose tool-approval credential the call carried, never one the request body names. The agents share one user in the sandbox, so one agent could present another's credential; every agent holds the same met rules, so that changes the name on the card and never the decision.
- Needs a harnesses image from 0.10.0 or later: an older image's launcher has no `--tasks`.

## Kept machines

A kept machine is a person's macOS VM, kept on their own Mac between runs ([macOS VMs](operations/macos-vms.md#keeping-the-machine)). The refusal codes are in the [reason table](#reason-branch-on-this-never-on-errors-prose) as `kept_machine_*`.

| Method | Route | Who |
|---|---|---|
| `CreateRun` with `Machine` | `POST /api/v1/runs` | a signed-in person, for an interactive macOS VM run on their own Mac |
| `ListMyMachines` | `GET /api/v1/me/machines` | a signed-in person: their own machines |
| `DeleteMyMachine` | `DELETE /api/v1/me/machines/{id}` | the machine's owner |
| `ListKeptMachines` | `GET /api/v1/machines/kept` | the super admin: every person's machines, narrowed by `?owner=` and `?runner=`; answers `machines` and `total` |
| `DeleteKeptMachine` | `DELETE /api/v1/machines/kept/{id}` | the super admin |

The run's `machine` request field is `RunMachine`:

| Field | Meaning |
|---|---|
| `keep` | `true` keeps this run's machine when the run stops |
| `name` | The new machine's name, yours alone and unique among yours. Empty lets the server pick one. Only with `keep` |
| `resume` | The kept machine this run starts from, by id or name |
| `fresh` | With `resume`: delete what is on the machine and start it again from the Mac's current base. Only with `resume` |

- `keep` and `resume` are exclusive. Both together, `fresh` without `resume`, or a bad name is `400` `kept_machine_invalid`.

`GET /api/v1/me/machines` answers `MyMachines`:

| Field | Meaning |
|---|---|
| `machines` | The caller's machines, deleting ones included. Each has `id`, `name`, `owner_subject`, `runner_id`, `runner_name`, `base`, `base_prepared_at`, `state` (`creating`, `in_use`, `kept` or `deleting`), `run_id`, the run that holds it now or held it last (a normal stop does not clear it, so use `state`, `creating` or `in_use`, to tell whether it is held), `scrubbed`, `disk_bytes`, `created_at`, `last_used_at`, `expires_at`, and `delete_started_at` with `delete_reason` while a delete is finishing |
| `count` | How many machines the caller keeps |
| `limit` | The most the caller may keep: `macos_vm.max_kept_machines_per_person` |
| `idle_days` | The site's current idle limit in days: `macos_vm.kept_machine_idle_days`. It does not move a machine's own `expires_at`, which was set at its last stop |
| `allowed` | Whether the caller may keep another machine |
| `reason` | When `allowed` is false, the refusal code: `kept_machine_site_off`, `governance_unit`, `kept_machine_not_your_mac` or `kept_machine_person_limit` (a resume still works) |
| `worst_bytes` | The most disk one new kept machine can take |

`GET /api/v1/runs/{id}` adds `machine` for a run that kept or resumed one. The Go client's `GetRunMachine` reads it as `RunMachineView`:

| Field | Meaning |
|---|---|
| `id` | The machine's id |
| `name` | Its name |
| `mode` | `new`, `resumed` or `fresh`: what this run did with it |
| `keep_at_end` | `true` while the machine still exists and is kept when the run stops |
| `state` | The machine's state now: `creating`, `in_use`, `kept` or `deleting` |
| `scrubbed` | Whether its last stop scrubbed it |
| `runner_name` | The Mac it is kept on |
| `holder_run_id` | The run that holds it now, or held it last. A stop does not clear it |
| `base` | The base it was made from |
| `disk_bytes` | What its last stop wrote |
| `expires_at` | When it expires if unused |
| `deleted` | `true` when the machine no longer exists. The row is then `id`, `mode`, `scrubbed` (false) and `deleted` |

- The row is the machine as it is now, shared by every run that used it. It leaves out `last_used_at`, which a resume overwrites.

`DELETE /api/v1/me/machines/{id}` answers `204` once the Mac has deleted the machine, or the Mac is no longer registered. It answers `202` with the machine while the Mac is offline, and the delete finishes when the Mac is back. `DELETE /api/v1/machines/kept/{id}` answers `200`, or `202` while the Mac is offline, with `id`, `name`, `owner`, `runner_id` and `outcome` (`deleted`, `already_absent`, `deferred` or `record_only`). For both:

- A machine a run holds is `409` `kept_machine_in_use`. A delete already started is `409` `kept_machine_deleting`.
- An id that is not yours, or is no machine, is `404` `kept_machine_not_found`. The admin route answers `404` only for no machine.
- `503` `kept_machine_delete_failed` means the Mac refused and the machine is kept. `503` `kept_machine_delete_unconfirmed` means the Mac's answer is unknown and the delete is retried.
- A delete is audited as `machine.delete`, and an admin's as `by: admin`.

## Model providers

`GetModelProviders` (`GET /api/v1/model-providers`, admin) answers a `ModelProvidersRead`: the provider records, and per provider id how many people hold a credential of their own for it. It returns configuration and counts, never a credential value. Changing providers, signing in and storing a credential stay in the console.

`GatewayIdentityBedrockSSO` (`gateway_identity_bedrock_sso`) is true when a `bedrock_sso` provider can send run identity on this install, which needs `WARDYN_AWS_SIGV4_AT_PROXY` on.

## Idempotent run create

`POST /api/v1/runs` honours an `Idempotency-Key` header. Send a new value, such as a UUID, for each run you mean
to launch, and the same value when you retry that launch.

- The value is 1 to 255 visible ASCII characters. An RFC 8941 string in quotes is read without its quotes.
- A key belongs to its caller. A portal acting for a person has a scope of its own.
- For 24 hours from first use, a retry with the same key and body gets the first answer back byte for byte, with
  `Idempotent-Replayed: true`. That is the same status, `Location` and body, a refusal made after the run was
  inserted included. A refusal before the insert keeps nothing, so a retry after you fix its cause is judged afresh.
- When the first answer was not kept, or was erased with the person's run tasks, the replay is a `201` rebuilt
  from the run, without the first answer's warnings.
- The same key with another body or break-glass reason gets `422` `idempotency_key_reused`. A retry while the first
  request is still being answered gets `409` `idempotency_key_in_flight` with `Retry-After: 1`.
- `/healthz` reports `"idempotent_run_create": true` when the server honours the key.
- Retrying is safe only once every control-plane replica runs 0.10.0 or later. An older replica ignores the key and
  makes a second run.

`Client.CreateRun` sends a fresh UUID as the key and makes one request. A lost answer comes back as the error; the call is never repeated for you. `CreateRunWith(ctx, req, CreateRunOptions{...})` adds:

- `IdempotencyKey`: the key to send. Use one value for every re-run of a job.
- `Retry: true`: up to three attempts, with the same key and bytes, after 1 s then 2 s. It retries a transport error, `409` `idempotency_key_in_flight` (after `Retry-After`, at most 5 s) and a `502`, `503` or `504` without a Wardyn `reason`; anything else is returned.
- Before retrying a transport error or gateway `5xx`, it reads `/healthz`. Without `idempotent_run_create` it returns the error, noting the run may exist.
- `CreateRunResult.Replayed`: the answer came from an earlier create with the key.

`/healthz` shows only the replica that answers, so `Retry` is unsafe while replicas run mixed versions.

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
    closed set in [`internal/api/reasons.go`](../internal/api/reasons.go), `reasons_routes.go`, `reasons_revive.go` and `reasons_governance.go` (a reason two
    routes share — the dispatch-time-snapshot family below, the hold-chain
    terminal/exhausted pair — means the same whichever
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
| `runner_proxy_setting_unsupported` | The `reason` on a run's `run.create` failure row: the run sets a proxy setting (`aws_sigv4`, `scrub_responses`, `vm`, `port_rule`, `local_models`, `kube_api`, `canonical_push_repos`, `gateway_identity`, `git_push_withheld`, `forge_hosts`, `allowed_models`, `aws_sigv4_gateways` or `local_model_relay`) its older runner cannot carry; upgrade its wardyn-runnerd. A revive of a run with `allowed_models` on such a runner is refused `409` with it. |
| `gateway_identity_changed` | A run asked for a gateway token, or a revive was asked for, and its model provider was changed or turned off after the run started: `403` on the token, `409` on the revive. Create a new run, or edit the provider back. |
| `gateway_identity_unavailable` | `503`: Wardyn couldn't read what a gateway token depends on, or the save that turns gateway identity on couldn't make the signing key, so nothing was issued or saved. |
| `gateway_access_cut` | A run asked for a gateway token, or a revive was asked for, and the portal that launched the run was revoked with `?cut_in_flight=true`: `403` on the token, `409` on the revive. Permanent for that run; start a new run. |
| `invalid_cut_in_flight_param` | `DELETE /admin/delegates/{id}`, `400`: `cut_in_flight` is not `true` or `false`, is given twice, or the query does not parse. Nothing was revoked. |
| `runner_local_model_port_not_allowed` | The `reason` on a run's `run.create` failure row: the run's local model server is on a port the runner's person has not allowed with `--local-model-port`. |
| `node_record_invalid` | `POST /runs/{id}/node-record`, `400`: `attestation` is not `node_powered_off` or `no_prior_holder`, `source` is not `provider_console`, `management_controller` or `other`, `source_id` is missing, longer than 256 characters or holds control characters, or `tuple` is missing. |
| `node_record_refused` | `POST /runs/{id}/node-record`, `409`: the run is not held as containment pending, the runner's record (`tuple`) changed since the form was loaded, `no_prior_holder` was sent after a recorded name reuse or for a run with a causal node association, or another stop for the run already waits for its runner. A held record whose run moved before its approval is refused at the approval the same way. |
| `node_record_human_only` | `POST /runs/{id}/node-record`, `403`: the admin token or an API token made it, or approved a held one (a node record is made and approved from a signed-in session), or the record needs two people and the server runs in local mode, which cannot show who they are. |
| `sign_in_required` | `401` on the public API of a deployment with single sign-on and no admin token, for a request that carries neither a session nor a bearer token: sign in through single sign-on. A deployment with neither single sign-on nor an admin token keeps `admin_token_not_configured`; the `auth.fail` row is the same `admin_token_not_configured` either way. |
| `host_capacity_refused` | `503` with `Retry-After: 30` on every run launch door (new run, preflight, record, harness sign-in, source scan, site-config probe, workspace build): the host is short of memory or over its load ceiling (`WARDYN_HOST_MIN_MEM_AVAILABLE_MIB` / `WARDYN_HOST_MAX_LOAD1`). The body is the one exception to the `error`/`reason` shape: `error` is this code, `reason` names each limit crossed with its measured value, and `message` is the sentence to show. Wait and retry. |
| `runner_limits_required` | `422`: a registration that declares no limits, or limits that are not a valid set. Register with this release's `wardyn-runnerd`. |
| `runner_session_required` | `/me/runners` needs the person's own session; administrative and delegated credentials own no runner. |
| `runners_enabled_required` | `PUT /runners/settings` needs `enabled` or `require_managed_device`, each true or false. |
| `runners_org_url_invalid` | Runners cannot be turned on until the organisation and control-plane URLs are HTTPS. |
| `site_config_runners_via_own_route` | Change `runners.enabled` or `runners.require_managed_device` with `PUT /runners/settings`. |
| `scheduled_runs_denied` | `403`: `start_at` under a profile that sets `deny_scheduled_runs`. A booked run ends `FAILED` with it. |
| `component_denied` / `model_provider_denied` | `403`: the run attaches a component, or would use a model provider, that your profile's `deny_components` or `deny_model_providers` lists, at create, Review and preview. A queued run ends `FAILED` with it at its start. Operators are not exempt. |
| `kept_machine_site_off` | `macos_vm.allow_kept_machines` is not true: this install keeps no machines. |
| `kept_machine_not_your_mac` | `403`: the caller is not a person, or has no Mac of their own outside other people's pools. |
| `kept_machine_person_limit` | The caller already keeps `macos_vm.max_kept_machines_per_person` machines. |
| `kept_machine_not_found` | `404`: no kept machine of the caller's has that id. |
| `kept_machine_in_use` | `409`: a run holds the machine, so it can't be deleted. |
| `kept_machine_deleting` | `409`: a delete of the machine has already started. |
| `kept_machine_delete_failed` | `503`: the Mac refused the delete before removing anything; the machine is kept. |
| `kept_machine_delete_unconfirmed` | `503`: the Mac's answer was lost or was not a refusal, so the machine may already be gone; the delete stays pending and is retried until the Mac confirms it, and the machine can't be resumed meanwhile. |
| `kept_machine_invalid` | `400`: the run's `machine` is malformed: both `keep` and `resume`, `fresh` without `resume`, or a name that is not a valid machine name. |
| `kept_machine_interactive_only` | `422`: a machine is kept or resumed only by an interactive macOS VM run. |
| `kept_machine_resident_secrets` | `422`: the run would put a lasting credential (an environment or file secret) inside the kept machine. |
| `kept_machine_with_work_folder` | `422`: `keep_work_folder` was asked beside a kept machine. |
| `kept_machine_expired` | `410`: the machine passed its idle limit; it is deleted, or its delete has started. |
| `kept_machine_base_too_old` | `409`: the machine's base is older than `macos_vm.max_base_age_days`; start it fresh. |
| `kept_machine_offline` | `409`: the Mac is offline, or is not the Mac the machine is kept on. |
| `kept_machine_runner_too_old` | `409`: the Mac's runner does not keep machines; update `wardyn-runnerd`. |
| `kept_machine_mac_limit` | `422`: another machine would pass the Mac's kept-disk limit, or the Mac keeps no machines. |
| `kept_machine_name_taken` | `409`: the caller already keeps a machine of that name. |
| `kept_machine_unavailable` | The run was not launched: its dispatch could not make or claim the machine. |
| `kept_machine_fresh_base` | `422`: a fresh start names a base other than its machine's own; start a new kept machine to use a different base. |
| `kept_machine_reset_unconfirmed` | `409`: a fresh start of the machine was asked of its Mac and never confirmed, so only another fresh start may use it. |
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
| `roster_unreadable` | The site configuration could not be read. Azure DevOps, AWS SSO, a service identity's `GET /workspaces` and `GET /me`, and a Record Mode promote (`503`). |
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
| `ado_capabilities_capped` / `github_capabilities_capped` / `capability_capped` | The boundary's or a restriction's capability cap (`vcs_capability_cap.go`): a launch whose cap leaves the Azure DevOps lane or the GitHub CLI nothing (or a policy list past it), and an Azure DevOps request or resolve for a capability the cap leaves out. |
| `approval_mismatch` / `approvals_unreadable` / `once_unspendable` | The named approval does not match, could not be read, or was already spent. Azure DevOps; `approvals_unreadable` also AWS SSO's re-auth hold. |
| `signin_closed` / `signin_holds_exhausted` | The hold chain has gone terminal (cancelled, expired, denied) or hit its per-run cap — see `injection_ado_signin.go`'s sign-in hold and `injection_awssso.go`'s re-auth hold, the same shape under two names. |
| `raise_failed` | The approval store itself errored while raising a capability, consent, sign-in or re-auth hold. Azure DevOps, AWS SSO. |
| `preset_unknown` / `preset_field_not_per_launch` / `preset_version_changed` / `preset_version_without_preset` | A `POST /runs` naming a launch preset: no such preset (or not open to the caller's user type, `422`), a field other than `title`/`task` beside `preset` (`400`), a pinned `preset_version` that is no longer current (`409`), or `preset_version` without `preset` (`400`). See OPERATIONS.md's launch presets section. |
| `not_captured` / `dead_credential` / `consent_required` / `interaction_required` / `unavailable` | `ADOEntraFailure`'s own closed enum ([`internal/api/ado_entra_store.go`](../internal/api/ado_entra_store.go)), carried through unchanged when the redemption classifies a renewal failure. Azure DevOps. |
| `invalid_approval_state` / `invalid_run_id_param` / `invalid_view_param` / `invalid_decidable_by` / `invalid_owner_param` / `invalid_status_param` / `status_needs_exclusive` / `status_needs_requires_view` / `invalid_ended_within_param` / `invalid_include_killed_param` / `invalid_sponsored_param` / `invalid_no_end_param` / `runs_search_query_too_long` / `invalid_limit_param` / `invalid_offset_param` | A `GET /approvals` or `GET /runs` query parameter is malformed or conflicts with another one. `invalid_view_param` is the same reason on both routes (the same shape); `invalid_limit_param`/`invalid_offset_param` are `parseListPage`'s, shared by every paginated list route in `internal/api` (`GET /audit`, `/policies`, `/secrets`, `/user-drives`, `/api-tokens`, `/permissions/grants`, `/setup/integrations`, `/ssh-keys`, `/runs/policy-history`, …). |
| `listing_unscoped_backend` | The store backend cannot scope this listing to the caller's own runs — the SAME missing capability on `GET /approvals` and `GET /runs`. |
| `approval_not_found` | The named approval does not exist, or the caller may not see it — `POST /approvals/{id}/{approve,deny}` and every other approval-lookup route, INCLUDING `GET /approvals/{id}/paths`: a foreign approval and a missing one answer byte-identically there, reason and body both, so the reason itself is not an existence oracle. |
| `approval_run_ended` / `approval_run_past_lifetime` / `approval_already_decided` / `credential_reauth_not_decidable` / `decision_scope_invalid_for_kind` / `invalid_request_body` | `POST /approvals/{id}/{approve,deny}`'s pre-decision refusals: the run ended first, the run reached its lifetime cap first, the approval was already decided, a credential-reauth approval takes a sign-in instead, `decision_scope` was sent on a kind that does not accept one, the body did not decode, or `reason` is over 512 characters. A body over 16 KiB is `413` `request_body_too_large`. |
| `takeover_not_decidable` / `not_a_takeover_request` / `takeover_not_holder` | "Needs you" (`human_takeover`) requests: `POST /approvals/{id}/{approve,deny}` on one (it is answered with `take-over` or `decline`, for every tier); `take-over`, `hand-back` or `decline` on an approval of another kind; `hand-back` by someone who does not have control. |
| `invalid_decision_scope` / `decision_scope_until_needs_expiry` / `decision_expiry_in_past` / `decision_expiry_too_far` / `decision_expiry_without_until` | The `decision_scope`/`decision_expires_at` shape rules (0-3) on a decide call. |
| `decision_scope_always_unavailable` / `decision_always_no_workspace_link` / `decision_always_invalid_host` / `decision_always_workspace_gone` / `decision_always_host_builtin` / `decision_always_host_denied` | The operator-only `decision_scope=always` persistence chain (rules 5-7): no store to persist to, the run names no workspace, the host does not parse, the workspace is gone, or the host is built-in-routed or on the reject list. `decision_scope_always_unavailable` also covers the internal push-content raise route's own "no store" arm (the identical `s.cfg.Store == nil` cause, one route apart). |
| `egress_second_human_local_mode` | `WARDYN_EGRESS_SECOND_HUMAN` (or `WARDYN_CAPABILITY_SECOND_HUMAN`) cannot be enforced in local mode (nobody is authenticated to prove a second human decided). |
| `invalid_push_scope` / `invalid_push_path_list` / `push_content_unattended` / `push_path_list_count_unavailable` / `push_path_list_cap_reached` / `push_not_held` / `push_path_lists_require_postgres` | The internal sidecar's push-content raise route (`POST /api/v1/internal/approvals`, `kind: push_content`) and `GET /approvals/{id}/paths`: a malformed raise, an unattended run's push refused rather than held, a per-run path-list cap, or a route that needs a Postgres-backed capability this backend lacks (`push_path_lists_require_postgres` also covers the raise route's own store-type-assertion arm, the identical cause one status apart). |
| `workspace_seed_store_unavailable` / `workspace_seed_unreadable` / `workspace_seed_source_target_invalid` / `workspace_seed_no_base_image` / `workspace_seed_policy_conflict` | `POST /runs`' `workspace_id` resolution (`seedRequestWorkspace`): no store configured, the workspace could not be read, a stored source's target fails the authored-target deny-list, an exec run named a workspace with no base image and no `--agent`/`--image`, or the seeded sources conflict with the policy's own mounts/repos. |
| `placement_invalid` / `resources_invalid` / `workspace_target_invalid` / `override_invalid` / `allowed_image_invalid` / `runner_pool_invalid` | `400`: a malformed `placement`, `resources`, workspace `target`, `overrides` entry, `allowed_image` or `runner_pool_id`; `422` for a target on a workspace with several repositories or folders. |
| `workspace_target_overlap` / `workspace_pin_conflict` / `image_conflict` / `workspace_ado_org_conflict` | `422`: the attached workspaces cannot share one run: equal or nested paths (two repositories in one directory included), two model-provider pins, base images or Azure DevOps organisations. |
| `image_digest_required` | `403`: the organisation's `images.require_digest` setting is on and a member's image (or any workspace's base image) is not pinned `image@sha256:<digest>`. Operators are not bound. |
| `image_admission_no_policy` / `image_signature_missing` / `image_signature_invalid` / `image_attestation_missing` | `403`: image signature checks (`images.admission` set to `enforce`, or a profile's `required_image_signers`) found, in order, no signer covering the image (decided before any registry request), no signature from a covering signer, a signature by another identity or key, or a missing required attestation. Ungoverned operators are not bound; a governed admin is. See [image signature checks](operations/image-signature-checks.md). |
| `image_admission_unavailable` | `503`: Wardyn could not check the image, and the sentence names the cause: the registry check got 401 or 403 or no answer, a TLS certificate it met is untrusted (`WARDYN_TRUSTED_CA_FILE`), a trust root could not be read, `WARDYN_REGISTRY_AUTH_FILE` could not be read, or the check took longer than 20 seconds. Never cached; retry. Under `audit` it is a warning, not a refusal. |
| `revive_image_not_admitted` | `POST /runs/{id}/revive` and the admin bulk restart, `403`: image admission is enforced (or the owner's profile requires an image signer) and the run has no digest admission checked and pinned when it started (admission was off, in audit mode, or did not check its image). A revive re-checks only the pinned digest, never the tag. A run on an image Wardyn built for it (a devcontainer or a generated workspace image) is not refused. Under scope all, a run that named no image and recorded no image origin is refused: one from an earlier release, or whose origin record failed to write under off or audit. Start a new run. |
| `workspace_selection_invalid` / `run_ref_invalid` | `400`: a malformed `workspaces[]` list (an id twice or malformed, over 32, or `workspace_id` not first) or `repos` entry (a repo twice, or a ref git could read as an option). |
| `workspace_repo_unknown` | `422`: a `repos` entry the workspace does not carry, or the `repo` of `GET /workspaces/{id}/branches` is not one of its repository sources. |
| `placement_unavailable` / `placement_credential` / `placement_capability` / `placement_local_path` / `placement_trusted_output` | `422`: a `placement: "local"` run with no user pool of yours, or one carrying a credential, mount, storage, path or trusted-output operation a runner cannot take. A credential is judged at the trust class of each eligible runner, highest first, and the detail names the class it was refused at. A `machine_kind: "linux_vm"` run with `interactive_start: "agent"` is a `placement_capability` too: a Linux VM's terminal opens a shell, not the agent CLI, in this release. So is a `linux_vm` run that carries a folder, on a runner that cannot share one into the guest (older than VM folder shares, or no working `virtiofsd`). |
| `runner_trust_changed` | `409`: a revive of a run on a runner whose trust class is no longer the one the run's credential plan was admitted at (or was never recorded). Launch the run again. |
| `runner_trust_required` | `422`: a `placement: "local"` run whose eligible runners are all `personal` trust, while `runners.require_managed_device` is on (off by default). Mark a runner `managed_device` or use an organisation's runner. |
| `runner_posture_unmet` | `422`: a `runner_resident` credential the runner's reported posture does not meet (the detail names every unmet field and the requirement; a runner that reports no posture fails every posture requirement), a run with a CC3 floor, which a runner-asserted class never meets for a resident credential, or a `macos_vm` run on a Mac whose posture does not say `mdm_managed` from the MDM's posture file (the managed-Mac gate; a Mac not marked `managed_device` is `placement_capability`). |
| `runner_keystore_unavailable` | `422`: the run's runner could not hold a `runner_resident` value: its OS keystore is unreachable or refused the write, or (macOS) the value is too long for the keychain. The run fails at dispatch, before its sandbox is created. |
| `placement_denied` | `403`: your governance profile's `deny_placements` refuses the run's placement, at create, preflight, policy preview and source scans; audited `authz.denied` (target `runs.placement`). At revive the same `403` is audited `run.revive` denied; at dispatch the run fails and is audited `run.dispatch` failure with this `reason`. A managed-harness sign-in is exempt at every door; a site-config probe at create and dispatch. |
| `runner_offline` | `422`: a `placement: "local"` run whose runner is offline or has not reported its capabilities yet, or none of your runners is online. `503` on the UI gateway's relay: the runner holding the run is offline; retry once it reconnects. |
| `runner_at_capacity` | `422`: a revive onto a runner at one of its limits (Runs at once, or a total); or a launch that lost the runner's last room to another launch between placement and the insert (launch it again). A launch whose runners are all at their limits waits in the pool's queue instead. |
| `runner_limits_unset` | `422`: no runner of the pool that could take the run has declared its limits. |
| `runner_recording_off` | `422`: the organisation or the pool requires recording (`recording: required`), and no runner of the pool that could take the run records: one started with `--recording-store off`, or one older than per-run recording. A macOS VM pool needs no recording runner: the organisation records it. |
| `recording_store_off` | `422`: the organisation or the pool requires recording, and the organisation keeps no session recordings. Saving `recording: required` (a pool, or the organisation's defaults) while it keeps none is refused with it too. |
| `override_refused` | `422`: a widening, base-host removal or foreign secret the narrowing table never allows. |
| `request_field_unavailable` / `main_drive_unenforceable` | `422`: a field this server cannot honour yet, refused and never dropped; or a disk size the run's policy sets that the pool's runners cannot cap. |
| `harness_not_installed` / `harness_set_no_image` / `harness_set_custom_image` / `harness_providers_conflict` | `422`: no image here carries a harness, or the harnesses together; two or more harnesses name a custom image; or two harnesses' providers share a host. `tool` names the harness. |
| `keep_work_folder_disabled` | `POST /runs` and preflight with `keep_work_folder: true`: `422` the organisation does not allow member drives, or `storage.user_drive.member_create.keep_main_drive` is off. The other refusals are the member-drive ones (`member_drive_requires_person`, `member_drives_denied_by_profile`, `member_drive_unsupported`, `member_drive_delete_unavailable`, `member_drive_limit_reached`), and `workspace_target_overlap` when a mount covers `/home/agent/work`. |
| `drive_selection_conflict` / `drive_invalid` / `drive_not_found` / `drive_not_in_pool` / `drive_target_conflict` / `drive_share_beside_managed` / `drive_one_disk` | `drives[]`: `400` `drive` beside `drives`, or a bad id; `422` an unreachable id, an unmountable pool, two drives on one path, a Kubernetes share beside managed, a second macOS VM disk drive. |
| `member_drive_requires_person` / `member_drive_rate_limited` / `member_drive_name_invalid` / `member_drives_disabled` / `member_drives_denied_by_unit` / `member_drive_unsupported` / `member_drive_delete_unavailable` / `member_drive_name_taken` / `member_drive_limit_reached` | `POST /me/drives`, a person's own drive: `403` not a person's session or own token; `429` over 6 creates and deletes a minute; `400` a bad name; `422` the organisation switch is off, no managed backend or drive size here, or a substrate that could not delete what it creates; `403` a governance profile denies it; `409` a drive you can use has the name; `422` you hold the most drives allowed. |
| `member_drive_in_use` / `member_drive_busy` / `member_drive_delete_failed` | `DELETE /me/drives/{id}` (also `member_drive_requires_person`, `member_drive_rate_limited`, `member_drive_delete_unavailable`): `409` a live or kept run holds the drive, or another delete is in flight; `503` the substrate failed and nothing was removed. A drive that is not yours, an admin drive and an unknown id all answer `404` `drive_not_found` with the same body. |
| `member_drive_delete_deferred` / `member_drive_record_only` | `DELETE /me/drives/{id}` of a disk drive on a Mac: `202` the Mac is offline, and the disk goes when it reconnects; `200` the Mac is no longer registered, so only the record went. The body carries `reason` and `message`. |
| `idempotency_key_invalid` / `idempotency_key_reused` / `idempotency_key_in_flight` | `POST /runs` with an `Idempotency-Key` (see [Idempotent run create](#idempotent-run-create)). `400`: the header is not one value of 1 to 255 visible ASCII characters. `422`: the key was used with a different body or break-glass reason in the last 24 hours; use a new key for a new run. `409` with `Retry-After: 1`: a request with the key is still being answered; retry. |
| `model_usage_unsupported` / `model_usage_read_failed` / `model_usage_write_failed` | `GET /runs/{id}/model-usage`: `501`, this store keeps no model usage; `500` `model_usage_read_failed`, the run's usage rows could not be read, retry. The proxy's usage record: `500` `model_usage_write_failed`, it could not be stored; a store with no model usage accepts and drops it. |
| `user_drive_member_owned` | `409`: `PUT`/`DELETE /drives/{id}`, `POST /drives/grants` or `POST /drives/{id}/reclaim` named a person's own drive; a super admin deletes it with `DELETE /drives/member/{id}`. |
| `run_mode_required` / `run_mode_invalid` / `start_folder_invalid` | `400`: a new client's mode (or a background task's workload) is unset, and is never inferred (`mode_contract: "0.9"` marks a request as a new client's even when it carries no other run-mode field); a run-mode carrier is malformed; a `start_folder` is malformed, names an attachment or a repository the run lacks, or leaves its mount. |
| `run_mode_conflict` | `422`: run-mode carriers contradict each other, or an older field they replace (`agent`, `task`, `interactive`) rides beside them. The CLI's `--agent` / `--task` (and `--interactive`, `--task-mode`) with no run-mode flag are the deliberate exception: that request carries no `mode_contract` and stays the older-client shape. |
| `multi_agent_tasks_off` | `422`: the request gives an agent its own `tools[].task` while `WARDYN_MULTI_AGENT_TASKS` is off. Launch one agent per run, or turn the switch on. |
| `policy_overlay_invalid` / `policy_overlay_conflict` | `400`: an unusable `policy_overlay`, or one beside `policy_id` or `inline_policy` (see [Run documents](#run-documents)). |
| `run_shape_conflict` | `422`: `shape` does not fit the run: it needs `experience`, and `application`, `environment` and `agent_task` each require the workload or tool shown under [Run documents](#run-documents). |
| `correlation_id_invalid` | `422`: `correlation_id` is over 128 characters or holds anything but ASCII letters, digits and `. _ : / -`. |
| `run_service_shape` | `422`: `service` rides a run that is not a background run of a command: it needs `experience` `background` and `workload` kind `command`. A VM `machine_kind` is `request_field_unavailable`. |
| `run_application_ui_apps_denied` | `422`: `POST /runs`, `POST /runs/preflight` and `POST /runs/policy-preview`: `shape` `application` declares `ui_apps` that the governance profile's `deny_ui_apps` strips or its allowed apps lack, or an inbound app that `deny_interactive` strips, and the sentence says which. Other shapes keep the strip and the warning. |
| `run_background_watch_site_off` | `403`: watching a background macOS run's desktop while the site config's `macos_vm.allow_background_watch` is `false`. A profile's `deny_background_desktop_watch` answers as `governance_unit` instead. |
| `run_background_watch_not_background` / `run_background_watch_old_boot` / `run_background_watch_unconfirmed` / `run_background_watch_offline` / `run_background_watch_control` / `run_background_watch_paused` | `409`: `POST /runs/{id}/desktop/watch-ticket`, the view-only ticket to a background macOS run's desktop, or that ticket at the gateway: a run that is not a background task; a boot from before its helper was asked to force the view; no report that this boot's view is forced, or caps that do not say `observe`; a runner that is offline; a Mac that offers desktop control; a paused run. `403` on the same reasons when a re-check refuses an open view. |
| `mac_window_site_off` / `mac_window_not_macos_vm` / `mac_window_not_your_mac` / `mac_window_unrecorded` | `403`, `409`, `403`, `403`: `POST /runs/{id}/desktop/window`, or `desktop_window: "at_start"` at create, which opens a window onto a macOS VM run on the Mac of the caller's own runner. `macos_vm.allow_attended_window` is not `true`; the run is not a macOS VM run; the Mac's runner is not the caller's; the recording level is Screen and actions, where a window on the Mac isn't recorded (its sentence: "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead."). A profile's denial answers as `governance_unit`. A 30 second re-check asks again and ends the window on any refusal, or any check it cannot make. |
| `mac_window_offline` / `mac_window_runner_too_old` / `mac_window_not_here` / `mac_window_desktop_control` / `mac_window_paused` / `mac_window_preparing` / `mac_window_open` / `mac_window_not_at_mac` / `mac_window_lock_unknown` / `mac_window_failed` | `409` (`502` for `mac_window_failed`): the runner is offline, cannot open a window, or is connected through another server (use a single replica or sticky runner routing); a browser desktop with control did not close in time; the run is paused, still being prepared, or already has a window; the person is not signed in at the unlocked Mac; the Mac can't tell whether its screen is locked, so the window stays closed; the runner could not open it. `GET /runs/{id}` has `macos_preparing: true` while setup runs. |
| `mac_window_revoked` | `403`: the caller's session was cut off; at a re-check it closes the window as `revoked`. |
| `desktop_window_invalid` | `400`: `desktop_window` is not `at_start`, or the run is not a macOS VM run. |
| `run_background_only` | `409`: an interactive door of a background task (terminal, exec, SSH, web gateway, desktop). Logs, audit, status and the lifecycle controls (kill, revive, extend) stay. The one exception is the built-in desktop of a background macOS VM run, which its owner can watch view only through `POST /runs/{id}/desktop/watch-ticket` (the `run_background_watch_*` rows above). `POST /runs/{id}/attach/ticket`, with `?purpose=ui` too, answers `run_background_only` for a background run: use the watch-ticket call. A declared noVNC app, the desktop's files and a desktop sign-in still answer this. |
| `template_document_invalid` / `template_field_invalid` / `template_field_unknown` / `template_version_unsupported` | `400`, `422` for the version: a document that is not one bounded document, a value of the wrong shape (null and redaction placeholders included), a key the schema lacks, or an `api_version` or `kind` this server does not read. |
| `template_secret_refused` / `template_run_state_refused` / `template_metadata_in_content` / `template_field_excluded` | `400`: a template never holds a secret value, state of one run (identity, session, claim, admission), an owner, scope, group or revision, or a launch-only (title, description) or retired field. The path is in the diagnostic. |
| `template_field_unavailable` / `template_dependency_missing` / `template_coverage_invalid` / `template_shared_field_refused` | `422`: a field this server cannot honour yet (refused, never dropped), a field whose dependency is neither included nor listed under `needs_setup`, a coverage that does not fit the content, or a field that names one person's own thing in an organisation or group template. |
| `template_scope_forbidden` / `template_group_unverified` / `template_not_found` / `template_revision_conflict` | `403`, `403`, `404`, `409`: publishing to a scope the caller does not administer, group membership that cannot be verified from this sign-in, no such template or not visible to the caller, or an update against a revision that has moved. |
| `templates_unavailable` | `501`: this server's store keeps no templates. Every `/templates` route except `POST /templates/import`, and the `/admin/template-group-admins` routes, answer it; the Postgres store keeps them. |
| `template_name_taken` / `template_limit_reached` | `409`: another template available to the same people already uses the name (compared case-insensitively, per person, per group and for the organisation), or that audience already holds its most templates (200 per person, 200 per group, 500 for the organisation). |
| `launch_intent_not_found` | `404` on `GET /runs/{id}/launch-intent`: the run kept no setup. A run you did not create answers the `404` a missing run does. |
| `desktop_sign_ins_off` / `desktop_sign_ins_unavailable` / `desktop_sign_ins_pause_failed` / `desktop_sign_ins_resume_failed` | The `/runs/{id}/desktop/sign-ins` routes of a Linux VM run. `403`: sign-ins are Not allowed for the run's agent, or its desktop keeps none. `409`, `501` or `503`: the run has no sign-in store, or it could not be read. `502`: the pause or resume did not reach the agent. |
| `desktop_signins_off` / `desktop_signin_runner_unsupported` / `desktop_signin_not_running` / `desktop_signin_no_desktop` | `501`, `409`, `409`, `409` on the `/runs/{id}/desktop-signin` routes: sign-ins are off on this deployment; the run started without sign-ins: its runner is not this deployment's own Docker one (a self-hosted runner, a remote-hosted runner or Kubernetes), its desktop image is not the sign-in image, or it started while sign-ins were off; the run is not running; its policy declares no `novnc` app. |
| `desktop_signin_image_not_granted` | `409` on `POST /runs/{id}/desktop-signin`: the run's image was your own choice and no grant of yours names its sign-in variant (`agent-novnc-signin` at the same registry and tag), so the run kept its image and started without sign-ins. |
| `desktop_signin_open` / `desktop_signin_not_open` / `desktop_signin_not_holder` | `409`, `409`, `403`: a sign-in is already open on the run; none is open (or its end is pending); it was begun by someone else. |
| `desktop_signin_pause_failed` / `desktop_signin_start_failed` / `desktop_signin_end_failed` | `502`: the agent could not be stopped and proven stopped (it was resumed, or, when the resume itself cannot finish, it stays stopped and Wardyn retries); the person's display did not start (the agent was resumed); the display could not be proven closed (the agent stays stopped and Wardyn retries). |
| `desktop_signin_site_invalid` / `desktop_signin_share_failed` | `400`, `502` on `POST .../desktop-signin/shares`: the site is not a host name under a public suffix; the export or import failed, and nothing was shared. |
| `desktop_signin_unavailable` / `desktop_signin_credential_refused` | `503`: the sign-in or the run's policy could not be read, or the sign-in could not be recorded; `500` on the begin when the server could not make the sign-in's credential. `403` from `POST /internal/desktop-signin/check`, the run proxy's question: not the run's open sign-in's credential. |
| `desktop_signin_resume_failed` | The `reason` on a `run.kill` row Wardyn writes: a sign-in that failed before it paused the agent could not continue the agent's stopped main process on 3 sweep passes, so the run was killed rather than left stopped. Start the run again. |
| `template_policy_field_refused` | `422`: a template saved as `kind: "policy"` holds something other than run-wide network, credential and access-rule keys under `inline_policy`. Each refused path is a diagnostic. |
| `runner_pools_unavailable` | `501`: this server's store has no runner pool storage (every pool route answers it), or it has no runner wired (a `POST /runs` that names a `runner_pool_id`). |
| `runner_pool_required` / `runner_pool_default_unavailable` / `runner_pool_run_type_not_allowed` / `runner_pool_barrier_not_allowed` | `422` from `POST /runs`, preflight and the policy preview. `runner_pool_required`: no pool was named and neither you nor the organization has a default. `runner_pool_default_unavailable`: the default that applies is gone or switched off, and no other pool replaces it. `runner_pool_run_type_not_allowed`: the pool's limits do not allow the run's `experience`. `runner_pool_barrier_not_allowed`: the pool's `barriers` do not allow the run's barrier. Each message names the next step: choose a pool, another pool, another run type, or another barrier. |
| `runner_pool_not_found` / `runner_pool_stale` / `runner_pool_unavailable` / `runner_pool_member_mismatch` / `runner_pool_no_eligible_member` | The pool routes: `404` no such pool or one you may not use, `409` a stale revision, `422` a switched-off pool as a default, a runner of the wrong kind (organization pools take remote-hosted and organization runners), a self-hosted runner that is not yours, a remote-hosted runner this server is not set up with or whose "Available to people's pools" policy does not admit you, or a use policy on a user pool. A pool carries `scope` (`organization` or `user`); a member `kind` (`self_hosted` or `remote_hosted`) and `runner_id` or `remote_runner_id`; a run `remote_runner_id`. `GET /runner-pools` (`ListRunnerPoolsFor`) gives an unavailable pool's first reason: `runner_pool_unavailable`, `placement_denied`, `placement_unavailable`, `runner_pool_no_eligible_member`, `runner_pool_no_online_runner`, `runner_pool_run_type_not_allowed`, or `runner_pool_not_found` (manage-only); a bad `?run_type=` is `400` `runner_pool_invalid`. A launch naming no run type into a pool with limits is `422` `runner_pool_run_type_missing`; an unknown one stays `runner_pool_run_type_not_allowed`. A user pool is its owner's alone (`CreateMyRunnerPool`, `UpdateMyRunnerPool`, `DeleteMyRunnerPool`, `AddMyRunnerToPool`, `AddMyPoolRemoteRunner`); anyone else gets the unknown-id `404`. An eleventh own pool is `422`. `CreateRunnerPool` (`{name, limits}`) makes an organization pool; `AddRunnerPoolRemoteRunner` puts a remote-hosted runner in one, and `AddRunnerPoolRunner` an organization runner. `ListRunnerPoolMembers` (`GET /runner-pools/{id}/members`) lists a pool's runners of both kinds to its readers: a user pool's owner, anyone who may use an organization pool, admins and security admins. The defaults are one pool: `PUT /runner-pool-defaults` (`SetOrgRunnerPoolDefaults`) takes `{pool_id, barrier, recording, revision}`, an organization pool only (a user pool is `400`), and replaces the document, so a PUT that leaves `barrier` out clears it; one that leaves `recording` out keeps it, and an empty `recording` is `preferred`: read first (`GetOrgRunnerPoolDefaults`, which answers the document's `revision`) or use `wardyn runner pool org-default set`. `revision` is optional: naming another revision is `409` `runner_pool_stale`, omitting it or naming `0` writes unconditionally; a person's own default has none and refuses it. A person's own (`SetMyRunnerPoolDefaults`) takes `{pool_id}` and refuses `barrier` (`400`). The retired `hosting_type`, `preferred_hosting`, `remote_provided`, `self_hosted` and `executor_id` fields are refused (`400`). `my_runners` lists the caller's runners in a pool; `owner` names a user pool's owner. |
| `scheduled_runs_off` / `scheduled_runs_unavailable` / `start_at_invalid` / `start_at_past` / `start_at_too_far` | `POST /runs` with `start_at`: `422` the server does not set `WARDYN_SCHEDULED_RUNS`, or places no runs; `400` a malformed `start_at` or `start_time_zone`, or one without the other; `422` a start time not in the future, or later than 7 days less the run's wait (24 hours background, 1 hour interactive). A booked run whose start comes after the switch was turned off ends `FAILED` `scheduled_runs_off`. |
| `runner_pool_queue_full` / `runner_pool_full` | `422`. `runner_pool_queue_full`: one person already has `100` runs waiting for the pool, so `POST /runs` is refused. `runner_pool_full`: `POST /runs/{id}/revive` into a pool at its run limit, or with others waiting. |
| `remote_runner_not_found` | `404` on the remote-hosted runner routes (`GetRemoteRunner`, `UpdateRemoteRunner`, `SetRemoteRunnerPeoplePolicy`, `ClearRemoteRunnerPeoplePolicy`; admins and security admins): no such runner. `ListMyRemoteRunners` lists the ones a person may add to their own user pools. A people-policy write is held for a second human where the server asks for one (`PendingApprovalError`). |
| `image_devcontainer_exclusive` / `image_builder_unavailable` | `POST /runs`' image/devcontainer build validation: `image` and `devcontainer_repo` were both set (the caller's own request shape), or a custom image was requested but this control plane has no image builder wired (a deployment capability neither field can fix) — two different causes, two reasons. |
| `workspace_sources_store_unavailable` / `workspace_sources_list_unavailable` / `workspace_source_not_onboarded` / `workspace_source_mount_not_allowed` | `POST /runs`' resolved-spec workspace-source checks (`validateWorkspaceSources`, `authorizeSpecWorkspaceSources`): no store, the workspace list could not be read, the member-safe mount gate (`POST /workspaces/{id}/scan` runs it too) refuses an onboarded source, or — `workspace_source_not_onboarded` — the source is not onboarded. That last one is DELIBERATELY one reason whichever function answers it, mount or repo: the message is byte-identical across all four sites for the existence-oracle reason `authorizeSpecWorkspaceSources`' doc comment explains (another member's onboarded source must read like one nobody onboarded), and a distinct reason would reopen it on the wire. |
| `workspace_source_runner_unavailable` / `workspace_session_runner_bound` | A folder bound to a runner (`runner_id`): the runner is revoked, gone or not the owner's (`runner_state` says which), or record and verify, which never use such a folder. |
| `runner_capabilities_unavailable` / `confinement_class_conflict` / `confinement_class_unsupported` / `run_grants_require_spire` | `POST /runs`' confinement-class resolution and the SPIRE-only-grants check (invariant 5). `runner_capabilities_unavailable` (`503`) is also what create, Review and the policy preview answer when no runner in the pool can take the run and a remote-hosted runner's facts could not be read: that runner might have taken it. |
| `agent_required` / `agent_not_enabled` / `run_task_reserved` / `confinement_class_unknown` / `task_mode_unknown` / `machine_kind_unknown` / `macos_base_invalid` / `interactive_start_unknown` / `tool_approvals_unknown` / `tool_approvals_hold_unsupported_agent` / `tool_approvals_hold_interactive_conflict` / `integration_id_retired` / `run_field_too_long` / `run_field_control_char` | `POST /runs`' request-shape validation (`decodeAndValidateCreateRun`): one reason per closed-enum field, plus the agent/text-field checks and the refusal of the retired `integration_id` (Review answers it the same way). |
| `local_folder_invalid` / `local_folder_unsupported` | `POST /runs`: a malformed `local_folder` (400), or a run that cannot hold its agent for it (422; [SSH.md](SSH.md#hold-the-agent-for-a-local-folder)). |
| `computer_folders_off` | `403` on `GET /me/runners/{id}/folders`, and on `POST /runs` with `local_folder.runner_id`: `WARDYN_COMPUTER_FOLDERS` is off. |
| `computer_runner_offline` / `computer_runner_no_folders` | `409` on the same routes: the runner holding the folder is not connected, or does not share folders (older than 0.10, or not `--substrate folders`). |
| `computer_folder_refused` | `422` on `GET /me/runners/{id}/folders`: the runner refused the path (outside its roots, a symlink, or a credential folder). |
| `desktop_transfer_blocked` / `desktop_transfer_too_large` / `desktop_transfer_bad_request` / `desktop_transfer_unavailable` / `desktop_transfer_not_found` / `desktop_transfer_failed` | The run's Files panel (`/runs/{id}/desktop/files`): `403` the run's `desktop` rules block that direction; `413` an upload over the limit; `400` a name that is not plain or an upload with no `Content-Length`; `409` a run with no UI app that is not a Linux VM; `404` no such file in `~/Transfers`; `502` the in-sandbox half failed. |
| `desktop_review_required` / `desktop_review_not_held` / `desktop_review_pending` / `desktop_review_not_approved` / `desktop_review_spent` / `file_changed_since_review` / `desktop_review_unavailable` | A reviewed download (`desktop.download` `review`): `403` a download with no `?approval=<id>`; `409` an ask (`POST /runs/{id}/desktop/files/{name}/review`) on a run whose downloads are allowed; `409` a review of that file is already waiting; `403` the approval is not an approved review of this file on this run; `409` the approval already released its file; `409` the file's bytes are not the ones reviewed, and nothing was sent; `503` the approvals could not be read, raised or spent, and nothing was asked for or sent. |
| `desktop_rules_unreadable` | `503`: the run's desktop rules could not be read, so the desktop's socket and its Files panel (`/runs/{id}/desktop/files`) refuse rather than fall back to the defaults. Try again. |
| `computer_folder_none` / `computer_folder_copy_in_only` / `computer_folder_not_synced` / `computer_folder_not_running` / `computer_folder_paused` / `computer_folder_agent_unstoppable` | `409` on `POST /runs/{id}/computer-folder/bring-back` (the run's own person only; anyone else gets `404`): no folder from your computer, Copy in only, first sync never landed, the run is not `RUNNING`, it is paused, or its sandbox cannot stop the agent first. |
| `data_out_blocked` / `data_out_unreadable` / `data_out_review_unavailable` | The run's `data_out` rule keeps content in: `403` on run output (not a security admin), on a recording replay by anyone but a super admin (`This run's data_out rule keeps its content in: only a super admin can replay its recording.`), on a web app other than the desktop, and on `POST /runs/{id}/computer-folder/bring-back`; `409` on `POST /runs/{id}/exec`, where an unreadable rule counts as `blocked`; `503` the rule could not be read; `409` the Files panel of a run whose machine cannot move files through Wardyn, under `review`. |
| `computer_folder_data_out` | `422` on `POST /runs` with `local_folder.direction` `both`: the run's `data_out` rule keeps content in, so a computer folder can only go in. |
| `harness_inner_walls_looser` / `harness_inner_walls_unmet` / `harness_sandbox_unavailable` | `422` from `POST /runs` and Review: the policy's `harness_inner_walls` is looser than the run's harness walls, or its harness and lane cannot meet it. `harness_sandbox_unavailable`: Codex's sandbox cannot start at the run's class. |
| `run_max_wait_invalid` | `POST /runs`: `max_wait_sec` is below 1. A wait above 604800 (7 days) is cut to it with a warning, not refused. |
| `run_queued` | `PATCH /runs/{id}`: the run is waiting for a slot in its runner pool (state `QUEUED`), so its end is set when it starts and cannot be changed yet. The `201` that stored a run `QUEUED` reports `resources_applied` as of the launch: the start sizes the run again against the limits as they stand then. |
| `run_kill_already_terminal` / `run_kill_state_changed` | `POST /runs/{id}/kill`: the run was already terminal, or ended or kept moving between the read and the write. |
| `revive_unsupported` | `POST /runs/{id}/revive` (409), and the same value in each refused run's `reason` in `POST /admin/runs/restart`'s `results`: the run's runner substrate cannot replace its proxy in place (`runner.ErrReviveUnsupported`; on Kubernetes the agent pod pins the proxy pod's IP, so the substrate implements no `runner.ProxyReviver`). Nothing changed. Stop the run and start a new one. |
| `revive_readback_unsupported` | `POST /runs/{id}/revive` (409), and in each refused run's `reason` in `POST /admin/runs/restart`'s `results`: the new proxy must confirm its policy, and the run's substrate cannot answer the hash of the config it hands that proxy. Nothing changed. |
| `revive_policy_unconfirmed_lost` | `POST /runs/{id}/revive` (502): the new proxy started but did not confirm its policy (no hash from the driver, or a report of different bytes), so it was stopped. The run is lost until revived or, when it cannot be kept, ended; the message says which, and when that stop is not confirmed yet. |
| `proxy_report_invalid` / `proxy_report_unrecorded` | `POST /internal/proxy/loaded`: `400`, the report's `config_sha256` is not a lowercase hex SHA-256; `503`, the store could not record the report, and the proxy retries. |
| `egress_edit_invalid` | `PATCH /runs/{id}/egress` (400): the body names no host, more than 256 in one list, or an entry the proxy cannot match. A field that would add a host is not part of the body, so it is the generic `invalid_request_body`. |
| `egress_edit_run_not_live` / `egress_edit_in_progress` | `PATCH /runs/{id}/egress` (409), nothing changed: the run is not running, has no sandbox or is lost (revive it first); or another edit of this run has not answered yet. |
| `egress_edit_host_not_listed` | `PATCH /runs/{id}/egress` (422), nothing changed: a `remove_hosts` entry is not an entry on the run's allowlist. To block it, deny it instead. |
| `egress_edit_unsupported` / `egress_edit_legacy_proxy` | `PATCH /runs/{id}/egress` (409), nothing changed: this deployment's store or the run's substrate cannot confirm what a new proxy loaded, or the substrate cannot replace a sandbox's proxy at all (Kubernetes: the agent pod pins the proxy pod's IP); or the run's runner or proxy image is older than 0.10.0-rc.18. Start a new run with the narrower policy. |
| `egress_edit_unconfirmed` | `PATCH /runs/{id}/egress` (503): the new proxy did not confirm the edited policy (no matching report by its deadline, or a missing or different hash), or the edited config could not be stored, so it was stopped. The run is lost until revived, or ended when it cannot be kept (a headless run off your own machine). When that end could not be recorded, the message says so, and the run is ended once its token lapses, 65 minutes after its last renewal; end it yourself to stop its agent sooner. |
| `egress_edit_superseded` | `PATCH /runs/{id}/egress` (503): another revive replaced the edit's proxy start before it confirmed, and nothing was stopped for the edit. When the edit's config was stored, that revive started from it and its own readback settles it. When it was not (the edit's replace or config write failed), the edit was not applied: send it again. The message says which. |
| `egress_edit_containment_unresolved` | `PATCH /runs/{id}/egress` (503): as `egress_edit_unconfirmed`, but stopping the new proxy is not confirmed yet. The run is being stopped and a sweep retries the stop. When both writes of the start's answer were refused, the start stays pending with no deadline, the message names `readback_wait_timeout`, and nothing retries its stop: an operator must stop or end the run. Until a stop lands, treat the run as possibly still having egress. |
| `workspace_repo_not_admitted` | The repository is not on this deployment's admitted list (`workspace_admission.go`), at create/update and at launch. |
| `repo_url_credentials` | `400`: an http(s) repository URL carries a user name, password or token before its host, or an `ssh://` URL a password, wherever a repository is entered or a stored one is launched or scanned. Give the plain URL and the token as a `git_pat` grant. `git@host:path`, `ssh://user@` and the Azure DevOps Clone-button form `https://<org>@dev.azure.com/<org>/...` are accepted. Stored runs, workspaces, sources, policies, presets, templates, governance profiles and launch intents are served without it. |
| `workspace_envcode_no_local_dir` / `workspace_envcode_no_profile` | `POST /workspaces/{id}/env-as-code/write` only: no `local_dir` source. That route and `GET /workspaces/{id}/env-as-code`: no scanned profile. |
| `workspace_providers_invalid` | The operator-only `PUT /api/v1/workspace-providers` (deployment-wide, not a per-workspace route): the submitted block fails validation. Its stale-`If-Match` arm shares `site_config_stale` below — the identical ETag cause, on the same underlying site-config document. |
| `workspace_request_invalid` / `workspace_ssh_sources_not_ready` / `workspace_sources_not_allowed` | `POST/PUT /workspaces`: the request body fails validation, an SSH-remote source names a secret not yet stored, or the caller's own `local_dir` sources fail the member-safe mount gate; on `POST /workspaces/{id}/env-as-code/write`, the stored `local_dir` source is, or now resolves to, a host path the mount deny-list refuses (nothing is written). |
| `workspace_delete_active_run` | `DELETE /workspaces/{id}`: the workspace is in use by a still-active run. |
| `workspace_name_taken` | `POST /workspaces`, `PUT /workspaces/{id}`: the workspace's owner already has another workspace of this name (names are unique per owner; operator-owned workspaces are one owner). Pick another, or rename the other one. `POST /workspaces/{id}/reassign`: an operator-owned workspace already has this name. |
| `workspace_approved_egress_denied` | `PUT /workspaces/{id}/approved-egress`, `400`: a host is on the workspace's denied egress list, and a deny beats an approval at the proxy. Remove it with `PUT /workspaces/{id}/denied-egress` first; the approval never removes it for you. |
| `workspace_update_conflict` | `PUT /workspaces/{id}`: the workspace changed (a run claimed it, a recording or a build landed) between the read and the save, so nothing was written. Reload it and apply the edit again. The two egress-list `PUT`s answer it as `412` when `If-Match` is stale. |
| `branch_list_rate_limited` | `GET /workspaces/{id}/branches`: the person passed `30` lists a minute (burst `10`, per wardynd replica), answered `429` before anything is read. Not audited; the admin token is exempt. |
| `groups_snapshot_stale` | `PUT/POST /governance/*` and the user-drive resolver: the caller's group-membership snapshot is missing or was truncated at sign-in, so a group-keyed governance profile cannot be resolved. The SAME value as authz's own registered reason ([`internal/authz/registry.go`](../internal/authz/registry.go)), a literal in `reasons.go` for the docs⟷reasons.go guard and tied to authz's constant by a documented `TestNoAdHocAuthz` exception. |
| `egress_baseline_invalid` / `egress_baseline_stale` | `PUT /governance/egress-baseline`: a host or suffix is invalid, or `If-Match` is stale. |
| `recording_level_invalid` / `recording_level_stale` | `PUT /governance/recording`: the level is not `off`, `terminal` or `screen_actions`, or `If-Match` is stale. |
| `credential_delivery_invalid` / `credential_delivery_unavailable` / `credential_delivery_stale` | `PUT /governance/credential-delivery`: an unknown or fixed class, a mode outside the class's bounds, or `require_posture` without `runner_resident`; under `by_trust`, an unknown trust class, an override that sets `via_org` or overrides a class delivered `via_org`, or `require_posture` under `org_cluster`; or the store is not Postgres; or `If-Match` is stale. `GET` returns `buildable_modes` beside `policy`: per class, the modes a local run can be delivered under today. A stored mode outside them refuses at every door; a write setting a class or `by_trust` override to one is `422`, and a stored document is left as it is. `GET` lists stored rows outside them as `unbuildable` (`class`, `trust_class`, `mode`, `buildable_modes`; absent when none); `/setup/status` warns of them. |
| `site_config_request_invalid` / `site_config_artifact_override_invalid` / `site_config_integrations_via_own_route` / `site_config_invalid` / `site_config_stale` / `site_config_egress_via_own_route` / `site_config_recording_via_own_route` | `PUT /site-config`: the body did not decode, a legacy artifact-override field fails validation, integrations were named inline instead of through their own endpoints, the submitted config fails one of the agent/model-provider/default-provider validators, `If-Match` is stale, or the body changes the egress baseline (the `egress` block or the baseline-marked `internal_hosts`), which only `PUT /governance/egress-baseline` writes (`PutSiteConfigResult` sends neither, so a captured document restores, and `EgressBaselineIgnored` reports a baseline it did not apply; `PutSiteConfig` sends the document as given and gets this refusal; a restore is still refused when the security tier marked an internal host the older capture does not list: lift the mark first), or changes the `recording` block, which only `PUT /governance/recording` writes (`PutSiteConfigResult` sends none, and `RecordingIgnored` reports one it did not apply). `PUT /agent-providers`, `/model-providers` and `/workspace-providers` check `If-Match` against this document's ETag and answer `site_config_stale` too: one reason for one cause, whichever sub-block the write targeted. |
| `expected_principal_mismatch` | `412`: any write carrying the console's `X-Wardyn-Expected-Principal` header whose person is not the one now signed in (another tab signed in as someone else after the draft was composed). Nothing is written; reload and retry. Writes without the header (the CLI, scripts) are not checked. |
| `site_config_probe_request_invalid` / `site_config_probe_url_invalid` / `egress_redirect_from_required` / `egress_redirect_not_found` | `POST /site-config/test-proxy` and `POST /site-config/test-redirect`. |
| `governance_unit_request_invalid` / `governance_ceiling_invalid` / `governance_unit_name_conflict` / `governance_unit_in_use` / `governance_membership_invalid` / `governance_preview_claims_invalid` | `PUT/POST /governance/units` and `/governance/memberships`, and the preview routes — `governance_preview_claims_invalid` is shared with the user-drive naming preview (`user_drives_preview.go`), which feeds `normalizeGovernancePreviewClaims` too. `governance_unit_in_use` is a `409` when a profile is still assigned or still the base of another profile (the body names those profiles). An SDK predating composition reads a composed profile as `ceiling: {}` and `limits: {}`, ignoring `effective`; its `PUT` omits the composition fields, which are kept, so it cannot flatten the profile. |
| `governance_overlay_invalid` | `PUT/POST /governance/units` (400): a composed profile's `overlay` or `overlay_limits` does not decode, names something its base does not permit (a domain outside the base, a method the base excludes, `allow_all_egress` on a base without it, a looser bound, a grant the base does not hold), would mean "everything" by being empty (`allowed_methods: []`), names a base that does not exist, or sits beside a non-empty `ceiling` or `limits` (a composed profile states its policy only in the overlay). |
| `governance_mount_keep_unknown` | `PUT/POST /governance/units` (422): a mount was sent back with the withheld source `<redacted>` on a target that stores no host path to keep (a new mount, or one the profile no longer has). A new mount needs its host path, and only a super admin sets one. |
| `governance_unit_cycle` | `PUT/POST /governance/units` (409): the write would make a profile, through its base, its own base. |
| `governance_unit_depth` | `PUT/POST /governance/units` (409): the write would put a unit or one below it deeper than `governance_max_depth` (16 unset or `0`; `null` keeps it). |
| `governance_unit_default_readonly` | `PUT` or `DELETE /governance/units/{id}` on the Default unit, or approving a change held against it (409): its walls are `WARDYN_DEFAULT_POLICY`. |
| `governance_unit_admin_invalid` / `governance_unit_admin_not_found` | `POST /governance/units/{id}/admins` and `DELETE /governance/units/{id}/admins/{admin}`: the grant names a `subject_type` other than `user` or `group`, or an empty subject (400), or a subject that already administers the unit (409); no such grant on that unit (404). |
| `model_usage_window_invalid` | `GET /governance/units/{id}/model-usage` (400): `since` or `until` is not an RFC3339 time, `since` is not before `until`, or they are more than 92 days apart. |
| `invalid_deciders` | `PUT /governance/units/{id}/deciders` (400), or approving a held write of it: a rule names an unknown kind or decider type, lists `security`, `manager` or `delegate`, repeats a kind, or breaks a kind's rule (`second_human` is required for `credential` and `push_content`, `owner` is refused for those and for `elevation`, `max_scope` is never `always`, an `elevation` rule needs `max_elevation` of 15m to 24h and one to eight `group:<name>` `eligible_requesters`, its `renew_after` is 0s to 168h and its `ticket_pattern` at most 256 characters that compile; those three fields are refused on any other kind). The message names the rule, for example `rules[1]`. |
| `decider_rule_wider_than_parent` | `PUT /governance/units/{id}/deciders` (422), or proposing or approving a held write of it: a rule names a decider the rules above the unit do not, or a wider `max_scope` or `max_elevation`, an `eligible_requesters` group they do not name or a shorter `renew_after`, or leaves off a `second_human` one sets. The message names the rule and the field. |
| `governance_membership_default_unit` | Adding or moving a membership into the Default unit, by write, dry run or approval (409). |
| `unit_not_a_context` | `POST /runs`, preflight and the policy preview (403): the unit asked for is not one of the caller's. |
| `workspace_unit_not_yours` | `POST /runs`, preflight and the policy preview (403): an attached workspace's unit is on no path of the caller's units; `policy` names it. |
| `unit_membership_gone` | Revive, admin restart, run-end extension (409): the run's owner holds no membership placing them in the run's unit ([governance writes](operations/governance-writes.md)). |
| `governance_overlay_unsatisfiable` | `PUT/POST /governance/units` (409): a base change would leave a profile built on it with a policy nothing satisfies (the body names that profile). The SAME value as authz's own registered launch refusal ([`internal/authz/registry.go`](../internal/authz/registry.go), `403` on create, preflight and every live door), a literal in `reasons.go` for the same reason `groups_snapshot_stale` is. |
| `governance_second_human_local_mode` | `WARDYN_GOVERNANCE_SECOND_HUMAN` cannot be enforced in local mode (`503`): nobody is authenticated there, so the proposer and the approver are both client-supplied. Every covered governance write (profile, assignment, grant, enforcement, availability, user-type priority, role mapping, egress baseline) and every `/governance/changes` approve or reject answers it. |
| `governance_change_pending` | A covered write (`/governance/units`, `/governance/memberships`, `/permissions/grants`, `/permissions/enforcement`, `/permissions/availability`, `PUT /user-types/{id}`, `/access/mappings`, `PUT /governance/egress-baseline`) with `WARDYN_GOVERNANCE_SECOND_HUMAN` on (409): a live change already waits for approval at this target; the message names it. Approve or reject that one first, or let it expire. |
| `governance_change_stale` | `POST /governance/changes/{id}/approve` (409): the target (or, for an assignment, the profile it points at) or the deployment default changed since the change was proposed, so approving would apply something other than what the reviewer saw. Nothing was applied and the change is now `stale`; propose it again. |
| `governance_change_not_pending` | `POST /governance/changes/{id}/approve` or `/reject` (409): the change was already applied, rejected, expired or marked stale. A change past its expiry is moved to `expired` by the request that finds it. |
| `governance_change_not_found` | `GET /governance/changes/{id}` and the approve and reject routes (404): no such change, or one of a kind the caller may not approve. |
| `governance_change_note_proposer_only` | `PUT /governance/changes/{id}/note` (403): the caller did not propose the change. Only its proposer writes the note. |
| `governance_change_note_invalid` | `PUT /governance/changes/{id}/note` (400): the reason is over 500 characters or holds a control character, or the ticket is over 128 characters or is not one line. |
| `governance_change_state_invalid` | `GET /governance/changes?state=` (400): not one of `pending`, `applied`, `rejected`, `expired` or `stale`. |
| `governance_request_needs_person` | `POST /governance/requests` (403): the caller is not a signed-in person (the admin token, a service, a device, local mode). Only a person can ask. |
| `governance_request_kind_invalid` | `POST /governance/requests` (400): `kind` is not one this server takes; the message lists them. |
| `elevation_not_offered` | `POST /governance/requests` (400): the unit offers the caller no elevation: no `elevation` rule on it or above it, the caller is in none of its `eligible_requesters` groups (or their group list is out of date), or the unit is Default or unknown. One answer for all of them. |
| `elevation_duration_invalid` | `POST /governance/requests` (400): `duration` is not a duration such as `4h`, or is under `15m`. |
| `elevation_too_long` | `POST /governance/requests` (400): `duration` is over the unit's `max_elevation`; the message names the unit and its limit. |
| `request_reason_required` | `POST /governance/requests` (400): the request has no reason. |
| `request_reason_too_long` | `POST /governance/requests` (400): the reason is over 500 characters. |
| `elevation_already_member` | `POST /governance/requests` (409): a membership (of the person, a group of theirs or their user type) already places the caller in the unit. |
| `elevation_ticket_required` | `POST /governance/requests`, or `PUT /governance/changes/{id}/note` on an elevation (400): a rule on the unit or above it has a `ticket_pattern`, and no ticket was given. |
| `elevation_ticket_invalid` | `POST /governance/requests`, or `PUT /governance/changes/{id}/note` on an elevation (400): the ticket does not match a `ticket_pattern` on the unit or above it; the message shows the pattern. |
| `elevation_cooldown` | `POST /governance/requests` (409): the caller's last elevation in the unit ended less than the rule's `renew_after` ago. `retry_after` (RFC 3339) is when they may ask again. |
| `image_request_not_pinned` | `POST /governance/requests`, kind `image` (400): the image is not `<name>@sha256:<64 hex>`, or its path has an empty, `.` or `..` segment. |
| `image_request_not_offered` | `POST /governance/requests`, kind `image` (400): the caller's default unit offers no image requests: no `image` rule on it or above it, the unit is Default, or the group list is out of date (one answer for all of them), or image grants are off in this deployment. |
| `image_already_granted` | `POST /governance/requests`, kind `image` (409): the caller already holds a grant of exactly this image. |
| `boundary_invalid` | `PUT /governance/boundary` and `POST /governance/dry-run` (400): the `PUT` names no `mode` or an unknown one, or the document sets `allow_all_egress` beside `allowed_domains`, a `max_end_ahead_sec` below 1, a bad `contact`, or a value a run policy would refuse; `501` on a store that keeps no boundary. |
| `boundary_stale` | `PUT /governance/boundary` (412): `If-Match` names a boundary that has since changed. |
| `unit_knob_locked` | `POST/PUT /governance/units`, `PUT /governance/boundary`, an approval of either, and `POST /governance/dry-run` (409): the write changes a knob that the boundary or a unit above locks, a narrowing included ("`<knob>` is locked by `<locker>`; ask `<contact>`"). Or it locks a knob that a unit below sets to another value: the body lists those units, and clearing their values would widen them. |
| `approval_rule_invalid` | `PUT /governance/approval-rules/{name}` and `.../dry-run` (400): a kind other than `tool_call` or `egress_domain`, a glob tool, a wildcard or ported host, a scope other than `once` or `run`, no repo and no preset, or a body name not the path's. `501` on a store with no approval rules. |
| `approval_rule_not_found` | `DELETE /governance/approval-rules/{name}` and `.../dry-run` (404): no rule has that name. |
| `approval_rule_host` | `PUT /governance/approval-rules/{name}` (409): a `hard_limits` boundary does not allow one of `approve.hosts`. |
| `approval_rule_changed` | An `approval.policy.skip` reason, never sent to a client: the rule changed after it matched. |
| `approval_rule_not_applicable` | An `approval.policy.skip` reason, never sent to a client: no rule decides that approval. |
| `approval_rule_host_denied` | An `approval.policy.skip` reason, never sent to a client: a person's deny for that host still stands on the run. |
| `boundary_host` | `POST /approvals/{id}/approve` (403): the Wardyn boundary is in `hard_limits` and denies the egress host, or does not allow it. |
| `envelope_invalid` / `envelope_nested` / `envelope_group_taken` | `PUT /governance/units/{id}/envelope`. `envelope_invalid` (400): the unit is unknown or Default, or `approved_groups` has an unusable name, a duplicate or more than 64 groups (`501` on a store that keeps no envelopes). `envelope_nested` (409): an envelope already sits on a unit above or below. `envelope_group_taken` (409): a group is approved on another unit's envelope, which the message names. |
| `restriction_invalid` / `restriction_not_found` / `restriction_host` / `workspace_owning_unit_default` / `component_owning_unit_default` / `workspace_owning_unit_strict_needs_unit` | The restriction routes, an egress approval, `PUT /workspaces/{id}/owning-unit` and `PUT /components/{id}/owning-unit`. |
| `component_unit_pinned` | Run create, preflight and preview; a credential mint or redeem; a revive, restart or end extension: an organisation component's unit pin does not cover the run's context. |
| `walls_invalid` / `walls_need_person` / `walls_unavailable` | `/me/walls` ([Your walls](#your-walls)): `400` walls that name a wider value, a bad site or a list over its cap; `400` a caller who is no signed-in person (the admin token, local mode, a service identity); `501` a store that keeps no personal settings. |
| `personal_walls_host` | `POST /approvals/{id}/approve` (403): the run owner's walls deny the egress host (`GET /approvals` marks the row `walls_block: true`). A host outside their list follows their choice for an unlisted site. |
| `wall_preset_invalid` / `wall_preset_scope_forbidden` / `wall_preset_builtin` / `wall_preset_not_found` | `/wall-presets` ([Wall presets](#wall-presets)): `422` a preset that names a wider value or no limit, or a launch whose preset no longer passes the walls check; `403` a save to the organisation or a group you may not save to; `403` an edit or delete of a built-in; `404` no such preset, or one you cannot see. |
| `wall_preset_name_taken` / `wall_preset_version_conflict` / `wall_preset_limit_reached` | `409`: another preset of the same audience, or a built-in, has the name; the preset changed since the version you read; the audience holds the most presets it may. |
| `wall_preset_unknown` / `wall_preset_host` / `wall_presets_unavailable` | `422` a launch names a preset that does not exist or is not open to you; `403` `POST /approvals/{id}/approve` of a host the run's wall preset denies; `501` this server's store keeps no wall presets. |
| `no_network_needs_egress` | `422` a `no-network` launch would need a host; the message names each source. |
| `host_rules_need_inspected_vm` | `422` a VM run (`linux_vm`, `macos_vm`) whose policy has `host_rules`; host rules are not yet judged on a VM guest's flows. |
| `credential_withheld` | `POST /internal/credentials/mint`, `GET /internal/injection/{grantID}` (403): the run's governance profile, the Wardyn boundary or the run owner's walls now withhold the grant or a host it reaches. |
| `credential_withheld_unread` | `POST /internal/credentials/mint` (503): the run, its grants, its profile, the Wardyn boundary or the run owner's walls could not be read to check the grant; retry. |
| `break_glass_reason_required` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` with the admin token (400): the boundary is in `hard_limits`, bounds something, a unit with its own walls or a restriction exists, and the `X-Wardyn-Break-Glass-Reason` header is missing, too long or has control characters. |
| `secret_name_invalid` / `secret_name_reserved` / `secret_owner_param_refused` / `secret_body_invalid` / `secret_value_too_short` / `secret_cap_reached` / `secret_not_found` | `PUT/DELETE /secrets/{name}` (and `/me/secrets/{name}`, `/shared-secrets/{name}`): the name fails the secret-name format or is reserved, `?owner=` was sent on a write, the body is malformed, the value is below the mask minimum (a trailing line break does not count), the owner already holds the maximum number of secrets, or (the admin-only `?owner=` delete arm) that owner holds no secret by that name. Also `GET /internal/injection/{grantID}` (424): a stored header credential under the minimum without its trailing line break. |
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
| `workloads_unavailable` / `workload_invalid` / `workload_not_found` / `workload_source_conflict` / `workload_cap_reached` / `workload_managed_by_k8s` | The `/workloads` routes (`ApplyWorkload`, `PreviewWorkload`, `GetWorkload`, `WorkloadHistory`, `ListWorkloads`, `RestartWorkload`, `DeleteWorkload`). `WorkloadHistory` (`GET /workloads/{name}/history`) reads that workload's audit rows, newest first, for the callers who may `GetWorkload`, a service identity's token excepted. `PreviewWorkload` (`POST /workloads/{name}/preview`) answers what `ApplyWorkload` would do and stores nothing; a refusal the store would make is in its answer (`refusal`), not an error. `501`: the store keeps no workloads. `400` `workload_invalid`: the name is not a lowercase DNS label, a header field is too long, or the intent fails the run-document door (`diagnostics` lists each problem, as `POST /templates/import` does). `404`: no workload of that name that you may reach; a `security_admin` reads and deletes another owner's workloads but gets this on a restart, which only the owner, the sponsor or a super admin may make. `409` `workload_source_conflict`: a custom resource owns it, so an apply cannot change it. `422`: you hold 100 workloads. `409` `workload_managed_by_k8s`: deleting a custom resource's workload, which only an admin or `security_admin` may. A launch the create path refuses is not an error: the answer is `200` with `launch.state` `refused` and that refusal's `reason`, and the previous run keeps running. |
| `workload_binding_needs_person` / `workload_binding_invalid` / `workload_binding_conflict` / `workload_binding_pool_scope` / `workload_binding_not_found` | The `/workload-bindings` routes (security tier). `403` `workload_binding_needs_person`: a binding is made by a signed-in person, never with an API token or the admin token. `400`: the pool or service is missing, or the namespace is not a DNS label. `409`: the namespace is bound on that pool already. `422` `workload_binding_pool_scope`: the pool, named by id or by name, is a person's own; a binding names an organization pool only. `404` `workload_binding_not_found`: no binding with that id. An unknown pool answers `runner_pool_not_found`; an unknown or inactive service identity `service_identity_not_found` or `service_identity_inactive`. |
| `workload_pool_mismatch` / `workload_binding_inactive` | A `WardynWorkload` custom resource's `status.reason`, never an HTTP error reason. `workload_pool_mismatch`: the spec names a `runner_pool_id` other than the pool its namespace is bound on, so nothing is recorded. `workload_binding_inactive`: the binding's service identity is suspended or retired, its sponsor changed since the binding was made, or the sponsor's access was revoked (a sign-out that keeps their tokens does not count), or the binding was deleted; the spec is recorded, nothing launches, and a running run keeps running. A restart that meets it answers `200` with it as `launch.reason`, and the workload's `last_refusal` records it. Bind the namespace again. |
| `api_token_no_human` / `api_token_from_delegated_token` / `api_token_member_mode_mint` / `api_token_name_invalid` / `api_token_ttl_invalid` / `api_token_cap_reached` | `POST /api/v1/tokens` (self-service mint) and `DELETE`; the invitation claim and the sponsor's service-token mint answer the mint guards with the same sentences, and `api_token_cap_reached` (`422`) there means `20` live tokens on the identity for the sponsor's mint, but on an invitation claim the cap is the claimer's own live tokens. |
| `owner_ambiguous` / `owner_unresolved` | The shared owner/principal resolver shape: more than one known principal matches a name, or none does. `secrets.go`'s admin `?owner=` resolution (`ownerRefusalReason`) and `sshkeys_admin.go`'s `resolveSSHKeyOwner` (also reached from `POST /sessions/revoke`'s SSH-key cutoff) both answer from this one vocabulary rather than each inventing its own. |
| `sessions_revoke_param_invalid` | `POST /sessions/revoke`: the body must set exactly one of `sub`/`all`, and `sessions_only` (browser sessions end; API tokens and SSH keys keep working) goes with `sub`, not `all`. |
| `identity_not_found` | `POST /admin/identities/{id}/unbind` named no identity row. |
| `identity_not_bound` | `POST /admin/identities/{id}/unbind` found the row with no principal to clear, or bound to another one by the time it was locked. Read it again. |
| `identity_not_rebindable` | `POST /admin/identities/{id}/unbind` named an identity with no object id (not an Entra person keyed by tenant and object id); unbinding it would orphan it. |
| `identity_principal_in_use` | `POST /admin/identities/{id}/unbind` found the principal it would release still holding an API token, SSH key, active run, claimed runner or unspent runner registration token, still sponsoring a service identity, or still named by a pending invitation. Revoke, end or retire them, then retry. |
| `identity_deactivated` | A run, console session, API token, SSH key, stored credential or runner registration token was refused (a session cookie gets `401` "Your access to Wardyn has been turned off") because its owner's identity is deactivated, or was suspended after the caller was admitted. Sign in again; if that is refused too, ask the Wardyn admin. `409` from `POST /admin/identities/{id}/unbind`: the identity is itself deactivated (or purged), so unbinding it would not let the person back in; the calling admin has nothing to sign in again for. |
| `sso_not_configured` / `access_role_map_value_invalid` / `access_mapping_target_invalid` / `access_email_mapping_disabled` / `access_lockout` / `access_unknown_user_type` / `access_preview_no_session_claims` | `GET/POST /access` (role-mapping admin, operator-only). |
| `capability_grant_invalid` / `capability_kind_unknown` / `capability_enforcement_stale` / `availability_kind_not_restrictable` / `availability_target_invalid` / `availability_restricted_required` / `availability_only_empty` | `POST /permissions/grants` and the capability-availability routes. |
| `delegation_store_unavailable` / `delegate_name_invalid` / `delegate_client_id_invalid` / `delegate_client_id_is_portal` / `delegate_group_invalid` / `delegate_run_in_under_invalid` | `POST /admin/delegates` (operator-only portal delegate registration). `delegate_run_in_under_invalid` (`422`): `run_in_under` names no existing unit; nothing was registered. |
| `integration_invalid` / `integration_not_found` / `setup_onboarding_store_unavailable` | `PUT/DELETE /setup/integrations/{id}` and `POST /setup/onboarding-complete` (operator-only). |
| `component_cap_reached` / `component_name_conflict` / `component_not_found` | `POST/PUT/DELETE /me/components[/{id}]` and `PUT/DELETE /components/{id}`: the owner already holds the maximum number of saved components (32 per person, 256 for the organisation), another component of the same owner already has the name (compared without case), or no such component of the caller's exists (another person's row and an absent id answer alike). These routes also answer `component_definition_invalid` (the definition fails the shared validator or names a secret Wardyn manages), `component_secret_missing` (an organisation component shares a secret the operator has not stored) and `component_store_unavailable`, documented with the run gate below. |
| `ssh_key_invalid` / `ssh_key_requires_human` / `ssh_key_cap_reached` / `ssh_key_revoked_session` / `ssh_key_registration_refused` / `ssh_key_fingerprint_invalid_encoding` | `POST/DELETE /me/ssh-keys` (self-service). `ssh_key_registration_refused` is DELIBERATELY generic (THREAT-MODEL.md): it never confirms whether, or by whom, the key is registered; a distinguishable reason would reopen the key-squatting reconnaissance oracle the shared sentence closes. |
| `ssh_key_admin_principal_invalid_encoding` / `ssh_key_admin_principal_required` | `DELETE /people/{principal}/ssh-keys` (security-tier). |
| `user_drive_request_invalid` / `user_drive_allocated_conflict` / `user_drive_slug_conflict` / `user_drive_home_namespace_conflict` / `user_drive_name_conflict` / `user_drive_still_allocated` / `user_drive_grant_invalid` / `user_drive_grant_conflict` | `POST/PUT/DELETE /drives` (operator-only), `POST /drives/grants` and `DELETE /drives/grants/{id}`. |
| `user_drive_rehome_list_unavailable` / `user_drive_rehome_invalid` | The drive list could not be read to run the nesting check (`POST/PUT /drives`) or the re-home guard (`PUT /drives/{id}`), or the guard refuses an unconfirmed identity-field change on an allocated drive. |
| `user_drive_ceiling_unavailable` / `drives_disabled` / `user_drive_size_refused` | `POST/PUT /drives`: the drive-size ceiling could not be read, or a `host_path` share did not answer in time; the org switch is off (shares `drives_disabled` with the launch-time door below); or the size override exceeds the ceiling. |
| `user_drive_reclaim_invalid` / `user_drive_reclaim_subject_ambiguous` / `user_drive_not_reclaimable` / `user_drive_reclaim_unsupported` / `user_drive_reclaim_conflict` | `POST /drives/{id}/reclaim` (operator-only). |
| `user_drive_reclaim_failed` / `user_drive_reclaim_no_directory_name` | The reclaim object resolver's own two causes: a store read failure, or the allocation resolving to no directory name at all. |
| `user_drive_preview_no_claims` / `user_drive_denied_by_unit` | The user-drive naming and bind-failure preview routes (operator-only, mirrors the launch-time resolver's own verdict so an admin reads the SAME sentence a member would). |
| `no_allocation` / `paused` / `runner_cannot_mount` / `backend_elsewhere` / `ceiling_moved` / `home_missing` / `home_unreadable` / `share_unreachable` / `read_only` / `drives_disabled` | `POST /runs`' drive-mount refusals (`seedRequestDrives`, `refuseDrive`), the closed set the `wardyn_drive_refusals_total` metric and its WARN line use. The runner-unavailable 503 and a caller-cancelled probe (`silent`) stay unclassed on purpose. |
| `user_type_unknown` / `unmountable` | The user-drive resolver's own closed enum ([`internal/api/user_drives_resolve.go`](../internal/api/user_drives_resolve.go)) members that reach `writeDriveError`'s wire body. Two siblings in that same enum, `unavailable` and `governance_unavailable`, never leave `GET /me`'s own field — no route sends them via `writeErrorReason` — so they are deliberately UNDOCUMENTED here; `unavailable` would also collide with `ADOEntraFailure`'s own reason of the same name above. |
| `drive_subject_unsupported` | `POST /runs`, `POST /drives/preview` (`422`) and the admin reclaim (`400`): the allocation exists, but no directory name is derived from this person's sign-in name under the drive's home template. An admin sets a home override on the allocation. |

#656 slice 3 converts the remaining route families — `run_*.go`'s per-run
actions, the UI gateway, device federation, and every other lane still
sending `error` alone — completing the sweep this issue tracks:

| Reason | Meaning |
|---|---|
| `local_mode_not_owner` / `revive_unsupported_deployment` / `revive_unsupported_runner` / `revive_bulk_cannot_start_agent` / `revive_already_in_progress` / `revive_mint_identity_failed` / `revive_encode_config_failed` / `revive_pull_image_failed` / `revive_claim_failed` / `revive_run_changed` / `revive_proxy_kept_current` / `revive_proxy_replace_failed_lost` / `revive_agent_start_failed_lost` / `revive_recovery_unresolved` / `revive_substrate_unreadable` / `revive_config_not_stored` / `revive_config_unreadable` / `revive_config_does_not_load` / `revive_not_running` / `revive_ended_files_gone` / `revive_past_end` / `revive_ended_task_run` / `revive_reboot_agent_stopped` / `revive_ended_agent_stopped` / `revive_unknown_lost_reason` / `revive_agent_status_unreadable` / `revive_config_run_mismatch` / `revive_ceiling_denies_git_broker` / `revive_ceiling_denies_azure_cli` / `revive_ceiling_denies_kubectl_cli` / `revive_owner_authority_unreadable` / `revive_admin_restart_count_invalid` / `revive_proxy_window_store_unavailable` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`): one reason per distinct revive-refusal cause. `revive_unsupported_runner` covers three arms with one fact: this runner cannot replace a proxy. `revive_recovery_unresolved` (`503`) is a revive that failed after its claim and then could not write the run's lost mark: the run is not recorded as lost (a bulk restart's `lost_again` is `false`), its broker credentials are revoked, its proxy is stopped (the answer says so only when that stop was confirmed, and otherwise that it is not confirmed yet), and a later sweep recovers it. The `_lost` reasons say the revive failed after its claim; the answer says the run is lost until revived (and a bulk restart's `lost_again` is `true`) only once the mark was written, that the run is being ended when it cannot be kept, and that it is being lost or ended by another operation when the mark or end did not land (`lost_again` `false` for both); it names a stopped proxy or "no egress" only for a confirmed stop. Only a failed write answers `revive_recovery_unresolved`. |
| `revive_live_too_soon` | `POST /runs/{id}/revive` and the admin bulk restart (`run_revive.go`), `429`: the run is live and this `wardynd` process started replacing its proxy less than a minute ago (a revive that left the proxy untouched, `revive_proxy_kept_current`, does not count). The bound is per process, so each replica allows one a minute. A revive of a lost run is never bounded. |
| `revive_containment_recovering` | `POST /runs/{id}/revive` and the admin bulk restart, `409`: the run's runner contained it while offline. A recovery is queued; revive the run again once the runner has reconnected. |
| `run_capacity_store_unavailable` | `GET /admin/runs/capacity`, `501`: the configured store cannot report fleet capacity. The Postgres store always can; only a test double or an alternative store lacks it. |
| `unit_unreadable` / `unit_gone` / `capability_agent` / `capability_workspace` / `capability_model_provider` / `capability_policy` / `capability_workspace_provider` / `capability_component` / `capability_unknown` / `model_credential_erased` / `model_provider_disabled` / `model_provider_gone` / `component_gone` / `owner_unverifiable` | `ownerRefusal`'s own reason values (`run_owner_authority.go`): the shared re-check a revive and a run-end-extension both run over the owner's launch-door capabilities, governance profile, model provider and model credential. `model_provider_disabled` is the run's credential-supplying integration or its own model provider turned off; `model_provider_gone` is that provider deleted (or re-created under the same id); `component_gone` is an organisation's component the run was launched with, deleted since (409, for the owner and for an admin). The `capability_*` values name the six kinds `capabilities.go` enumerates; `capability_unknown` is a fallback outside that closed set. `owner_unverifiable` is `extendRefusal`'s bucket for three arms answering "the owner's authority could not be confirmed now". |
| `run_end_wait_neither_field` / `run_wait_budget_not_a_number` / `run_end_wait_already_finished` / `run_end_wait_files_gone` / `run_end_wait_max_lifetime` / `run_end_wait_store_unavailable` / `run_limits_gate_denied` / `run_end_no_end_not_allowed` / `run_end_must_be_future` / `run_wait_budget_too_small` / `run_end_wait_changed` | `PATCH /runs/{id}`'s end-time and wait-budget door (`run_end_wait.go`). It can also answer `lock_unavailable` (`503` with `Retry-After`). `run_limits_gate_denied` is shared across all four fields the `user_changes_limits` gate can block — the same gate regardless of which field triggered it. |
| `run_end_conflicting_fields` / `run_end_after_too_small` / `run_end_after_too_large` | `POST /runs`' create-time end (`run_end_create.go`): `ends_after_sec` and `no_end` together, `ends_after_sec` below 1, or past 2147483647. A `no_end` the pool, the deployment or the person's governance does not allow answers `403` `run_end_no_end_not_allowed` or `run_limits_gate_denied`, the same reasons the end-time `PATCH` gives; an `ends_after_sec` past a limit is cut to it and the `201` warns, never refused. |
| `workspace_not_found` / `user_drive_not_found` / `user_drive_allocation_not_found` / `ssh_key_not_found` / `preset_not_found` / `policy_not_found` / `capability_grant_not_found` / `run_not_found` / `governance_membership_not_found` / `governance_unit_not_found` / `enrolment_token_not_found` / `device_not_found` / `delegate_not_found` / `api_token_not_found` / `role_mapping_not_found` | `notFoundIf`'s (`helpers.go`) per-resource-kind 404s, one for each of its ~27 call sites across the package. `run_not_found` is the same literal `auditRenewDenied` already wrote for a renewal on a run that no longer exists; `preset_not_found` is the admin preset-management route's own 404, distinct from `preset_unknown` (a run naming an unknown preset at launch); `governance_unit_not_found` is `governance.go`'s own by-id GET/PUT/DELETE, distinct from `unit_gone` above (a run's already-captured profile going missing later). |
| `invalid_id_param` / `missing_run_claims` / `run_id_mismatch` / `workspace_store_unavailable` / `scan_upload_run_not_found` / `scan_upload_not_governed` / `scan_upload_wrong_task` / `workspace_requirements_invalid` / `workspace_approved_egress_invalid` / `workspace_approved_egress_dead_host` / `workspace_denied_egress_invalid` / `workspace_llm_cred_invalid` / `request_body_too_large` / `request_body_unreadable` | `helpers.go`'s shared refusals, reused by dozens of the package's call sites since the cause is the same whichever handler hits it: a `{param}` path segment that fails to parse as a UUID, the internal run token's claims failing to read, a scan upload's own 3-arm refusal (`scanresult.go`'s sole caller), `scopedWorkspaceWrite`'s 4 validators (one cause bucket per route; `workspace_approved_egress_dead_host` is its own reason because the remedy differs — use the already-routed door, not fix a malformed domain), and `readCappedBody`'s too-large/unreadable split. |
| `ado_capability_unknown` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, and a launch preset's `inline_policy` (`/api/v1/presets`): the spec's `azure_devops_capabilities` names something the Azure DevOps capability catalogue cannot grant. Its own reason rather than the door's `policy_request_invalid` / `inline_policy_invalid` bucket. |
| `ado_capabilities_none_permitted` | `POST /runs/preflight` and `POST /runs/policy-preview`: a member's `azure_devops_capabilities` leaves nothing standing on the per-person Azure DevOps lane (none of it is in the provider row's default profile or their governance profile's list). Review's mirror of the same-named refusal launch gives at dispatch. |
| `ado_capabilities_above_ceiling` | `POST /runs/preflight` and `POST /runs/policy-preview`: the `azure_devops_capabilities` that stand for the person hold a capability past the provider row's `capability_ceiling`. Asked after `ado_capabilities_none_permitted`. Launch gives the same sentence at dispatch as `capability_ceiling`; create does not refuse it. |
| `git_pat_narrowing_needs_broker` | `POST /runs/preflight` and `POST /runs/policy-preview`, the failure of a launched run, and `409` from `POST /runs/{id}/revive` and the admin restart of a run launched while the broker was on: a `git_pat` grant sets `repos`, `access` or `api` while the PAT broker is off (`WARDYN_GIT_PAT_BROKER`). Only the broker enforces the narrowing; with it off the PAT is resident in the sandbox and nothing narrows it, so the run is refused. |
| `git_pat_narrowing_ssh_conflict` | `PUT/POST /api/v1/policies`, `POST /runs`' `inline_policy`, a launch preset's `inline_policy` and `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access` or `api` and the same policy or run holds an `ssh_key` for the same forge (`github.com` and `ssh.github.com` are one forge). SSH is a second push path the broker cannot see, so the narrowing would not bind. |
| `git_pat_narrowing_unsupported_host` | `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `repos`, `access`, `api` or `forge` for a host another lane serves and that lane does not read those fields: an Azure DevOps host or one the run's Azure DevOps gate covers, or the host of a forge the run is GitHub-brokered for (its PAT is withheld). |
| `git_pat_api_forge_disabled` | `POST /runs/preflight` and `POST /runs/policy-preview`, and the failure of a launched run: a `git_pat` grant sets `api` for `bitbucket_server` while `WARDYN_GIT_PAT_API_BITBUCKET_SERVER` is off on this deployment. Policy and governance writes refuse the same grant with `400`. |
| `git_pat_broker_off_resident` | `POST /runs/preflight` and `POST /runs/policy-preview` (a run no pool places), the failure of a launched run, and `409` from `POST /runs/{id}/revive` and the admin restart: 0.10.0, the PAT broker is off (`WARDYN_GIT_PAT_BROKER`), so a `git_pat` grant's PAT would be minted into the sandbox, and the run is a member's (on revive, one not launched with the admin token or in local mode, unless its owner revives it while their role is an operator's), or `WARDYN_GIT_PAT_BROKER_OFF_REFUSED` is set. A `runner_resident` grant, an Azure DevOps host and a GitHub-brokered forge are not refused. An admin whose runs `WARDYN_GOVERN_ADMIN_RUNS` governs is still refused on revive (`409`). |
| `push_deny_all_resident_pat` | `POST /runs/preflight` and `POST /runs/policy-preview`, the failure of a launched run (at create or the queue's start), and `409` from `POST /runs/{id}/revive` and the admin restart: the run's `push_rules` set `deny_all` while the PAT broker is off (`WARDYN_GIT_PAT_BROKER`), so a `git_pat` would sit in the sandbox, where its pushes can't be stopped. Refused on an operator's run too. |
| `push_deny_all_ssh_key` | The same doors: the run's `push_rules` set `deny_all` and it holds an `ssh_key`, whose pushes over SSH no broker sees. |
| `policy_request_invalid` / `policy_secret_refs_invalid` / `policy_name_conflict` | `PUT/POST/DELETE /api/v1/policies`: the create/update body fails `decodePolicyRequest`, a secret reference in the spec fails shape validation, or a policy by that name already exists. |
| `model_provider_id_invalid` / `model_provider_not_applicable` / `model_provider_no_block_configured` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` model-provider authorization (`run_provider_authorization.go`), the three field-validation arms outside `writeProviderRefusal` (which always carries its own reason, either the credential-refusal's audit reason or the generic `model_provider_unavailable`): `model_provider` is not a plain provider id, was set on a run that calls no model, or was named but this deployment has no model providers. `integration_id` is refused earlier, unconditionally (`integration_id_retired`), before this door is reached. |
| `component_ref_invalid` / `component_store_unavailable` / `component_definition_invalid` / `component_secret_not_owned` / `component_secret_missing` / `component_host_denied` / `component_host_serves_model` / `component_host_collision` / `component_resident_delivery_denied` / `component_builtin_lane_off` / `component_builtin_access_above_ceiling` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` component gate (`components_run.go`), over the request's `components[]`: the list is malformed (400); this store cannot record components (501); or one component breaks a rule (422) — its definition, a secret that is not the caller's own, a secret the organisation provides and has not stored (the preview alone does not refuse this one), a host that is blocked, that serves a model or that already carries a credential on the run, or env/file delivery the organisation turned off; or a built-in GitHub component whose provider has its GitHub App lane off, or whose `access` is above the ceiling. A built-in GitHub component is also refused `400` `component_ref_invalid` when its grant fails the broker's scope-shape rule, or when it would give the run a brokered GitHub grant beside an `ssh_key` or `git_pat` grant for the same forge (a brokered forge is single-lane). Who may attach a component is the authorization reasons `capability_component` and `capability_feature`. |
| `cli_no_github_repo` | `POST /runs` and `/runs/preflight` (`stepGitHubCLIRepo`, `runs_cli_github.go`): a GitHub CLI row is attached and the run has no GitHub repository on the App lane for it to act on (422). The policy preview reports it as the row's `reason` instead. |
| `component_cli_aws_inject_off` / `component_cli_sign_in_required` / `component_cli_aws_bedrock_sign_in` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for an AWS CLI component (422): `WARDYN_AWS_SSO_PROXY_INJECT` is off, which would put the person's AWS sign-in in the sandbox, so every door refuses the row; the caller has not signed in for the row (`POST /components/{id}/sign-in`); or the run's model, or an included harness's, comes through an Amazon Bedrock sign-in, and a sandbox holds one AWS profile. The preview does not refuse the second one: it lists a `sign_in` setup item instead. An Azure CLI component answers `component_cli_sign_in_required` the same way when the caller has not signed in to Azure for it (`GET /components-entra/signin?component=<id>`). |
| `allowed_models_unchecked` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` (403): your governance profile limits the models runs may use, and the run's chosen model provider makes calls the run's proxy cannot read to check (an Amazon Bedrock sign-in the sandbox signs itself, with `WARDYN_AWS_SIGV4_AT_PROXY` off). Dispatch refuses the same lane, and the run fails with the same sentence. |
| `component_cli_azure_no_entra` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for an Azure CLI component (422): this deployment's console does not sign people in with Microsoft Entra, and only that application's sign-in can be bound to a person. The row is refused at every door, since the other way to serve it is a token in the sandbox. |
| `cli_sign_in_not_aws` / `cli_sign_in_preview_blocked` / `cli_sign_in_capture_changed` | `POST /components/{id}/sign-in` (`cli_aws_signin.go`): the component is not an AWS CLI row (422), or an admin previewing a person's view would capture into their own identity (409). The third is the AWS sign-in sandbox's upload (409): the row was removed, or its portal, region, account or role changed, while the sign-in was open. A component the caller may not attach answers as the run doors do, `capability_component`. |
| `cli_sign_in_not_azure` | `GET /components-entra/signin?component=<id>` (`cli_azure_signin.go`), the Azure CLI component's Entra sign-in: the component is not an Azure CLI row (422). The door shares the Azure sign-in doors' other answers: `azure_sign_in_no_session` (403), `azure_sign_in_unconfigured` (404), `cli_sign_in_preview_blocked` (409), and through the shared callback `azure_callback_cookies_invalid`, `azure_callback_missing_code` and a redirect to `/?azure_cli_signin_error=<code>&component=<id>` with the Azure sign-in callback's codes. |
| `credential_host_collision` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview`, for any run (422, `credentialHostRefusal` in `runs_dispatch_components.go`): two of the run's credentials are bound to one host, and a host carries only one — two `api_key` grants, or an `api_key` grant on a host that a token-bearing redirect, the Azure DevOps lane or a `git_pat` API grant also credentials. The body names no host. Also the `reason` on a failed run's `run.create` row when the collision only appears at dispatch; that row carries the `grant_ids` involved and never the host. `component_host_collision` is the same rule for a component's own header. |
| `run_title_store_unavailable` | `PATCH /runs/{id}/title` (`run_title.go`): this store cannot rename a run. |
| `run_inspect_no_runner` / `run_inspect_terminal` / `run_inspect_no_sandbox` / `run_inspect_paused` / `run_inspect_agent_stopped` / `run_inspect_exec_stream_unsupported` / `run_resources_read_failed` / `run_files_no_exec_session` | `GET /runs/{id}/resources` and `GET /runs/{id}/files` (`run_resources.go`, `run_files.go`): the two widgets read the identical run-state facts and share a reason per cause rather than each inventing its own synonym. `run_inspect_paused` also answers the Files panel's list (`desktop_files.go`). |
| `run_exec_invalid` / `run_exec_unavailable` / `run_exec_busy` / `run_exec_failed` | `POST /runs/{id}/exec` (`run_exec.go`): the command is empty or over 128 KiB, or `timeout_sec` is outside 1 to 300 (`400`); the run cannot take a command now, and the message says why (`409`); the run's SSH channels are all in use (`429`); the runner could not start the command, or lost it before it ended (`502`, with no exit code). A runner with no exec stream answers `501 run_inspect_exec_stream_unsupported`. |
| `audit_unavailable` | Strict audit (`WARDYN_AUDIT_STRICT`, `audit_strict.go`), `503`: a covered change was refused because its audit intent row could not be written first. Nothing was changed; try again. |
| `run_output_tail_invalid` / `run_output_interactive` / `run_output_not_captured` / `run_output_off` / `run_output_not_kept` / `run_output_expired` / `run_output_erased` | `GET /runs/{id}/output` (`run_output.go`): `?tail=` is not a positive number of bytes (`400`); the run is interactive with no pane snapshot (`409`): with recording off the sentence says nothing was kept, with it on it points at the recording; the runner exposes no direct stdout and no authorized recording-derived output is available (`409`); this deployment keeps none, or refuses stored rows (`WARDYN_EXEC_OUTPUT_TAIL=off`, `409`); no output is kept for the run — it finished before output was persisted, its runner captures none and no recording arrived, or it is still being captured and a later read serves it (`409`); a sign-in run is interactive and answers `run_output_interactive`; the in-memory tail outlived `WARDYN_EXEC_OUTPUT_TAIL_TTL`, or the run ended longer ago than `WARDYN_RUN_OUTPUT_RETENTION_DAYS` and its row was deleted (`410`); the run's output was erased (`404`). |
| `run_sign_in_not_aws` / `run_sign_in_unreadable` | `GET /runs/{id}/sign-in` (`run_sign_in.go`): the run is not an AWS sign-in run (`409`); the sign-in pane, or the capture's audit row, could not be read in time — a read a moment later may answer (`503`). |
| `run_resume_not_running` / `run_resume_failed` | `POST /runs/{id}/resume` (`run_pause.go`): the run is not in a resumable state, or thawing it for exec failed. |
| `internal_decision_log_invalid` / `groundtruth_batch_invalid` / `groundtruth_batch_too_large` / `groundtruth_action_not_kernel` / `groundtruth_write_failed` / `internal_approval_request_invalid` / `unsupported_internal_approval_kind` / `missing_requested_scope` / `reserved_scope_key` / `mcp_call_scope_invalid` / `mcp_call_too_large` / `kube_call_scope_invalid` / `host_call_scope_invalid` / `internal_approval_count_unavailable` / `internal_approval_cap_reached` / `broker_not_configured` / `mint_grant_id_required` / `brokered_forge_single_lane` / `brokered_forge_single_lane_unverifiable` / `grant_run_mismatch` / `grant_not_found` / `grant_requires_spire` / `grant_run_revoked` / `grant_not_approved` / `grant_ref_ruleset_required` / `grant_kind_unsupported` / `run_renew_store_unavailable` / `run_renew_read_failed` / `run_renew_stamp_failed` / `run_renew_lease_busy` / `internal_liveness_read_failed` | `POST /internal/*` (`internal.go`, `internal_live_run.go`): the sidecar/proxy surface, not the member-facing API. Most values are already the exact strings each route's own audit row wrote before #656 slice 3 put them on the wire too; `internal_liveness_read_failed` is the shared `/internal/*` liveness gate every sidecar door runs through. |
| `store_unavailable` / `not_found` / `refused` / `resolve_failed` / `store_refused` | `POST /runs` also answers `store_unavailable` when the secret store cannot be listed to check a policy's secret references. The credential-injection sinks' own closed set (`injection.go`'s `storeReadRefusal` and its callers across `injection_provider_key.go`, `injection_awssso.go`, `provider_subscription.go`, `internal.go`): the credential store did not answer, the named secret is not in the store, the secret exists but the store refused to serve it, resolving a subscription/managed token failed for a reason other than an unreachable store, or (`store_refused`, a person's own model-provider credential specifically) the store refused it. |
| `local_mode_peer_not_loopback` / `local_mode_host_not_loopback` / `admin_token_not_configured` / `missing_bearer_token` / `invalid_admin_token` / `admin_token_rate_limited` / `identity_provider_not_configured` / `missing_run_token` / `invalid_run_token` / `missing_sensor_token` / `invalid_sensor_token` | `http.go`'s own auth middlewares — local mode's loopback/CSRF gates, and the admin/run/sensor bearer-token chains. `admin_token_rate_limited` is a `429` with `Retry-After: 10`: the client address has used its failed admin-token attempts. Every value is already the exact string its own `auditAuthFailed(As)` call wrote before #656 slice 3 put it on the wire too; `identity_provider_not_configured` is shared by the run-token and sensor-token chains (the identical cause, no embedded identity provider wired). |
| `ui_gateway_not_found` / `ui_gateway_ticket_query_on_post` / `ui_gateway_invalid_form_body` / `ui_gateway_method_not_allowed` / `ui_gateway_invalid_run_id` / `ui_gateway_wrong_host` / `ui_gateway_ticket_invalid` / `ui_gateway_ticket_lookup_failed` / `ui_gateway_ticket_run_mismatch` / `ui_gateway_not_running` / `ui_gateway_run_kept` / `ui_gateway_policy_lookup_failed` / `ui_gateway_app_not_declared` / `ui_gateway_no_session` / `ui_gateway_session_destination_mismatch` / `ui_gateway_no_runner` / `ui_gateway_resume_failed` / `ui_gateway_conn_cap_reached` / `ui_gateway_exec_failed` / `ui_gateway_launcher_exec_failed` / `ui_gateway_launcher_missing` / `ui_gateway_launcher_not_listening` / `ui_gateway_launcher_probe_failed` / `ui_gateway_connection_closed` / `ui_gateway_bind_missing_ticket` / `bind_not_same_site` / `bind_origin_not_console` | The UI gateway (`uigateway.go`, `uigateway_session.go`, `uigateway_bind.go`): the second, un-authenticated-by-session origin that serves a sandbox's own web app. `ui_gateway_ticket_invalid` covers "not bound to this browser" and "invalid/expired/already-used", so a probe cannot tell a binding failure from a bad ticket; `ui_gateway_ticket_run_mismatch` covers "the run could not be read" and "the ticket's principal is not this run's owner", the existence-oracle-safe parity `getRunAuthorized` uses. `bind_not_same_site`/`bind_origin_not_console` are `uiBindRefusal`'s own closed set. |
| `inbound_credential_invalid` / `inbound_not_found` / `inbound_rate_limited` / `ui_gateway_cross_site` | The UI gateway's inbound arm (`uigateway_inbound.go`), for a call to an app declared `inbound: true`: the `Authorization: Bearer` token is not a live inbound token (401, `WWW-Authenticate: Bearer`; an API, CLI or admin token is refused the same way); the token's holder cannot call this app, one answer for no run, no such app, an app that is not inbound and a holder who is neither the owner nor a caller (404); the run's inbound apps are past 50 requests a second, burst 100 (429, `Retry-After: 1`). `ui_gateway_cross_site` (403, host mode) refuses a session request from another site that is not a top-level navigation, or one naming an `Origin` other than its own. |
| `inbound_needs_host_mode` | `POST /runs/{id}/apps/{app}/ticket` (`inbound_ticket.go`), the browser ticket to an inbound app: the caller is one the app's `callers` names, not the run's owner, and this deployment serves apps from one shared host (path mode), where another run's app could script this one (403). Only the owner opens an app in a browser until an administrator sets per-run hosts (`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`); an inbound token works in both modes. |
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
| `model_provider_credential_no_store` / `model_provider_credential_no_person` / `model_provider_credential_is_sign_in` / `model_provider_credential_body_invalid` / `model_provider_credential_too_short` / `model_provider_credential_store_unavailable` / `model_provider_credential_signs_in` | `/api/v1/model-providers/{id}/credential` (`model_provider_credentials.go`): the console's own key/token storage door, distinct from the sign-in door and run-create's model-provider choice. `model_provider_credential_signs_in` is the 409 for a provider that signs people in: its credential is made at that provider's sign-in door, not pasted. |
| `endpoint_oidc_unconfigured` / `endpoint_oidc_unknown_row` / `endpoint_oidc_no_session` / `endpoint_oidc_authority_unavailable` / `endpoint_oidc_callback_invalid` / `endpoint_oidc_callback_missing_code` | The sign-in door and callback of a custom endpoint that signs people in (`endpoint_oidc.go`). `endpoint_oidc_callback_invalid` buckets every state, one-time cookie, signature, session and stamped-row cause as one (start the sign-in again). |
| `lock_unavailable` | Any write door that serializes on a lock, a cross-replica one or a runner's row lock (a site-configuration or capability-enforcement write, a credential erase, a sign-in capture, an edit of a run's end (`PATCH /runs/{id}`), a revive of one run or, for a run on a runner, another revive on a runner of the same owner, a runner revoke (`DELETE /runners/{id}` and `/me/runners/{id}`), the audit chain verification), `503` with `Retry-After`: the lock is held elsewhere past its wait, the process's lock connections are all in use, or the database could not be asked. Nothing was done and the request is safe to retry; a lock is never skipped. |
| `audit_invalid_run_id` / `audit_export_store_unavailable` / `audit_export_read_failed` / `audit_scope_unavailable` / `audit_chain_verify_store_unavailable` / `audit_chain_verify_busy` / `audit_chain_sweep_failed` / `audit_invalid_timestamp_param` / `audit_invalid_actor_type` / `audit_invalid_origin` / `audit_unit_target_filter` / `audit_invalid_export_form` / `audit_partition_not_found` / `audit_partition_open` / `audit_retention_store_unavailable` / `audit_retention_read_failed` / `audit_retention_body_invalid` / `audit_retention_drop_failed` | `/api/v1/admin/audit` (`audit.go`, `audit_partition_export.go`, `audit_retention.go`): the security tier's audit-log query, export, chain-verification and retention doors. `audit_invalid_export_form`, `audit_partition_not_found` and `audit_partition_open` are `GET /audit/export?partition=`'s: a `form` neither `readable` nor `raw`, a name no audit partition has, and a partition that can still receive rows (no digest yet). `audit_retention_store_unavailable` (`501`), `audit_retention_read_failed` (`500`, the status could not be read), `audit_retention_body_invalid` (`400`, `POST /audit/retention/drop` needs `{"partition", "digest"}`) and `audit_retention_drop_failed` (`500`, nothing was dropped; `503` under `WARDYN_AUDIT_SEAL=full` while the caller's audit subject cannot be had) are `GET /audit/retention`'s and `POST /audit/retention/drop`'s; an unknown partition on the drop is `404` `audit_partition_not_found`. `audit_unit_target_filter` (`400`): a unit admin's `target` filter outside `run.`, `egress.` and `approval.` actions. |
| `audit_receipt_request_invalid` / `audit_receipt_not_found` / `audit_receipt_pending` / `audit_receipt_unchained` / `audit_receipt_too_large` / `audit_receipt_signing_off` / `audit_receipt_store_unavailable` / `audit_receipt_read_failed` | `GET /api/v1/audit/receipt`, `/audit/checkpoints` and `/audit/receipt-keys` (`audit_receipt.go`), the security tier's signed audit receipts. `400`: events or one run, not both and not neither; `after_seq` and `limit` belong with `run_id`. `404`: no such event, or no rows for the run. `409` `audit_receipt_pending` with `Retry-After`: the event is newer than the last signed checkpoint. `409` `audit_receipt_unchained`: it is in no checkpoint and never will be (no row hash, or older than the first checkpoint). `413`: more than 1000 events, 10 checkpoints holding them or 500 between them; a run's receipt is cut at the cap instead, with `next_after_seq`. `503`: checkpoint signing is off or its key could not be loaded. |
| `recording_chain_key_required` / `recording_chain_busy` | `GET /runs/{id}/recording-chain`: `400`, no `key`; `503` with `Retry-After` while another check walks the log on that replica. |
| `source_not_found` / `source_scan_already_running` / `source_scan_unsupported_kind` / `source_scan_failed` / `source_scan_no_runner` | `POST /api/v1/sources/{id}/scan` and the admin bulk scan (`source_scan.go`). `source_scan_failed` is `scanLocalDirSource`'s own bucket for whatever detail the scan itself failed on. |
| `source_scan_runner_bound` | A source scan of a folder bound to a runner, which Wardyn never reads; its status is unchanged. |
| `inline_policy_xor` / `policy_id_not_found` / `inline_policy_invalid` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` policy resolution (`run_policy_resolution.go`): `policy_id` and `inline_policy` were both set, the named policy does not exist, or (`inline_policy_invalid`, the whole resolution chain's bucket — grant filtering, domain-count cap, spec/secret-ref validation) the policy fails validation. |
| `ui_layout_invalid_preset` / `ui_layout_too_many_widgets` / `ui_layout_unknown_widget` / `ui_layout_invalid_geometry` / `ui_layout_persistence_unavailable` | `GET/PUT /me/run-layout` (`ui_layout.go`): the console's own saved-layout door. |
| `ado_sign_in_unconfigured` / `ado_sign_in_foreign_app` / `ado_sign_in_no_session` / `ado_sign_in_scope_invalid` / `ado_sign_in_prompt_invalid` | `/scm/azure-devops/signin` and its callback (`ado_entra.go`): the console's own Azure DevOps per-person sign-in doors, distinct from `ADOEntraFailure`'s own enum above. `DELETE /scm/azure-devops/connection` (`ado_pat_console.go`) answers `ado_sign_in_no_session` for a caller with no sign-in subject and `ado_sign_in_unconfigured` (404) when there is no per-person row the caller may use. `ado_sign_in_prompt_invalid` is `?prompt=` set to anything other than empty or `select_account`. |
| `azure_sign_in_unconfigured` / `azure_sign_in_unknown_row` / `azure_sign_in_no_session` / `azure_callback_cookies_invalid` / `azure_callback_missing_code` | `GET /model-providers-entra/signin?uid=` and the callback it shares with the Azure DevOps sign-in (`azure_foundry_entra.go`): the per-row Azure Foundry capture. No console Entra sign-in is configured (404); the `uid` is not an `azure_foundry` provider (404); the caller has no session subject (403); the one-time nonce or verifier cookie is missing, or the stamped row is malformed (400); the authority redirected back with no code (400). A refusal the person can act on is a redirect to `/?azure_signin_error=<code>` with the Azure DevOps callback's codes plus `row_changed`. |
| `ado_own_pat_unknown_row` / `ado_own_pat_token_invalid` / `ado_own_pat_expiry_invalid` / `ado_own_pat_expiry_too_long` / `ado_own_pat_rejected` / `ado_own_pat_identity_mismatch` / `ado_own_pat_check_unavailable` / `ado_own_pat_request_refused` | `PUT`/`DELETE /me/scm/azure-devops/token` (`ado_own_pat.go`): a person adding or removing their own Azure DevOps token. On PUT, no own-token row the caller may use has that address (a row they may not use answers the same). On DELETE only when no Azure DevOps row there holds a token of the caller's and none is usable: a token holder can remove it after their access was withdrawn (#1479); the pasted value is empty, too long or has spaces; `expires_on` is not a date after today, or is past the row's `pat_max_days`; Azure DevOps did not accept the token for the organisation; the token belongs to another account (never named); Azure DevOps could not be asked; or Azure DevOps answered `400`, refusing the request, not the token (`502`; a stored token is never marked refused for it). |
| `github_own_pat_no_session` / `github_own_pat_preview_blocked` / `github_own_pat_unknown_row` / `github_own_pat_token_invalid` / `github_own_pat_expiry_invalid` / `github_own_pat_expiry_too_long` / `github_own_pat_rejected` / `github_own_pat_check_unavailable` / `github_own_pat_account_mismatch` | `PUT`/`DELETE /me/scm/github/token` (`gh_own_pat.go`): a person adding or removing their own GitHub token on a GitHub row with `credential_source: per_user`. The caller has no browser sign-in (`403`) or is in the user view (`409`, both doors); no such row the caller may use has that address (a row they may not use answers the same; DELETE answers it only when they hold nothing on any GitHub row there); the value is not one fine-grained token (`github_pat_…`), is too long or has spaces; `expires_on` is not a date after today, or is more than 30 days out; GitHub did not accept the token (`422`); GitHub could not be asked or said nothing about the token (`503`); or the token belongs to another GitHub account than the row's and `relink` was not set (`409`, the account never named). |
| `github_own_token_required` / `github_own_token_expired` / `github_own_token_undeliverable` / `github_own_token_resident` | A run whose `git_pat` grant for github.com is decided by an own-token GitHub row (`gh_own_pat_run.go`), refused at launch (`422`): the person has added no token of their own, or it has reached the expiry they entered; or it could not be kept out of the sandbox (the PAT broker is off, the run is on the person's own runner) or two such rows claim the run's github.com repositories. `github_own_token_resident` is the mint door (`403`) refusing that grant while the PAT broker is off. |
| `ado_callback_cookies_invalid` / `ado_callback_missing_code` / `identity_binding` / `unusable_grant` | The callback half of the same door (`consumeADOCookies`, `handleADOCallback`): browser-reachable, since the identity provider's own redirect lands here directly. `ado_callback_cookies_invalid` buckets all three single-use state/nonce/pkce cookie causes — the remedy is identical for all three (start the sign-in again). `identity_binding` and `unusable_grant` are the SAME strings this callback's own audit row already carried for those two causes. |
| `identity_issuer_mismatch` | `GET /api/v1/auth/provider-oidc/callback`, as its `provider_oidc_error` value: under the `console` binding the identity token came from an authority other than the console's own. The admin fixes `oidc.issuer`; `identity_binding` is someone else's account. |
| `user_view_no_human` / `user_view_invalid_field` / `user_view_type_invalid` / `user_view_no_session` | `POST /api/v1/me/view` (`user_view.go`): the admin/security-admin user-view toggle. |
| `source_write_invalid` / `source_delete_conflict` / `source_in_use` | `/api/v1/sources` (`sources.go`): the shared source library. `source_write_invalid` is `validateSourceWrite`'s own bucket. |
| `branding_not_branded` / `branding_store_unavailable` / `branding_body_unreadable` | `/api/v1/admin/branding` (`branding.go`). |
| `org_revoked` / `internal_error` | `writeServerError`'s own classified/unclassified split (`writeservererror.go`): the one 5xx chokepoint every otherwise-unclassified server-side failure in this package routes through. `internal_error` is deliberately the single generic fallback — never the driver text the error carries (that stays in the log line, not the wire), just enough for a caller to tell "server-side, not yours" from a specific classified cause. |
| `namespace_quota_exceeded` | `POST /api/v1/runs` and `POST /api/v1/runs/preflight` (`run_fit.go`): the runs namespace's `ResourceQuota` objects cannot hold this run (both its pods, requests and limits, honouring quota scopes, plus its one Secret and two NetworkPolicies against `secrets`, `count/secrets` and `count/networkpolicies.networking.k8s.io`), `422` before the identity mint, so no run row and no sandbox. The message names the quota and the numbers. Not audited, like `run_quota`. The quota is read as it stands and nothing is reserved, so a concurrent run can still take the room; the quota's own admission stays the authority. Kubernetes only |
| `preflight_rate_limited` | `POST /api/v1/runs/preflight` (`preflight.go`): the person already made `WARDYN_PREFLIGHT_RATE_PER_MIN` checks this minute (burst 5), answered `429` before any gate runs. Not audited, like `run_quota`. The limit is per wardynd replica and never applies to the admin token or to `POST /runs`; [`scripts/ci-run.sh`](../scripts/ci-run.sh) already treats a failed preflight as a warning. |
| `profile_synthesis_busy` | `POST /runs/{id}/profile`, `429` with `Retry-After`: two profile syntheses already run on this replica; retry when one finishes. |
| `policy_preview_rate_limited` | `POST /api/v1/runs/policy-preview`: this person's independent draft-preview bucket is exhausted (`WARDYN_POLICY_PREVIEW_RATE_PER_MIN`, default 60/minute, burst 15); `429` with `Retry-After` seconds, before gates and without audit. Per replica; admin token exempt. |
| `directory_search_query_too_short` / `directory_search_unknown_type` / `directory_search_rate_limited` / `directory_search_failed` | `GET /api/v1/directory/search` (`directory_search.go`). |
| `base_image_write_invalid` / `base_image_in_use` / `base_image_not_found` | `/api/v1/base-images` (`base_images.go`). `base_image_write_invalid` is `validateBaseImageWrite`'s own bucket. |
| `prewrapped_image_not_pinned` / `prewrapped_check_unavailable` / `prewrapped_check_in_progress` / `prewrapped_already_ready` | `/api/v1/base-images` (`base_images_prewrapped.go`): a prewrapped image is registered by digest and checked on this control plane's own Kubernetes cluster, which must have NetworkPolicy enforcement proven; `POST /base-images/{id}/check` retries a refused or interrupted check only. |
| `prewrapped_image_not_registered` | A workspace write or a run named a prewrapped image with no ready row: register it under Images first, or check it again. |
| `prewrapped_platform_mismatch` / `prewrapped_platform_unknown` | Audit reasons on a failed `run.create`: the run's runner is not an architecture the prewrapped image was checked on, reports no architecture (older than 0.9.1), or the run is a macOS VM. |
| `credential_erase_principal_required` / `credential_erase_operator_namespace` / `credential_erase_signin_config_unreadable` | `DELETE /people/{principal}/credentials` (`credential_erase.go`). `credential_erase_signin_config_unreadable` (503): the Azure DevOps sign-in configuration could not be read, so the erase could not take the sign-in's lock and erased nothing; try again. |
| `secret_personal_needs_person` | `GET/PUT/DELETE /me/secrets` (`secrets_explicit.go`): the caller is no verified person (the admin token, local mode or a device), so it has no personal namespace; `403`, nothing stored. A delegated portal is refused earlier, with `403` `delegation_scope`. Use `PUT /shared-secrets/{name}` for a shared service secret. |
| `credential_erased_during_write` | Any write of a person's stored credential (`PUT /secrets/{name}`, a model-provider key or sign-in, an Azure DevOps own-token paste): the person's credentials were erased after the write began, so `409` and nothing was saved. Retrying starts a new write, which lands. |
| `erasure_scope_unknown` / `erasure_self_refused` / `erasure_operator_namespace` / `erasure_incomplete` | `POST /people/{principal}/erasure` (`person_erasure.go`, security tier). `erasure_scope_unknown` (400): `scopes` is not a non-empty list of `mask_copies`, `run_outputs`, `recordings`, `run_tasks`, `components`, `run_setups`, `runner_pools`, `member_drives`, `audit_personal_fields` and `credentials`; nothing was erased. `erasure_operator_namespace` (400): the principal names the operator namespace, no person's; nothing was erased. `erasure_self_refused` (403): the person named is the caller and a scope other than `credentials` was asked for; nothing was erased (the admin token, no person, is never refused). `erasure_incomplete` (500): a scope failed part way; the body's `done` and `remaining` name the scopes, and retrying the same scopes finishes the rest. The principal also refuses `owner_unresolved` / `owner_ambiguous` (422) as the credential erase does. |
| `explain_principal_invalid` | `GET /permissions/explain` (`capabilities_explain.go`). |
| `credential_inventory_no_meta` | `GET /model-providers/credentials` (`credential_inventory.go`). |
| `recording_off` / `recording_too_large` / `recording_invalid_part` / `recording_erased` / `part_limit` | `PUT /internal/recordings/{runID}` and `.../parts/{part}` (`recording.go`). `recording_off` (`409`) is an organisation that keeps no recordings, or a run marked `runner_declined`, its runner having declined to record it (only a recording runner older than 0.9.3 may still upload, which clears the mark); the routes are served either way, behind the run token and the relay binding. `recording_erased` is a durable per-run erasure fence (`410`), an upload streaming when the erase completed included; authentication, masking and size refusals keep precedence. `recording_invalid_part` is `{part}` failing to parse as canonical decimal >= 2; `part_limit` is the ONE name for a part above the limit, both on the wire and in the refusal's own `recording.upload` audit row's nested `reason` detail field. |
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
| `admin_surface` / `security_admin_surface` | The tier gates every admin/security-admin-only route runs through (`isOperator`/`isSecurityOperator`): the caller's stamped role is below the route's own floor. Both answer the byte-identical sentence "requires admin role" — the reason is what tells the two tiers apart. `admin_surface` also answers a governance profile write (`PUT/POST /governance/units`, and approving a held one) from below the super admin tier that sets a mount's host path or makes a kept mount writable; its target is `governance.unit.<field>` and its sentence names the field. |
| `audit_export_partition_filter` | `GET /audit/export?partition=` carried another filter (`run_id`, `since`, `until`, `action`, `action_prefix`, `actor`, `target`, `actor_type`, `outcome` or `origin`): a partition export always covers the whole partition. Answered `400` and not audited. |
| `audit_retention_not_oldest` | `POST /audit/retention/drop` named a partition that is not the oldest retained one. Answered `409` and audited (`authz.denied`, target `audit.retention`, `partition` in the row). |
| `audit_retention_not_closed` | `POST /audit/retention/drop`: the oldest partition can still receive rows. Answered `409` and audited. |
| `audit_retention_inside_window` | `POST /audit/retention/drop`: the partition ended less than the effective retention window ago, or retention is forever. Answered `409` and audited. |
| `audit_retention_live_run` | `POST /audit/retention/drop`: the partition holds audit rows of a run that is still live. Answered `409` and audited. |
| `audit_retention_digest_mismatch` | `POST /audit/retention/drop`: the digest supplied is not the one the database computes for the partition. Answered `409` and audited. |
| `audit_retention_actor_reserved` | `POST /audit/retention/drop`: the caller's actor name is `wardynd`, which is reserved for the retention sweeper. Answered `403` and audited. |
| `byoi_user` | A Bring-Your-Own-Identity principal reached a route BYOI does not extend to. |
| `capability_egress_host` / `capability_feature` / `capability_secret` | The capability-grant gates outside the five launch-door kinds above: an egress host, feature or secret the caller's grants do not cover. A feature flag also gates the invitation claim and the sponsor's service-token mint (`api_token`) and, as the foreign-run `404`'s audit reason, a sponsor entering a service identity's run (`service_attach`). A request that declares an inbound app is refused `403` on `POST /runs`, `/runs/preflight` and `/runs/policy-preview` until an administrator allows the `inbound` feature (target `runs.ui_apps`). The body's `feature` names it. |
| `delegation_scope` | A portal's delegated token asked for a route outside its own delegation allow-list (#1142). |
| `portal_unit_scope` | `POST /runs` or `POST /runs/preflight` through a portal registered with `run_in_under`, `403`: the context the run resolved to (the one named in `run_in`, or the person's default pick) is not that unit or one below it. Nothing was created. Choose a context inside the unit. |
| `portal_unit_unavailable` | The same check, `503`: the portal's record or the unit list could not be read. The launch is refused, never treated as unpinned; retry. |
| `service_scope` | A service identity's token asked for a route outside its fixed allow-list (#1508): run create, preflight, list, read, events, output, `PATCH` and kill, `GET /audit?run_id=`, `GET /presets`, `GET /workspaces`, `GET /me`, `GET /setup/status` and its own model-provider credential. Everything else, `GET /audit/export` included, answers `403`. |
| `event_stream_cap` | The caller already holds 32 open `GET /runs/{id}/events` streams, the most one principal may (#1407); close one and retry. Answered `422` and not audited, like `run_quota`. |
| `component_autonomy` | The organisation's autonomy cap on runs that use the caller's own custom components closes the door, with or without a governance profile; the body's `policy` names the deployment. |
| `placement_component_self_defined` | A run on your own runner carries a component you defined while your limits omit `local_self_defined_components` (default deny); `403`, target `runs.placement`. |
| `governance_unit` | The caller's resolved governance profile itself closes the door (distinct from `groups_snapshot_stale`, which is the profile being unresolvable at all). |
| `unit_scope_denied` | A unit-admin governance route (`requireUnitScope`, then the route's own scope check): `403`, audited. The caller is neither a security admin nor a unit admin ("this needs a unit admin or a security admin"). Or the caller is a unit admin and the target is outside the units they administer: a unit above or beside theirs, their own unit's admins, a person with no membership in their units, or a held change touching such a unit ("this is outside the units you administer"). |
| `mask_state_unavailable` | The server cannot prove a run's masking corpus complete, so a door that relays or persists the run's output (recording upload, live attach, SSH shell, live output read) refuses with `503` instead of passing bytes through. A run dispatched before 0.8.6 stays refused after a server restart until it ends; any other run clears when the server can read its manifest again. Since the shared masking registry an injection or capture route also answers it when a value could not be committed to the registry: the value is not handed out. |
| `governance_overlay_unsatisfiable` | The caller's governance profile, or the run's own, is composed and nothing satisfies it together with the profile or deployment default it builds on, so the launch and every live door refuse (`403`) rather than widen. The sentence names the person's own profile and never a base. |
| `grant_pairing_not_eligible` | The named capability grant is not eligible to pair with the request it was offered against. |
| `model_provider_unavailable` | `POST /runs`, `/runs/preflight` and `/runs/policy-preview` model-provider authorization, and the record door's (`POST /workspaces/{id}/record`) (`writeProviderRefusal`, `run_model_provider.go`): the generic bucket for a non-credential refusal (provider off, not serving this agent, none chosen, no such provider) — the credential-shaped refusal instead sends `model_credential` (below), which the console's sign-in door recognizes. |
| `recording_governed` | `POST /workspaces/{id}/record` by an admin whose runs are governed (`WARDYN_GOVERN_ADMIN_RUNS`): `403`, audited at target `workspaces.record`. The body names the remedy, `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT` set to `recording`. The admin token and local mode are never refused. |
| `run_kept` | The run is kept (ended or lost); its agent is stopped and nothing may act on it as if it were live. |
| `run_owner_only` | Interactive entry (attach-ticket mint or consume, the cookie attach lane, a UI app, take-over) to a run whose owner is a person, asked by a super admin who is not that person (#1476): `403`, with `error` "only the person who started this run can open it interactively". Unlike `not_owner` it is not hidden — the admin can already see the run. A run with no personal owner (operator-owned) stays enterable; kill, approve, policy, grants, revoke, audit, revive and resume are unchanged. Also the SSH gateway's refusal of a service identity's sponsor, whatever grant they hold (#1508). |
| `role_stamp_stale` | The `wdn_` API token's role and group stamp is older than the deployment's `WARDYN_ROLE_STAMP_TTL` (0.8.6); answered `401`. The token's owner signs in to the console again, which re-stamps it; mint nothing new. An inbound token on the app gateway is refused the same way. |
| `key_domain_unknown` | A key-domain assignment named a domain the deployment's key domains file does not declare (`PUT /key-domains/assignments/{subject_type}/{subject}`): `422`, audited as `authz.denied`. |
| `key_domain_ambiguous_membership` | A group key-domain assignment would leave people in groups assigned to different domains with no assignment of their own, or while a person whose last sign-in lost groups (a Microsoft Entra overage) has none, so their next principal key would be refused: `409`, audited as `authz.denied`, counting and naming them. Deactivated or purged people are not counted; deleting such a person's user assignment while a group is assigned is refused the same way, as is deleting the user assignment of a person whose groups are assigned to different domains. |
| `run_quota` | The acting principal's governance profile run-count or concurrency limit is at its cap. Not audited on its own (`Audit: false` in the registry): an authorized caller at a limit is no attacker. |
| `run_terminal` | The run has already reached a terminal state; the requested action no longer applies. |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN`'s own gate (and `WARDYN_CAPABILITY_SECOND_HUMAN`'s, for an Azure DevOps capability escalation): the deciding principal is the same one who raised the approval. |
| `not_a_decider` | `POST /approvals/{id}/{approve,deny}` (403): the run's owner decided an `egress_domain`, Azure DevOps or MCP call request that the deciders of the run's unit leave them out of. The message names who decides. |
| `decision_scope_above_rule` | `POST /approvals/{id}/{approve,deny}` (403): `decision_scope` reaches further than the unit deciders rule allows: past its `max_scope`, or `always`, which no rule allows. |
| `deciders_unreadable` | `POST /approvals/{id}/{approve,deny}` (503): the unit deciders, or the unit tree, could not be read, so the request is not decided now. The run's owner gets this; anyone else gets the usual `404`, and a security admin still decides. |
| `user_view_type_deleted` | An admin viewing through a user type that has since been deleted. `admin_view` (below) is the launch-door row this same cause answers with on `POST /runs`/`/runs/preflight`/`/runs/policy-preview`. |
| `admin_view` | An admin in the user view launched a run after the type the view looks through was deleted — the launch-door twin of `user_view_type_deleted`; the underlying cause is audited under that reason, this one is not audited on its own. |
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
| `logo_from_site_config` | `remove_logo` or a new `logo` on a logo the site configuration's `branding.logo_path` delivers; its next apply would put the file back. |

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

A deployment can require a second approver for governance writes: a covered write is
stored as a pending change, answered `202` with a `{"pending_change": {...}}` body, and
applied only once another approver approves it. The SDK never reads it as a saved object:

- Any method on the shared request path returns a `*client.PendingApprovalError`
  (`errors.As`), not a zero value. Only a `202` whose body has a
  `pending_change` key counts; `KillRun`, `RecordWorkspaceTask`, `ScanWorkspace`,
  `BuildWorkspace` and `ScanSource` also answer `202` and decode as before.
- With any write pending, `ApplyGovernance` (signature unchanged) returns the current document
  and a `*client.PendingApprovalError`, so an older caller fails loudly.
- `ApplyGovernanceResult` returns that outcome as data: the document, `Pending` changes,
  `Deferred` writes and `PruneSkipped`.
  - An assignment naming a profile whose write is
    pending is deferred, never sent.
  - So is a composed profile whose base's write is pending
    (a `Deferred` entry with `Base` set), and in turn its children and their assignments.
  - Prune does not run after a pending write.
- `PlanGovernance(ctx, doc, prune)` returns the creates, updates and deletes `ApplyGovernance` would
  make, as a `GovernancePlan`, and writes nothing: one `GET /api/v1/governance`.
  Its matching rules are `ApplyGovernance`'s. A plan is a snapshot: the server may change before an apply.
- `GET /governance` returns every profile and every assignment; a composed profile's own `ceiling` holds only what it stores, so read the resolved one under `effective.ceiling`. Units carry `kind`, `mode` and `depth`, memberships `home` and `legacy`, and the document `max_depth` ([governance writes](operations/governance-writes.md)). `/governance/units` and `/governance/units/{id}` take writes only, so a `GET` on either answers `405`.
  It also carries `boundary` (security admins only), `unit_admins` and, per unit but Default, `people`: the membership subjects in it or below.
  A unit admin reads only their subtrees, plus `parent_walls`: the walls above each subtree, with `provenance`.
  `unit_names` gives the name of each unit above their subtrees, by id, and nothing else about those units.
- `GrantUnitAdmin` (`POST /governance/units/{id}/admins`) and `RevokeUnitAdmin` (`DELETE …/admins/{admin}`) are `wardyn governance admins add|remove`; a unit admin's are always held ([unit admins](operations/unit-admins.md)).
  The governance file's `admins`, keyed by unit name, and each unit's `locked_knobs` apply and prune like memberships.
- `SetEnvelope` (`PUT /governance/units/{id}/envelope`) and `ClearEnvelope` (`DELETE` on it) set and clear a platform envelope, the unit that roots a platform and the IdP groups security approved for it. Security tier only, and held under the second-human switch. `GET /governance` lists `envelopes`; a unit admin reads those on or above their subtrees and inside them, with `set_by` `security` in place of the setter.
- `UnitModelUsage` (`GET /governance/units/{id}/model-usage`) reads the token usage of the runs frozen to a unit and the units below it, by model, for a unit admin of the unit or above, or a security admin. Runs created in `[since, until)` count (30 days to now by default, 92 at most); zero times send no parameter. Tokens are shown, not cost.
- `PutUnitDeciders` (`PUT /governance/units/{id}/deciders`) is `wardyn governance deciders set <unit> --file`; the file is `{"rules": [...]}`, and a unit admin's write is always held.
- A held change and a dry-run report list `approvers`: who could decide it. `GET /me`'s `unit_admin_of` names the caller's granted units.
- `MoveGovernanceMembership` (`PUT /governance/memberships/{id}`) and `PromoteGovernanceMembership` (`POST …/promote`, a legacy one becomes a context) follow the second-human switch ([governance writes](operations/governance-writes.md)).
- `CheckGovernance` (`POST /governance/check`, `wardyn governance check`) takes `RunIn`: returns `contexts` (each with its `path`), `dormant` memberships and the chosen or home context's `effective` walls with `provenance` and admin-only `path_provenance`, which path unit changed each knob (on `GET /governance` units, not SDK ones).
- `GovernanceDryRun` (`POST /governance/dry-run`, `wardyn governance dry-run`) takes a `target`: `boundary` (the default), `unit` (a unit body plus `id`) or `membership_move` (`{id, unit_id}`); the last two report `{target, people, live_runs}`.
- `CreateRunRequest.RunIn` (`run_in`, `wardyn run --run-in`) names one of the caller's units, by id or exact name. `GET /policies/default` takes `?run_in=`, returning `contexts` (absent for a member no unit places) and `unit_path` (security admin only).
- A run's `provenance`, and the check's, marks a knob `locked` when a unit above the run's unit, or the boundary in `hard_limits`, locks it, naming the nearest locker; a ceiling refusal on that knob sets `policy.locked`.
- `GovernanceApplyResult.Steps` lists an apply's writes in order, each `applied`, `pending`
  or `failed`. On a failed write `ApplyGovernanceResult` returns the steps so far with the error.
- `ApplyDrivesResult` is `ApplyDrives` with the same steps, as `DrivesApplyResult.Steps`; `ApplyDrives`
  keeps its signature. Applies are sequential, not atomic: a failed write leaves the earlier ones.
- `ListGovernanceChanges(ctx, state)`, `GetGovernanceChange`, `ApproveGovernanceChange` and
  `RejectGovernanceChange(ctx, id, reason)` read and decide the stored changes
  (`/api/v1/governance/changes`).
- `ListGovernanceRequests` (`GET /api/v1/governance/requests`), `RequestElevation(ctx, unit, duration, reason, ticket)`
  (`POST /api/v1/governance/requests`, kind `elevation`), `RequestImage(ctx, image, reason, ticket)` (kind `image`, one
  image by digest; `ImageOffered` on the list says whether the caller may ask), `ApproveGovernanceRequest` and
  `RejectGovernanceRequest(ctx, id, reason)` (`POST /api/v1/governance/requests/{id}/approve` and `/reject`) are the
  held requests. Any signed-in person reaches them; the list holds the requests the caller asked or may decide
  (pending, and decided in the last day) and `offered`, the units the caller may ask an elevation in. The
  requester's own reject withdraws. A server without requests answers `404`, returned as the `*APIError`.
  `PlanGovernance` and `ApplyGovernanceResult` leave memberships with an `expires_at` (elevations) out of both
  sides, so a saved document never writes one back as permanent or prunes it.

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
`unit_id: required` or is itself queued. Nothing applies without approval, but the error is
confusing. Upgrade the CLI and any SDK callers before requiring a second approver.

## Renamed in 0.8

The attach route family had three sub-resource shapes, and one concept was spelled four ways across the wire, Go, audit and the Helm chart.

- Each HTTP route below keeps its old path mounted as a chi alias for one minor; the SDK and CLI call the new path.
- The chart key is a clean break with no alias; see the "User drives" section of [`deploy/helm/wardyn/README.md`](../deploy/helm/wardyn/README.md).
- The 0.8.4 CLI command renames are listed in [releases/0.8.md](releases/0.8.md).

| Old | New | Kind |
|---|---|---|
| `GET /runs/{id}/attach-holder` | `GET /runs/{id}/attach/holder` | HTTP route, aliased for one minor |
| `POST /runs/{id}/attach-ticket` | `POST /runs/{id}/attach/ticket` | HTTP route, aliased for one minor |
| `POST /runs/{id}/profile` | `POST /runs/{id}/profile/synthesize` | HTTP route, aliased for one minor |
| Helm `userDrives.enabled` | Helm `drives.enabled` | Chart value, clean break (no alias); the chart refuses `userDrives.enabled=true` |
| Helm `userDrives.reclaim.enabled` | Helm `drives.reclaim.enabled` | Chart value, clean break (no alias); never released under the old name |

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

# The outcome contract: follow GET /runs/{id}/events (or poll GET /runs/{id}) until the run ends,
# then read the task's real exit code off the run.complete audit event. `wardyn run --wait` polls.
curl -s -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>   # .state: COMPLETED | FAILED | ...

curl -s -H 'Authorization: Bearer demo-admin-token' \
  'http://localhost:8080/api/v1/audit?run_id=<id>'   # run.complete -> .data.exit_code

# Or follow the run's lifecycle as it happens (see "Following a run's lifecycle").
curl -sN -H 'Authorization: Bearer demo-admin-token' \
  http://localhost:8080/api/v1/runs/<id>/events
```

`GET /api/v1/runs` accepts an opt-in scoping/filtering surface beyond
`&limit=&offset=` (#1197 L1a):

- `view` (`user`/`admin`; absent leaves the endpoint
  as above), `owner` (`me`/`all`; `view=user` forces `me` for
  every caller, admin tokens included),
- repeatable `status`
  (`active`/`ended`/`failed`/`killed`/`needs`), `ended_within` (`24h`/`7d`/`30d`/`all`),
- `include_killed=1` (a KILLED run older than 24h is hidden by default), `no_end=1`
  (only live runs with no end), `workspace`
  (exact match on the run's repo/workspace-path label) and `q` (case-insensitive
  substring search over title/task/repo/created_by).
- `sponsored=exclude` drops a sponsor's service runs from their own list; no-op where already left out.

- With any of them the
  response is ordered live runs first, then ended runs by end time, and two headers
  — `X-Wardyn-Hidden-Older`, `X-Wardyn-Hidden-Killed` — count the rows the
  `ended_within` window and the killed-run default hid.
- `POST /api/v1/workspaces/{id}/record/{task}/promote-egress`
  skips, rather than refuses, a requested host that is a model provider row's own host. It names them in the additive
  `X-Wardyn-Record-Skipped-Hosts` header (comma list, absent when none) and as `skipped` in the
  `workspace.egress.approve` audit row.
- `GET /api/v1/approvals`
  accepts the same opt-in `?view=user`, scoping the queue to the caller's own runs'
  approvals for every caller.
- `GET /api/v1/approvals?decidable_by=me`
  lists the `PENDING` approvals the caller may decide now, second human included (a row's `decidable` leaves that
  out), newest first, at most 500; `X-Wardyn-Truncated: true` marks more. Requests answered by taking over or
  declining are not listed. `limit` and `offset` are not read, and `state`, if sent, must be `PENDING`. A security admin's list is every pending approval the switches let them
  decide; anyone else's is the holds on runs they created plus those their unit's deciders rule names them for.
  `wardyn approval list --waiting-on-me` prints it.
  A row served to a decider who is not the run's creator carries `requested_by`. A hold the caller may not decide
  is left out whether it exists or not, so `run_id=` on it answers the same empty list for a run the caller
  may not decide on as for one that does not exist. A portal's delegated token gets `403 delegation_scope`.
- An approval row a unit deciders rule governs names how it is decided. A `PENDING` row carries the rule's
  `max_scope` and `second_human` beside `deciders`. A decided row carries `decision_basis` (`owner`,
  `unit_admin:<unit id>`, `group:<name>`, `security`, `policy` or `admin_token`) and `decision_rule_unit`. Each is
  absent when no rule governs the row.

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
- `keep_work_folder: true` on `POST /api/v1/runs` keeps the run's work folder (`/home/agent/work` only) as the caller's own drive, named after the run, under the same doors and limit as `POST /me/drives` plus `storage.user_drive.member_create.keep_main_drive` (off by default). It stays until deleted; a later run attaches it through `drives[]`. The catalogue's `create.keep_work_folder` is the switch (on even when `create.reason` says the door is closed), `create.enforcement` is `none` on Docker, where the drive's size is not enforced, and a kept drive carries `kept_from_run`, the run's id. `kept_from_run_title` is the run's current title; empty when the run is gone, untitled, or not yours. At the limit the sentence is "Delete one to keep this run's work folder."
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

### Unit trees

`ApplyGovernance` reproduces a graph of composed profiles (`parent_id`, `overlay`,
`overlay_limits`, `contact`) from `wardyn governance get` on one install to `wardyn governance set`
on another, and a repeat apply is a no-op.

- **Order.** Profiles are written bases first, whatever order the document lists them in; a
  `parent_id` cycle in the document is an error before any write. Prune deletes the profiles
  composed on a base before the base, so it never meets the server's `409`.
- **Ids.** Every graph reference, `parent_id` and an assignment's `unit_id`, is matched by the
  id the document's own entry for that profile carries and rewritten to the id the profile holds on the
  target. A base the document does not contain is sent as given.
- **Fields.** A profile is rewritten only when its ceiling, limits, contact, base, overlay or
  overlay limits differ from the stored one. The read-only `effective` view is never written back.
- **Pending approval.** A child whose base has a pending write is deferred, not sent; apply again
  once the base is approved.
- **Older clients.** A client built before 0.8.6 drops `parent_id`, `overlay` and
  `overlay_limits`.
  - A composed profile it would create is refused with a `400` (`invalid ceiling:
    min_confinement_class is required`), because the exported row carries an empty ceiling and no
    overlay.
  - One it updates keeps the composition the server stored, since an absent member keeps the
    stored value.
  - It also writes in document order, so a child listed before its base fails.
  - Upgrade
    before applying a composed document.
