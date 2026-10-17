# Entra payload record

Every payload here is a **published example, not a capture from a live Entra provisioning job.**
No fixture in this directory was recorded from real Entra traffic. Owner ruling, 2026-10-03: build the
goldens from RFC 7644 and the request examples Microsoft publishes for Entra provisioning, cite the source in
each file, and diff a live tenant capture against them before the release.

Each `*.json` file is `{"source": <url>, "captured": false, "body": <payload>}`. `body` is the published
payload with its values unchanged; the only edits are the trailing commas Microsoft's pages leave in some
JSON (invalid in JSON, dropped). The published payloads carry only synthetic names and ids
(`testuser.com`, `example.com`, documentation GUIDs), so nothing was sanitised.

## Sources

| Prefix | Source |
|---|---|
| `entra-doc-*` (users, groups, list and error responses) | Microsoft Learn, "Tutorial: Develop and plan provisioning for a SCIM endpoint", `https://learn.microsoft.com/en-us/entra/identity/app-provisioning/use-scim-to-provision-users-and-groups` |
| `entra-doc-*` (the `active` string, path-less replace, multi-attribute replace, member-remove forms) | Microsoft Learn, "Known issues with SCIM 2.0 protocol compliance", `https://learn.microsoft.com/en-us/entra/identity/app-provisioning/application-provisioning-config-problem-scim-compatibility` |
| `rfc-*` | RFC 7644, `https://www.rfc-editor.org/rfc/rfc7644` (section 3.5.1 for PUT, section 3.5.2.2 for member remove) |

## Files

| File | Shape | Published by |
|---|---|---|
| `entra-doc-post-user.json` | POST /Users with `externalId` carrying the Entra objectId | Entra tutorial |
| `entra-doc-post-group.json` | POST /Groups | Entra tutorial |
| `entra-doc-patch-user-email-name.json` | PATCH replace of `emails[type eq "work"].value` and `name.familyName` | Entra tutorial |
| `entra-doc-patch-user-username.json` | PATCH replace of `userName` | Entra tutorial |
| `entra-doc-patch-user-active-false-bool.json` | PATCH `Replace` of `active` with a boolean | Entra tutorial |
| `entra-doc-patch-user-active-string-false.json` | PATCH `Replace` of `active` with the string `"False"` | Known-issues page, behaviour without the compliance flag |
| `entra-doc-patch-user-active-bool-lowercase-op.json` | PATCH lower-case `replace` of `active` with a boolean | Known-issues page, behaviour with the compliance flag |
| `entra-doc-patch-user-add-nickname.json` | PATCH `Add` of an attribute Wardyn ignores | Known-issues page |
| `entra-doc-patch-user-multi-replace.json` | PATCH of six `Replace` operations, one `externalId` and one extension attribute | Known-issues page |
| `entra-doc-patch-user-pathless-replace.json` | PATCH `replace` with no `path` and a value object | Known-issues page, behaviour with the compliance flag |
| `entra-doc-patch-group-displayname.json` | PATCH replace of group `displayName` | Entra tutorial |
| `entra-doc-patch-group-add-members.json` | PATCH `Add` of members with `"$ref": null` | Entra tutorial |
| `entra-doc-patch-group-remove-members.json` | PATCH `Remove` of members with a value array and `"$ref": null` | Entra tutorial |
| `entra-doc-patch-group-remove-members-value-array.json` | PATCH `Remove` of members with a value array | Known-issues page, behaviour without the compliance flag |
| `entra-doc-patch-group-remove-members-filter-path.json` | PATCH `remove` with `members[value eq "..."]` | Known-issues page, behaviour with the compliance flag |
| `entra-doc-response-list-users.json` | ListResponse for a user query | Entra tutorial |
| `entra-doc-response-error-404.json` | Error envelope | Entra tutorial |
| `rfc-put-user.json` | PUT /Users full replacement | RFC 7644 section 3.5.1 |
| `rfc-patch-group-remove-member-filter-path.json` | PATCH remove of one member by filter path | RFC 7644 section 3.5.2.2 |
| `rfc-patch-group-remove-all-members.json` | PATCH remove of `members` with no value | RFC 7644 section 3.5.2.2 |

The `rfc-*` files are shapes Entra does not send (PUT) or sends only under a compliance flag (filter-path
remove); they are **not captured from Entra**. So are all the `entra-doc-*` files.

## Shapes Entra actually sent

None recorded yet. Which of the documented shapes the live client sends, and in which mode (with or without
the compliance flag), is exactly what the live capture settles.

## The gap, and how it closes

Before the release the owner points the test tenant's provisioning job at a capture endpoint and drives
create, rename, `active` false and true, a path-less replace, a group member add and a group member remove.
The captures are sanitised (tenant, object and group ids to synthetic GUIDs, addresses to placeholders) and
added here as `entra-live-*.json` with `"captured": true`, and this file records when and against what. Then
each live payload is diffed against its published twin; a difference is a parser change or a new fixture,
never an edit to an existing one.

Known points the capture must settle:

- A Microsoft sample shows an unquoted filter value (`externalId eq jyoung`). `ParseFilter` follows RFC 7644
  and refuses it as `invalidFilter`; the live client is expected to send the quoted form.
- Whether `active` arrives as the string `"False"` or a boolean on this tenant. Both parse.
- Whether member add and remove carry `"$ref": null` and in which form (value array or filter path).
- Whether any operation carries an attribute outside the ones in the table above.
