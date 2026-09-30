# Console branding — frozen strings (#1125)

The approved copy from the console-branding mock packet (#1125, approved by the owner on
2026-09-27 with every recommended answer), byte for byte. Code carries these strings
(`ui/src/app/lib/branding-copy.ts`); this table is where a reviewer checks them, and
`branding-copy.test.ts` parses it back out. A `{name}` in a cell is the placeholder the key takes.
`512 KB` in `BRANDING.LOGO_HINT` and `BRANDING.ERR_LOGO(size)` is joined by a no-break space
(U+00A0), as in the packet.

## Owner decisions

| Id | Decision |
|---|---|
| name format | Per org, either `<Company> Wardyn` or `Wardyn for <Company>` (issue comment, 2026-09-27). |
| B-1 | A Branding card in Admin view Settings, for the super admin. |
| B-2 | The logo is uploaded to and served by Wardyn: SVG or PNG, at most 512 KB. |
| B-3 | Only the primary colour, its text colour, the logo and the name are brandable. Danger, warning, success, info and the Admin view cue stay Wardyn's own. |
| B-4 | The dark-mode primary is optional; Wardyn derives a dark-safe pair when it is unset and contrast-checks whichever pair is in effect. |
| B-5 | One optional https-only Support link in the header. |

Reused, unchanged by branding: `SIGNIN.*` (`lib/sign-in-copy.ts`), `SESSION_ENDED_REASON`, the
Denied hosts card and `ApprovalStateBadge`. The initial loading screen keeps the Wardyn mark.

## Frozen strings

| Key | Renders at | Frozen string |
|---|---|---|
| `BRAND_NAME.PREFIX(company)` | sign-in, top bar, browser tab title | {company} Wardyn |
| `BRAND_NAME.SUFFIX(company)` | same surfaces | Wardyn for {company} |
| `BRANDING.TITLE` | Admin view Settings card heading | Branding |
| `BRANDING.LEDE` | Branding card | How this Wardyn console looks and introduces itself to the people who sign in. |
| `BRANDING.ORG_NAME_LABEL` | Branding card | Organisation name |
| `BRANDING.NAME_FORMAT_LABEL` | Branding card | How the product name appears |
| `BRANDING.PRIMARY_LABEL` | Branding card | Primary colour |
| `BRANDING.TEXT_LABEL` | Branding card | Text on primary |
| `BRANDING.LOGO_LABEL` | Branding card | Logo |
| `BRANDING.LOGO_HINT` | Branding card | SVG or PNG, up to 512 KB. Square works best — it appears at 28px in the header and 32px as the browser tab icon. |
| `BRANDING.LINK_LABEL` | Branding card | Support link (optional) |
| `BRANDING.LINK_HINT` | Branding card | Shown in the header. Must be https. |
| `BRANDING.DARK_LABEL` | Branding card | Use a different colour in dark mode (optional) |
| `BRANDING.SAVE` | Branding card | Save branding |
| `BRANDING.PREVIEW_TITLE` | Branding card | Live preview |
| `BRANDING.ERR_COLOR` | Branding card, colour fields | Enter a valid hex colour, like #0f766e. |
| `BRANDING.ERR_CONTRAST(n)` | Branding card, computed live from the two hex fields | This text colour has a contrast ratio of {n}:1 against the button background. Wardyn requires at least 4.5:1 for body text (WCAG AA). |
| `BRANDING.ERR_LINK` | Branding card | This link must use https. http:// links, and links with no scheme, aren't allowed. |
| `BRANDING.ERR_LOGO(size)` | Branding card | This logo is {size}. Upload an image under 512 KB (SVG or PNG). |

## Frozen strings — remove controls (#1215)

The Remove logo and Remove branding packet, approved by the owner on 2026-09-30 as drawn, byte for
byte (the apostrophes are U+2019, as in the packet). "Cancel" on both dialogs is the console's own
word.

| Key | Renders at | Frozen string |
|---|---|---|
| `BRANDING.REMOVE_LOGO` | Branding card, beside the stored logo | Remove logo |
| `BRANDING.REMOVE_BRANDING` | Branding card, beside Save branding | Remove branding |
| `BRANDING.REMOVE_LOGO_TITLE` | Remove logo dialog, title | Remove the logo? |
| `BRANDING.REMOVE_LOGO_BODY(company)` | Remove logo dialog, body | The header and the browser tab show {company}’s initials instead. The name and colours stay. |
| `BRANDING.REMOVE_LOGO_CONFIRM` | Remove logo dialog, confirm button | Remove logo |
| `BRANDING.REMOVE_LOGO_TOAST` | toast after the logo is removed | Logo removed. |
| `BRANDING.REMOVE_BRANDING_TITLE` | Remove branding dialog, title | Remove all branding? |
| `BRANDING.REMOVE_BRANDING_BODY` | Remove branding dialog, body | The console goes back to Wardyn’s own name, colours and mark for everyone, including the sign-in page. The logo and Support link are deleted. |
| `BRANDING.REMOVE_BRANDING_CONFIRM` | Remove branding dialog, confirm button | Remove branding |
| `BRANDING.REMOVE_BRANDING_TOAST` | toast after all branding is removed | Branding removed. |
| `BRANDING.FILE_LOGO_NOTE` | Branding card, under a logo the site configuration delivers (no Remove logo there) | This logo comes from your site configuration. To remove it, take branding.logo_path out of that file. |
| `BRANDING.FILE_LOGO_DIALOG_LINE` | Remove branding dialog, second paragraph, only for that logo | The logo from your site configuration comes back the next time it is applied. |

## Implementation strings

Drawn in the packet's prototype but not rowed in its Strings table; taken from the prototype as
drawn. The last four are the card's own save and removal feedback, which the prototypes do not draw.

| Key | Renders at | Frozen string |
|---|---|---|
| `BRAND_HEADER.SUPPORT_CHIP` | top bar, beside the theme toggle | Support |
| `BRANDING.FORMAT_PREFIX_HINT` | Branding card, under the first name option | `<Company> Wardyn` |
| `BRANDING.FORMAT_SUFFIX_HINT` | Branding card, under the second name option | `Wardyn for <Company>` |
| `BRANDING.CONTRAST_OK(n)` | Branding card, under Text on primary | {n}:1 — passes WCAG AA. |
| `BRANDING.FIX_ONE` | Branding card, beside a disabled Save | Fix the highlighted field to save. |
| `BRANDING.FIX_MANY` | Branding card, beside a disabled Save | Fix the highlighted fields to save. |
| `BRANDING.FIXED_TITLE` | Branding card, preview column | Fixed, never brandable |
| `BRANDING.SAVED` | toast after a save | Branding saved. |
| `BRANDING.SAVE_FAILED` | toast when the server refuses a save | Branding wasn't saved. |
| `BRANDING.REMOVE_LOGO_FAILED` | toast when the server refuses Remove logo | Logo wasn't removed. |
| `BRANDING.REMOVE_BRANDING_FAILED` | toast when the server refuses Remove branding | Branding wasn't removed. |

## Where the brand applies, and where it never does

- Sign-in page: the logo (or a monogram tile in the primary colour when there is none) and the
  product name replace the shield and "Wardyn".
- Top bar: the logo or monogram and the product name replace the Wardyn wordmark; the Support
  chip sits beside the theme toggle and opens in a new tab with `rel="noopener noreferrer"`.
- Browser tab: the title is the product name (the Admin view keeps its ` admin` suffix, the
  Admin view cue); the icon is the logo when one is set.
- Colours: `--primary` and `--primary-foreground`, per theme, and nothing else.
- Never: danger, warning, success and info, the approval-state chips, the denied-host rows, the
  Admin view eyebrow, and the loading screen's Wardyn mark.
