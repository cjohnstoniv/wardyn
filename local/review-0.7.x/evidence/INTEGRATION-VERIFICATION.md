# Frozen 0.7.x integration verification

Date: 2026-09-18. Verified tree: `0d63ab2e3ade4302b9f51b08146965cf21655c46`, branch `review/0.7x-integration`, worktree `/tmp/wardyn-review-0.7x-integration`. Base: `dfa89f608fa223b469b0a2435226538f0035d533`. The main checkout was not edited. No push, release, or version bump was performed.

## Results on the frozen tree

- One uninterrupted `make ci`: exit 0. Full log: `integration-ci-final.log`.
- `make test-report-pg test-race-pg`: exit 0, after `make ci`. Full log: `integration-pg-final.log`. Dedicated review fixture `wardyn-review-07x-pg`, endpoint `localhost:58432`, database `wardyn_review_pg_gate`; `WARDYN_TEST_PG_SUPERUSER=1`.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 -show verbose ./...`: exit 0. Detail: `integration-govuln-verbose.log`.
- Worktree remained clean after the gates. `git diff --check dfa89f60..HEAD` and notice artifacts versus HEAD both passed.

All commands used `GOFLAGS=-buildvcs=false`, `GOMAXPROCS=4`, and the shared review caches under `/tmp/wardyn-review-cache/{go-build,go-tmp,go-path,go-mod}`. Docker-aware commands used `DOCKER_HOST=unix:///var/run/docker.sock`. Standard Makefile gate semantics were preserved, including normal Go test caching; no test selection or guard was weakened.

| Go report suite | Passed test events | Skipped test events | Failed test events | Report coverage |
| --- | ---: | ---: | ---: | ---: |
| Tagless | 7678 | 315 | 0 | 78.9% |
| Docker-tagged | 8089 | 337 | 0 | 78.5% |
| Kubernetes-tagged | 7884 | 316 | 0 | 78.9% |
| PostgreSQL | 5508 | 5 | 0 | 69.5% |

Counts include subtests; the overlapping suites must not be added as distinct tests. Go union coverage was **78.4%**, above the unchanged **78%** floor. All 17 tagless required probe parents and all nine PostgreSQL `TestPG_ProbeF11_` parents passed. Tagless, Docker-tagged, and Kubernetes-tagged race passes passed. The explicit PostgreSQL race gate passed with broker 2.684s and store 22.737s.

UI unit/component coverage: **155 files, 2880 tests passed**, 0 failed; statements/lines 94.91%, branches 90.25%, functions 82.35%. UI typecheck, live-spec loading, production build, and the bundle-size regression test passed. Browser acceptance was a separate agent/lane: its same-commit report is `integration-ui-acceptance.md` with `integration-ui-e2e.log` (30 specs / 347 tests passed; no failures, skips, or flakes). That result is separate from, not covered by, `make ci`.

Other merge gates passed: all three Go builds and vets, tidy, golangci-lint (0 issues), size/image-pin gates, shell regressions, staticcheck for all three builds, govulncheck for all three builds, license headers and dependency licenses, generated notices (224 entries), gitleaks full history (3308 commits, no leaks found), Helm lint/render, compose validation, DCO, diagrams (7 blocks), production npm licenses (91 entries), npm production audit, and stub conformance.

## Qualifications and residuals

- The daemon-free gate does not prove live Docker/Kubernetes confinement, SSH/SSO end-to-end behavior, image scans/signing, or release assets. Stub conformance intentionally skips substrate assertions. Earlier dedicated-kind lifecycle evidence has its own exact commit and limitations; it is not final-tip cluster acceptance.
- The PostgreSQL suite's five skips were two Docker-required real-botocore SSO tests, `TestDriveSubstrateSectionsUseTheirOwnShape` (both current backends use the same shape), `TestInstallSh_ComposeFetchIsVerified` (existing accepted risk: compose integrity), and `TestThreatModelDocRootlessRefusalOwner` (existing probe-based condition). No required PostgreSQL probe skipped.
- Govulncheck reports **GO-2026-5932** in required module `golang.org/x/crypto@v0.56.0`: `golang.org/x/crypto/openpgp` is unmaintained and unsafe; fixed version **N/A**. The scanner found **0 affected symbols and 0 affected imported packages** in all three shipped build scans; the tagless verbose log identifies the advisory. This is a scanner reachability assessment, not a claim that the module has no advisories. No dependency upgrade was made.
- UI tests emit existing environment/React warnings (`getContext`, `window.open`, `act`, and the separate `SheetOverlay` ref wrapper); the production build warns about a >500 kB chunk. These do not fail the current gates. `SheetOverlay` is outside the selected Dialog/AlertDialog overlay fix and remains a separate follow-up.
- The later filesystem-recording rooted-read patch is independent and **not present** in this frozen integration tree. Its tests require separate attribution.

## Integration repair and dependencies

The initial notices drift came from running the generator without UI dependencies. Frozen `pnpm install --frozen-lockfile` followed by the existing notices generator restored exact HEAD artifacts; no notice-content patch was needed.

The earlier full-gate attempt on `43aa1dc113a815dd5f4f90e7bf870afeee6ac613` failed the real audit-document citation guard after SSH/operations docs moved the referenced lines. Its log and exact failures are preserved as `integration-ci-43aa1dc1-failed.log` and `integration-ci-43aa1dc1-failure-details.txt`.

Signed citation-only repair `5adf090cd828097c47cc07ce7fbfe001191054e4` lives on `review/0.7x-doc-integration-citations`, applied to integration as `4640554c343bef155ddb42b6a9b7fb9be26bd285`. It re-points seven citation groups in five rows without removing evidence or weakening the guard. It explicitly depends on the combined selection of SSH revocation `8da1885b`, audit-spool recovery `b2042f52`, age-key startup `7d155903`, and desktop/SSH masking `baa4611e`, atop the eight-patch `a6c47ae5` integration. Its exact line offsets are **not** a standalone companion for every possible patch subset. Original docs commits do not gain a standalone full-gate claim.

The final integration additionally includes Dialog/AlertDialog overlay `9ca33584`, support-bundle redaction `c4c5fa16`, and explicit test cleanup import `94c4fa84`. Original independent patch commits remain available; the integration branch is the tested combined selection, not an instruction to merge all patches into one release.

## Evidence integrity

SHA-256 values at completion:

- `integration-ci-final.log`: `dbcbff0ff04d68e604f92a2cf3d875f40a8e29e48b229219e97fe3a6b6a0f080`
- `integration-pg-final.log`: `a7ca5f2608fd43c1f4da7102cb5f238acd241d31164f3b6946b6b8657041d069`
- `integration-govuln-verbose.log`: `873f36c3679897ff26b4f856a49deba0a921bf037225161031eee1ef86349580`

Detailed generated reports remain in the integration worktree's ignored `test/reports/go/{unit,docker,k8s,pg,union}` and `test/reports/ui` directories. Earlier `integration-pg.log` was preserved unchanged.
