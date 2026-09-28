> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Console branding

A super admin can make the console introduce itself as the organisation's
own: **Admin view → Settings → Branding**. One record covers the whole
deployment (migration `0091_branding`). With no record, the console is the
unbranded Wardyn console, byte for byte.

## What a brand sets, and nothing more

| Field | What it does |
| --- | --- |
| Organisation name | Sets how the product name reads: `<Company> Wardyn` (`prefix`) or `Wardyn for <Company>` (`suffix`). Replaces the wordmark on the sign-in page, the top bar and the browser tab title (Admin view keeps its ` admin` suffix there) |
| Primary colour + text on primary | Hex pair, must reach 4.5:1 (WCAG AA). Dark mode uses its own pair: set one explicitly, or let Wardyn derive it (the primary mixed toward white until it reaches 4.5:1 against the dark background, with dark text); a set pair is held to the same 4.5:1 |
| Logo | SVG or PNG, at most 512 KB (a PNG at most 4096 pixels a side). Replaces the mark on the sign-in page, the top bar and the browser tab icon |
| Support link (optional) | `https://` only, shown in the header to everyone signed in; opens in a new tab with `rel="noopener noreferrer"` |

- A logo is uploaded to wardynd and served from it (`GET /api/v1/branding/logo`,
  its validated type, `nosniff`) — never hot-linked, so the console's
  Content-Security-Policy is unchanged.
- **Not brandable, on purpose:** the danger, warning, success and info
  colours, the approval-state chips, the denied-host rows and the Admin view
  cue stay Wardyn's own. No brand can recolour a security signal to look like
  decoration. The initial loading screen keeps the Wardyn mark.

## SVG logos are rebuilt, not stored as sent

wardynd parses the upload and re-emits it from an allowlist of shape, text
and gradient elements and presentation attributes; editor metadata is
dropped. An SVG is refused, with the reason, if it carries any of:

- `<script>`, `<foreignObject>`, `<image>`, `<a>`, `<style>`
- animation, an `on*` handler
- an `href` or `url()` that is not a same-document `#fragment`
- a DOCTYPE

Export from a design tool with "presentation attributes" rather than CSS if a
`style` attribute is refused.

## Routes

| Route | Who |
| --- | --- |
| `GET /branding` | Anyone, signed in or not: the name, format, colours (the dark pair in effect) and logo URL the sign-in page draws; `{}` when unbranded |
| `GET /branding/logo` | Anyone, signed in or not |
| `GET /branding/settings` | Any signed-in person: the above plus the Support link |
| `PUT /branding/settings` | Admin only. Replaces the record; a body without `logo` keeps the stored logo, `"remove_logo": true` drops it |
| `DELETE /branding/settings` | Admin only. Back to unbranded |

A refused save names its rule in `reason`: `invalid_org_name`,
`invalid_name_format`, `invalid_colour`, `low_contrast` (the message carries
the ratio), `link_not_https`, `invalid_link`, `logo_too_large`,
`invalid_logo`.

Every save is audited as `branding.write` and a removal as `branding.delete`
(see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

Branding is not part of `wardyn site-config set` yet — set it from the card
or with the routes above.
