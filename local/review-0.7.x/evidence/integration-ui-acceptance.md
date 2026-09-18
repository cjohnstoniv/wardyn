# Combined UI acceptance

Frozen integration commit: `0d63ab2e3ade4302b9f51b08146965cf21655c46`.
Acceptance branch: `review/0.7x-acceptance-ui`.
Acceptance worktree: `/tmp/wardyn-review-0.7x-acceptance-ui`.

This worktree is separate from the individual patch branches and from the
integration `make ci` worktree. It has its own installed dependencies and build
outputs. No production edits were made during acceptance.

- `pnpm install --frozen-lockfile`: exit 0; no tracked changes.
- `pnpm typecheck`: exit 0.
- `pnpm exec tsc --noEmit`: exit 0.
- Typecheck transcript: `integration-ui-typecheck.log` (includes exact SHA).
- Full `scripts/run-ui-e2e.sh`: exit 0; all 30 spec files and 347 tests passed,
  zero failures, skips, or flakes. Transcript `integration-ui-e2e.log` includes
  exact SHA and all build/spec output.
- The acceptance worktree remained clean after all checks.

The browser run uses Docker at `unix:///var/run/docker.sock`, the existing
`wardyn-review-07x-pg` container on `localhost:58432`, and its own
`wardyn_review_combined` database. Backend/UI-sandbox ports are 18892/18893;
both were checked unused before launch. The configured Go build/module/temp
caches are under `/tmp/wardyn-review-cache`; `GOFLAGS=-buildvcs=false` and
`GOMAXPROCS=4` were set explicitly.

An earlier execution request was interrupted while awaiting approval. It had
not created the log or binaries and neither port was occupied. The subsequent
approved run is the only browser acceptance execution; no server was duplicated.

Scope: the canonical hermetic Chromium console suite against the none runner,
not a new live SSO or live Kubernetes acceptance run. Those separate gates and
the broad merge gate are owned by the integration lane.

The later standalone recording-root-read patch `18fb91fd` is not part of the
frozen commit above and is not covered by this acceptance result.
