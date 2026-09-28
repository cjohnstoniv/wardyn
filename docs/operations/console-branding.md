> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Console branding

A super admin can make the console introduce itself as the organisation's own:
**Admin view → Settings → Branding**. One record for the whole deployment
(migration `0091_branding`); with none, the console is exactly the unbranded
Wardyn console, byte for byte.

What a brand sets, and nothing more:

- **Organisation name** and how the product name reads: `<Company> Wardyn`
  (`prefix`) or `Wardyn for <Company>` (`suffix`). It replaces the wordmark on
  the sign-in page and the top bar, and the browser tab title (the Admin view
  keeps its ` admin` suffix there).
- **Primary colour** and **text on primary**, as hex. The pair must reach 4.5:1
  (WCAG AA). Dark mode uses a pair of its own: set one, or leave it and Wardyn
  derives it (the primary mixed toward white until it reaches 4.5:1 against
  the dark background, with dark text); a set pair is held to the same 4.5:1.
- **Logo**, SVG or PNG, at most 512 KB (a PNG at most 4096 pixels a side). It
  replaces the mark on the sign-in page, in the top bar and as the browser tab
  icon. A logo is uploaded to wardynd and served from it
  (`GET /api/v1/branding/logo`, its validated type, `nosniff`), never
  hot-linked, so the console's Content-Security-Policy is unchanged.
- **Support link** (optional), `https://` only, shown in the header to
  everyone signed in; it opens in a new tab with `rel="noopener noreferrer"`.

Not brandable, on purpose: the danger, warning, success and info colours, the
approval-state chips, the denied-host rows and the Admin view cue stay Wardyn's
own, so no brand can recolour a security signal to look like decoration. The
initial loading screen keeps the Wardyn mark.

**SVG logos are rebuilt, not stored as sent.** wardynd parses the upload and
re-emits it from an allowlist of shape, text and gradient elements and
presentation attributes. An SVG with anything that can run script or fetch —
`<script>`, `<foreignObject>`, `<image>`, `<a>`, `<style>`, animation, an
`on*` handler, an `href` or `url()` that is not a same-document `#fragment`, a
DOCTYPE — is refused with the reason; editor metadata is dropped. Export from
a design tool with "presentation attributes" rather than CSS if a `style`
attribute is refused.

| Route | Who |
| --- | --- |
| `GET /branding` | anyone, signed in or not: the name, format, colours (the dark pair in effect) and logo URL the sign-in page draws; `{}` when unbranded |
| `GET /branding/logo` | anyone, signed in or not |
| `GET /branding/settings` | any signed-in person: the above plus the Support link |
| `PUT /branding/settings` | admin only. Replaces the record; a body without `logo` keeps the stored logo, `"remove_logo": true` drops it |
| `DELETE /branding/settings` | admin only. Back to unbranded |

A refused save names its rule in `reason`: `invalid_org_name`,
`invalid_name_format`, `invalid_colour`, `low_contrast` (the message carries
the ratio), `link_not_https`, `invalid_link`, `logo_too_large`,
`invalid_logo`. Every save is audited as `branding.write` and a removal as
`branding.delete` (see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

Branding is not part of `wardyn site-config set` yet; set it from the card
or with the routes above.

