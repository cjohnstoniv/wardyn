# User types: "What this type gets" — canon

The owner approved user types packet A (the model, rev 4) on 2026-09-23. Its "Access → User types →
what each type gets" frame draws the grid on a type's editor. The strings below are copied from that
frame exactly and ship byte for byte. `user-types-copy.test.ts` parses the frozen table back out of
this file and compares it with `EXPLAIN` (`ui/src/app/lib/user-types-copy.ts`) in both directions.

## How the grid reads

- Each capability kind is one family, headed by its Permissions label (`KIND[kind].label`,
  `permissions-copy.ts`). The kind's default (`*`) row comes first, then one row for each value a
  grant names or "Available to" restricts. That is the order `GET /permissions/explain` returns.
- A row is the state chip, then the value. Tones follow the packet: This type is green (`success`),
  Blocked is red (`danger`), and Everyone, Not available and Admins only are grey (`neutral`, and the
  row text is muted).
- A value shows by its name when a list the caller can read names it: a workspace, a stored policy,
  an agent the console knows, or a model provider (super admins only, since that roster is
  `operatorOnly`). The packet's own words cover `*` on workspaces and images and the two feature
  values. Any other value shows as written, in mono.
- A restricted value this type is not listed for reads `Not available`, then ` · `, then `ONLY(who)`.
  `who` is each audience of the value's allow rows: a type by its name (`AVAILABILITY.CHIP_TYPE`),
  a group as `AVAILABILITY.CHIP_GROUP`, and a person as `AVAILABILITY.CHIP_USER`.
- Under the first family that has a Blocked row there is one amber note: `WALL_HEAD` in bold, then
  `WALL_BODY`.
- A row decided by a grant written for this type carries `REMOVE`. It opens the Permissions
  screen's own confirmation (`PERM.REMOVE`, `PERM.REMOVE_CONFIRM(type name)`) and deletes that grant.
  A row decided by a grant for everyone (`all`) carries no Remove.

## Frozen strings

| Key | Where | Frozen string |
|---|---|---|
| `TITLE` | section heading on the type editor | What this type gets |
| `STATE.everyone` | state chip | Everyone |
| `STATE.this_type` | state chip | This type |
| `STATE.blocked` | state chip | Blocked |
| `STATE.admins_only` | state chip | Admins only |
| `STATE.not_available` | state chip | Not available |
| `ONLY(who)` | after ` · ` on a restricted value this type isn't listed for | only {who} |
| `WALL_HEAD` | wall note, bold | A block here is a wall. |
| `WALL_BODY` | wall note, after the head | It blocks everyone of this type, and an allow for a person or a group does not override it. |
| `REMOVE` | row button on this type's own row | Remove |
| `ALL_WORKSPACES` | the `workspace` kind's `*` row | All org workspaces |
| `ALL_IMAGES` | the `image` kind's `*` row | Images |
| `SSH_KEYS` | the `feature` value `ssh_key` | SSH keys |
| `API_TOKENS` | the `feature` value `api_token` | API tokens |
| `SSH_AND_TOKENS` | the `feature` kind's `*` row | SSH keys · API tokens |

## Not frozen: needs an owner mock round

Packet A does not draw the following, so the screen either leaves it out or carries strings that
are not canon yet:

- **The rest of the screen.** `USER_TYPES.*` (the list, the editor fields, delete, the Ceiling
  section's lead lines and `EXPLAIN_LOAD_FAILED`) was written against design rev 4 §2.6/§2.7,
  not a mock.
- **Families the packet doesn't have.** Egress hosts, Secrets and Model integrations have no
  family in the packet's grid. The packet also merges "Workspaces and images" and says "Policies" and
  "SSH and API tokens", where the grid uses the Permissions labels ("Workspaces", "Base images",
  "Stored policies", "SSH keys and API tokens").
- **The `*` row on the other seven kinds.** The packet names only the workspace, image and feature
  defaults, so the others show `*`.
- **Git provider names.** The packet shows a provider by its kind and organisation, but a git
  provider row has no name and its roster is `operatorOnly`, so the grid shows the provider id.
- **More than one audience after "only".** The packet shows one. The grid joins several with ", ".
- **Ceiling and Drives inside the grid.** The packet draws both as grid families. The editor keeps
  the Ceiling as its own read-only section above the grid, and drive allocations are not on the
  Explain wire.
- **The two frame footers.** "Grey rows are available to everyone; nothing was written for this
  type. Green rows are this type's own." is false for the grey Not available and Admins only rows,
  and for a This type row that a grant for everyone decides. "Same rows, written from the other side:
  adding a row here is the same as ticking this type on the resource." promises an add the grid does
  not have. Neither is rendered.
- **Adding a row from the grid.** Design §2.6 says the grid adds rows, but the packet draws no add
  control.
