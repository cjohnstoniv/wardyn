# Held-push canon (#181)

Built on #180/#494's server side (`internal/types/push_content.go`,
`internal/api/approvals_push.go`, `push_rules.require_review_paths` /
`hold_seconds`). Mock approved by the owner, packet 7, 2026-09-22 — see the
three mock questions below.

## Wire contract

Approval kind `push_content`. `requested_scope`:

| field | notes |
|---|---|
| `repo` | the repository as the run's grant names it |
| `branch` | sorted refs joined by `", "` if several |
| `acts_as` | `"<grant kind>:<grant id>"` — a credential reference, never rendered |
| `acts_as_kind` | `github_app` \| `git_pat` \| `ado_entra` — server-set |
| `acts_as_label` | the run owner principal, or `"operator"` for a shared PAT — server-set, the ONLY thing the console renders for "who" |
| `paths` | ≤10, sorted |
| `paths_total` | exact count; the full list is in the run's audit trail |
| `commits` | for an Azure DevOps REST push this is the SHA-256 of the request body, not a commit — never rendered as "commits" |
| `paths_digest` | the dedup key's own hash — never rendered |

Only admins/security-admins may decide (`canDecideApproval` falls through to
the same branch `credential`/`tool_call` take — no ownership carve-out, unlike
the Azure DevOps escalation card). A decision carries no `decision_scope` —
decide's rule 4 refuses one on this kind, so the console never builds one.

## Frozen strings

| key | string |
|---|---|
| `PUSH.KIND_LABEL` | Push |
| `PUSH.CARD_TITLE` | A push is held for review |
| `PUSH.FIELD_REPOSITORY` | Repository |
| `PUSH.FIELD_BRANCH` | Branch |
| `PUSH.FIELD_ACTS_AS` | Acts as |
| `PUSH.PATHS_TITLE` | Files under review |
| `PUSH.PATHS_MORE(n)` | +{n} more |
| `PUSH.PATHS_NOTE` | These are the paths your push rules hold for review. The push carries other files too. |
| `PUSH.WHAT(repo, person)` | the run pushes this branch to {repo}, as {person}. (after `APPROVAL_BANNER_LABEL.what`) |
| `PUSH.BLAST` | the whole push reaches the branch, including files not listed here. Approving does not narrow it to the paths under review. (after `APPROVAL_BANNER_LABEL.blast`) |
| `PUSH.HELD_NOTE` | The push is waiting at the proxy for a short time. Approving lets it through now, or the next time the run pushes. |
| `PUSH.APPROVE_NOTE` | Approving lets this push through. The next push asks again. |
| `PUSH.TIMEOUT_BODY` | Nobody answered in time, so the push was refused and the run was told. It can push again. |
| `PUSH.DENIED_PATH_BODY` | This push touched a path your rules deny, so it was refused and the run was told. Nobody was asked. |
| `PUSH.RAIL_TITLE` | Push rules |
| `PUSH.RAIL_BODY(d, r)` | {d} paths denied · {r} paths held for review (singular "path" for exactly 1) |
| `PUSH.RAIL_UNATTENDED` | Nobody is here to answer, so a push that would be held is refused instead. |

Reused, not reinvented: `APPROVAL.CANCELLED_BODY`, `VIEWER_APPROVAL_BLOCKS_NOTE`,
`APPROVAL_BANNER_LABEL.what`/`.blast`, `SECURITY_ONLY_REASON`. State ("Pending"/
"Approved"/"Denied"/"Expired") comes from the shared `ApprovalStateBadge`
(primitives.tsx) every approval kind already uses — there is no separate
`RUN_STATE.WAITING`-style constant for this kind.

### DRAFT — pending owner approval (packet 7b)

Not yet frozen. Marked `DRAFT` at each definition site (`copy/push.ts`,
`lib/types/audit.ts`) until the owner signs off.

| key | string |
|---|---|
| `PUSH.HELD_EXPIRED` | No longer waiting — approving lets the next push of these same commits through. |
| `PUSH.LIST_TITLE(repo)` | Push to {repository} |
| rule_source `brokered:git:push-rules` | Push refused — a denied path |
| rule_source `brokered:git:push-uninspectable` | Push refused — couldn't be inspected |
| rule_source `brokered:git:push-too-large` | Push refused — too large to inspect |
| rule_source `brokered:git:push-held` | Push refused — not approved |
| rule_source `brokered:git:push-held-unattended` | Push refused — needs a review nobody can give |

## The three mock questions (packet 7)

- **Q181-1** — the rail reads "{d} paths denied · {r} paths held for review".
- **Q181-2** — the card lists ten paths, then "+N more"; the full list is in
  the run's audit trail. No expanding control on the card.
- **Q181-3** — no forge badge. The repository line names the forge implicitly
  (`github.com/...` / `dev.azure.com/...`); GitHub and Azure DevOps share one
  card.

## States

`PushContentCard` (`ui/src/app/components/wardyn/push-content-card.tsx`),
shared by three mounts: the standalone `/approvals` queue
(`screens/approvals.tsx`), the run cockpit's live strip
(`live-approvals.tsx`), and the run page's own Approvals tab
(`run-detail-approvals-tab.tsx`, split out of `run-detail.tsx` to keep that
file under the 1000-line file-size gate). A DECIDED push_content row on the
Approvals tab never falls through to that tab's generic `JsonBlock` either
(review finding 3) — a short repo/branch/acts_as_label summary instead, the
same fields this card's own header carries.

- **held** — PENDING and within the proxy's own bounded hold window
  (`push_rules.hold_seconds`, at most `maxHoldTimeout` = 600s —
  `internal/egress/proxy/approvals.go`). `isHeld` (`lib/types/approvals.ts`)
  is the ONE predicate this and every other reader (the board, the run
  cockpit) share; unlike tool_call/credential_reauth (unconditional while
  PENDING, since #509), push_content's own hold genuinely ends when the proxy
  times the connection out — a retry of the SAME commits rejoins this row and
  re-enters the hold, but the sandbox is not parked on it in between. The
  card carries its own timer (not just the next poll tick) so `PUSH.HELD_NOTE`
  flips to the DRAFT `PUSH.HELD_EXPIRED` right at the window's end, still
  admin/security-admin decidable either way (no ReasonDialog, no scope menu).
- **deciding** — both buttons disabled, one busy flag shared between them.
- **timed out** — EXPIRED (the sweeper's ~24h ceiling, not the hold window
  above); rendered in the decided list with `PUSH.TIMEOUT_BODY`.
- **cancelled** — the run ended; reuses `APPROVAL.CANCELLED_BODY`.
- **member watching** — sees the full card, no decision control
  (`SECURITY_ONLY_REASON`).
- **refused outright** — a `push_rules.deny_paths` match. NOT a held request:
  the sidecar refuses synchronously, before any approval row exists
  (`push_rules.go`), so this never reaches the card. It surfaces only in the
  run's audit trail, where `brokered:git:push-rules` gets a distinct label
  (`ruleSourceLabel`) and the canned `PUSH.DENIED_PATH_BODY` sentence
  (`RuleSourceChip`) — the sidecar logs no per-push cause text for this
  source, so the canned sentence is the whole explanation.

The rail's "Push rules" section (`ui/src/app/components/screens/new-run/new-run-rail.tsx`)
renders only when `pushRulesIsSet(spec.push_rules)` — mirroring Go's
`PushRulesSpec.IsSet()` exactly, so a stored `{}` reads as no section, not
"0 paths denied · 0 paths held for review". An unattended run adds
`PUSH.RAIL_UNATTENDED`.
