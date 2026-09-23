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
`RUN_STATE.WAITING`, `APPROVAL_BANNER_LABEL.what`/`.blast`, `SECURITY_ONLY_REASON`.

## The three mock questions (packet 7)

- **Q181-1** — the rail reads "{d} paths denied · {r} paths held for review".
- **Q181-2** — the card lists ten paths, then "+N more"; the full list is in
  the run's audit trail. No expanding control on the card.
- **Q181-3** — no forge badge. The repository line names the forge implicitly
  (`github.com/...` / `dev.azure.com/...`); GitHub and Azure DevOps share one
  card.

## States

`PushContentCard` (`ui/src/app/components/wardyn/push-content-card.tsx`),
shared by the standalone `/approvals` queue and the run cockpit's live strip:

- **held** — PENDING, admin/security-admin may Approve/Deny directly (no
  ReasonDialog, no scope menu).
- **deciding** — both buttons disabled, one busy flag shared between them.
- **timed out** — EXPIRED; rendered in the decided list with `PUSH.TIMEOUT_BODY`.
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
