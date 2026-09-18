# Round-two recording-root merge gate

Verified 2026-09-18 by the lifecycle review lane. This is evidence, not release approval.

- Worktree: `/tmp/wardyn-review-0.7x-round2-integration`
- Branch: `review/0.7x-round2-integration`
- Exact tested commit: `cd179c617acee6dd14ba6bc273db44569310355b`
- Composition: frozen first-round `0d63ab2e3ade4302b9f51b08146965cf21655c46` plus a cherry-pick of independent recording-root patch `18fb91fdb6a3e656eed2a922ea73ddd22998ef3f`.
- Installed `ui` dependencies with `pnpm install --frozen-lockfile` (exit 0) BEFORE the notice generator ran. No tracked dependency or notice changes resulted.
- Uninterrupted `make ci`: **PASS, process exit 0**. Worktree clean afterward; `git diff --check 0d63ab2e..HEAD` passed. Original first-round worktree remains clean at `0d63ab2e`.
- Log: [round2-ci-recording-root.log](round2-ci-recording-root.log)
- Log SHA-256: `94d1883e2140c2cb9dd1d2d9d09a67f1b98189d8baca67242ee94c24cebed948`

Command environment: `GOFLAGS=-buildvcs=false`, `GOCACHE=/tmp/wardyn-review-cache/go-build`, `GOTMPDIR=/tmp/wardyn-review-cache/go-tmp`, `GOPATH=/tmp/wardyn-review-cache/go-path`, `GOMODCACHE=/tmp/wardyn-review-cache/go-mod`, `GOMAXPROCS=4`, `DOCKER_HOST=unix:///var/run/docker.sock`. Bash `pipefail` retained the gate's exit status while teeing its complete output to the log.

## Results and boundaries

All default, Docker-tagged and Kubernetes-tagged build/vet/report/race/staticcheck gates passed, along with lint, shell, file-size, image-pin, license, notice, secret-scan, deployment-schema, DCO and diagram checks. Union Go coverage was 78.4% against the 78% floor; all 17 required probe parents passed. UI typecheck, live-spec loading, production audit and build passed; Vitest passed 155 files / 2,880 tests (statements/lines 94.91%, branches 90.25%, functions 82.35%). Notices remained at 224 entries without repair.

`govulncheck` reported no affected reachable symbols under all three tag sets, with the same previously disclosed uncalled module advisory. See the first-round verbose evidence for `GO-2026-5932`, `golang.org/x/crypto/openpgp` at `v0.56.0` (no fixed version then reported); this gate is not a claim of zero dependency advisories. No dependency upgrade was folded into this batch.

The passing stub conformance includes eight disclosed subtest skips; it is not live Kubernetes/Docker acceptance. The shell installer T6 accepted-risk skip, existing React act/canvas/window.open/SheetOverlay-ref warnings, and UI large-chunk warning remain disclosed rather than treated as newly fixed.

No PostgreSQL or full Playwright suite was rerun at this exact commit. Their earlier passes belong to first-round `0d63ab2e`, not `cd179c61`. This run closes the combined daemon-free merge-gate gap for R076-023 only; it contains none of the later round-two CLI, proxy or SheetOverlay patches.
