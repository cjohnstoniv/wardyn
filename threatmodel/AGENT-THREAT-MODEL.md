# A threat model for agent systems

**Portable on purpose.** This is about the class of system — a model-driven agent
executing tools on someone's behalf — not about Wardyn: the categories, terms and
ownership lines are meant to be usable by a team running something else. Every row
still carries a **Wardyn coverage verdict**, because a taxonomy with no verdict is
a whitepaper. The taxonomy travels; the verdict column is the honest local answer.

**Read this for** shared vocabulary and the category set.
**Read [THREAT-MODEL.md](THREAT-MODEL.md) for** the threat model of the Wardyn
implementation: its assets, trust boundaries, and the dated residual risks in §5. That
document is deliberately Wardyn-shaped and does not travel.

**The anti-overclaim rule applies here too.** A verdict this document asserts that
the code does not support is a bug, not prose licence — report it under
[SECURITY.md](../SECURITY.md).

---

## 1. Terminology

Pinned because each already means two or three things in ordinary usage, and a
shared model cannot survive that.

| Term | Means here | Commonly confused with |
|---|---|---|
| **Agent** | the logical actor: a model plus the tools it may call, acting for a human sponsor | the *harness* (the CLI/process) and the *run* (one execution). Say "agent harness" or "agent run" when you mean those |
| **Agent author** | whoever chose the task, the tools and the sources for a run | the operator. They are different people with different powers |
| **Operator** | whoever sets the ceiling: policy, allowlists, which credentials may ever be requested | an admin user of the product; and, on single-user deployments, the developer — who is then both |
| **Sandbox** | the isolation boundary around one run | the whole platform. "Sandbox the agent" is ambiguous; say boundary or platform |
| **Confinement class** | how strong the isolation is (kernel-shared → user-space kernel → microVM) | permitted autonomy. **They are orthogonal**: the strongest isolation can host the least constrained agent |
| **Reach** | what a run can actually contact — network destinations, credentials, mounted data | permission. Reach is the *effect*; permissions are one input to it |
| **Autonomy** | how much the agent may do without a human deciding | capability. An agent may be highly capable and minimally autonomous |
| **Gateway** | ALWAYS qualified: egress gateway, LLM gateway, tool/MCP gateway, SSH gateway | each other. Bare "gateway" is banned in this document — and note "gatewayless" means *no default route*, the opposite sense |
| **Grant** | eligibility for a credential — permission to ask | the credential. The mint happens later, if approved |
| **Approval** | a recorded human decision on one request | a policy. A policy is standing; an approval is an event |
| **Residual** | a **published, accepted** limit | an open bug. Residuals are disclosed on purpose; treating them as bugs misreads the document they live in |
| **Defense layer** | one of the numbered egress layers (L0–L3) | OSI layers. L2 here is a policy proxy, not data-link |

**Four axes, not one ladder** — routinely collapsed, and they measure different
things:

- **Containment** — what stops code leaving its box (confinement classes).
- **Egress** — what stops data and calls leaving the box (L0–L3).
- **Authority** — grant → approval → mint → scope → revoke.
- **Evidence** — what is recorded, where, and whether it can be altered.

A control strong on one axis says nothing about the others. Most overclaiming in
this space is an axis substitution.

---

## 2. Threat categories

Verdicts are exactly three values. `partial` must name the bypass class;
`not addressed` must say by-design or by-omission. There is no fourth value,
because a fourth value is where hedging goes.

Verdict column: 🟢 mitigated · 🟡 partial · ⛔ not addressed — the icon prefixes
the word, never replaces it.

| # | Category | What it is | Wardyn verdict |
|---|---|---|---|
| 1 | **Prompt injection / instruction hijack** | untrusted content redirects the agent's goals | ⛔ **not addressed, by design.** Nothing inspects inbound prompts. This is treated as an assumed precondition — the platform contains the *consequences* rather than preventing the hijack. Outbound content scanning exists (`internal/contentscan`) and is a different control |
| 2 | **Tool / capability misuse** | the agent invokes a legitimate tool for an illegitimate end | 🟡 **partial.** Tool calls can route to a human approval that fails closed (`cmd/wardyn-toolgate`). Bypass class: the gate is **in-sandbox and cooperative** — an agent that does not call it is not gated; it covers one harness; and that harness auto-approves its own read-only-safe command class without consulting it. The out-of-band layer that would be a boundary is not built |
| 3 | **Credential exfiltration** | the agent obtains a secret and moves it out | 🟢 **mitigated, with a published exception list.** Credentials are minted per-run, TTL-capped and injected proxy-side, so the sandbox holds a placeholder. The exceptions — where a credential IS resident — are enumerated in THREAT-MODEL.md §5.1a rather than restated here, because a second copy of that list is how it drifts |
| 4 | **Data exfiltration via the model channel** | data leaves inside a legitimate model request | ⛔ **not addressed as prevention; instrumented.** An agent can encode, split across turns, or encrypt around any scanner. What exists is inspection with **honestly reported coverage** — an uninspectable channel is marked uninspectable rather than counted as clean. Do not describe this as DLP |
| 5 | **Data exfiltration via other egress** | data leaves over an ordinary network call | 🟡 **mitigated for unlisted destinations; partial for allowlisted ones.** Default-deny plus first-use approval. Bypass class: a permitted destination that is also attacker-reachable (dual-use hosts, DNS through the permitted resolver) |
| 6 | **Model-traffic interception** | someone reads the model channel — including the platform itself | 🟡 **partial, by design, both directions.** TLS interception of the model channel is opt-in per run and its CA key stays in proxy memory. Teams need a neutral name for "we MITM our own agents" or each invents one |
| 7 | **Sandbox escape** | code leaves the isolation boundary | 🟢 **mitigated to the declared class, honestly bounded.** Each confinement class documents what it does **not** stop, and a demanded class that cannot be enforced fails closed rather than silently downgrading |
| 8 | **Lateral movement** | one run reaches another, or the host | 🟢 **mitigated.** Per-run networks with no default route; on Kubernetes a default-deny NetworkPolicy verified by a boot-time canary that refuses to start if it does not hold |
| 9a | **Supply chain — the platform's own artifacts** | you pull a tampered platform image | 🟢 **mitigated.** Keyless signing, SBOMs scanned from the image rather than the source tree, build provenance |
| 9b | **Supply chain — content executing inside a run** | a malicious package's install script runs | 🟡 **partial** — bypass class: any code that executes inside the sandbox. Nothing scans it, so it collapses into category 1 and is answered by containment, not prevention |
| 9c | **Supply chain — the image the sandbox is built from** | a hostile base image | 🟡 **partial — the weakest of the three.** A base carrying build-time triggers is refused, but wrapping is not vetting: base content is unscanned, and build steps run on the host **before any confinement class exists** |
| 10 | **Privilege escalation within the platform** | a member gains operator powers | 🟡 **partial.** A capability-grant model exists with deny-beats-allow precedence — but **every enforcement switch ships off**, so an upgraded deployment enforces nothing until an admin turns kinds on |
| 11 | **Attribution evasion** | an action cannot be traced to a human sponsor | 🟡 **partial.** The run token carries both the human and the agent; audit events record a single actor field, so sponsor and agent are not separable per event |
| 12 | **Audit tampering** | the record is altered after the fact | 🟡 **partial, deliberately graded.** Append-only enforcement plus a hash chain — which is tamper-**evidence**, not tamper-proofness, and only if a head hash is retained off-box. Session recordings are writable by the run that produces them |
| 13 | **Approval fatigue** | the human approves everything because there are too many prompts | ⛔ **not addressed, by omission.** The platform leans heavily on human approvals and has no rate limit, no batching guard, no anomaly signal on approval volume, and no separation of duty. This is the failure mode every approval-based control shares, and no code fixes it |
| 14 | **Resource abuse / denial of wallet** | the agent burns money rather than data | 🟡 **partial, and the enforced set differs BY SUBSTRATE.** Docker: CPU, memory and PIDs are requested and the run fails closed BEFORE start if the daemon reports it discarded one (`verifyCapsEnforced`; `WARDYN_ALLOW_UNENFORCEABLE_CAPS=1` downgrades that to a warning) — a DISK cap degrades to uncapped-with-a-warning on a storage driver that cannot take a size quota, by design. Kubernetes: CPU and memory only; a pid or disk cap is **never requested at all** — there is no per-container equivalent in the Pod API, so the substrate logs "requested but not enforced" and creates the pod anyway, and there is nothing for a fail-closed check to catch. Lifetime: what exists is an optional IDLE auto-stop (`auto_stop_after_sec`, measured against `updated_at`, which an active agent keeps resetting), and the shipped default policy sets it to `0` — which `docs/POLICIES.md` documents as *never reaped*, and which the reaper honours by skipping every run whose policy is `<= 0` (the kind quickstart's default policy and the Helm chart's all-on values ship `0` too; the CI example policies set `3600`). **There is no wall-clock lifetime bound anywhere, and no token-spend or model-call budget anywhere** — an agent holding a valid model credential can exhaust it |

**Deliberately excluded**, so the absence reads as a decision: model-weight theft
(no models are hosted), training-data poisoning (not a runtime-containment
concern), and agent-to-agent collusion (no multi-agent construct exists — though
nested-sandbox degradation is where it would first appear).

**No mapping to STRIDE or ATLAS.** A mapping table is attractive to reviewers and
would claim coverage of a framework nothing here is tested against.

---

## 3. Ownership

Most disagreement about agent security is really disagreement about who owns a
control. Four owners; the assignments that surprise people are the point.

| Control | Owner |
|---|---|
| The isolation boundary; minting and revoking credentials; recording evidence; refusing what cannot be enforced | **Platform** |
| The ceiling: policy, allowlists, confinement floor, which grant kinds exist, whether capability enforcement is on at all | **Operator** |
| Cloud IAM scoping behind a federated credential | **Operator** — the platform neither enforces nor verifies it |
| Source-forge rulesets (branch protection and equivalents) | **Operator** — creating one needs repo-admin the platform deliberately does not hold |
| A credential supplied whole (an unbrokered PAT or SSH key) | **Operator** — bounded by what they issued, not by the platform |
| Off-box retention of audit head hashes | **Security team / SIEM** — a hash chain detects tampering only against a copy kept somewhere the platform cannot reach |
| The task, the tools, the sources | **Agent author** — who owns nothing on the enforcement side, by construction |
| Resistance to prompt injection | **Agent author and model vendor** — explicitly **not** the platform |
| The taxonomy, residual review, red-team exercises | **Security team** |

> **The platform owns the boundary; the operator owns the ceiling; the agent
> author owns the task; nobody owns the model's judgment.**

---

## 4. Using this alongside the implementation model

**Reviewing:** pick a category, read the verdict, follow it into
[THREAT-MODEL.md](THREAT-MODEL.md), then into the code.

**Reporting:** a category with no verdict, or a verdict the code does not support,
is a bug under [SECURITY.md](../SECURITY.md)'s overclaim clause — not a
documentation nit.

**On the shape of this document:** it carries more `not addressed` and `partial`
verdicts than `mitigated` ones. That asymmetry is deliberate, and is the reason to
trust the `mitigated` rows.
