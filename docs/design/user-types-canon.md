# User types: "What this type gets" — canon

The owner approved user types packet A (the model, rev 4) on 2026-09-23. Its "Access → User types →
what each type gets" frame draws the grid on a type's editor. Packet UT-G (2026-09-27) approved the
eight gaps packet A left open — the families packet A didn't draw, the `*` row on the other seven
kinds, a git provider's label, more than one audience after "only", the legend, the Add button, and
`USER_TYPES.*` (the rest of the screen), which packet A never drew at all. Every string below —
`EXPLAIN` and `USER_TYPES`, both in `ui/src/app/lib/user-types-copy.ts` — is now frozen and ships
byte for byte. `user-types-copy.test.ts` parses each of the two tables back out of this file and
compares it with its namespace in both directions.

## How the grid reads

- Each capability kind is one family, headed by its Permissions label (`KIND[kind].label`,
  `permissions-copy.ts`). The kind's default (`*`) row comes first, then one row for each value a
  grant names or "Available to" restricts. That is the order `GET /permissions/explain` returns.
  Every kind renders. (G-1 hid Model integrations, `integration`, while its only row was the
  default; 0.8 retired that kind with the AI integrations, so there is nothing left to hide.)
- A row is the state chip, then the value. Tones follow the packet: This type is green (`success`),
  Blocked is red (`danger`), and Everyone, Not available and Admins only are grey (`neutral`, and the
  row text is muted).
- The default (`*`) row of every kind names itself: `ALL_WORKSPACES`, `ALL_IMAGES` and
  `SSH_AND_TOKENS` for the three packet A drew, and `ALL_{family}` for the other six (G-3). A value
  shows by its name when a list the caller can read names it: a workspace, a stored policy, an agent
  the console knows, or a model provider (super admins only, since that roster is `operatorOnly`,
  unless the row already carries a server `label`). A git provider row and a model provider row carry
  a server-set `label` when the server can name them — the kind and its organisation or host for a
  git provider, the admin-set name for a model provider (G-4, `internal/api/capabilities_explain.go`'s
  `explainLabel`) — so a security admin reads the same words an operator does, with no id ever shown
  in its place. Any other value shows as written, in mono.
- A restricted value this type is not listed for reads `Not available`, then ` · `, then `ONLY(who)`.
  `who` is each audience of the value's allow rows: a type by its name (`AVAILABILITY.CHIP_TYPE`),
  a group as `AVAILABILITY.CHIP_GROUP`, and a person as `AVAILABILITY.CHIP_USER`. One audience reads by
  itself; two or three read as `WHO_LIST`; four or more read as `WHO_MORE`, naming the first two and
  counting the rest (G-5). Only the elided (four-or-more) case has anything left to disclose: the
  full, unelided list is a hover away (the truncated span's `title`) for a mouse user, and a same-DOM
  `sr-only` span carries it as real text for a keyboard or screen-reader user — never only an
  attribute a plain, non-focusable span can't expose.
- Under the first family that has a Blocked row there is one amber note: `WALL_HEAD` in bold, then
  `WALL_BODY`.
- A row decided by a grant written for this type carries `REMOVE`. It opens the Permissions
  screen's own confirmation (`PERM.REMOVE`, `PERM.REMOVE_CONFIRM(type name)`) and deletes that grant.
  A row decided by a grant for everyone (`all`) carries no Remove.
- Every family carries an `ADD` button that opens Permissions' own "Add a grant" dialog
  (`permissions.tsx`'s `AddGrantForm`), with Who (this type) and Capability (the family) fixed and
  read-only — the same strings, including `PERM.TYPE_DENY_TITLE`/`_BODY` before a type-level deny,
  and the same wire write. The grid reloads after a save. A refused write shows the server's own
  sentence inside the dialog, which stays open — never a toast (G-7).
- Under the grid, `LEGEND` is the one true statement about the tones above, and `FOOTER_OTHER_SIDE`
  — packet A's own second footer, kept verbatim — is true now that Add exists: adding a row here is
  the same grant Permissions would write (G-6).

## Frozen strings

| Key | Where | Frozen string |
|---|---|---|
| `TITLE` | section heading on the type editor | What this type gets |
| `STATE.everyone` | state chip | Everyone |
| `STATE.this_type` | state chip | This type |
| `STATE.blocked` | state chip | Blocked |
| `STATE.admins_only` | state chip | Admins only |
| `STATE.not_available` | state chip | Not available |
| `ONLY(who)` | after ` · ` on a restricted value this type isn't listed for, one audience | only {who} |
| `WHO_LIST(a, b, c)` | `ONLY`'s audience, two or three named (`c` empty means exactly two) | {a}, {b} and {c} |
| `WHO_MORE(a, b, n)` | `ONLY`'s audience, four or more — the first two, then a count | {a}, {b} and {n} more |
| `WALL_HEAD` | wall note, bold | A block here is a wall. |
| `WALL_BODY` | wall note, after the head | It blocks everyone of this type, and an allow for a person or a group does not override it. |
| `REMOVE` | row button on this type's own row | Remove |
| `ADD` | family heading button, opens Permissions' "Add a grant" | Add |
| `LEGEND` | under the grid | Green: this type gets it. Red: blocked for this type. Grey: not this type's own — the chip says why. |
| `FOOTER_OTHER_SIDE` | under `LEGEND`, packet A's own second footer | Same rows, written from the other side: adding a row here is the same as ticking this type on the resource. |
| `ALL_WORKSPACES` | the `workspace` kind's `*` row | All org workspaces |
| `ALL_IMAGES` | the `image` kind's `*` row | Images |
| `SSH_KEYS` | the `feature` value `ssh_key` | SSH keys |
| `API_TOKENS` | the `feature` value `api_token` | API tokens |
| `SSH_AND_TOKENS` | the `feature` kind's `*` row | SSH keys · API tokens |
| `ALL_EGRESS_HOSTS` | the `egress_host` kind's `*` row | All egress hosts |
| `ALL_SECRETS` | the `secret` kind's `*` row | All secrets |
| `ALL_AGENTS` | the `agent` kind's `*` row | All agents |
| `ALL_GIT_PROVIDERS` | the `workspace_provider` kind's `*` row | All git providers |
| `ALL_MODEL_PROVIDERS` | the `model_provider` kind's `*` row | All model providers |
| `ALL_POLICIES` | the `policy` kind's `*` row | All stored policies |

A git provider or model provider row's own `label` (G-4) is not a console string — it is server-set,
per row, from the resource it names (`explainLabel`, `internal/api/capabilities_explain_test.go`'s
`TestCapExplainLabelsGitAndModelProviderRows` pins its shape: `"{kind label} · {organisation or
host}"` for a git provider, the provider's own `Name` for a model provider). It is never an id and
never a secret.

## Frozen strings — the rest of the screen

`USER_TYPES.*` is the list, the editor, delete and the Ceiling section's lead lines — everything on
the screen packet A never drew. Packet UT-G (G-8) approved it as built, with six edits: `LEAD`,
`ID_HINT`, `DELETE_CONFIRM`, `DELETE_BUILTIN`, `CEILING_PROFILE` and `EXPLAIN_LOAD_FAILED` — the rest
freezes exactly as shipped.

| Key | Where | Frozen string |
|---|---|---|
| `TITLE` | nav and heading | User types |
| `LEAD` | list heading | A kind of person your organisation defines, like Portfolio manager or Contractor. Resources name it in "Available to", and its ceiling and run limits come from the governance profile assigned to it. |
| `COL_NAME` | list column | Name |
| `COL_DESCRIPTION` | list column | Description |
| `COL_PRIORITY` | list column | Priority |
| `COL_UPDATED` | list column | Updated |
| `BUILT_IN_BADGE` | list, the built-in row | Built in |
| `NEW_CTA` | list header button | New type |
| `EDIT` | list row button | Edit |
| `DELETE` | list row button | Delete |
| `EMPTY_TITLE` | list, no custom types | No custom types yet |
| `EMPTY_BODY` | list, no custom types | Everyone signs in as Standard user until you add one. |
| `FETCH_FAILED_TITLE` | list, load failed | Couldn't load user types |
| `FETCH_FAILED_BODY` | list, load failed | The list below may be stale or empty. Nothing was changed. |
| `ADD_TITLE` | editor, new type | New type |
| `EDIT_TITLE(name)` | editor, existing type | Edit {name} |
| `FIELD_NAME` | editor field | Name |
| `FIELD_DESCRIPTION` | editor field | Description |
| `DESCRIPTION_HINT` | editor field, under Description | Shown to a person of this type on their Getting started page. |
| `FIELD_PRIORITY` | editor field | Priority |
| `PRIORITY_HINT` | editor field, under Priority | Breaks a sign-in tie between two custom types — higher wins. Standard user never takes part in a tie. |
| `FIELD_ID` | editor field, new type only | Id |
| `ID_HINT` | editor field, under Id, new type only | Role mappings and grants use this to refer to the type. It's made from the name unless you set it, and it can't change once saved. |
| `SAVE` | editor button | Save |
| `CANCEL` | editor button | Cancel |
| `DELETE_CONFIRM(name)` | delete, confirm | Delete {name}? If a role mapping, grant, assignment, drive grant or run still names it, the delete is refused and nothing changes. |
| `DELETE_BUILTIN` | delete, the built-in type | Standard user can't be deleted — it's the type everyone signs in as when no other type matches. |
| `DELETE_RESTRICT_TITLE` | delete, refused | Still in use |
| `CEILING_TITLE` | editor, Ceiling section | Ceiling and run limits |
| `CEILING_LEAD` | editor, Ceiling section | Read-only here — assign or change the profile from Governance. |
| `CEILING_NONE` | editor, Ceiling section, no profile assigned | No governance profile is assigned to this type. Runs fall back to the deployment ceiling. |
| `CEILING_PROFILE(name)` | editor, Ceiling section, a profile assigned | Uses the {name} governance profile. |
| `OPEN_GOVERNANCE` | editor, Ceiling section | Open in Governance |
| `EXPLAIN_LOAD_FAILED` | "What this type gets", load failed | Couldn't load what this type gets. |

## Kept as built, not asked

Packet UT-G's approve note settles two more things without a question: the Ceiling and run limits
section stays its own read-only block above the grid (the ceiling is a profile, not a grant), and
drive allocations stay off the grid (they are not on the `GET /permissions/explain` wire). Neither is
a canon string — there is nothing here for either to freeze.
