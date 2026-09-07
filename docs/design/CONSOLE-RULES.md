# Console design rules

The rulebook for Wardyn's web console. Every rule names the token or component that
implements it, so review is a lookup, not an argument. A rule needing something the
theme lacks names the token to add — it never invents a value.

## 1. Source of truth

| Layer | Lives in | Owns |
|---|---|---|
| Tokens | `ui/src/styles/theme.css` — `:root` light (L14–119), `.dark` (L121–204) | Every color, radius, font family, weight |
| Utility bindings | `theme.css` `@theme inline` (L206–285) | Token → utility class (`--color-primary` → `bg-primary`) |
| Element defaults | `theme.css` `@layer base` (L287–316) | `body`, `h1`–`h4`, `label`, `button`, `input` |
| Utilities | `theme.css` `@layer utilities` (L319–338) | `.scroll-thin`, `.label-eyebrow` |
| Primitives | `ui/src/app/components/ui/` | Role shells: `Button`, `Input`, `Dialog`, `DropdownMenu`, … |
| Pattern layer | `ui/src/app/components/wardyn/` | Wardyn's vocabulary: `Chip`, `RunStateBadge`, `ConfinementChip`, `Field`, `OptionCard`, `EmptyState` |

- Never hard-code a hex, radius, or size when a token covers the role.
- A new color goes into **both** `:root` and `.dark` and is bound in `@theme inline`
  *before* it is used anywhere.
- Contrast is a token property — the `:root` comments record the WCAG measurement behind
  each value. Re-measure on the tightest surface.
- Fonts are self-hosted (`index.css`) — a new weight is one more local font import,
  never a CDN link.

## 2. Color budget — the rule that keeps the console quiet

Teal is the only loud color, and it means *"press this."* Everything else is grey.

| Role | Token(s) | May appear on | Never on |
|---|---|---|---|
| Affirmative action | `--primary` / `--primary-foreground` | The **one** `default` Button per surface (`button.tsx:12`) | Hover backgrounds, decoration, headings, ornamental icons |
| Selected state | `--primary` at low alpha | `OptionCard` selected (`form-primitives.tsx:80`), `Checkbox`/`RadioGroup` checked, text `selection:` (`input.tsx:11`) | Anything not actually selected |
| Focus ring | `--ring` | `focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]` (`button.tsx:8`, `input.tsx:12`) — full alpha; a `/50` ring never met the 3:1 floor | Anything merely selected — the ring stays neutral so focus and selection read differently |
| Active nav item | `--sidebar-accent` fill + `--sidebar-primary` rail | `navLinkClass` (`app-shell.tsx:305–311`) and the active rail (`app-shell.tsx:331`) | A third active treatment — use the shipped one |
| Barrier tier | METALS: `--fence-*` bronze, `--wall-*` silver, `--vault-*` gold | `ConfinementChip` (`primitives.tsx:246–267`), `BarrierStrengthStrip`, `tier-illustration.tsx:70–72`, the tier matrix (`setup/environment-step.tsx:53–55`) | Card borders, run state, buttons, section headers |
| Run / approval / health state | `--success` `--warning` `--danger` `--info` `--cyan` + their `-subtle` fills | `Chip` tones (`primitives.tsx:51–60`), shell banners (`app-shell.tsx:494`), `TruncatedNote` | Decoration. Always paired with a glyph or word — never color alone |
| Agent identity | `--agent-claude` and the per-agent badge colors | `AgentBadge` monogram (`primitives.tsx:389–395`) | Anything but the WHO badge |
| Destructive | `--destructive` (= `--danger`) | `destructive` Button, delete confirmations | Deny, Cancel, or any reversible action |
| Everything else | `--muted`, `--muted-foreground`, `--accent`, `--border`, `--border-strong`, `--surface-2` | Surfaces, rows, hairlines, secondary text | — |

- **Risk grade is semantic, not metal.** `SafetyMeter` grades a *policy*, so it uses
  `--success/--warning/--danger` (`safety-meter.tsx:41–45`). Metals say how separated
  the agent is; semantics say how it went.
- **Text links are `--info`, not teal** (`button.tsx:21`'s `link` variant already says
  so). Teal underlined text reads as a button that failed.

Known violations, cited as what the rule forbids:

| Site | What it does | Why it is out of budget |
|---|---|---|
| `setup/environment-step.tsx:548` | `<Chip tone="primary">Recommended</Chip>` | Decorative teal. A recommendation is not an action — `tone="neutral"`, or let the default selection carry it |
| `run-context-row.tsx:57` | `text-primary` on "Open run" | Teal on a disclosure control. It is a link: `--info`, or a `ghost` Button |

`runs.tsx`'s two former violations at this same class ("Show all N" and "Load N more")
are fixed: both now render `text-info` (`runs.tsx:541`, `runs.tsx:663`).

## 3. Type scale

Four body rungs. Nothing between them.

| Rung | Size | Weight | Use | Today |
|---|---|---|---|---|
| 11px | `0.6875rem` = `text-meta` | 600 uppercase for meta labels, 400 for captions | Section eyebrows, trailing metadata, captions, hints | `.label-eyebrow` (`theme.css:330–337`) — 600, `0.06em`, uppercase, `--muted-foreground` |
| 12px | `0.75rem` = `text-xs` | 400 | Helper text, paths, secondary content, chips | `Chip` is `text-xs` (`primitives.tsx:90`) |
| 13px | `0.8125rem` = `text-body` | 400–500 | Dense table rows, sidebar items | `--text-body` (`theme.css`, `@theme inline`) |
| 14px | `0.875rem` = `text-sm` | 400 body, 500 row titles | Body copy, button text, labels | `button`/`label` are 500 by base rule (`theme.css:308–309`) |

Headings come from `@layer base`, used as-is: `h1` `1.5rem`/600/1.3/`-0.01em` · `h2`
`1.125rem`/600/1.35/`-0.005em` · `h3` `1rem`/600/1.4 · `h4` `0.875rem`/500/1.4.

- **Weights:** 400 body and inputs, 500 row titles / buttons / labels, 600 headings and
  uppercase meta labels — the three `--font-weight-*` tokens.
- **Body tracking is `0.01em`.** `theme.css`'s `body` sets none today — add
  `letter-spacing` there, not `tracking-*` at call sites.
- **Mono (`--font-mono`) is for literals only:** run ids, paths, commands, fingerprints,
  exit codes, URLs, wire values — never prose, labels, or emphasis (`Chip` takes `mono`
  for this, `primitives.tsx:92`).
- **No ad-hoc sizes.** The rungs have named homes: `--text-meta` (11px) and
  `--text-body` (13px) sit in `@theme inline` beside `text-xs` and `text-sm`, and the
  polish tier swept every `text-[…]` size onto one of them — a new `text-[…]` size is a
  regression, not a style choice.
- Off-rung sizes were rounded to a rung (11.5→11, 12.5→12, 13.5→13). The one
  deliberate exception class is a glyph in fixed chrome (the `AgentBadge` monogram, the
  tile-size pill), which sits at the 11px rung today; give it its own token if it ever
  needs to be smaller than text.
- Two 11px uppercase labels disagree on tracking: `.label-eyebrow` is `0.06em`,
  `SectionCard`'s `h2` is `tracking-wider` (`primitives.tsx:426`). Use `.label-eyebrow`,
  as `WidgetCard` does (`primitives.tsx:488`).

## 4. Radius and elevation

`--radius: 0.625rem` (10px) is the base; the rest derive from it (`theme.css:270–273`):
`rounded-sm` (6px) chip dots · `rounded-md` (8px) inputs, chips, small buttons, menus ·
`rounded-lg` (10px) buttons, option cards, widget cards · `rounded-xl` (14px) document
cards, run cards, icon wells. Exactly three elevation levels:

| Level | How | Where |
|---|---|---|
| Hairline | `border-border` only | Rows, dividers, banners, table cells |
| Lift | `shadow-xs` + border | Cards, `outline` buttons. Only `checkbox.tsx:17` and `radio-group.tsx:30` carry it today — the cards are border-only, which is in budget |
| Floating | one `--shadow-floating` token | Popovers, dropdowns, selects, dialogs, sheets, floating toolbars |

`--shadow-floating` is ONE shared value in `theme.css` (`@theme inline`), deliberately not
per-theme — a floating surface reads the same way in light and dark. It replaced the two
spellings that shipped one idea: `shadow-md` (`dropdown-menu.tsx:45`, `select.tsx:68`,
`popover.tsx:33`) and `shadow-lg` (`dialog.tsx:60`, `alert-dialog.tsx:57`,
`sheet.tsx:61`, L233). A floating surface is one thing; it gets one shadow. Anything
wanting a fourth level wants the focus ring.

## 5. Status vocabulary and glyph pairing

**Every actor on a row is two adjacent glyphs, never fused:** WHO (the `AgentBadge`
monogram, `primitives.tsx:389–395`) and WHAT (the state). Fusing them means neither can
change independently. State is an 8px dot (`size-2`) or an icon, **plus a text label** —
6px in a `Chip` or dense row, which `Chip` renders (`size-1.5`, `primitives.tsx:98–103`).
A live state adds `pulse`; a terminal state swaps the dot for an icon. `RunStateBadge`
is the one badge for board, table, and detail header (`primitives.tsx:195–217`):

| Wire state | Label | Tone | Glyph |
|---|---|---|---|
| `PENDING` | Pending | neutral | dot |
| `STARTING` | Starting | info | dot + pulse |
| `RUNNING` | Running | success | dot + pulse |
| `WAITING_FOR_CONFIRMATION` (running, approval held) | Awaiting confirmation | warning | dot + pulse |
| `COMPLETED` | Completed | success | `Check` |
| `STOPPED` | Stopped | neutral | `Square` |
| `ARCHIVED` | Archived | neutral | `Archive` |
| `FAILED` | Failed | danger | `CircleX` |
| `KILLED` | Killed | danger, **solid fill** | `ShieldX` |

Solid saturated red is reserved exclusively for `KILLED`, the enforcement outcome. An
unrecognized state degrades to a neutral chip showing the raw value (`metaFor`, `primitives.tsx:156`) —
never crashing, never borrowing an unearned tone.

**Precedence when indicators compete on one row** — highest wins, and only the winner
gets the row's accent: needs-you (held approval / awaiting confirmation) > failed or
killed > working > starting or pending > done > idle. The board flattens the first two
into one amber treatment (`runs.tsx:60, 633–639`) — the divergence to close: an approval
is a request, a failure is a report.

## 6. Buttons and back-out paths

| Variant | Role | Rule |
|---|---|---|
| `default` (teal) | The affirmative action | **Exactly one per surface.** Two teal buttons means the surface has not decided what it is for |
| `secondary` | A second action of equal weight | Rare — usually a sign the surface does two jobs |
| `outline` | A safe alternative or non-committal action | `Attach`, `Retry` (`states.tsx:69–71`), and **`Deny`** |
| `ghost` | Back-out and chrome | Cancel, Dismiss, Close, Discard, icon buttons |
| `destructive` | Irreversible loss | Delete, purge — confirmation dialog required |
| `link` / `info` | Inline navigation | `--info`, underline on hover |

- **Back-out is quiet.** Cancel / Dismiss / Close / Discard are `ghost`: never
  destructive, never colored, no keyboard chip. Leaving is not a decision.
- **Deny is `outline`, not red** — denying is the safe answer, and painting it as
  destruction pushes operators toward Approve.
- **The irreversible action wears the weight.** A quiet dangerous choice beside a teal
  safe one is a surface lying about its stakes.
- Sizes: `default` h-9, `sm` h-8, `lg` h-10, `icon` size-9 (`button.tsx:25–28`) — picked
  by density, never by emphasis.

## 7. In-flight feedback

Bind `disabled` the instant the action fires; reveal the spinner later.

| Expected duration | Show |
|---|---|
| 0–100ms | Nothing. A flash of spinner reads as a fault |
| 100ms–1s | `disabled` only (`disabled:opacity-50`, `button.tsx:8`) |
| 1–3s | `disabled` + `Loader2 animate-spin`, or a label swap ("Save" → "Saving…") |
| 3s+ | Stage labels that say what is happening ("Building image…", "Starting sandbox…") |

- **Pre-reserve the control's width** before a label swaps, or the row reflows and the
  button moves out from under the cursor.
- **Remote or slow actions:** disable immediately, spinner after ~200ms — that gap is
  what makes a fast response feel instant, not flickery.
- A busy control is `disabled`, not removed. Never two spinners for one action.

## 8. Forms

`Field` (`form-primitives.tsx:20–54`) is the shape; do not rebuild it.

| Part | Rule |
|---|---|
| Label group | `space-y-2` between label, control, and hint; `Label` is 14px/500 by base rule |
| Required marker | A `*` **sibling** of the label, `aria-hidden`, plus native `required` on the control — never color alone, never inside the accessible name |
| Helper text | 12px `--muted-foreground`, below the control |
| Trailing meta | 11px `--muted-foreground`, right-aligned (`OperatorOnlyHint`, `primitives.tsx:127`) |
| Errors | `aria-invalid` on the control — `Input` already renders the destructive ring and border. Never hand-paint an error style |
| Placeholder | Illustrative only, `--placeholder-foreground`. A placeholder is never a label |

- **Default focus lands on the primary field** when a form or dialog opens — not on
  Cancel, not on the first tabbable icon button.
- **Enter submits from single-line inputs only** — in a textarea it inserts a newline,
  and nothing else may claim it.
- **Esc backs out quietly:** no prompt for an untouched form, an explicit discard prompt
  for a dirty one.

## 9. Lists, rows, empty and error states

| Row state | Treatment |
|---|---|
| Idle | Transparent |
| Hover | `--accent` (`hover:bg-accent`, `hover:border-border-strong`) |
| Selected / current | `--accent` plus `aria-current` (nav) or `aria-selected` (list) — the attribute is the truth, the fill is its rendering |
| Focus | The `--ring` ring. Focus and selection are different states and must look different |

- **Cards** are `bg-card` + `border-border`, distinct from their parent: `SectionCard`
  (`rounded-xl`, `p-4`) is the document card, `WidgetCard` (`rounded-lg`) the pane card.
  **Never nest a card in a card** — that is a section.
- **Empty states carry the action that fills them.** `EmptyState` (`states.tsx:11`)
  takes an `action`; omit it only when the emptiness is good news. Put the doc link next
  to the need.
- **Persistent errors render inline** with something to read, retry, or act on:
  `ErrorState` (`states.tsx:44`) for a pane, a shell banner (`app-shell.tsx:494`),
  `TruncatedNote` (`states.tsx:84–103`) for a partial result.
- **Toasts (`sonner`) are transient confirmations only** — "Copied", "Secret saved".
  Anything worth reading twice is not a toast.
- **Skeletons match the final layout's height** (`TableSkeleton`, `states.tsx:105`).
- **`.scroll-thin` on every scroller** — a default OS scrollbar in a console pane is a
  visual leak.

## 10. Copy

- **Sentence case** everywhere except the 11px uppercase meta label.
- **Never overclaim.** Neutral process language while pending ("Checking egress…");
  result verbs — "verified", "protected", "found" — only with a real result behind them.
  A confident empty state is a false claim.
- **Specific nouns:** "3 runs awaiting confirmation", not "Some items need attention".
  Say the thing, say how many.
- **No filler** — "please", "simply", "just" add length and subtract credibility.
- **Canonical strings are the app's strings.** `copy.ts` and `cc-meta.ts` own the shared
  vocabulary so a chip and the sentence beside it cannot disagree. A copy change is
  called out as one — tests and demo narration read them.

## 11. Screen review rubric

Run this on any screen or mock before it ships:

- [ ] **Top 3 fixes** — name them before anything else
- [ ] **Friction** — where does the operator stall, re-read, or backtrack?
- [ ] **Progressive disclosure** — anything shown that is not needed yet?
- [ ] **Action hierarchy** — exactly one `default` button, and it is the right one
- [ ] **1–2 action workflows** — the main job takes one or two moves
- [ ] **Default focus** on the primary field or primary action
- [ ] **Keyboard** — full tab order, visible ring, Esc backs out, no traps
- [ ] **Alignment** — one grid; labels, controls, and trailing meta line up
- [ ] **Copy** — §10, canonical strings unchanged
- [ ] **Dialog sizing** — `sm:max-w-md` to confirm, `lg` for a form, `2xl`+ only with a
      scroll container
- [ ] **Empty and error states** both designed and both actionable
- [ ] **External links near the need**, never only in a footer
- [ ] **Density** — one rung per role, no fifth size sneaking in
- [ ] **Cards** distinct from parent, never nested
- [ ] **Row-by-row over side-by-side** unless comparison is the actual point
- [ ] **Reduced motion honored** — `prefers-reduced-motion` is handled nowhere in
      `ui/src`, so the `animate-ping` pulse and `animate-pulse` skeletons run
      regardless. Gate them before adding motion

## 12. Mock-first

Any visual change gets a mock round before implementation, constrained to existing
tokens and the four rungs — canonical strings unchanged unless called out as a copy
change.
