> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Delegation: standing, session and one-time access

A run gets more than its policy gives only when a person or a rule consents. This page sets the three tiers of that consent side by side: what gives each tier, who may give it, and when a running run loses it. Each rule links to the page that owns it.

## The three tiers

| Tier | What it reaches | What gives it | Where it is kept |
|---|---|---|---|
| Standing | Every later run the decision covers, until someone removes it | An `always` egress decision; a policy's `allowed_domains` and `eligible_grants`; a workspace's approved hosts; a capability grant; a unit membership; an approval rule; the image grant an image request writes | The policy, the workspace's `approved_egress` and `denied_egress`, the capability grants, the unit memberships and the approval rules |
| Session | The rest of one run, or a set time | A `run` or `until` egress decision; a host rule `hold` approved for the run; a `run`-scoped lease on a `git_pat` credential; an Azure DevOps escalation on a `minted_pat` run, even one approved once; an elevation | The run's proxy, for an egress decision or a host rule hold; a membership that ends at its time, for an elevation |
| One-time | One connection, one call, one mint or one file | A `once` egress decision or host rule `hold`; each call an `approve_each` rule holds; each call a `hold` sends to a person; one held Kubernetes call; a `credential` approval; a held push; a held file download | The approval. A `once` decision is spent on its first use; an HTTPS connection that got nothing back gives an egress one back once; a spent host rule `hold` one never comes back ([scopes](../POLICIES.md#approval-decision-scopes)) |

- Every decision by a person or a rule is an `approval.decide` audit row.
- `once`, `run`, `until` and `always` are the [approval decision scopes](../POLICIES.md#approval-decision-scopes) of an egress decision. A `credential` approval takes `run` alone, for the lease ([member runs](member-runs.md)).
- `tool_rules` effects run `allow`, `hold`, `approve_each`, `deny`, and every layer keeps the stricter ([tool rules](../POLICIES.md#tool_rules--toolrule)). A host rule `hold` is in [host rules](../POLICIES.md#a-host-rule-hold-effect-hold).
- During a `workspace record` session, a `run` egress decision also writes the host into the workspace's requirements, so it reaches every later run of the workspace ([approval decision scopes](../POLICIES.md#approval-decision-scopes)).
- An Azure DevOps escalation approved once still widens the run's token for the rest of the run ([Azure DevOps](../AZURE-DEVOPS.md#how-a-runs-token-lives-minted_pat)).
- An elevation is a membership of a unit for a while. A run launched under it ends at the elevation's end ([elevations](deciders.md#elevations)).

## Who may consent

The security tier is a security admin or a super admin ([what admin-only still means](admin-only-writes.md)).

| What is decided | Who may consent | When a second person is needed |
|---|---|---|
| An `egress_domain` approval at `once`, `run` or `until` | The run's owner, who also needs an `egress_host` grant on the host once that kind is enforced; the security tier; a person a unit's deciders rule names | `WARDYN_EGRESS_SECOND_HUMAN`: never the run's creator. A deciders rule's `second_human`: none of the run's own people |
| An `egress_domain` approval at `always` | The security tier only, whoever owns the run. No deciders rule allows `always` | As for the other scopes |
| An Azure DevOps escalation, one MCP call under `approve_each`, one held Kubernetes call | The run's owner; the security tier; a person a unit's deciders rule names | `WARDYN_CAPABILITY_SECOND_HUMAN`, for an Azure DevOps escalation and an MCP call. A deciders rule's `second_human` |
| A host rule `hold` | The run's owner or the security tier. A unit's `egress_domain` deciders rule governs it | Either second-human switch, `WARDYN_EGRESS_SECOND_HUMAN` or `WARDYN_CAPABILITY_SECOND_HUMAN` |
| A tool call a `hold` sends to a person | The security tier. On a service identity's run, an approval rule too | No second-human switch names it |
| A `credential` approval, a held push | The security tier; a person a deciders rule names, never the run's owner | A deciders rule for either kind must set `second_human` |
| A held file download | The security tier, or a unit's `file_download` deciders. Nobody who created the run or asked decides it; the admin token is the audited break-glass | A `file_download` rule is stored with `second_human` |
| A first-use host or a held tool call on a service identity's run | An approval rule, in its own name, through every gate a person meets. Never a credential, a push, an Azure DevOps escalation or a person's run | A rule is never a second human |
| An elevation, an image request | The people the unit's rule names, and always a security admin. Never the person who asked | These rules take no `second_human` |
| A stored policy | A super admin only | `WARDYN_GOVERNANCE_SECOND_HUMAN` does not hold a policy write |
| A capability grant, the Wardyn boundary, an approval rule | The security tier | `WARDYN_GOVERNANCE_SECOND_HUMAN` holds each write for a second person. Removing an approval rule is never held |
| A restriction, a workspace's or a component's unit pin | The security tier | Under `WARDYN_GOVERNANCE_SECOND_HUMAN`, deleting or loosening a restriction and moving a pin off its branch are held. A restriction's first `allowed_domains` host waits for a second person |
| A unit membership | The security tier; a unit admin, inside their own units | A unit admin's widening is always held. `WARDYN_GOVERNANCE_SECOND_HUMAN` holds every membership write |
| A unit's deciders rules | A security admin; a unit admin, for a unit strictly below theirs | A unit admin's write is always held. A security admin's is held under `WARDYN_GOVERNANCE_SECOND_HUMAN` |

- Without a deciders rule, [today's rule](deciders.md#todays-rule) decides. A portal's delegated token never decides a hold.
- Rules, grants and groups are read again when a decision arrives, so a removed grant or rule binds the next decision ([how a hold is resolved](deciders.md#how-a-hold-is-resolved)).
- The second-human switches are in [governance writes](governance-writes.md). An approval rule is in [approval rules](approval-rules.md).

## When a running run loses access

| What is revoked or narrowed | When a running run loses the access | To cut it sooner |
|---|---|---|
| A `component` capability grant withdrawn | At the next credential mint or redeem, and an organisation MCP server's next gateway token: refused `capability_component` once its owner lost it, by their last sign-in's groups (a portal's run: their last exchange's) less SCIM removals. Hosts stay open ([capabilities](capabilities.md)) | A live egress edit for its hosts, or end the run |
| An organisation component deleted | At the next credential mint or redeem, and an organisation MCP server's next gateway token: each is refused `component_gone`. A revive, restart or end extension is refused too ([custom components](custom-components.md)) | A live egress edit for its hosts, or end the run |
| A component's unit pin moved off the run's unit | At the run's next credential mint or redeem, and an organisation MCP server's next gateway token: each is refused `component_unit_pinned`. A revive, restart or end extension is refused too ([custom components](custom-components.md)) | End the run |
| A restriction the run started under, narrowed | At the next mint, for a credential it withholds or one for a host it denies or does not allow. Its egress binds at the next revive. No approval admits a host outside it ([governance writes](governance-writes.md)) | A live egress edit for a host, or **Restart with current limits**, which gives the run a new proxy |
| A restriction the person gains after launch | When the next start or revive resolves it. An admin's revive does not find a new group restriction; the owner's own sign-in does | End the run and start a new one |
| `withheld_grants` added to a restriction the run started under, or to the owner's walls | At the next mint or redeem of that credential: `403` `credential_withheld`. A value copied into the sandbox at dispatch, such as an `env_secret`, stays there ([credentials](../CREDENTIALS.md#every-kind-side-by-side)) | End the run, which takes the sandbox with it |
| The owner's walls narrowed: a deny, a blocked direction or a withheld credential | At the run's next revive and next credential mint. An approval for a host a new deny covers is refused at once (`personal_walls_host`). A narrower site list does not reach the run ([how your walls apply](../POLICIES.md#how-your-walls-apply)) | A live egress edit for a host, or end the run |
| The owner's walls loosened | Nothing changes: loosening frees nothing for a run already started | Not a revocation. A new run gets the looser walls |
| The Wardyn boundary narrowed | At the next mint or redeem, for a grant it withholds or one for a host it denies or does not allow. Its egress binds at the next revive. No one approves a host outside it (`boundary_host`) ([credentials withheld](boundary.md#credentials-withheld)) | A live egress edit for a host, or end the run. A revive is refused when the boundary no longer allows the run's git broker or Azure CLI |
| A SiteConfig egress field: `internal_hosts`, `upstream_proxy_no_proxy`, `trusted_ca_pem`, the egress redirects | Not while the run lives: they are read once at proxy start, and a write reaches only runs dispatched after it lands ([network settings](../OPERATIONS.md#network-upstream-proxy-and-egress-redirects)) | A live egress edit to deny a host, or end the run |
| An `until` decision at its expiry | At the first use after the expiry: the run's proxy checks it on every use. The host then reads as unknown, so the run asks again ([approval decision scopes](../POLICIES.md#approval-decision-scopes)) | `deny_hosts` in a live egress edit, which denies the host for the rest of the run |
| An `always` decision removed from the workspace's approved hosts | For runs started after the removal. The deciding run's proxy keeps the decision, and a run that started with the host keeps the policy its proxy started with | `remove_hosts` or `deny_hosts` in a live egress edit on each running run |
| A stored policy edited after launch | Never: the run keeps the policy dispatch recorded, and a revive starts from the run's stored proxy config. The run's policy shows whether the saved policy moved ([the policy a run got](../OPERATIONS.md#the-policy-a-run-got)) | A live egress edit for a host, or end the run and start a new one |
| A live egress edit (`PATCH /runs/{id}/egress`) | At once: it answers `200` only once the new proxy's readback is enforced, and each edited host is denied for the rest of the run, with its credential lanes ([live egress edit](run-lifetime.md#live-egress-edit)) | It is the fastest cut for a host. It never adds one |
| An approval rule removed, or a host taken out of its `approve.hosts` | At the next raise, on a running run too. A host it already approved at `run` stays open until the run's proxy raises it again, as a replaced proxy does ([approval rules](approval-rules.md)) | A live egress edit for the host |
| A decider's grant, group or rule removed | At the next decision on a hold: they are read when it arrives ([how a hold is resolved](deciders.md#how-a-hold-is-resolved)) | For a host an earlier decision opened, a live egress edit |
| An elevation ended early: its membership deleted | Runs started under it keep running and still end at the elevation's end. A revive, restart or end extension is refused once the owner holds no membership of the unit ([elevations](deciders.md#elevations)) | End the runs |
| A person's sessions and tokens revoked in full (`POST /sessions/revoke`, not `sessions_only`) | Their sessions and API tokens end. Their running runs go on, but lose AI gateway access within half the gateway token's life while wardynd is reachable (`identity_deactivated`) ([gateway access](ai-gateway.md#when-a-runs-gateway-access-ends)) | Kill their runs, as [offboarding](ownership-and-offboarding.md) does. With Entra SCIM on, a suspend kills them too |
| The run reaching its end time | It is kept: its proxy is removed, so it has no network, pending approvals are cancelled and every internal door refuses its token (`run_kept`) ([end](run-lifetime.md#end)) | **End run** tears it down at once |
| The run ended: **End run**, `POST /runs/{id}/kill` | At once: broker credentials, the run's identity and the sandbox go together, and pending approvals are cancelled ([end](run-lifetime.md#end)) | It is the cut that reaches everything Wardyn holds. What a person copied out of the run is not recalled |

- A live egress edit is the owner's or a super admin's. It changes nothing on a run that is not live, has No network, or whose proxy cannot confirm what it loaded ([live egress edit](run-lifetime.md#live-egress-edit)).
- **Restart with current limits** (`POST /admin/runs/restart`) is a super admin's, over live runs ([revive](run-lifetime.md#revive)).
