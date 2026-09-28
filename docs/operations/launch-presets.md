> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Launch presets

A launch preset is a named, versioned bundle of `POST /runs` fields (image,
repo, workspace, drive, a stored `policy_id` or an `inline_policy`). A
non-console launcher — a portal button, a CI dispatcher — sends the preset
name plus the per-launch fields instead of the whole spec. A preset grants
nothing beyond what the caller's own ceiling already allows.

## Sending a preset

```sh
curl -fsS -X POST "$WARDYN_URL/api/v1/runs" -H "Authorization: Bearer $TOKEN" \
  -d '{"preset":"nightly-tests","title":"nightly","task":"run the test suite"}'
```

1. The server replaces the request body with the preset's stored one.
2. It keeps the caller's own `title` and `task`.
3. It runs the unchanged create path: the caller's governance ceiling,
   capability grants, owner-scoped secrets and drive apply exactly as they
   would to the same request sent explicitly. A preset that exceeds a
   member's ceiling is refused the same way an explicit request is.
4. `POST /runs/preflight` expands a preset the same way.

Alongside `preset`, a request may set only `title`, `task` and
`preset_version`.

| Refusal | Status | Reason | When |
| --- | --- | --- | --- |
| Extra field | `400` | `preset_field_not_per_launch` | The request sets any field other than `preset`, `title`, `task` or `preset_version` |
| Unknown or closed preset | `422` | `preset_unknown` | The name doesn't exist, or isn't open to the caller's user type |
| Stale version | `409` | `preset_version_changed` | `preset_version` pins a version the preset has since moved past |

The run records `preset` and `preset_version` it was launched from.

## Routes

| Route | Who |
| --- | --- |
| `GET /presets`, `GET /presets/{name}` | Any signed-in person; a non-admin sees only the presets open to their user type (`user_types`, empty is every type) |
| `PUT /presets/{name}` | Admin only. Creates the preset at version 1, or replaces it and moves the version by one; an identical body changes nothing, not even the version |
| `DELETE /presets/{name}` | Admin only. Runs launched from it keep their stamp |

A write checks the name, the user types it names, and that the request is a
well-formed create body with no per-launch field. Everything else is checked
at launch, under the launching caller.

`ui_apps` in an `inline_policy` reach a member only where their ceiling
admits them. To hand members an app, point the preset at a stored policy they
are granted.

## Audit

Every write that lands is audited (`preset.create`, `preset.update`,
`preset.delete`; see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)). The create and
update rows carry the stored request — the only record of what an older
version contained.

## Round-trip

Presets round-trip declaratively, like drives:

1. Export the current presets:
   ```sh
   wardyn preset get > presets.json
   ```
2. Edit `presets.json`.
3. Apply it back:
   ```sh
   wardyn preset apply presets.json   # upserts by name
   ```
   A `get` immediately followed by `apply` is a no-op.
