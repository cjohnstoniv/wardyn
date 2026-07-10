# Wardyn Design System — sync notes

Repo-specific gotchas for `/design-sync`. Append one bullet per lesson.

## Build setup (package shape, synth-entry)

- **`@wardyn/ui` is an app, not a library** — no `exports`/`module` in `ui/package.json`. The converter runs in **synth-entry mode** (`[NO_DIST]`), re-exporting PascalCase components from source. Build with **no `--entry`**; point `--node-modules` at `ui/node_modules` (holds the `@wardyn/ui` self-symlink → `../..` and `react`).
- **`srcDir: "src/app/components"`** is required. Without it, discovery's default "keep every `.tsx`" pulls in `src/main.tsx` (the app entry), which does `import "./styles/index.css"` → `@import 'tailwindcss'` (Tailwind v4 source) → esbuild `Could not resolve "tailwindcss"`. Scoping to the components dir excludes `main.tsx`/`app.tsx`. All real components live under `src/app/components/**`.
- **`cssEntry` must be the COMPILED stylesheet, never the source.** `src/styles/index.css` imports Tailwind v4 source (`@import 'tailwindcss'`), which esbuild can't compile. Use the Vite-built CSS. Because Vite content-hashes it, we copy it to a stable path before building:
  ```
  cp "$(ls -S ui/dist/assets/index-*.css | head -1)" ui/dist/ds-styles.css
  ```
  and set `cssEntry: "dist/ds-styles.css"`. **Re-sync step: rebuild the UI (`cd ui && pnpm build`), re-copy ds-styles.css, THEN run the converter** — otherwise the DS ships stale/placeholder CSS.
- `provider: {component: "ThemeProvider"}` — components read theme context from `wardyn/theme-provider.tsx`.
- Render check needs Playwright matching the cached chromium build (`~/.cache/ms-playwright/chromium-1228` → **playwright 1.61.1**). Install into `.ds-sync` with `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1` (browser already cached from the MCP Playwright).

## Known render warns

- **`WardynMark` — `[RENDER_THIN]` (benign).** It's the SVG shield logo mark; it has no text and paints as a small vector, which trips the text/paint heuristic. The authored preview renders it at `size-16 text-primary` and it looks correct in the screenshot. Not a failure — a logo mark legitimately has no text.

## Dark-first card rendering (emit.mjs override)

- Wardyn is dark-first; `ThemeProvider` applies `.dark` via a post-paint `useEffect`, so preview cards captured light by default. Fixed by forking `lib/emit.mjs` (durable copy in `.design-sync/overrides/emit.mjs`, declared in `cfg.libOverrides`): both card `<html>` templates get `class="dark"` and `body{background:var(--background);color:var(--foreground)}`. **If a re-sync renders cards light again**, the override didn't load — re-apply those two edits to the staged `.ds-sync/lib/emit.mjs` (change `<html>`→`<html class="dark">` and `background:#fff`→`background:var(--background,#0a0a0a);color:var(--foreground,#e5e5e5)` in the two preview templates).
- Switching to dark flipped which empty controls render "blank" — `Input`, `Textarea`, `StatusChip` needed authored previews (empty controls are invisible on dark). All authored under `previews/`.

## Re-sync risks

- `ui/dist/ds-styles.css` is a build artifact copied by hand; a re-sync that forgets the copy step ships stale CSS. (See build-setup bullet.)
- Component count grows with the UI (179 as of this sync, up from a prior 154). New components appear automatically via synth-entry discovery.
- First sync shipped **floor cards for all components** (no authored previews yet). Authored previews can be added incrementally on any later re-sync — graded files carry forward.
