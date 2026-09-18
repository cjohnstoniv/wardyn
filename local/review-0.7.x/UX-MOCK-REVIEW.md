# Small corrections to existing role-aware console states

Baseline dfa89f60. Mock round for R076-005 and R076-007, before their replacement
and new implementations respectively. Existing tokens, layout and canonical
strings are reused. These are copy and action-visibility corrections.

| Surface and viewer | Current presentation | Intended presentation |
| --- | --- | --- |
| Workspace recording pane, member | Pane and launch form both say Requires the admin role. | Pane says Requires the admin or security admin role.; launch form still says Requires the admin role. |
| Workspace recording pane, security admin | Egress decision controls enabled; launch form disabled with admin-only reason. | Same. |
| Member Getting Started, synthetic shared Bedrock expiring fixture | Provided by your admin chip and Sign in to AWS button. This is not a normal server response: memberModelAccess maps shared expiring to live. | No product change: reject candidate absent a reachable server path. |
| Member Getting Started, per-person AWS expiring/expired/not configured | Existing sign-in buttons and pane. | Same. |

R076-007's proposed per-user gate was coherent but lacks a reachable current
defect. Do not treat a synthetic fixture as evidence of a broken user journey.
No new dialog, labels, semantic colors, role privilege or CSS is introduced.

Existing R076-005 implementation predates this round and is not called reviewed
retroactively. Its replacement is applied only after this state review. Final
browser checks remain required. Docs lane independently approved the R076-005
pane-only security hint and confirmed the launch form must stay admin-only.
