# Round-two final combined merge gate

Verified 2026-09-18 by the lifecycle review lane. **One uninterrupted `make ci` passed, process exit 0**, at exact frozen commit `6a3dc8659ea7105ae81d127b5f3f1a198333f67d`.

- Worktree: `/tmp/wardyn-review-0.7x-round2-final`
- Branch: `review/0.7x-round2-final`
- Full log: [round2-ci-final.log](round2-ci-final.log)
- Log SHA-256: `ed717e2b34afcffaaa5e4b37b7688ea7c43a3959392aecf2c87ae7c75209db31`
- Clean before and after. `git diff --check 0d63ab2e..HEAD` passed. No source fixes, reduced scope, or gate retries occurred during this run.
- Frozen UI dependencies were installed before the gate; its own frozen installs remained up to date. Notices remained unchanged at 224 entries.
- Earlier worktrees independently rechecked clean and unchanged at `cd179c617acee6dd14ba6bc273db44569310355b` and `0d63ab2e3ade4302b9f51b08146965cf21655c46`.

## Selection

The original first-round `0d63ab2e` batch plus these independently authored patches:

| Finding | Original patch | Integrated commit |
| --- | --- | --- |
| R023 recording-root reads | `18fb91fdb6a3e656eed2a922ea73ddd22998ef3f` | `cd179c617acee6dd14ba6bc273db44569310355b` |
| R025 private export temporary files | `2307b07c57443776baddf1b9926c04f9c27c2587` | `a4dd89d6d8236f9a06ece0634f838c6824a8024a` |
| R026 single policy document | `f5fbb1669175074fc9e418a744b2c8cf9ed21054` | `9c0b644b006b5010a8576da2f7f78917a0b01dab` |
| R027 demand-driven recording upload masking | `0bb4699b9de6f462552ddc031393ea875b437928` | `72bd3e35af6e34f0d90c3e73c9c706425bf937f7` |
| R024 SheetOverlay ref | `f6e787a5d37a2efac00881923865df06ed5eba53` | `6a3dc8659ea7105ae81d127b5f3f1a198333f67d` |

These are combined-tip results, not standalone full-gate claims for each original commit. Select originals for release integration, not an umbrella merge that duplicates previously selected patches.

## Command and results

Run from the final worktree with Bash `pipefail`, teeing the full output into the log above:

```sh
GOFLAGS=-buildvcs=false \
GOCACHE=/tmp/wardyn-review-cache/go-build \
GOTMPDIR=/tmp/wardyn-review-cache/go-tmp \
GOPATH=/tmp/wardyn-review-cache/go-path \
GOMODCACHE=/tmp/wardyn-review-cache/go-mod \
GOMAXPROCS=4 DOCKER_HOST=unix:///var/run/docker.sock make ci
```

- Default, Docker-tagged and Kubernetes-tagged builds, vet and detailed test reports passed. All 17 required probe parents passed. Coverage: default 78.9%, Docker 78.5%, Kubernetes 78.9%; **union 78.5%**, above the 78% floor.
- JSON report terminal events: default **7,715 pass / 315 skip / 0 fail**, Docker **8,126 / 337 / 0**, Kubernetes **7,921 / 316 / 0**. Counts include parent tests and subtests; they are not distinct top-level cases. Conditional live/PG skips remain skips.
- All three race sweeps passed. Examples: CLI 4.911s / 4.856s / 5.015s; recording 1.050s / 1.058s / 1.051s; Kubernetes runner 6.828s in its compiled tag set.
- Module tidiness, golangci-lint (0 issues), staticcheck in all three tag sets, file-size, image-pin and shell regression checks passed.
- `govulncheck` passed all three tag sets with **0 affected symbols, 0 affected imported packages, 1 uncalled required-module advisory**. The earlier verbose evidence identifies `GO-2026-5932`, `golang.org/x/crypto/openpgp` from `golang.org/x/crypto@v0.56.0`, fixed version N/A. This run did not repeat verbose advisory identification or upgrade the dependency; it does not claim an advisory-free module graph.
- License headers: 1,655 files; Go license checks passed for default, Docker, Kubernetes and combined tags. Full-history secret scan: 3,343 commits / about 49.58 MB, no leaks found. Helm, Compose, DCO, seven Mermaid diagrams, 91 production npm licenses and production npm audit passed.
- UI official typecheck and live-spec loading passed. Vitest: **156 files / 2,886 tests passed**, no failed/skipped tests in the summary; 126.15s. Coverage: statements/lines 94.91%, branches 90.25%, functions 82.35%. Embedded UI production build passed (5.88s).
- Driver-agnostic conformance honesty/stub tests passed with eight explicitly skipped substrate-dependent subtests.

## Disclosed limits and warnings

This is the daemon-free merge gate, not live Docker/Kubernetes conformance, PostgreSQL acceptance, full Playwright UI acceptance, remote CI, a published-image scan, SBOM publication or release approval. Same-tip browser/PG lanes have separate owners and evidence; earlier `0d63ab2e` results must not be relabeled as `6a3dc865` results.

Existing shell installer T6 compose-integrity accepted-risk skip, license scanner assembly-inspection warnings, optional Compose UPN warnings, React `act` warnings, canvas/window.open test-harness limitations, and the production bundle's >500 kB warning remain visible in the log. No React function-component ref warning matched in this final log; that narrow observation does not establish that all UI warnings are fixed.

No cache relocation, image rebuild, schema/policy migration, release/version change or push was performed. R003 remains a separately documented read-only cache-accounting assessment, outside this selection.
