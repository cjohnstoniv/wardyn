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
- **`DemoDetail` is NOT a component** — it's `React.lazy(() => import("./demos-step"))` in `setup-screen.tsx`; discovery picks it up but it fails `✗ [BUNDLE_EXPORT]` (not on `window.Wardyn`). Excluded via `"componentSrcMap": {"DemoDetail": null}` in config.json (persisted). Any future validate failure on a new `React.lazy` default-export screen: exclude it the same way.
- **`! [FONT_DANGLING] "inter"`** (benign) — the inter body-font woff2 isn't copied into the bundle, so `fonts/fonts.css`'s `@font-face url()` dangles; body text falls back to system sans in preview cards. Cosmetic — Wardyn's mono fonts load at runtime via `runtimeFontPrefixes`. Not a failure.
- **2026-07-19 re-sync** (nigel/"Chaz" account, project `3c913b33`): 189 components. 14 changed, 11 added (incl. `HarnessLoginPane`, `DemoScreen`, `StepList`, `CopyPill`, `WorkspaceLLMCredDialog`), 1 removed (`LlmAccess`). `ModelStep` moved group `setup`→`general` (old path deleted, new uploaded). Uploaded a minimal correct set (changed+added+moved+shared), not all 189 — unchanged components' remote files are byte-identical.
- **2026-08-02 fresh-project sync** (new account, project `da9c8e4e` "Wardyn Mockups"): first sync to this project — NO `--remote` anchor, full upload.
- **`libOverrides` CANNOT host emit.mjs** (tried 2026-08-02, twice-failed): the loader imports the fork in place from `.design-sync/overrides/`, where (a) `esbuild` doesn't resolve (fixable with a node_modules symlink) and (b) `./common.mjs` sibling imports don't exist (not fixable without duplicating the whole lib). The dark patch lives ONLY in the staged `.ds-sync/lib/emit.mjs`; the guard is the pre-flight `grep -c 'class="dark"' .ds-sync/lib/emit.mjs` ≥ 2 before every sync (re-apply the two template edits per the dark-first bullet above if it drops).

## Upload protocol (DesignSync MCP) — the ordering that makes a half-upload safe

1. `finalize_plan` with the exact writes AND deletes from `.resync-verdict.json`'s upload set, `localDir` = `ds-bundle/`.
2. `write_files` in chunks of ≤256 paths per call (MCP hard cap), `localPath` for everything on disk.
3. **Sentinel ordering**: write `_ds_needs_recompile` FIRST (arms the app's recompile), then content (bundle, css, previews, aux), re-arm the sentinel if any later chunk touches the bundle, and write **`_ds_sync.json` LAST** — the anchor is the "sync complete" marker, so a half-upload never reads as complete to the next resync.
4. `delete_files` for removed/moved component paths (from the verdict's deletePaths) before the final anchor write.
- **`CopyButton` — `! [RENDER_BLANK]` (benign, 2026-08-15).** Icon-only copy button; the floor card renders the bare icon correctly on dark (verified screenshot) — a tiny glyph on a dark canvas is under the 5KB PNG heuristic. Same class as WardynMark. Not a failure.
