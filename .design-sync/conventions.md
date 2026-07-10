# Wardyn Design System — how to build with it

Wardyn is a **dark-first security console**: near-black surfaces, a teal primary, and semantic status colors. Build UI from the components in this library; style your own layout glue with the Tailwind-v4 utilities below, never ad-hoc hex or inline colors.

## Wrapping & setup

Wrap the app (or any previewed screen) in **`ThemeProvider`** from the library. It puts the `.dark` class on the document root and defaults to dark ("dark-first"); every color token below is defined for both `:root` (light) and `.dark`. Without the provider you get the light palette and the theme toggle won't work.

```tsx
import { ThemeProvider, Button, Chip } from '<pkg>';

<ThemeProvider>
  <div className="min-h-screen bg-background text-foreground">
    …your screen…
  </div>
</ThemeProvider>
```

Colors come from CSS custom properties (`--background`, `--primary`, …); you address them through the Tailwind utilities below, not by reading the variables directly.

## The styling idiom (Tailwind v4 utilities bound to semantic tokens)

Never write raw colors. Use the semantic utilities — they carry the design language and flip correctly between light and dark:

| Surface / text | Utility |
|---|---|
| Page / card / raised surface | `bg-background`, `bg-card`, `bg-surface-2`, `bg-popover` |
| Primary text / dimmed text | `text-foreground`, `text-muted-foreground` |
| Hairline / strong border | `border-border`, `border-border-strong` |
| Teal accent (CTAs, selected) | `bg-primary text-primary-foreground`, `ring-ring` |

Status tones (each has a base, a `-foreground`, and a `-subtle` fill) — use for chips, badges, and inline state:
`text-success` · `text-warning` · `text-danger` · `text-info` · `text-cyan` (e.g. `bg-danger-subtle text-danger`, `bg-success-subtle text-success`).

Barrier-tier colors (the Fence / Wall / Vault identity): `bg-fence-bg text-fence-fg border-fence-border`, and the `vault-*` equivalents.

Layout is plain Tailwind: `flex`, `grid`, `gap-*`, `p-*`, `rounded-*` (radius token), `size-*`. Prefer library components over raw elements — `Button` (variants: default/outline/ghost), `Chip` (`tone="success|warning|danger|info|cyan|primary|neutral"`, optional `dot`), `OptionCard`, `Table`, `Dialog`, `Sheet`, `DropdownMenu`, `Tabs`, `Input`, `Select`, `Checkbox`, `Switch` — instead of restyling native controls. Raised panels use a `bg-card`/`bg-surface-2` div with `border-border rounded-xl`.

## Where the truth lives

- **`styles.css`** (and its `@import` closure — `_ds_bundle.css`, `fonts/`) carries the compiled tokens + utilities. Read it before inventing a class; if a utility isn't there, it isn't in the system.
- **`<Name>.d.ts`** — each component's real prop contract. **`<Name>.prompt.md`** — its usage notes. Read the component's own files before composing it.
- The brand font is **Inter** (shipped); monospace/terminal views use a mono stack.

## One idiomatic snippet

```tsx
import { ThemeProvider, Chip, Button } from '<pkg>';

<ThemeProvider>
  <section className="max-w-[780px] rounded-xl border border-border bg-card p-6 text-foreground">
    <h2 className="text-lg font-semibold tracking-tight">This host right now</h2>
    <div className="mt-3 flex flex-wrap gap-2">
      <Chip tone="success" dot>Barrier: Fence ready</Chip>
      <Chip tone="warning">Model: needs setup</Chip>
    </div>
    <div className="mt-6 flex items-center gap-2.5">
      <Button>Get set up</Button>
      <Button variant="outline">Skip to the console</Button>
    </div>
  </section>
</ThemeProvider>
```
