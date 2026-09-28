> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Launch presets

A launch preset is a named, versioned bundle of existing `POST /runs` fields
(image, repo, workspace, drive, a stored `policy_id` or an `inline_policy` with
its `ui_apps` and ports, and so on). A launcher that is not the console, such
as a portal button or a CI dispatcher, sends the name and the per-launch fields
instead of the whole spec:

```sh
curl -fsS -X POST "$WARDYN_URL/api/v1/runs" -H "Authorization: Bearer $TOKEN" \
  -d '{"preset":"nightly-tests","title":"nightly","task":"run the test suite"}'
```

The server replaces the request with the preset's stored one, keeps the
caller's `title` and `task`, and runs the unchanged create path. A preset
grants nothing: the caller's governance ceiling, capability grants,
owner-scoped secrets and drive apply exactly as they would to the same request
sent explicitly, so a preset that exceeds a member's ceiling is refused the way
the explicit request is. Alongside `preset`, a request may set only `title`,
`task` and `preset_version`; any other field is refused `400`
(`preset_field_not_per_launch`). An unknown preset, or one not open to the
caller's user type, is `422` (`preset_unknown`). `preset_version` pins the
version the caller expects; a preset changed since is refused `409`
(`preset_version_changed`). The run records `preset` and `preset_version`.
`POST /runs/preflight` expands a preset the same way.

| Route | Who |
| --- | --- |
| `GET /presets`, `GET /presets/{name}` | any signed-in person; a non-admin sees only the presets open to their user type (`user_types`, empty is every type) |
| `PUT /presets/{name}` | admin only. Creates the preset at version 1, or replaces it and moves the version by one; an identical body changes nothing, not even the version |
| `DELETE /presets/{name}` | admin only. Runs launched from it keep their stamp |

Every write that lands is audited (`preset.create`, `preset.update`,
`preset.delete`, see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)); the create and
update rows carry the stored request, which is the only record of what an
older version contained. A write checks the name, the user types it names and
that the request is a well-formed create body with no per-launch field;
everything else is checked at launch, under the launching caller. `ui_apps` in
an `inline_policy` reach a member only where their ceiling admits them; to
hand members an app, point the preset at a stored policy they are granted.

Presets round-trip declaratively, like drives:

```sh
wardyn preset get > presets.json
wardyn preset apply presets.json   # upserts by name; get then apply is a no-op
```

