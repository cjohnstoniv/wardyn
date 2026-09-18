# Round 2 — same-tip UI acceptance

Frozen source commit: `6a3dc8659ea7105ae81d127b5f3f1a198333f67d`.
Worktree: `/tmp/wardyn-review-0.7x-round2-ui`.
Branch: `review/0.7x-round2-ui`.

This lane performs acceptance only. No product, test, dependency-manifest or
lockfile edits are authorized. The separate integration lane owns `make ci`.

## Isolation

- Initial `git status --short` was empty and `git rev-parse HEAD` matched the
  frozen commit above.
- `pnpm install --offline --frozen-lockfile` completed successfully with 532
  packages; resolution was skipped because the lockfile was current.
- Review-owned PostgreSQL container: `wardyn-review-07x-pg`, explicitly through
  `DOCKER_HOST=unix:///var/run/docker.sock`, host port `localhost:58432`.
- Dedicated database: `wardyn_review_round2`. No other database is selected.
- Backend/UI listener ports: `18896` / `18897`. Root verified both free before
  this lane started and independently confirmed no listeners remained on
  either port after the successful run exited.

## Gates

- Official `pnpm typecheck` and default/editor `pnpm exec tsc --noEmit`: both
  passed, exit 0.
  Evidence: `evidence/round2-ui-typecheck.log`.
- Complete canonical `scripts/run-ui-e2e.sh`, with no positional spec filter:
  passed, exit 0 from original unified execution session 49178. All 347 tests
  in 30 spec files passed; 0 failed, 0 skipped and 0 flaky. One complete run,
  with no rerun, spec exclusion or permission stall.
  Evidence: `evidence/round2-ui-e2e.log`.
- Final `git status --short` was empty and `git rev-parse HEAD` still returned
  `6a3dc8659ea7105ae81d127b5f3f1a198333f67d`. Acceptance introduced no source,
  manifest or lockfile changes. Root's post-run listener-cleanup check passed.

Evidence SHA-256 after the successful commands exited:

```text
8c3ffd074da4a1e0fd6fb051e6cb7fe7d5b6b6e8b3a3fd8a3b31255351916bc9  evidence/round2-ui-typecheck.log
d6b617f48eb4c1e9ab30be2134e67811be21050e34af6a259ecabb268c2698b6  evidence/round2-ui-e2e.log
```

The browser runner builds the backend and UI, then reseeds the dedicated
database for each spec file. It is the hermetic runner-none acceptance path,
not a live-provider, Kubernetes or production-deployment exercise. No result
is inferred from the earlier independent Sheet worktree's successful run.

## Reproduction

From the worktree's `ui/` directory:

```sh
pnpm install --offline --frozen-lockfile
pnpm typecheck
pnpm exec tsc --noEmit
```

From the worktree root:

```sh
set -o pipefail
GOFLAGS=-buildvcs=false \
GOCACHE=/tmp/wardyn-review-cache/go-build \
GOTMPDIR=/tmp/wardyn-review-cache/go-tmp \
GOPATH=/tmp/wardyn-review-cache/go-path \
GOMODCACHE=/tmp/wardyn-review-cache/go-mod \
GOMAXPROCS=4 \
DOCKER_HOST=unix:///var/run/docker.sock \
WARDYN_E2E_PG_CONTAINER=wardyn-review-07x-pg \
WARDYN_E2E_PG_HOSTPORT=localhost:58432 \
WARDYN_E2E_PG_DBNAME=wardyn_review_round2 \
WARDYN_E2E_ADDR=:18896 \
WARDYN_E2E_UI_ADDR=127.0.0.1:18897 \
scripts/run-ui-e2e.sh 2>&1 | tee /tmp/wardyn-review-0.7x-ledger/local/review-0.7.x/evidence/round2-ui-e2e.log
```

`GOFLAGS=-buildvcs=false` is the recorded workaround for Git discovery outside
the main checkout during temporary-worktree test binary builds. It does not
change the tested source commit.
