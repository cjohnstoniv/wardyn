# Which barrier for which agent

- A barrier decides how far a sandbox escape reaches: Fence (`CC1`), Wall (`CC2`) or Vault (`CC3`).
- Pick it by what the agent runs inside the sandbox. This page is guidance, not enforcement.
- An agent that must run outside a sandbox is out of scope for 0.10.x. The [last section](#a-tool-only-agent-with-the-thinnest-sandbox) gives the smallest sandbox instead.

## What a barrier changes

| Same at every barrier | Differs by barrier |
|---|---|
| Egress filtering, short-lived brokered credentials, human approvals and full audit are set by policy | How strongly the sandbox is walled off from your machine |

| Barrier | Class | Does not protect against | How it works | Where it runs |
|---|---|---|---|---|
| Fence | `CC1` | A kernel exploit or container escape: it shares your machine's kernel | Hardened runc container | Any host |
| Wall | `CC2` | A flaw in the sandbox software itself (rare); it is not a separate machine | gVisor intercepts system calls | A Docker host with the `runsc` runtime installed |
| Vault | `CC3` | A flaw in the virtualization layer itself (very rare) | Kata microVM with its own guest kernel | A Docker host with a Kata or krun runtime and KVM (`/dev/kvm`) |

## Pick by agent shape

| Agent shape | Runs code it chose, in the sandbox? | Recommended `min_confinement_class` | Why |
|---|---|---|---|
| No shell: model and tool or MCP calls only; nothing the model writes is executed in the sandbox | No | `CC1` | No code to escape with. Egress, the MCP broker and tool rules matter, and they are the same at every barrier |
| Tool-calling with fixed local tools you vetted: no package installs, no arbitrary scripts | Only your tools | `CC1`; `CC2` if a tool parses untrusted files (documents, archives, images) | A parser bug is the realistic way in, and gVisor shrinks the kernel surface a bug reaches |
| Code and shell: installs packages, builds and runs tests | Yes, anything | `CC2` where the host offers it; `CC1` only for code you trust on your own machine | Arbitrary code is the kernel-exploit case Fence leaves open |
| Any shape holding a write-capable credential, or working on untrusted code on a shared host | Either | `CC3` | Wardyn already requires it for write-capable grants and non-baseline API keys; set it yourself for untrusted code |

The shipped example policies use `CC1` because it needs nothing beyond Docker, not because it fits every agent ([examples/policies/README.md](../examples/policies/README.md)).

## Where the barrier is set

| Case | What happens | Where |
|---|---|---|
| A grant with write permissions (a GitHub token with a write scope, or `cloud_sts`) | The run is raised to `CC3` | [`RequiredConfinementFloor`](../internal/composer/risk.go) |
| An `api_key` grant to a host outside the coding-agent baseline | The run is raised to `CC3` | Same function |
| `git_pat` and `ssh_key` grants | No floor of their own, so clones still work on a host without KVM | Same file |
| A component's header credential | Raises the run to `CC3` only when `require_vault_for_credentials` is on; it is `false` by default | [Custom components](operations/custom-components.md) |
| A governance profile's `min_confinement_class` | A run under the profile below it is raised to it | [`clamp.go`](../internal/composer/clamp.go) |
| A policy's `min_confinement_class` | The run refuses to launch below it | [POLICIES.md](POLICIES.md) |
| The pool cannot offer the barrier | The pool list's `barrier_options` gives each barrier as `available` or one `reason_code`, and the launch is refused | [Runner pools](operations/runner-pools.md#barriers) |
| A virtual machine run | It has its own kernel and no barrier | [New run](NEW-RUN.md) |

## A tool-only agent with the thinnest sandbox

Wardyn has no gateway-only mode in 0.10.x: no endpoint that serves an agent's MCP calls with no sandbox. Policy, approvals and audit belong to a run, and a run is a sandbox.

- The broker is a route on a run's own egress proxy, at `http://<proxy>/wardyn/mcp/<token>/<server key>`. The token is minted at dispatch for one harness.
- An external AI gateway can receive a signed `X-Wardyn-Identity` token naming the run ([AI gateway](operations/ai-gateway.md)). The run it names still starts in a Wardyn sandbox.
- This changes when two or more deployments have agent runtimes that cannot run in a container and still need Wardyn's approvals and audit.

The smallest sandbox that keeps run-bound audit for a no-shell agent:

1. Run on Fence (`CC1`) with `allowed_domains` empty and `first_use_approval` set to `always_deny`.
2. Attach the MCP server as a component. Its host list names only the server.
3. Turn the server on for the harness that calls it, with `components[].mcp_harnesses`. Left out, it is on for every harness that can take it.
4. The agent reaches the server only at the broker address. The broker adds the server's credential, strips the credential headers the sandbox sent (`Authorization`, `X-Api-Key` and the other common ones), and sends the call itself.
5. A `tool_rules` entry with effect `deny` is refused by the broker, and the call is never sent. `approve_each` makes a person approve each call with its exact arguments ([POLICIES.md](POLICIES.md#tool_rules--toolrule)). A `hold` is not enforced by the broker: the harness asks before it calls.
6. Every other host is refused, and so is the server's own host on the plain or `CONNECT` lane. Every request leaves a decision record for the run.

> [!NOTE]
> Steps 1 to 6 are proven by a proxy-level test, `TestMCPBroker_ThinSandboxEmptyAllowlist` ([`mcp_thin_sandbox_test.go`](../internal/egress/proxy/mcp_thin_sandbox_test.go)), and an API test that an MCP-only run keeps an empty allowlist. No end-to-end run in a real container has been done.

> [!WARNING]
> An empty allowlist leaves the harness no model host unless its model traffic uses a brokered lane. That case was not tested.
