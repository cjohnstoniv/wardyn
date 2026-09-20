# Keeping the design system in sync

Wardyn's component library lives in `ui/src/app/components`. A browsable copy of it
lives in a claude.ai design-system project, so a design round can be reviewed without
running the app.

The two are kept in step by the `/design-sync` skill. This page is what a person needs
to pick that up on an account that has never done it before.

## What is in the repo, and what is not

| | Where | Tracked | Why |
|---|---|---|---|
| Build configuration | `.design-sync/config.json` | yes | Same for every account. |
| The gotchas | `.design-sync/NOTES.md` | yes | Hard-won; re-deriving it costs a day. |
| Library conventions | `.design-sync/conventions.md` | yes | Becomes the project's README header. |
| Authored previews | `.design-sync/previews/*.tsx` | yes | Components that render blank without one. |
| Dark-card override | `scripts/design-sync-apply-overrides.sh` | yes | See below. |
| Project id | `.design-sync/project.local.json` | **no** | Belongs to one account. |
| Render cache and grades | `.design-sync/.cache/` | **no** | Machine state. |
| Staged tooling | `.ds-sync/` | **no** | The skill stages it; 46 MB; regenerable. |
| Built bundle | `ds-bundle/` | **no** | Build output. |

The split is the point: **everything an account needs is tracked, and everything an
account cannot share is not.** A project id hardcoded in a tracked file sends the next
person's sync at a project they cannot open.

## First run on a new account

1. `/design-login` in the session. The design tool needs its own authorization even
   when the session is already signed in.
2. List the projects that account can write to. A brand-new account has none.
3. Create a design-system project, or target an existing one. The type is fixed at
   creation — pushing to a regular project never turns it into a design system.
4. Write the id where the repo expects it, and nowhere else:

   ```bash
   printf '{ "projectId": "<uuid>" }\n' > .design-sync/project.local.json
   ```

5. Run the sync below.

Expect to redo steps 2-4 whenever the account changes. That is normal, and it is why
the id is a single local file rather than a value threaded through the config.

## Syncing

```bash
cd ui && pnpm build && cd ..                                   # 1
cp "$(ls -S ui/dist/assets/index-*.css | head -1)" ui/dist/ds-styles.css   # 2
bash scripts/design-sync-apply-overrides.sh                    # 3
node .ds-sync/resync.mjs --config .design-sync/config.json \
  --node-modules ui/node_modules --out ./ds-bundle             # 4
```

1. The design system ships the **compiled** stylesheet. Skipping the build ships the
   previous one.
2. Vite content-hashes its CSS, so it is copied to a stable name the config can point at.
   A missing `cssEntry` only warns, and the result is an unstyled bundle.
3. Wardyn is dark-first, and the theme class is applied after paint, so card templates
   capture light unless patched. `.ds-sync/` is restaged often and is not tracked, so
   this is re-applied every time rather than kept as an edit. It is idempotent, it
   touches only the two card templates, and it fails loudly if the library's shape
   changed underneath it.
4. Never pipe this to `tail` — a build crash prints to the pipe and vanishes.

Then grade the previews, `finalize_plan` against the bundle, and `write_files`.
`.design-sync/NOTES.md` carries the rest, including the chunk limits and the order the
sentinel files have to be written in.

## Prototypes are not screenshots

A mock that exists to settle a flow is a **prototype you can click through**, not a
picture of one screen. It carries the real states, the real copy, and the transitions
between steps, so the review answers "does this process work" rather than "does this
screen look right".

A single screen with no flow may be a static mock. Anything with steps, a decision, an
error path or a wait state is built as a prototype and shared as a link the reviewer
can drive.

The prototype's strings are canon: whatever it says is what the app says, byte for byte.
That is what makes the review binding rather than advisory.
