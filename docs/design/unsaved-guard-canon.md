# Unsaved-edit guard, save-conflict copy, and the sidebar Settings entry (#460)

Owner-approved mock for issue #460. Strings below are canon, byte-for-byte. `UNSAVED.*` and
`NAV.*` are re-exported from `ui/src/app/lib/unsaved-copy.ts`, itself sourced from
`UNSAVED_GUARD` in `ui/src/app/components/wardyn/copy/shell.ts` — nothing here duplicates them.
`CONFLICT.*` and `PROVIDERS.DISCARD_AND_RELOAD` stay in their existing home,
`ui/src/app/lib/workspace-providers-copy.ts`'s `PROVIDERS`/`PROVIDERS_DRAFT`, and are consumed
directly from there (`saved-elsewhere-banner.tsx`) rather than re-exported a second time: that
file is a ~400-line, route-split copy table, and `unsaved-copy.ts` stays on the console's EAGER
entry path (`use-unsaved-guard.tsx` -> `app-shell.tsx`), so it imports only the lightweight
`shell.ts` (`bundle-split.test.ts`'s entry-chunk budget — the same lesson `model-access.ts`'s own
file-header note already recorded once).

## 1. Frozen strings

| Key | String |
| --- | --- |
| `UNSAVED.DIRTY_CHIP` | Unsaved changes |
| `UNSAVED.TITLE` | Leave without saving? |
| `UNSAVED.BODY` | Your changes on this page haven't been saved. Leaving loses them. |
| `UNSAVED.STAY` | Keep editing |
| `UNSAVED.DISCARD` | Discard changes |
| `CONFLICT.TITLE` | Someone else saved this first |
| `CONFLICT.BODY` | Your copy is out of date, so saving it would overwrite their change. Copy your edits somewhere safe, then reload and redo them. |
| `CONFLICT.COPY_MINE` | Copy my changes |
| `CONFLICT.COPIED_TOAST` | Changes copied |
| `PROVIDERS.DISCARD_AND_RELOAD` | Discard mine and reload |
| `NAV.SETTINGS` | Settings |

## 2. Decisions

- **Q460-1 (sidebar placement).** Settings joins the sidebar as an **admin-only** entry, LAST, under
  a hairline divider. A member's own three-item nav (Runs · Approvals · Workspaces) is unchanged —
  the item is gated on `role !== "member"` in `app-shell.tsx#SidebarNav`, the same file that already
  gates it on identity being resolved (#217).
- **Q460-2 (avatar menu).** The account menu (`top-bar.tsx`) keeps its own Settings entry, for every
  role, unconditionally — the sidebar addition is a second door, not a replacement. Existing muscle
  memory (and a member's only way in) doesn't break.
- **Q460-3 (conflict-copy scope).** "Copy my changes" puts the **WHOLE document** on the clipboard,
  as the editor currently holds it — reversing #217's original "changed fields only" rule
  (`saved-elsewhere-banner.tsx`). A partial copy risked leaving out an edit the admin never noticed
  was cut. A clipboard-write failure falls back to selecting the shown text, so the person can still
  copy it themselves.

## 3. Guarded editors (the dirty chip)

Every admin editor that already tracks a draft vs. an original snapshot shows `UNSAVED.DIRTY_CHIP`
beside its title while they differ:

- **Providers screen** (`providers-screen.tsx`) — the `PageHeader` title, driven by the combined
  Git/Storage draft (they share one document and one Save) or the Agents tab's own draft.
- **Git tab**, **Storage tab** — both chip on that same combined Git/Storage dirty fact, via their
  Segmented tab labels (`permissions.tsx#Segmented`'s new `dirty` option), so an edit made on one tab
  is still visible from the other.
- **Agents tab** (`agents-tab.tsx`) — its own separate resource and draft; chips its own Segmented
  label via an `onDirtyChange` callback to the parent screen.

The per-tab chip is `aria-hidden` — it's a visual echo of a fact the PageHeader chip and the
beside-Save marker already announce, so it never becomes part of a tab button's accessible name.

## 4. The navigation guard

One shared hook (`ui/src/app/lib/use-unsaved-guard.tsx`) and one shared registry
(`ui/src/app/lib/unsaved-registry.ts`):

- `unsaved-registry.ts` is plain module state, not React context — `registerUnsaved`,
  `unsavedSnapshot`, and the `useRegisterUnsaved` hook. A dirty editor registers a `getText` closure;
  `unsavedSnapshot()` is `null` when nothing is registered, otherwise every registered editor's text
  joined by a blank line. A sibling branch (#483, forced reauth) reads the same registry.
- `use-unsaved-guard.tsx`'s `useUnsavedGuard(id, dirty, getText)` registers with it and arms
  `window.beforeunload` while `dirty` — the browser's own native prompt on a tab close/reload, whose
  text the browser controls, not this app.
- The app runs a plain `BrowserRouter` (`main.tsx`), not a data router, so react-router's
  `useBlocker` isn't available. In-app navigation is guarded by intercepting the sidebar link's click
  itself (`useGuardedNavClick`, `app-shell.tsx#SidebarNav`) rather than the router — a blocking
  confirm dialog (`UnsavedGuardProvider`), never an inline banner a click could sail past.

## 5. Save conflict (412)

`saved-elsewhere-banner.tsx`, shared by every screen that PUTs a whole draft with an `If-Match` etag.
Title/body from `PROVIDERS.SAVED_ELSEWHERE_TITLE`/`SAVED_ELSEWHERE_BODY` (= `CONFLICT.TITLE`/`BODY`
above); two buttons, in order — "Copy my changes" (outline, `PROVIDERS_DRAFT.CONFLICT_COPY`) puts the
whole document on the clipboard and toasts `PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST`, then "Discard
mine and reload" (ghost, `PROVIDERS_DRAFT.DISCARD_AND_RELOAD`). No "save over theirs" control exists
— a security document is never last-writer-wins from this banner.
