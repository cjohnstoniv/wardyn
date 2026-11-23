# This repository's own CI

Contributor and maintainer notes for the GitHub Actions workflows in this repository. Teams running Wardyn in their own pipeline want [CI.md](CI.md) instead.

## This repository's own CI

Wardyn's own [`ci.yml`](../.github/workflows/ci.yml) runs on every pull request and push to `main`. Runner slots constrain throughput: with many open pull requests, queueing outlasts execution. This section measures check counts, runner-minutes and elapsed time.

- `pull_request:` carries no `branches:` filter — that field matches the PR's *base*, and the 0.8 working practice stacks work on `<kind>/<issue#>-<slug>` branches, not on `main`, so a filtered trigger gave a stacked PR no checks at all.
- `push:` stays narrow to `main`, `master` and `feature/**`, since every other commit already gets a run from its own PR.
- It has no `release/**`: `release.yml` accepts CI by tree, and [`release-branch-checks.yml`](../.github/workflows/release-branch-checks.yml) runs only DCO and gitleaks there.
- The push-gate DCO exemption for GitHub-made merges needs gpg and GitHub's web-flow key id `B5690EEEBB952194`; if GitHub rotates that key, update the id in the Makefile's `dco` recipe.
- [`scripts/ci-parity-local.sh`](../scripts/ci-parity-local.sh) runs changed packages under `-race`, then a throwaway Postgres (pool guard, `localhost` DSN).
- `make candidate-preflight` vets release candidates.

| Go-suite control | Where | Rule |
|---|---|---|
| `api` and `store` shards run only DSN-gated tests | `make test-report-pg-api`, `make test-report-pg-store`, [`scripts/pg-test-select.sh`](../scripts/pg-test-select.sh) | The unit run fails if a DSN-skipped test is outside the pick; every `pg` probe is picked |
| Postgres shards write no coverage | `WARDYN_TEST_REPORT_COVER=0` | `build` unions `unit`, `docker`, `k8s` only |
| internal/api's tests build without DWARF | `GOFLAGS` on each step that builds them, [`scripts/ci-api-prebuild.sh`](../scripts/ci-api-prebuild.sh) | A step that reuses a prebuild sets the same `GOFLAGS`, since with any other value it compiles the package again; each prebuild's notice records the compile's largest single process |
| Flaky Go test quarantine | [`test/go-quarantine.txt`](../test/go-quarantine.txt), `go test -skip` | Issue, owner, expiry within 14 days; expired or malformed fails the suite |

Image-script tests set `WARDYN_MITM_CA_DIR` to a private temporary directory to isolate generated CA files, and `WARDYN_TRUSTED_CA_PATH` to a stand-in for the mounted `/etc/wardyn-trusted-ca.pem`.
The agent script defaults to `/tmp/wardyn`; deployment behavior is unchanged.

**`ui-e2e` runs as two shards**, `ui-e2e (1/2)` and `ui-e2e (2/2)`, because it would otherwise be the longest job of a pull request.

- Each shard takes every other spec of the sorted list (`WARDYN_E2E_SHARD`, [`scripts/run-ui-e2e.sh`](../scripts/run-ui-e2e.sh)), with the same three parallel workers and the same fresh seed per spec, so the per-spec isolation is unchanged.
- The four-eyes run and the `e2etmux` harness pins run once, in shard 1.
- It is not a required check, so it needs no aggregator.

**Per-job budget.** A job's budget is its `timeout-minutes`, at least twice its measured maximum with a ten-minute floor. Minutes, successful runs only. A `pending` cell means no measurement yet.

| Check | Runs | Median | Max | Timeout |
|---|---|---|---|---|
| `changes` | – | pending | pending | 10 |
| `go` (unit, docker, k8s matrix) | – | pending | pending | 40 |
| `go-lint` (check `go (lint)`) | – | pending | pending | 40 |
| `stop-early-go-lint` | – | pending | pending | 10 |
| `stop-early-compose` | – | pending | pending | 10 |
| `stop-early-gates` | – | pending | pending | 10 |
| `stop-early-notices` | – | pending | pending | 10 |
| `stop-early-diagrams` | – | pending | pending | 10 |
| `stop-early-dco` | – | pending | pending | 10 |
| `build` | – | pending | pending | 10 |
| `conformance-k8s` | 58 | 11.2 | 12.8 | 40 |
| `conformance-k8s-agent-sandbox` (not required; train and main only) | – | pending | pending | 109 |
| `vm-conformance` (not required) | – | pending | pending | 30 |
| `macos` (not required) | – | pending | pending | 30 |
| `macos-xcode-27` (not required) | – | pending | pending | 10 |
| `ui-e2e` (1/2, 2/2 matrix) | 1 | 7.0 | 7.0 | 25 |
| `test-pg-shard` (api-1, api-2, store, race) | – | pending | pending | 20 |
| `test-pg` (aggregator) | – | pending | pending | 10 |
| `ui` | 60 | 4.5 | 4.8 | 20 |
| `conformance` | 60 | 4.2 | 4.7 | 45 |
| `envbuild-integration` | 60 | 3.6 | 4.0 | 20 |
| `staticcheck` (tagless, docker, docker,k8s matrix; checks `staticcheck (staticcheck-…)`) | – | pending | pending | 40 |
| `gates-staticcheck` (aggregator, check `gates (staticcheck)`) | – | pending | pending | 10 |
| `helm-install-test` | – | pending | pending | 20 |
| `vm` | – | pending | pending | 30 |
| `trivy-wardynd` (check `trivy (wardynd)`) | 50 | 2.0 | 2.5 | 40 |
| `trivy-wardynd-fips` (check `trivy (wardynd-fips)`) | 49 | 2.0 | 2.3 | 40 |
| `trivy-agent-novnc` (check `trivy (agent-novnc)`) | 51 | 1.6 | 2.2 | 40 |
| `trivy-agent-vscode` (check `trivy (agent-vscode)`) | 50 | 1.6 | 2.0 | 40 |
| `trivy-agent-novnc-signin` (check `trivy (agent-novnc-signin)`) | – | pending | pending | 50 |
| `notices` | 60 | 1.5 | 1.9 | 15 |
| `gates (licenses)` | 60 | 1.4 | 2.0 | 15 |
| `trivy-agent-codex-cli` (check `trivy (agent-codex-cli)`) | 51 | 1.4 | 1.7 | 40 |
| `trivy-agent-base` (check `trivy (agent-base)`) | 50 | 1.3 | 1.6 | 40 |
| `trivy-agent-aws-sso` (check `trivy (agent-aws-sso)`) | 52 | 1.3 | 1.8 | 40 |
| `gates (gitleaks)` | 60 | 0.8 | 1.0 | 15 |
| `trivy-wardyn-proxy` (check `trivy (wardyn-proxy)`) | 53 | 0.9 | 1.2 | 40 |
| `trivy-wardyn-runnerd` (check `trivy (wardyn-runnerd)`) | – | pending | pending | 40 |
| `gates (govulncheck)` | 60 | 0.7 | 1.0 | 15 |
| `diagrams` | 60 | 0.6 | 0.9 | 10 |
| `image-scope` | – | pending | pending | 10 |
| `compose` | 60 | 0.3 | 0.5 | 10 |
| `gates (license-headers)` | 60 | 0.3 | 0.4 | 15 |
| `helm` | 60 | 0.1 | 0.7 | 10 |
| `dco` | 60 | 0.1 | 0.1 | 10 |
| `main-red` | – | pending | pending | 10 |
| `notify-flaky` | – | pending | pending | 10 |
| `runner-units` | – | pending | pending | 15 |
| `ci-ran` | – | pending | pending | 10 |
| `train-suites` | – | pending | pending | 180 |
| `mixed-version` | – | pending | pending | 50 |
| `train-gate` | – | pending | pending | 10 |
| `go-modules` | – | pending | pending | 15 |
| `go-gate-tools` | – | pending | pending | 15 |
| `go-cache-prune` | – | pending | pending | 10 |
| `multi-arch build (agent-claude-code, amd64\|arm64)`, nightly | – | pending | pending | 30 |
| `multi-arch build (wardynd)`, nightly | 60 | 3.1 | 3.5 | 45 |
| `multi-arch build (agent-aws-sso)`, nightly | 60 | 2.9 | 4.0 | 45 |

- `helm-install-test` also runs the desktop-envelope proof; its 20-minute budget is twice the sum of the two jobs' earlier maxima (3.2 and 2.5 minutes).

Re-measure (job name, runs, median and maximum over successful jobs):

```sh
gh run list -R cjohnstoniv/wardyn --workflow ci.yml --status completed --limit 60 \
    --json databaseId --jq '.[].databaseId' |
  while read -r id; do
    gh api "repos/cjohnstoniv/wardyn/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]
      | select(.conclusion == "success")
      | [.name, ((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))] | @tsv'
  done | sort -t$'\t' -k1,1 -k2,2n |
  awk -F'\t' '{n[$1]++; v[$1,n[$1]]=$2} END {for (j in n) printf "%-40s n=%-3d median=%5.1fm max=%5.1fm\n", j, n[j], v[j,int((n[j]+1)/2)]/60, v[j,n[j]]/60}' | sort
```

`make ci`, the local merge gate, prints the same kind of table for its own targets when it finishes or stops.

### Nightly scope and release runs

- The 07:00 nightly and a plain dispatch run every lane. A release candidate dispatches it with `-f scope=candidate`, which [`scripts/release-patch.sh`](../scripts/release-patch.sh) does right after pushing the branch and before opening the PR.
- Five jobs then skip through a job-level `if: inputs.scope != 'candidate'`: `e2e-live`, `ci-mode-dogfood-model-fake`, `test-e2e-concurrent`, `migration-merge-check` and `published-image-scan`.
- `ai-gateway-kind`, `kind-survival-walk`, `k6-kind-conformance`, `k6-workload-e2e`, `kind-acceptance-walks`, `hybrid-walk`, `kind-upgrade-walk` and `ssh-editors-live` run under `scope=candidate`, are in `release.yml`'s `watched=`, and gate a release. [`scripts/green-by-tree.sh`](../scripts/green-by-tree.sh) requires every leg of a matrix job to be green.
- [`scripts/test-repo-guards.sh`](../scripts/test-repo-guards.sh) guard 18 keeps the skip set in step with `notify-new-lanes`. A skipped job reports `skipped`, so that job stays quiet.
- A dispatched nightly has one concurrency group per ref and scope, so a second dispatch cancels the first. A scheduled run has its own group and is never cancelled.
- On a same-repo `train/*` or `prep/*` pull request, `train-suites` dispatches the nightly (`scope=candidate`, `publish=false`) on the head and waits; `train-gate` needs that run green. A call of the nightly would fail on a fork's read-only token, so `train-suites` dispatches it; the one job that does call a workflow is `mixed-version`, below.
- On the same heads, `mixed-version` calls [`mixed-version.yml`](../.github/workflows/mixed-version.yml): the published 0.10.1 wardynd runs as an old replica beside this tree's in one kind cluster, and the issuer-portal activation barrier is checked. `train-gate` needs it. A call runs from the head, before its file is on `main`.
- The nightly restores the `go-<suite>-` caches `ci.yml` saves and saves none. Only `publish` on (the default) stages and attests images.
- [`overlay-proofs.yml`](../.github/workflows/overlay-proofs.yml) runs [`scripts/overlay-proof.sh`](../scripts/overlay-proof.sh) revert proofs; each must exit 1 with its named FAIL. Never dispatch it while a train attempt 1 is queued.
  - Dispatch it on the bundle branch `proofs/<branch>-<sha12>`, one commit on `origin/main` adding `.proofs/`; on `main` every job skips.
  - Code on that branch can save cache entries under its run's ref whatever the token allows, and `ci.yml` restores `main`'s. So it never runs there, and it uses no cache.
  - A run counts only once `git diff --quiet origin/main...<head sha> -- ':(exclude).proofs'` exits 0 locally, so the workflow and script were main's.
  - The `prove` job has the same Postgres service and `WARDYN_TEST_PG` as `test-pg-shard`, so a proof of a PG-backed test in `./internal/store` or `./internal/api` runs there instead of skipping.
- `release.yml` has one group per ref and cancels nothing, so a dispatch and the tag push cannot publish at once.
- `release.yml`'s `runner-pkg` job (macOS) builds `wardyn-runnerd.pkg` with [`scripts/build-runner-pkg.sh`](../scripts/build-runner-pkg.sh). Each signing step runs only when its `APPLE_DEVELOPER_ID_*` or `APPLE_NOTARY_*` secrets are set, and logs a skip line otherwise; [`scripts/test-runner-pkg.sh`](../scripts/test-runner-pkg.sh) covers the script on Linux.
- The same job builds the macOS VM helper with cgo and signs it ad hoc with its one virtualization entitlement even without secrets. It then checks that entitlement, and that the package holds no macOS image.
- The non-required `macos` job compiles and tests the helper and the darwin guest agent on a hosted Mac. `macos-xcode-27` checks the macOS 27 selector names. Neither boots a VM.
  - The `macos` job also runs eighteen folder sync copy pins on an exFAT disk image, which has no hard links, and shows the volume's file numbers as notices. It runs the member-scan letter-case pin after checking that the runner's volume ignores letter case.
  - `macos` is not a required check, but a red `macos` makes the run unusable as release evidence (green-by-tree reads the run's conclusion). It also runs the package postinstall as root against a temp root and reads the ledger back.
  - `macos-xcode-27` runs on a public-preview image with `continue-on-error`, so it is informational and never release evidence.

### Incremental CI

Three mechanisms, on top of the per-job Go cache and the single race + coverage pass per tag set:

1. **Tag sets in parallel.**
   - The `go-lint` job (reported as `go (lint)`: `make build tidy-check lint`) and the `go` matrix job's `go (unit)`, `go (docker)` and `go (k8s)` (`make test-report`, `-docker`, `-k8s`) run on four runners instead of one after another.
   - `go (lint)` is a job of its own, not a matrix leg, so the stop-early jobs below can read its result before the test legs end.
   - Its first step runs the whole-tree guard tests (below), its second `make lint`.
   - Each test leg uploads its reports as `go-test-reports-<suite>`.
   - `build` waits for all four (`go` and `go-lint`), downloads the three profiles and runs `make cover-union`, the same `COVER_MIN` floor over the same union that `make cover-check` enforces locally.
2. **Skip what a change cannot affect.**
   - The `changes` job classifies the pull request's changed paths (`git diff --name-only --no-renames HEAD^1 HEAD` on GitHub's merge commit, so a moved file counts at both its old and its new path).
   - Each path gets one class, by the first rule that matches, so where a file lives decides before its extension does:

   | Order | Paths | Class |
   |---|---|---|
   | 1 | `deploy/**` and `LICENSING.md` | backend |
   | 2 | `docs/**`, `threatmodel/**` | docs |
   | 3 | `ui/**` | ui |
   | 4 | anything else under a directory, `.md` files included | backend |
   | 5 | a `.md` at the repository root | docs |
   | 6 | any other file at the repository root | backend |

   - Rules 1 and 4 are why a `.md` is documentation only at the repository root.
   - Helm renders every file under `deploy/helm/wardyn/templates/` as a manifest, whatever its extension, and every Dockerfile copies `LICENSING.md` and [`deploy/images/README.md`](../deploy/images/README.md).
   - When any `.md` counted as docs, a NetworkPolicy written in `templates/x.md` skipped `helm`, `helm-install-test`, both conformance jobs, `test-pg`, the image scans and `notices`,
     - and deleting a copied `.md` broke every image build with none built on the pull request.
   - `TestCIClassifierPutsWhereAFileLivesBeforeItsExtension` pins the order.
   - [`scripts/check-helm-templates.sh`](../scripts/check-helm-templates.sh) holds the chart half without the classifier: it refuses any file under `templates/` that is not a `*.yaml`, a `*.tpl` or `NOTES.txt`, from `make lint` on every change and again from `make helm-lint`.
   - (`THIRD-PARTY-NOTICES.md`, the third `.md` the Dockerfiles copy, stays docs: it sets `notices`, and `make notices` fails when the file is missing.)
   - The classes set five outputs, each `true` when at least one changed path sets it:

   | Output | Set by | Read by |
   |---|---|---|
   | `code` | any ui or backend path | `ui-e2e`, `helm-install-test` |
   | `backend` | any backend path (Go, `deploy/**`, `scripts/**`, `.github/**`, …) | `go` (full suites or guard packages), `build` (union), `test-pg-shard`, `test-pg`, `conformance`, `conformance-k8s`, `envbuild-integration`, `helm`, `helm-install-test`'s kind half |
   | `ui` | `ui/**`, the `Makefile`, [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) | `ui` (whole vitest suite, or only the files that read outside `ui/`) |
   | `images` | a `backend` path other than a Go `*_test.go` file | the ten `trivy (<image>)` jobs (then narrowed per image by `image-scope`, below) |
   | `notices` | `images`, [`ui/package.json`](../ui/package.json), [`ui/pnpm-lock.yaml`](../ui/pnpm-lock.yaml), `THIRD-PARTY-NOTICES.md` | `notices` |

   - A path that matches no earlier rule counts as backend, so a new directory runs everything until someone classifies it.
   - A push, a merge queue run, a pull request from a `train/*` branch and a pull request into `release/*` always run everything.
   - So, for example, a docs-only change skips `ui-e2e`, `helm-install-test`, both conformance jobs, `envbuild-integration`, `helm`, `test-pg`, `notices` and every `trivy` scan;
     - a console-only change skips the same except `ui-e2e` and `helm-install-test`'s desktop-envelope half;
     - a change to Go test files only skips the `trivy` scans and `notices`.
   - Go source counts as an image input:
     - of the 52 pull requests that changed non-test Go among the last 66, 43 changed an import of a module-qualified package, which is what decides which modules a binary links and so what `trivy` and `notices` report.
   - The Go jobs (`go`, `build`) never skip, whatever changed.
   - Dozens of Go test files read the docs, the CHANGELOG, `ui/src` or the workflows (the citation, CHANGELOG-freeze, RELEASING job-list and copy-parity guards among them),
     - so skipping Go on a docs-only change would let a docs change break the guards that check docs.
   - **Docs or console only: guard packages only.**
     - A change confined to the docs and `ui/` cannot change compiled Go, race behaviour or coverage.
     - So when `backend` is `false`, the three test legs do not run the race and coverage suites.
     - They run plain `go test -count=1`, with no race detector and no coverage, over the guard packages only.
     - Those are the directories of every `*_test.go` file that names `docs/`, `threatmodel/`, `ui/`, `.github/`, `CHANGELOG.md`, `RELEASING.md`, `AGENTS.md` or any `.md` file.
     - The step finds them from the test sources at run time, so a new guard is picked up without a list to maintain.
     - (Every Go test that walks the tree from its root filters to `.go` or `.sql` files, so none reads `ui/` without naming it.)
     - `go (unit)` runs them with no tags.
     - `go (docker)` and `go (k8s)` run, with their tag, only the packages whose guard files carry that build tag.
     - Today that is `./internal/runner/docker` for docker (`hardening_test.go` reads [`threatmodel/THREAT-MODEL.md`](../threatmodel/THREAT-MODEL.md)) and none for k8s, so the k8s leg says so and passes.
     - If the tagless leg finds no guard file at all, it fails: that would mean the search pattern broke.
     - `build` then needs every leg green and skips the coverage union, since no profiles were written.
     - `go (lint)` runs in full on every change: it carries the console's ESLint and the whole-tree guard tests.
   - **No console change: the vitest files that read outside `ui/` only.**
     - When `ui` is `false`, the `ui` job still installs, audits, typechecks and builds, but a vitest verdict can move only in a test that reads a file outside `ui/`: the Go wire-parity tests and the `docs/design` canon pins.
     - A test reads a file through `node:fs` (or `child_process`, a cwd- or module-relative path, a `?raw` or glob import),
       - so the step runs the test files that do, plus the callers of the two helpers that read a path their caller passes (`copy-doc-parity.ts`, and `test-fixtures.ts`'s `expectNoOwnCopy`):
       - 76 of 341 files, 71 s where the whole suite takes 547 s, without coverage.
     - A relative path counts as leaving `ui/` when it climbs to the name of any entry at the repository root other than `ui`;
       - the step reads those names from the tree, so a new top-level directory needs no edit here.
     - It fails closed:
       - if any other module under `ui/src` reads files or names a path outside `ui/`, a test could read through it unseen, so the whole suite runs instead; and finding no test at all fails.
3. **Docker layer cache.**
   - `helm-install-test` (wardynd, wardyn-proxy), `conformance-k8s` (wardyn-proxy) and `conformance` (wardyn-proxy, agent-claude-code) build through `docker/build-push-action` with `cache-from: type=gha,scope=<image>`.
   - `cache-to` (`mode=max`) is written only from a push to `main`, like the Go caches, so pull requests read main's layers and add no cache entries of their own.
   - Not cached:
     - `trivy` (a cached `apt-get` layer would scan older packages than the release builds, which changes what the gate says),
     - the conformance agent image (`make build-conformance-agent-image`)
     - and `trivy`'s `agent-vscode`/`agent-novnc` jobs, which build `FROM` a local image that a buildx builder cannot see.

**Required checks and skipped jobs.**

- GitHub reports a job skipped by a job-level `if:` as "skipped", and branch protection accepts that as passing (a console-only change was once mergeable with `envbuild-integration` skipped).
- A skipped job takes no runner, where a job that only skips its steps still waits for one:
  - in run 37392974500, a console-only pull request, `conformance` queued 1426 s to run for 6 s, `helm` 1779 s and `test-pg` 1785 s.
- So a job that a change cannot affect skips at the job level, under two rules:
  - A job reads the classification only as `!cancelled() && … != 'false'`. If `changes` *failed*, every output is empty, so the job runs instead of skipping.
  - A required job with a **matrix** never skips at the job level.
    - GitHub does not expand a skipped matrix: it reports one check with the name unexpanded (`gates (${{ matrix.gate }})`, the same row [`scripts/green-by-tree.sh`](../scripts/green-by-tree.sh) documents for the nightly),
      - so `gates (gitleaks)` would never report and the pull request would wait on it forever.
    - This is why the ten image scans are separate jobs, not a matrix:
      - their steps are written once and shared through a YAML anchor, and each job skips on `images == 'false'` or on its own `image-scope` answer.

So:

- `test-pg` (the aggregator over its `test-pg-shard` matrix, which fails unless every shard passed), `conformance`, `conformance-k8s`, `vm`, `helm` and `envbuild-integration` skip on `backend == 'false'`, `notices` on `notices == 'false'`, the ten `trivy (<image>)` jobs on `images == 'false'` or on `image-scope`'s answer for their image.
- `vm` boots the reference Linux VM guest under QEMU/KVM through the real Docker driver ([`test/conformance/vm/vm_test.go`](../test/conformance/vm/vm_test.go)), after GitHub's documented udev rule opens `/dev/kvm`. A required check.
- `build` needs `go-lint`, `image-scope` and every `go` leg and runs with `if: always()`.
  - Its first step fails unless every leg passed, so the `build` context is green only when all four legs and the union floor are.
  - The union steps carry `if: needs.changes.outputs.backend != 'false'`, so only an explicit docs- or console-only classification skips them.
  - A failed or missing classification runs the full suites in the legs and requires the union.
- `ui` always runs and narrows only vitest (above).
- Non-required jobs (`ui-e2e`, `helm-install-test`) skip at the job level on `code`.
  - `helm-install-test` puts `backend != 'false'` on its kind half's steps, so a console-only change runs just its desktop-envelope half.
  - `notify-flaky` skips when both `ui-e2e` shards said their flaky list was empty, and runs on any other answer, including none.
- `compose`, `dco`, `diagrams` and `gates (…)` do not read the classification and run on every change.
  - (`helm` stays on `backend`, not on the chart's own paths: `make helm-lint` also renders [`examples/policies/demo.json`](../examples/policies/demo.json) and [`cmd/wardynd/testdata/values-0.7.yaml`](../cmd/wardynd/testdata/values-0.7.yaml).)
- Only a run whose six outputs and ten `image-scope` answers are all `true` uploads the `ci-full-tree-<tree>` marker that [`scripts/green-by-tree.sh`](../scripts/green-by-tree.sh) accepts as release evidence.
  - A run that narrowed anything did not test everything, even where every check it reported is green.
  - `build` writes the marker, because only after `image-scope` can it know whether a scan was narrowed.

**Scanning an image only when its inputs change.**

- `changes` sets `images` for nearly every pull request that touches Go, so all ten images were built and scanned each time.
- `image-scope` runs [`scripts/image-scope.sh`](../scripts/image-scope.sh) on the changed paths and answers per image. The scan jobs' `TRIVY_IMAGE` and `TRIVY_DOCKERFILE` are its list.
- A changed path reaches an image when it is:
  - the image's Dockerfile, or that of a local base it builds `FROM` (`agent-vscode` and `agent-novnc` sit on `agent-base`);
  - a path those Dockerfiles `COPY`, except `ui/`, which is a built bundle `trivy` does not read;
  - a non-test file in the directory of a Go package in the `go list -deps` closure of a binary those Dockerfiles build, or a file such a package embeds;
  - `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/`, [`deploy/base-images.txt`](../deploy/base-images.txt), `.trivyignore`, the `trivy.yaml` config files, `.dockerignore` and a `<Dockerfile>.dockerignore`, `ci.yml`, the `base-image-mirror` action, or the script.
- The closure is read from the tree under test, so a new import of a package already in the module changes it, which a list of paths would miss.
- A push, a `train/*` pull request, a pull request into `release/*` and a script failure scan every image.
  - The nightly scans the published images on its own.
  - A release accepts a CI run only through the full-tree marker.
- A Dockerfile form the script does not model scans every image: a multi-line `COPY`, a `RUN --mount=type=bind` without `from=`, `CGO_ENABLED=1`, or a `RUN` that runs a script.
- The `agent-run --selftest` step stays in the `agent-aws-sso` and `agent-codex-cli` scans, so it runs whenever their Dockerfile or `agent-run` scripts change.
- [`image_scope_guard_test.go`](../cmd/wardynd/image_scope_guard_test.go) plants every input it derives from the Dockerfiles and `go list`, and fails when the script says "skip".

**Start order under the 20-job cap.**

- The account runs 20 jobs at once and a change to Go asks for about 35, so some wait, and GitHub picks which among the jobs that became ready together.
- The long jobs (`go`, `ui`, `ui-e2e`, `helm-install-test`, both conformance jobs, `envbuild-integration`, `test-pg-shard`) need only `changes`.
- The short ones (`gates`, `notices`, the `trivy` scans) also need `diagrams`, a 45-second job that starts with the run (the scans also need `image-scope`, which has no `needs` and so starts with it),
  - so they become ready about half a minute after the long jobs, which by then hold their runners, and share what is left.
- Nothing reads `diagrams`' result: each of them runs on `!cancelled()`, whether it passed or failed.
- The job they wait on has to start with the run: one that itself needs `changes` would send the short jobs through the queue a third time when the account is busy.

**Stop a red run early.**

- A pull request run is cancelled once a cheap, deterministic gate has failed.
  - Measured 2026-10-20: 44 red runs spent 2,761 of 7,751 runner-minutes after the first real failure; 1,309 of them followed a cheap gate.
- `go (lint)` is a job of its own (`go-lint`), so its result is readable before the test legs end. Its first step runs the whole-tree guards, `python3 scripts/lib/candidate-tests.py guards`, the list `scripts/lane-preflight.sh --candidate` builds.
  - Five runs had one guard red on all three test legs after about 14 minutes each.
- `stop-early-go-lint`, `-compose`, `-gates`, `-notices`, `-diagrams` and `-dco` each need one gate and start when it fails.
  - Each calls the run cancel endpoint with the workflow's `GITHUB_TOKEN` and `actions: write`, checks out nothing and runs nothing from the pull request.

| What | Rule |
|---|---|
| Never fires on | `ui-e2e`, `test-pg-shard`, the `go` test legs: their reds can be flakes |
| Events | `pull_request` from this repository; push, `merge_group`, `train/*` and `prep/*` pull requests, forks and Dependabot keep a full run |
| Required checks | none renamed or added; `build` and `test-pg` run on `always()` and fail after a cancel |
| Pinned by | [`cmd/wardynd/ci_stop_early_guard_test.go`](../cmd/wardynd/ci_stop_early_guard_test.go) |

- A cancelled run cannot read green: the failed gate keeps `failure`, and every unfinished job ends `cancelled`, which branch protection rejects.
  - `build` and `test-pg` run on `always()`, so they report `failure`, never `skipped`.
- The cost: one failure shows at a time, a flaky gate stops the run too, and a stop job waits for a slot when the account is at its cap.

**By kind of change (2026-10-06).**

- One pull request of each kind, each run on its own (nothing else running in the account), against the last run of that kind before the classification gained `ui`, `images` and `notices`.
- "Jobs" counts the jobs that took a runner; a skipped job takes none.

| Change | Run | Jobs | Runner-minutes | Wall | Longest job |
|---|---|---|---|---|---|
| Everything, before: push to main | 37269326378 | 32 | 116.9 | 18.3 min | `test-pg` 17.4 min |
| Go only, after | 37405900397 | 35 | 108.2 | 9.1 min | `conformance-k8s` 8.9 min |
| Console only, before | 37392974500 | 31 | 70.0 | 41.9 min, of which up to 29.8 queued | `ui-e2e` 10.3 min |
| Console only, after | 37406693248 | 18 | 39.6 | 10.5 min | `ui` 10.3 min |
| Docs only, before | 36789150048 | 27 | 33.5 | 11.3 min, of which up to 7.7 queued | `ui` 6.6 min |
| Docs only, after | 37407613617 | 15 | 18.1 | 4.1 min | `go (unit)` 3.8 min |

- A change to Go still asks for 35 runners.
- Branch protection requires 24 contexts and a change to Go can affect every one of them,
  - so that count moves only if contexts are merged (the image scans into one or two jobs, the five `gates` into one), which is a branch-protection change.
- Its wall time changed: `test-pg` and `ui-e2e` are split, the long jobs get their runners first, and vitest runs 76 files (71 s) where it ran 341 (547 s).
- A console-only change is held at 10.5 minutes by the whole vitest suite in `ui`; two vitest shards behind a `ui` aggregator would bring it to about 7.5 minutes for two more jobs.
- A run of everything (a push to `main`, a `train/*` pull request) runs that same `ui` job, so `ui` is its longest job as well.

### Docker Hub base-image mirror

Docker Hub rate-limited CI even after `docker/login-action`, so CI reads the Docker Hub images it uses from a byte-identical GHCR copy:

- [`deploy/base-images.txt`](../deploy/base-images.txt) lists them. Digest pins are the Dockerfile `FROM` pins, the compose pins and kind's node image. Tag pins (`tag ` lines), kept by hand, are images CI pulls by tag.
- [`mirror-base-images.yml`](../.github/workflows/mirror-base-images.yml) copies each entry, digest unchanged, into `ghcr.io/cjohnstoniv/base-images`: on a list change on `main`, weekly, and on dispatch.
  - Dependabot cannot run it: add the bump's digest to the list on its branch, dispatch with `ref` set to that branch, then re-run the pull request's checks.
- [`base-image-mirror/action.yml`](../.github/actions/base-image-mirror/action.yml) runs before a job builds or pulls, instead of a separate `mirror.gcr.io` step.
  - It puts a loopback mirror over the copy, then `mirror.gcr.io`, in dockerd's `registry-mirrors`. One dockerd restart applies it (a `SIGHUP` reload resets the host-gateway address), and the job's service containers start again.
  - It pulls digest pins under their `docker.io` names, so a pinned `FROM` resolves locally, and tag pins from GHCR, retagged `<name>:<tag>`.
  - Anything the copy lacks, and any tag not listed, goes to `mirror.gcr.io`, then Docker Hub. If the loopback mirror does not start, `mirror.gcr.io` is still configured.
- The Postgres service containers start before any step, so they name the copy directly and pull it with the job's `GITHUB_TOKEN` (`packages: read`).
- Dockerfiles keep their `docker.io` `FROM` lines for Dependabot and release provenance.
- Guard 17 in [`scripts/test-repo-guards.sh`](../scripts/test-repo-guards.sh) fails while a Docker Hub `FROM` pin is not listed.
- [`mirror-selftest.yml`](../.github/workflows/mirror-selftest.yml) (dispatch only) checks the action on ubuntu-24.04 and ubuntu-26.04 with Docker Hub unreachable.
- Not covered: pulls inside kind nodes, which skip dockerd (`postgres:16` is copied for a future `kind load`), and BuildKit container builders.

### Go module cache for image builds

`WARDYN_BUILD_GOMODCACHE` names a Go module cache directory. The e2e scripts pass its `cache/download` tree to the proxy and oracle builds as `--build-context gomodcache=<dir>`, so `go mod download` reads them. go.sum is checked against each module's recorded hash, as with any module cache. Unset, builds download.

