> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Orchestrators: one governed run per step

A workflow engine or an agent framework can use Wardyn as the sandbox for each step that runs code. The orchestrator's worker stays outside the sandbox and calls the API. Each such step is one background run, and the orchestrator keeps the job's state.

## One step

1. Create the run with `POST /api/v1/runs`: `"experience": "background"`, the step's `task`, and an `Idempotency-Key` header that names this step. Store the key before you send, and the run id from the `201` answer ([idempotent run create](../sdk.md#idempotent-run-create)).
2. Put the job's id and the step's name in `correlation_id`, such as `job-7/build`: at most 128 letters, digits and `. _ : / -`. Keep it when you retry the step. The run keeps it, and it scopes [a retried call](#a-retried-call-that-needs-an-approval). An event of the run that reaches an audit sink carries it while the run's masking is complete ([`correlation_id`](../sdk.md#scheduled-runs)).
3. Wait. Follow `GET /api/v1/runs/{id}/events` to its `ended` event, or poll `GET /api/v1/runs/{id}` until `state` is terminal. With several wardynd replicas a reconnect can miss events, so read the run once the stream closes ([following a run](../sdk.md#following-a-runs-lifecycle)).
4. Read the result: `GET /api/v1/runs/{id}/output` for the end of the run's stdout and stderr, `64 KiB` by default, and the exit code on the run's `run.complete` audit event ([output](../sdk.md#reading-a-runs-output), [raw HTTP](../sdk.md#raw-http-curl)). Trust the output only when `complete` is true and neither `incomplete` nor `capture_gap` is.
5. Retry a create whose answer was lost with the same key and the same body. For 24 hours the answer is the first one, with `Idempotent-Replayed: true`. `409` `idempotency_key_in_flight` means wait `Retry-After`; `422` `idempotency_key_reused` means the body changed.
6. Retry a step that failed with a new `Idempotency-Key` and the same `correlation_id`. It is a new run.

- Create the runs as a [service identity](api-tokens.md#service-identities). Its gateway tokens name it as `sub`, and an [approval rule](approval-rules.md) may approve a held call or a first-use host on its runs.

> [!WARNING]
> A retry is safe only once every control-plane replica runs 0.10.0 or later. An older replica ignores the key and makes a second run, and `/healthz` speaks only for the replica that answers.

## What not to do

- Do not run the worker inside the sandbox. The sandbox is the untrusted side, and its only path out is its own egress proxy ([architecture](../../ARCHITECTURE.md#components)).
- Do not make a command the step. A background run takes one through `POST /runs/{id}/exec`, but its output is neither masked nor recorded, a profile's `deny_task_mode_exec` refuses it, and it does not keep the run going ([commands](../sdk.md#running-a-command-in-a-run)).
- Do not keep the job's state in the sandbox. **End run** tears it down, and of its output only the tail is kept ([end](run-lifetime.md#end), [run output](../OPERATIONS.md#run-output)).
- Do not expect `wardyn_workload` on a step's run: only a run a workload launched carries it. A step's run has the SPIFFE ID `spiffe://<trust domain>/agent-run/<run id>`, which its gateway token carries in `act.sub`. A gateway tells steps apart by `wardyn_run` ([gateway claims](ai-gateway.md#the-claims-a-gateway-receives)).

## A retried call that needs an approval

A step whose agent calls an MCP tool under an `approve_each` rule can be retried within 24 hours of its send with no second request to a person and no second send ([tool rules](../POLICIES.md#tool_rules--toolrule)). Give the run a `correlation_id`, as in step 2, and keep it when you retry the step.

- Wardyn records an approved call before it sends it, and sends it at most once in 24 hours. The record is the run owner's, within the run's `correlation_id`, so a retry in a new run of the same step finds it.
- The record lasts 24 hours from the send. After that, the same call is a new call and needs approval again.
- A call is the same when it has the same server, tool and arguments, in any key order or spacing. An agent that rewords a retried call makes a new call, and a person is asked about it.
- Within one run, the same call made again after its send is asked about and sent as before. A retry's calls are matched to the sends in order: the first such call to the first send, the second to the second. So are a run's once a revive, a restart or a live egress edit replaces its proxy.
- A retry of a call that was sent is not asked about and not sent. The agent gets a tool error that names the approval the call was sent under, when, and whether the server's answer was recorded. Its audit row has the rule source `brokered:mcp:approval-sent`.
- A call that was sent with no answer recorded may or may not have run. Read the server's own state before you send it again.
- To send the same call again on purpose, start the run with a new `correlation_id`. A run without one asks about and sends each call that has no key of its own, each time. So does a run whose `correlation_id` changes with each attempt, such as a fresh `traceparent`.
- A client that builds its own MCP request can name the operation itself, in the `_meta` member `wardyn.dev/operation-key`: 1 to 128 ASCII letters, digits or `. _ : / -`. That key is used in place of the one made from the call. In a run without a `correlation_id`, it counts within the run.
- A key outside that form is refused, and so is one that starts with `call:`, which Wardyn keeps for the keys it makes, or the same key on another call. None of them is sent.
- Claude Code and Codex CLI cannot put a `_meta` member on a call, so their calls use the key made from the call. The key of a call an `approve_each` rule covers is never sent to the MCP server, and it is not part of what a person approves.
- On any other call Wardyn does not read the key: the call is sent as it is, key included, each time.
