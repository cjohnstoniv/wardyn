> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Console branding

- A super admin can make the console introduce itself as the organisation's own: **Admin view → Settings → Branding**.
- One record covers the whole deployment (migration `0091_branding`).
- With no record, the console is the unbranded Wardyn console, byte for byte.

## What a brand sets, and nothing more

| Field | What it does |
| --- | --- |
| Organisation name | Sets how the product name reads: `<Company> Wardyn` (`prefix`) or `Wardyn for <Company>` (`suffix`). Replaces the wordmark on the sign-in page, the top bar and the browser tab title (Admin view keeps its ` admin` suffix there) |
| Primary colour + text on primary | Hex pair, must reach 4.5:1 (WCAG AA). |
|  | Dark mode uses its own pair: set one explicitly, or let Wardyn derive it (the primary mixed toward white until it reaches 4.5:1 against the dark background, with dark text). |
|  | A set pair is held to the same 4.5:1 |
| Logo | SVG or PNG, at most 512 KB (a PNG at most 4096 pixels a side). Replaces the mark on the sign-in page, the top bar and the browser tab icon |
| Support link (optional) | `https://` only, shown in the header to everyone signed in; opens in a new tab with `rel="noopener noreferrer"` |

- A logo is uploaded to wardynd and served from it (`GET /api/v1/branding/logo`, its validated type, `nosniff`) — never hot-linked, so the console's Content-Security-Policy is unchanged.

> [!IMPORTANT]
> **Not brandable, on purpose:** the danger, warning, success and info colours, the approval-state chips, the denied-host rows and the Admin view cue stay Wardyn's own. No brand can recolour a security signal to look like decoration. The initial loading screen keeps the Wardyn mark.

## SVG logos are rebuilt, not stored as sent

- wardynd parses the upload and re-emits it from an allowlist of shape, text and gradient elements and presentation attributes; editor metadata is dropped.
- An SVG is refused, with the reason, if it carries any of:
  - `<script>`, `<foreignObject>`, `<image>`, `<a>`, `<style>`
  - animation, an `on*` handler
  - an `href` or `url()` that is not a same-document `#fragment`
  - a DOCTYPE
- Export from a design tool with "presentation attributes" rather than CSS if a `style` attribute is refused.

## Routes

| Route | Who |
| --- | --- |
| `GET /branding` | Anyone, signed in or not: the name, format, colours (the dark pair in effect) and logo URL the sign-in page draws; `{}` when unbranded |
| `GET /branding/logo` | Anyone, signed in or not |
| `GET /branding/settings` | Any signed-in person: the above plus the Support link, and `logo_from_file` (read-only, `true` while the site configuration delivers the logo, see below) |
| `PUT /branding/settings` | Admin only. Replaces the record; a body without `logo` keeps the stored logo, `"remove_logo": true` drops it. `remove_logo` on a logo the site configuration delivers is refused with a 400 |
| `DELETE /branding/settings` | Admin only. Back to unbranded |

- A refused save names its rule in `reason`: `invalid_org_name`, `invalid_name_format`, `invalid_colour`, `low_contrast` (the message carries the ratio), `link_not_https`, `invalid_link`, `logo_too_large`, `invalid_logo`, `logo_from_site_config` (`remove_logo` on a logo the site configuration delivers).

## Removing a logo, or all of it

The card has two removals, each behind a confirmation:

- **Remove logo** (beside the logo) drops only the logo. The name, format and colours stay and the initials tile takes its place in the header, the tab icon and the sign-in page.
- **Remove branding** (beside Save branding) deletes the whole record: name, colours, dark pair, logo and Support link. The console is Wardyn's own again for everyone, sign-in page included. It is `DELETE /branding/settings`.

Every save is audited as `branding.write` and a removal as `branding.delete` (see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

## The logo from the site configuration

`wardyn site-config set` and the MDM file (`/etc/wardyn/site-config.json`) can deliver the **logo** (not the name or colours, which stay on the card):

```json
{ "branding": { "logo_path": "/etc/wardyn/branding/logo.svg" } }
```

- `logo_path` is an absolute path, with no `..`, to a `.svg` or `.png` file **as wardynd sees it**: on its host, or inside its container, where a file under the MDM directory keeps the same `/etc/wardyn/...` path.
- wardynd reads it each time the document is applied and checks it exactly as an upload is checked.
- That is at most 512 KB, a PNG that decodes (at most 4096 pixels a side), or an SVG rebuilt from the allowlist above.
- A path that resolves through links must stay inside the directory it names, so a Kubernetes ConfigMap mount works and a link to another directory does not.
- The file must be a regular file.

- **A bad file refuses the whole apply** with a 400 (`site_config_invalid`, the reason names the path and the rule), like any other invalid block.
- **No branding yet is reported, not refused.**
  - The name and colours must exist before a logo has anything to attach to.
  - An apply that names `logo_path` first succeeds with `"branding_logo_pending": true` in the response and logs a warning, and `wardyn site-config set` prints a `warning:` line for it.
  - Apply again after saving the card and the logo is attached.
  - (An MDM file re-applied at every boot does this on its own.)
- While the file owns the logo the card shows where it comes from and offers no Remove logo: the next apply would put it back.
  - **Remove branding** stays, and its dialog says the logo returns once branding is set up again.
  - Uploading a different logo in the console replaces it until then.
- Taking `logo_path` out of the block, leaving `"branding": {}`, and applying removes a logo the file delivered; a logo someone uploaded is never touched.
  - Deleting the whole `branding` key changes nothing: a document that does not name `branding` leaves both the path and the logo as they are, as it does for the provider blocks.
- Every apply that names `branding` records `branding_logo_path` (and `branding_logo_sha256` of the stored bytes) on `site_config.write`.
- The file-delivered mark is migration `0105_branding_logo_from_file` (`branding.logo_from_file`).
