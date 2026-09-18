# 0.7.x review batch — acceptance and integration boundary

Date: 2026-09-18. Baseline: `dfa89f608fa223b469b0a2435226538f0035d533` (0.7.5).
No main-checkout changes, pushes, tags, migrations, 0.8 work or release decisions.

## Frozen first batch

`review/0.7x-integration` at `0d63ab2e3ade4302b9f51b08146965cf21655c46`
contains the 15 independent patches listed in PATCH-LEDGER.md, plus the
selection-specific audit-citation repair R022. Each original patch has the
baseline as its only parent and is signed off. Independent composition review
matched each original patch ID to its combined copy. The 26-file combined diff
contains no evidence, local mock, dependency or schema changes.

| Check | Outcome | Evidence |
| --- | --- | --- |
| Uninterrupted make ci | Exit 0; integration worktree clean afterward | evidence/integration-ci-final.log |
| Default, Docker-tagged and Kubernetes-tagged Go reports/races | Passed within make ci; 17 required probe parents passed and coverage met the gate | Same CI log and integration test/reports/go/ |
| UI unit suite | 155 files / 2,880 tests passed | Same CI log |
| Staticcheck, dependency scans, notices, guards, DCO, deployment manifests and remaining CI targets | Passed within make ci | Same CI log |
| Real PostgreSQL report and broker/store race tests | Exit 0; 69.5% coverage and all 9 required PG probe parents passed | evidence/integration-pg-final.log |
| Official UI typecheck and default/editor tsc | Both exit 0 at the frozen SHA | evidence/integration-ui-typecheck.log |
| Complete browser suite at the same SHA in a separate worktree | Exit 0; 30 spec files / 347 tests, zero failures/skips/flakes; clean worktree and test ports released | evidence/integration-ui-e2e.log |

The PostgreSQL report contained 5,508 passed and five skipped test events,
including subtests; mandatory PG probe parents were not skipped. This is not
5,508 independent end-to-end scenarios. UI unit coverage was 94.91% statements,
90.25% branches and 82.35% functions. Coverage is a gate, not proof of correctness.

Govulncheck reported zero affected symbols and zero imported-package findings
in all three shipped build-tag scans. A verbose follow-up identified the one
module-only finding as [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), for
unused x/crypto/openpgp in required x/crypto v0.56.0, with no fixed version
listed. No dependency upgrade is included or claimed necessary from this result.

Captured log hashes:

- make ci: `dbcbff0ff04d68e604f92a2cf3d875f40a8e29e48b229219e97fe3a6b6a0f080`.
- PG: `a7ca5f2608fd43c1f4da7102cb5f238acd241d31164f3b6946b6b8657041d069`.
- UI typechecks: `d558c9fdafba84c82bff3c85af850db40400371a196c873ea97b9ae61a52caec`.
- Browser suite: `417323b40695ada1fb624f08f5db077937630bc09d4c8efd95c00b4b816142a2`.
- Verbose vulnerability scan: `873f36c3679897ff26b4f856a49deba0a921bf037225161031eee1ef86349580`.

## Separate post-freeze recording patch

`18fb91fd` / `review/0.7x-recording-root-read` is the sixteenth independent patch.
It is NOT present at `0d63ab2e`, and none of the first batch's combined checks
should be attributed to it. It has a red-first synthetic regression, complete
recording-package race pass, conformance/vet/size checks and independent peer
review. Full default-tree Go tests passed (exit 0), including daemon guard tests;
the patch worktree stayed clean. Its complete make-ci release-combination gate
has not been run. Detailed preconditions,
compatibility and residual limits are in RECORDING-ROOT-READ.md.

## Environment and scope of the evidence

The worktrees use frozen pnpm dependencies and the repository's Go toolchain.
`GOFLAGS=-buildvcs=false` avoids an environment-specific nested `/tmp/.git`
discovery failure; actual source SHAs are recorded explicitly. Tests use isolated
Go caches, the review-owned PostgreSQL container at loopback 58432, and explicit
`DOCKER_HOST=unix:///var/run/docker.sock` rather than the other local daemon.
Browser acceptance uses database `wardyn_review_combined` and ports 18892/18893.
Other agents' databases, clusters and product worktrees are not test fixtures.

The earlier scoped Kubernetes eviction/exit checks, synthetic restore rehearsal,
two individual browser sweeps and real CLI support-bundle collection are separate
pieces of evidence; their precise limits are retained in the ledger and linked
reports. In particular, the scoped Kubernetes run did not establish healthy
proxy/recording or full API-finalization behavior. There is no claim of real
Entra/AWS/private-endpoint SSO, full application/PVC/user-drive disaster recovery,
production role separation, or fresh/upgrade Helm acceptance from these tests.

## Release-owner handoff

Review and select the individual commits from PATCH-LEDGER.md. Do not cherry-pick
the integration umbrella on top of those originals. R022 is a citation repair for
one exact docs selection, not a universally applicable base patch; repoint
guarded references after merging the selected docs with the other 0.7.6 work.
Do not remove or weaken the citation guards. Re-run applicable gates on the actual
release combination. No patch is assigned a release number by this campaign.

R009 intentionally changes oversized brokered-upload handling from truncated
success to HTTP 413. R017 omits malformed/multi-document Compose configuration
from support bundles, with secret detection still heuristic. R023 intentionally
refuses absolute/out-of-root recording symlinks. These are compatibility-relevant
security/correctness fixes, not new feature surfaces. Ordinary reverts need no
data migration; reverting a security fix reintroduces the original exposure.
