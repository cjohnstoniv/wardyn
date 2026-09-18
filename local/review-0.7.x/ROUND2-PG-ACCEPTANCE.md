# Round-two PostgreSQL acceptance

Status: **PASS**, 2026-09-18. Root owned the only gate process (session `13289`)
and confirmed its final exit status was zero. The security-review lane
independently inspected the completed report events, required probes, skips,
log hash, and product-worktree state.

## Frozen revision and isolation

- Revision: `6a3dc8659ea7105ae81d127b5f3f1a198333f67d`.
- Worktree: `/tmp/wardyn-review-0.7x-round2-pg`.
- Branch: `review/0.7x-round2-pg`.
- Product worktree was clean before launch and after completion; exact SHA is unchanged.
- Command: `make test-report-pg test-race-pg`.
- Log: `evidence/round2-pg-final.log`; its first line records the exact revision.
- Generated report: `/tmp/wardyn-review-0.7x-round2-pg/test/reports/go/pg/`.

The database is the reserved local test database `wardyn_review_pg_gate`, not
an operator or production database. It is separate from the browser databases
but shares the review PostgreSQL server/container `wardyn-review-07x-pg` on
`localhost:58432`. This is database isolation, not a separate server; role probes
can exercise cluster-level role creation and permissions. Root's read-only
precheck returned `wardyn_review_pg_gate|wardyn|t|t` for database, role,
superuser, and create-role capability.

## Execution environment

Root confirmed the following exact environment against its launch command:

```text
WARDYN_TEST_PG=postgres://wardyn:wardyn@localhost:58432/wardyn_review_pg_gate?sslmode=disable
WARDYN_TEST_PG_SUPERUSER=1
DOCKER_HOST=unix:///var/run/docker.sock
GOFLAGS=-buildvcs=false
GOCACHE=/tmp/wardyn-review-cache/go-build
GOTMPDIR=/tmp/wardyn-review-cache/go-tmp
GOPATH=/tmp/wardyn-review-cache/go-path
GOMODCACHE=/tmp/wardyn-review-cache/go-mod
GOMAXPROCS=2
```

The launch shell used `set -euo pipefail` and piped a command group containing
`git rev-parse HEAD` followed by the environment-prefixed Make command into
`tee` at the log's absolute path. Thus a test-target failure is not hidden by a
successful logging process. The fixture precheck exited zero. An earlier
reviewer-side read-only precheck stalled at approval and was aborted; it did not
write to the fixture or launch any tests.

`test-report-pg` runs the repository's coverage/report script with `-p 1` over
the store, database, secretstore, broker, API, API integration, recording, and
daemon packages. Package serialization is retained because these tests mutate
shared test-database state. The report's required-pass floor is the repository
default `^TestPG_ProbeF11_`, expected to execute all nine top-level probes.

`test-race-pg` runs the official `go test -race -p 1 -count=1 -run 'TestPG_'`
command for the broker and store packages. This is not the entire Go tree under
the race detector.

## Results

The official combined Make command exited **0**. The completed JSON event
stream contains 26,351 events. Counts below count terminal `pass`/`skip`/`fail`
events, separating package outcomes from named tests and subtests:

| Scope | Passed | Skipped | Failed |
| --- | ---: | ---: | ---: |
| Packages | 11 | 0 | 0 |
| Top-level tests | 2,245 | 5 | 0 |
| Tests including subtests | 5,525 | 5 | 0 |

Reported module-wide instrumented coverage for the selected package suite was
**69.6%**. This is not coverage from running the full Go test tree.

The required-pass floor reports nine passing probes. Independent inspection of
their actual top-level test events confirmed all nine passed, none skipped:

- `internal/store`: `TestPG_ProbeF11_RewrittenRowReportsExactSeq`.
- `internal/store`: `TestPG_ProbeF11_SplicedOutRowReportsSuccessorSeq`.
- `internal/store`: `TestPG_ProbeF11_UnchainedRowAfterGenesisIsNotClean`.
- `internal/store`: `TestPG_ProbeF11_UnlockedWriterDoesNotForkChain`.
- `internal/store`: `TestPG_ProbeF11_StoreLaneCannotSilentlySelfSkip`.
- `internal/db`: `TestPG_ProbeF11_LaneCannotSilentlySelfSkip`.
- `internal/db`: `TestPG_ProbeF11_AuditDDLProtected`.
- `internal/db`: `TestPG_ProbeF11_DroppedChainTriggerIsRestoredByMigrate`.
- `internal/api`: `TestPG_ProbeF11_SpoolReplayCannotForgeChain`.

All five skipped tests were top-level tests, not PostgreSQL-fixture skips:

| Package / test | Reported skip boundary |
| --- | --- |
| `internal/api` / `TestAWSSSOConfigAcceptedByRealBotocore` | Requires explicit `WARDYN_TEST_DOCKER=1`; the PG lane does not enable this Docker end-to-end test. |
| `internal/api` / `TestAWSSSOConfigAcceptedByRealBotocore_MultiAccountPinned` | Same explicit Docker gate. |
| `cmd/wardynd` / `TestDriveSubstrateSectionsUseTheirOwnShape` | Both backends currently use the same object-name shape, so the difference guard has nothing to compare. |
| `cmd/wardynd` / `TestInstallSh_ComposeFetchIsVerified` | Existing accepted Compose-integrity residual; the opt-in `F10_EXPECT_COMPOSE_INTEGRITY=1` enforcement was not enabled. This gate does not validate that residual away. |
| `cmd/wardynd` / `TestThreatModelDocRootlessRefusalOwner` | Existing conditional guard skips because `classToRuntime` now probes rootlessness and the documentation claim may be current. |

Official PostgreSQL race target results: broker **PASS (2.764s)**, store
**PASS (16.772s)**. No race warning or package failure appears in the completed
gate log. The race command is not JSON-formatted, so this report does not invent
per-test race counts.

Final checks reconfirmed `HEAD` equals the frozen SHA, `git status --porcelain`
is empty, and `git diff --check` passes. Generated ignored test reports remain
in the isolated acceptance worktree.

## Evidence integrity

SHA-256 hashes after process completion:

```text
aea81e648723653464a909d301759cdb8ad1063e87b4453520c7976d4144fcf2  evidence/round2-pg-final.log
8e8110df24689c5c79d10e35800698334f4797d2e7b0761761285f3cedc9f05a  test/reports/go/pg/test-output.json
ff07b707001bb84b5006ff233bfe2453035a0712449bcebdcf7251f983a1d011  test/reports/go/pg/cover.out
```

The last two paths are relative to the isolated acceptance worktree; the log
path is relative to this report's ledger directory.

This record does not claim a full `make ci` or browser run; those are separate
acceptance lanes. No product source changes were made for this gate.
