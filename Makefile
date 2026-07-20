.PHONY: license-headers diagrams build build-docker test test-docker lint ui compose-build compose-up compose-down demo clean test-conformance-docker test-conformance-stub test-envbuild-integration govulncheck staticcheck agent-images test-drive help test-report test-report-pg test-report-docker cover-check release-check ui-test ui-typecheck test-e2e test-e2e-live test-e2e-subscription test-e2e-byoi test-e2e-ui screenshots setup stage-claude stop-host reset reset-all doctor dev-pg agent-images-core test-race agent-image-full gitleaks licenses helm-lint compose-config dco sbom npm-license ci

COMPOSE_FILE := deploy/compose/docker-compose.yaml

# ── pinned tool versions (single source of truth for CI + release-check) ─────
# CI (.github/workflows/ci.yml) routes its gates through the make targets below
# so a bump here is the ONLY place a version changes — no more drift between the
# Makefile and the workflow. Override on the CLI for a one-off (e.g.
# `make govulncheck GOVULNCHECK_VERSION=v1.7.0`).
GOVULNCHECK_VERSION  ?= v1.6.0
STATICCHECK_VERSION  ?= v0.7.0
GITLEAKS_VERSION     ?= v8.30.1
GO_LICENSES_VERSION  ?= v1.6.0
SYFT_VERSION         ?= v1.46.0
GOLANGCI_LINT_VERSION ?= v2.12.2
# Throwaway local registry for the real-daemon envbuild smoke test (U064). Pinned
# by tag like the other daemon images CI pulls (postgres:17, alpine:latest).
ENVBUILD_REGISTRY_IMAGE ?= registry:2

# ── corporate-build pass-through ─────────────────────────────────────────────
# Behind a TLS-MITM proxy / internal package mirror, every image build needs an
# alternate npm registry and/or an HTTP(S) proxy. The Dockerfiles already declare
# these as build ARGs; wire them from make so `make agent-images` reaches them
# instead of forcing a hand-run `docker build`. Empty by default (OSS builds pass
# nothing). HTTP_PROXY/HTTPS_PROXY inherit from the environment if already exported.
#   make agent-images NPM_REGISTRY=https://mirror.corp/api/npm/npm-remote \
#                     HTTPS_PROXY=http://proxy.corp:8080
# Stage the corp CA at deploy/images/corp-ca.pem (gitignored) for TLS trust.
NPM_REGISTRY ?=
HTTP_PROXY   ?=
HTTPS_PROXY  ?=
NO_PROXY     ?=
# Emit "--build-arg NAME=VALUE" only when VALUE is non-empty, so an unset knob
# never overrides a Dockerfile default with an empty string.
_build_arg = $(if $(2),--build-arg $(1)="$(2)",)
DOCKER_BUILD_ARGS = \
	$(call _build_arg,NPM_REGISTRY,$(NPM_REGISTRY)) \
	$(call _build_arg,HTTP_PROXY,$(HTTP_PROXY)) \
	$(call _build_arg,HTTPS_PROXY,$(HTTPS_PROXY)) \
	$(call _build_arg,NO_PROXY,$(NO_PROXY))

help:
	@echo "Wardyn governance control plane"
	@echo ""
	@echo "Targets:"
	@echo "  setup                 - One-command Wardyn: containerized by default (Enter = containerized), host is advanced,"
	@echo "                          credential, builds, up, opens browser. Headless defaults to host; scripts pick"
	@echo "                          with WARDYN_SETUP_MODE=local|container. Team (multi-user) is coming soon."
	@echo "                          (non-interactive opt-ins: WARDYN_STAGE_CLAUDE=1, WARDYN_IMPORT_AWS=1,"
	@echo "                           WARDYN_IMPORT_SCM=1, WARDYN_FORCE_RESET=1)"
	@echo "  stage-claude          - Stage your Claude login for per-run subscription mounts (restarts wardynd)"
	@echo "  stop-host             - Stop the host-mode wardynd started by make setup (pidfile under ~/.wardyn)"
	@echo "  reset                 - Clean slate: wipe local volumes (runs + audit + recordings) then setup"
	@echo "  reset-all             - FULL undo of local setup: host daemon + compose + ~/.wardyn install files"
	@echo "                          (ARGS='--dry-run' to audit; '--purge-images'/'--purge-env' to go further)"
	@echo "  doctor                - Read-only preflight (docker, ports, confinement classes, WSL/Windows)"
	@echo "  dev-pg                - Start/ensure the dockerized dev/e2e Postgres (wardyn-test-pg :55432)"
	@echo "  build                 - Build Go binaries (default tags)"
	@echo "  build-docker          - Build Go binaries with -tags docker"
	@echo "  test                  - Run all Go tests"
	@echo "  test-docker           - Run all Go tests with -tags docker"
	@echo "  test-conformance-docker - Run conformance suite on Docker (requires WARDYN_TEST_DOCKER=1)"
	@echo "  test-conformance-stub - Run the driver-agnostic conformance honesty stub (no cluster required)"
	@echo "  test-envbuild-integration - Real-daemon envbuild push/pull smoke test (provisions a throwaway"
	@echo "                          registry + tools dir; needs Docker; runs TestBuild_SmokeDockerd)"
	@echo "  govulncheck           - Run govulncheck for known vulnerabilities"
	@echo "  staticcheck           - Run staticcheck static analysis"
	@echo "  diagrams              - Validate the mermaid diagrams in the public docs (syntax + label-truth)"
	@echo "  license-headers       - Check (CI) SPDX headers on source files; ARGS=fix to apply"
	@echo "  lint                  - Run go vet (both tag sets) + golangci-lint size/complexity gate + file-size gate"
	@echo "  ui                    - Build embedded web UI"
	@echo "  ui-typecheck          - Typecheck the web UI (tsc --noEmit)"
	@echo "  ui-test               - Run web UI vitest unit/component tests + coverage"
	@echo "  test-e2e-ui           - Playwright UI e2e vs a seeded backend (needs Docker + chromium)"
	@echo "  screenshots           - Regenerate docs/img UI screenshots (run after visible UI changes, commit the diff)"
	@echo "  test-e2e              - Live security e2e: L0 egress, metadata block, kill cascade (needs Docker)"
	@echo "  test-e2e-live         - Live TASK e2e: real sandboxes run the corpus, graded on state (needs Docker)"
	@echo "  test-e2e-subscription - Live SUBSCRIPTION e2e: proxy-side inject-on attach + inject-off escape hatch (restarts wardynd)"
	@echo "  test-e2e-byoi         - Live BYOI e2e: wrap stock/harness/hostile/nonexistent bases + selftest gate (needs Docker)"
	@echo "  test-report           - Go unit tests with detailed md/coverage reports"
	@echo "  test-report-pg        - Postgres-gated suite with reports (needs WARDYN_TEST_PG)"
	@echo "  test-report-docker    - -tags docker suite with reports (fakeDocker; no daemon needed,"
	@echo "                          WARDYN_TEST_DOCKER=1 adds the real-daemon cases)"
	@echo "  cover-check           - Enforce the COVER_MIN coverage floor over BOTH shipped builds"
	@echo "                          (tagless + -tags docker, unioned; see scripts/cover-union.sh)"
	@echo "  release-check         - Pre-tag gate: the RELEASING.md gate list in one command"
	@echo "                          (pushes/tags nothing; WARDYN_TEST_PG adds the Postgres lane)"
	@echo "  ci                    - Daemon-free merge gate (the CI checks that need no daemon/service);"
	@echo "                          excludes docker-conformance, WARDYN_TEST_DOCKER e2e, PG + UI-e2e lanes"
	@echo "  compose-build         - Build compose images (wardynd -tags docker + proxy)"
	@echo "  compose-up            - Start docker-compose stack (postgres + dex + wardynd)"
	@echo "  compose-down          - Stop docker-compose stack"
	@echo "  demo                  - End-to-end compose demo (build, up, run, audit)"
	@echo "  agent-images          - Build all agent OCI images (claude-code + codex-cli + oracle e2e stand-in)"
	@echo "  agent-images-core     - Build just the user-facing agent images (claude-code + codex-cli)"
	@echo "  test-drive            - Guided governance test-drive against a running compose stack"
	@echo "                          (ARGS='--up' to bring the stack up first)"
	@echo "  clean                 - Remove built binaries"

# Core = the two real agent harnesses a user actually runs. The oracle image is
# a deterministic e2e stand-in (no LLM) — dev/e2e only, so setup paths build
# core and the e2e scripts build oracle themselves.
agent-images-core:
	@echo "Building agent images (build context: repo root)..."
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:local .
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/codex-cli/Dockerfile   -t wardyn/agent-codex-cli:local   .
	@echo "Agent images built: wardyn/agent-claude-code:local  wardyn/agent-codex-cli:local"

agent-images: agent-images-core
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/oracle/Dockerfile      -t wardyn/agent-oracle:local      .
	@echo "Oracle e2e image built: wardyn/agent-oracle:local"

# The full toolchain image: the core claude-code agent PLUS real language toolchains
# (Go, Python, Rust, JDK/Maven, pnpm). Workspace import's Record/Verify runs the
# repo's OWN setup commands (go build, pnpm install, …), which the toolchain-less
# core image cannot do — it dies at "command not found" (exit 127). Kept out of
# agent-images-core because it is a fat image and most setups never need it.
#
# It had no make target at all, so nothing ever rebuilt it: boxes were left running
# a stale :demo tag whose Go (1.23.5) predated this repo's own go.mod (1.26) and
# which shipped no pnpm. Build it explicitly, then point runs at it with:
#   WARDYN_AGENT_IMAGES='{"claude-code":"wardyn/agent-full:local"}'
agent-image-full: agent-images-core
	@echo "Building the fat toolchain image (Go/Python/Rust/JDK/pnpm)..."
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/full/Dockerfile -t wardyn/agent-full:local .
	@echo "Full toolchain image built: wardyn/agent-full:local"

build:
	@echo "Building Go binaries..."
	go build ./...

build-docker:
	@echo "Building Go binaries (-tags docker)..."
	go build -tags docker ./...

test:
	@echo "Running Go tests..."
	go test ./...

# Race-detector sweep. The kill/dispatch FSM has dedicated concurrent tests
# (internal/api/kill_dispatch_race_test.go) that only mean something under -race;
# the rest of the tree rides along. Required green before restructuring runs.go.
# BOTH tag sets: the tagless pass alone never compiles the -tags docker runner
# tree (internal/runner/docker, internal/envbuild) — the concurrency-heavy
# sandbox lifecycle — so it would get zero race coverage. The docker-tagged pass
# needs no daemon (the real-Docker cases self-skip unless WARDYN_TEST_DOCKER=1).
test-race:
	@echo "Running Go tests under the race detector (tagless)..."
	go test -race ./...
	@echo "Running Go tests under the race detector (-tags docker)..."
	go test -race -tags docker ./...

test-docker:
	@echo "Running Go tests (-tags docker)..."
	go test -tags docker ./...

# ── detailed test reports (markdown + coverage) ─────────────────────────────
# Emits per-test-case reports under test/reports/go/<suite>/. See
# scripts/test-report.sh and scripts/test2md.py.
test-report:
	@echo "Running Go unit suite with detailed reports..."
	./scripts/test-report.sh unit "Wardyn Go unit tests" ./...

test-report-pg:
	@echo "Running Postgres-gated suite with reports (requires WARDYN_TEST_PG)..."
	./scripts/test-report.sh pg "Wardyn Postgres integration tests" \
		./internal/store/... ./internal/db/... ./internal/secretstore/... ./internal/broker/... \
		./internal/api/... ./test/apie2e/...

# The whole tree under -tags docker, so the container-hardening driver
# (internal/runner/docker), internal/envbuild and the wardynd wiring that calls
# them — none of which the tagless build can even compile — are actually tested
# and measured. No daemon needed: the real-Docker cases self-skip unless
# WARDYN_TEST_DOCKER=1, leaving the fakeDocker-backed tests to run anywhere.
test-report-docker:
	@echo "Running docker-tagged suite with reports (fakeDocker; WARDYN_TEST_DOCKER=1 adds the real-daemon cases)..."
	./scripts/test-report.sh docker "Wardyn Go tests (-tags docker)" -tags docker ./...

# Coverage floor gate. Override with `make cover-check COVER_MIN=NN`.
# Enforced over the UNION of both shipped builds (tagless + -tags docker), not
# the tagless subset alone — measuring only the tagless build reported a number
# for code that is not what ships. Pulling the excluded packages in moved the
# honest total from 67.1% (tagless-only) to 66.1% (union); the floor sits just
# under that with a small margin for routine churn. Raise it as coverage climbs.
# scripts/cover-union.sh documents exactly what is and is not counted.
COVER_MIN ?= 65
cover-check: test-report test-report-docker
	@./scripts/cover-union.sh --self-test
	@./scripts/cover-union.sh $(COVER_MIN) test/reports/go/union \
		test/reports/go/unit/cover.out test/reports/go/docker/cover.out

# ── pre-tag release gate ────────────────────────────────────────────────────
# The gate list RELEASING.md names, as ONE command, so cutting a release is not a
# hand-copied checklist. Runs the same commands the CI jobs run, at the same
# pinned tool versions. It PUSHES NOTHING and TAGS NOTHING — it only tells you
# whether the commit is tag-able.
#
# Deliberately NOT a full CI replica: test-report-pg runs only when
# WARDYN_TEST_PG is set (and says so loudly when it isn't), and the
# service-dependent lanes (test-conformance-docker), the UI jobs, and DCO are
# CI-only. CI remains the authority — see RELEASING.md.
release-check: build build-docker lint cover-check test-race staticcheck govulncheck license-headers
	@grep -q "## \[Unreleased\]" CHANGELOG.md || (echo "CHANGELOG missing [Unreleased]"; exit 1)
	@if [ -n "$$WARDYN_TEST_PG" ]; then \
	  echo "==> Postgres-gated suite"; $(MAKE) test-report-pg; \
	else \
	  echo ">> SKIPPED test-report-pg — set WARDYN_TEST_PG=postgres://... to run it (CI always does)"; \
	fi
	@echo "==> Dependency licenses (tagless + -tags docker)"
	@$(MAKE) licenses
	@echo "==> Secret scan (full git history)"
	@$(MAKE) gitleaks
	@echo ""
	@echo "release-check PASSED. NOT covered here: test-conformance-docker, the UI"
	@echo "jobs, and DCO — confirm CI is green on the commit before tagging."

test-conformance-docker:
	@echo "Running conformance tests on Docker (WARDYN_TEST_DOCKER=1 required)..."
	WARDYN_TEST_DOCKER=1 go test -v -tags docker -timeout 10m ./test/conformance/...

test-conformance-stub:
	@echo "Running driver-agnostic conformance honesty-stub tests (no cluster required)..."
	go test -v -timeout 2m ./test/conformance/...

# ── real-daemon envbuild integration (U064) ─────────────────────────────────
# TestBuild_SmokeDockerd is the ONLY test that drives the real envbuilder
# push -> finalize-FROM-pushed -> pull cycle (and thus the pushedBaseRef
# assumption in builder.go) end-to-end against a live daemon. It never ran in
# CI because it is triple-gated on WARDYN_TEST_DOCKER + a writable registry
# (WARDYN_TEST_CACHE_REPO) + a runner-tools dir (WARDYN_TEST_TOOLS_DIR), and no
# job provisioned the latter two. This target provisions all three against the
# ambient daemon and runs just that package, so the assumption is validated
# automatically. Bounded to internal/envbuild (single test, 15m cap).
#
# Networking: the build container must reach the loopback git daemon + registry
# the test stands up on 127.0.0.1. That only works under HOST networking, and
# only when the daemon shares the host network namespace — a host-native dockerd
# (ubuntu-latest CI, or a native local dockerd). The test defaults to "host"
# (override with WARDYN_ENVBUILD_TEST_NETWORK). This target CANNOT pass against a
# VM-based daemon like Docker Desktop: the build container cannot reach the
# WSL/host loopback in any network mode. The tools are staged from the same
# in-repo sources the agent images ship (cmd/* + deploy/images/*), so the
# finalize COPY has real binaries to layer, not stubs.
test-envbuild-integration:
	@echo "Running real-daemon envbuild integration tests (U064; requires Docker)..."
	@set -eu; \
	tools_dir="$$(mktemp -d)"; \
	trap 'docker rm -f wardyn-envbuild-registry >/dev/null 2>&1 || true; rm -rf "$$tools_dir"' EXIT; \
	echo "==> staging runner tools into $$tools_dir"; \
	go build -o "$$tools_dir/" ./cmd/wardyn-rec ./cmd/wardyn-verify ./cmd/wardyn-git-helper; \
	cp deploy/images/claude-code/agent-run "$$tools_dir/agent-run"; \
	cp deploy/images/common/agent-run-lib.sh "$$tools_dir/agent-run-lib.sh"; \
	chmod +x "$$tools_dir/agent-run" "$$tools_dir/agent-run-lib.sh"; \
	echo "==> starting throwaway registry $(ENVBUILD_REGISTRY_IMAGE) on :5000"; \
	docker rm -f wardyn-envbuild-registry >/dev/null 2>&1 || true; \
	docker run -d --name wardyn-envbuild-registry -p 5000:5000 $(ENVBUILD_REGISTRY_IMAGE) >/dev/null; \
	echo "==> running TestBuild_SmokeDockerd against the live daemon"; \
	WARDYN_TEST_DOCKER=1 \
	WARDYN_TEST_CACHE_REPO=localhost:5000/wardyn-envbuild-test \
	WARDYN_TEST_TOOLS_DIR="$$tools_dir" \
	go test -tags docker -run TestBuild_SmokeDockerd -timeout 15m -v ./internal/envbuild/

# Live full-stack security e2e (L0 egress, metadata block, kill cascade,
# brokered creds, recording). Heavy: stands up the compose stack. Guarded by
# WARDYN_TEST_DOCKER=1 inside the script. Runs in the nightly workflow.
test-e2e:
	@echo "Running live security e2e (requires Docker; WARDYN_TEST_DOCKER=1)..."
	WARDYN_TEST_DOCKER=1 ./test/e2e/e2e.sh

# Live TASK e2e: real sandboxes running the test/e2e/tasks corpus, graded on
# final workspace STATE (did the agent actually do the work?), plus per-tier
# allow/block confinement, interactive PTY, and recording-replay. The $0 oracle
# lane runs by default; add WARDYN_E2E_REAL_MODEL=1 (+ staged creds via
# scripts/stage-claude-creds.sh) for the real claude-code lane. Guarded by
# WARDYN_TEST_DOCKER=1 inside the script.
test-e2e-live:
	@echo "Running live TASK e2e (real sandboxes + graders; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-live.sh

# Live SUBSCRIPTION e2e: proves proxy-side OAuth-token injection end-to-end. The
# driver RESTARTS wardynd with WARDYN_SUBSCRIPTION_INJECT flipped to run both the
# inject-on attach-walkthrough and the inject-off escape-hatch lane, then restores
# the safe default. Needs Docker + staged Claude creds (scripts/stage-claude-creds.sh).
test-e2e-subscription:
	@echo "Running live SUBSCRIPTION e2e (inject-on attach + inject-off escape hatch; restarts wardynd)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-subscription.sh

# Live BYOI e2e: an operator-supplied base image (stock/harness/hostile/
# nonexistent) is wrapped with the runner tools and every sandbox control is
# proven to hold, including the fail-closed agent-run --selftest launch gate.
# Needs Docker + the envbuild path; the script self-skips without
# WARDYN_TEST_DOCKER=1.
test-e2e-byoi:
	@echo "Running live BYOI e2e (wrap + selftest gate; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-byoi.sh

govulncheck:
	@echo "Running govulncheck (tagless + -tags docker, the shipped build)..."
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags docker ./...

staticcheck:
	@echo "Running staticcheck (tagless + -tags docker)..."
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags docker ./...

# go.mod/go.sum must stay tidy: `go mod tidy` produces no diff. A stray require,
# or an indirect that a test/code now imports directly (e.g. moby/docker-image-spec
# used by internal/envbuild/fake_test.go), would otherwise drift uncaught. `-diff`
# prints the tidy diff and exits nonzero when untidy WITHOUT mutating go.mod/go.sum,
# so it never clobbers uncommitted edits (unlike a tidy + git checkout dance).
tidy-check:
	@echo "Checking go.mod/go.sum are tidy (go mod tidy -diff must be empty)..."
	go mod tidy -diff

lint:
	@echo "Running go vet (default + docker tags)..."
	go vet ./...
	go vet -tags docker ./...
	@echo "Running golangci-lint $(GOLANGCI_LINT_VERSION) (function-size/complexity gate, .golangci.yml)..."
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	@echo "Running file-size gate (scripts/check-file-size.sh)..."
	./scripts/check-file-size.sh
	@echo "Running image-pin gate (scripts/check-image-pins.sh)..."
	./scripts/check-image-pins.sh

# ── CI supply-chain / deploy gates (single-sourced, called by ci.yml) ────────
# Each target below is the authority for one CI gate: ci.yml runs `make <target>`
# so the tool + version + flags live in exactly one place.

# Secret scan over full git history (NOT gitleaks-action, whose default scan
# range is only the triggering diff — see ci.yml's gitleaks-job comment).
gitleaks:
	@echo "Scanning full git history for secrets with gitleaks $(GITLEAKS_VERSION)..."
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git -c .gitleaks.toml -v

# Forbid copyleft / non-permissive Go dependencies. go-licenses has no -tags
# flag, so the docker-tagged deps (moby/moby/*, containerd/errdefs) are covered
# by driving the tag through GOFLAGS on the second pass (U112).
licenses:
	@echo "Checking Go dependency licenses (tagless + -tags docker)..."
	go run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check --disallowed_types=forbidden,restricted ./...
	GOFLAGS=-tags=docker go run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check --disallowed_types=forbidden,restricted ./...

# Helm chart lint + template-render (must render the load-bearing objects).
helm-lint:
	@echo "Linting + rendering the Helm chart..."
	helm lint ./deploy/helm/wardyn
	@out=$$(helm template wardyn ./deploy/helm/wardyn); \
	echo "$$out" | grep -q "kind: Deployment" || { echo "chart rendered no Deployment"; exit 1; }; \
	echo "$$out" | grep -q "kind: Service" || { echo "chart rendered no Service"; exit 1; }; \
	echo "$$out" | grep -q "kind: NetworkPolicy" || { echo "chart rendered no NetworkPolicy (default-on L0 egress control)"; exit 1; }; \
	echo "$$out" | grep -q "runAsNonRoot: true" || { echo "chart rendered no runAsNonRoot: true securityContext"; exit 1; }; \
	echo "$$out" | grep -q "readOnlyRootFilesystem: true" || { echo "chart rendered no readOnlyRootFilesystem: true securityContext"; exit 1; }

# Validate the docker-compose file parses (does NOT need a running daemon).
compose-config:
	@echo "Validating docker-compose config..."
	docker compose -f $(COMPOSE_FILE) config >/dev/null

# DCO sign-off: every non-merge commit in DCO_RANGE carries a Signed-off-by.
# CI passes the PR range (BASE..HEAD); default is origin/main..HEAD for local use.
DCO_RANGE ?= origin/main..HEAD
dco:
	@echo "Checking DCO sign-off (Signed-off-by) over: $(DCO_RANGE)..."
	@COMMITS=$$(git log --no-merges $(DCO_RANGE) --format='%H') || { echo "ERROR: git log failed for DCO_RANGE=$(DCO_RANGE) (bad/unreachable range) — failing closed"; exit 1; }; \
	MISSING=0; \
	for sha in $$COMMITS; do \
	  msg=$$(git log -1 --format='%B' "$$sha"); \
	  if ! echo "$$msg" | grep -qE '^Signed-off-by: .+ <.+@.+>'; then \
	    echo "MISSING Signed-off-by in commit $$sha:"; \
	    echo "$$msg" | head -3; \
	    MISSING=$$((MISSING+1)); \
	  fi; \
	done; \
	if [ "$$MISSING" -gt 0 ]; then \
	  echo ""; \
	  echo "ERROR: $$MISSING commit(s) lack a Signed-off-by trailer."; \
	  echo "Add it with: git commit --signoff (or git commit -s)"; \
	  exit 1; \
	fi; \
	echo "All commits carry Signed-off-by. DCO check passed."

# CycloneDX SBOM via syft (release stub; installs syft if absent, pinned).
sbom:
	@echo "Generating CycloneDX SBOM via syft $(SYFT_VERSION)..."
	@command -v syft >/dev/null 2>&1 || curl -sSfL https://raw.githubusercontent.com/anchore/syft/$(SYFT_VERSION)/install.sh | sh -s -- -b /usr/local/bin $(SYFT_VERSION)
	syft . -o cyclonedx-json > wardyn-sbom.cdx.json

# Fail closed on a copyleft license in a SHIPPED (prod) UI dependency.
npm-license:
	@echo "Checking UI production dependency licenses (no copyleft)..."
	./scripts/check-ui-licenses.sh

# ── daemon-free merge gate ───────────────────────────────────────────────────
# green `make ci` != CI is green. This runs the merge-gating checks
# that need NO Docker daemon and NO live service — it deliberately EXCLUDES
# test-conformance-docker, every WARDYN_TEST_DOCKER e2e lane, the Postgres suite
# (test-pg), the Playwright UI e2e (ui-e2e), and the push-only sbom stub. CI
# remains the authority; use this locally to catch most failures before pushing.
ci: build build-docker lint cover-check test-race staticcheck govulncheck \
    license-headers licenses gitleaks helm-lint compose-config dco diagrams \
    npm-license ui-typecheck ui-test ui test-conformance-stub
	@echo ""
	@echo "make ci PASSED (daemon-free merge gate). NOT covered here:"
	@echo "  test-conformance-docker, the WARDYN_TEST_DOCKER e2e lanes, the"
	@echo "  Postgres suite (test-pg), the Playwright UI e2e (ui-e2e), and the"
	@echo "  push-only SBOM stub — confirm CI is green before merging."

ui:
	@echo "Building embedded web UI..."
	cd ui && pnpm install --frozen-lockfile && pnpm build

ui-typecheck:
	@echo "Typechecking web UI (tsc --noEmit)..."
	cd ui && pnpm install --frozen-lockfile && pnpm typecheck

ui-test:
	@echo "Running web UI unit/component tests (vitest + coverage)..."
	cd ui && pnpm install --frozen-lockfile && pnpm test:coverage

# Playwright UI e2e against a seeded none-runner backend. Each spec runs against a
# FRESHLY SEEDED backend (deterministic isolation). Requires Docker (Postgres) and
# the Playwright chromium browser (`cd ui && pnpm exec playwright install chromium`).
test-e2e-ui:
	@echo "Running Playwright UI e2e (fresh seed per spec)..."
	cd ui && pnpm install --frozen-lockfile
	./scripts/run-ui-e2e.sh

# regenerates docs/img UI screenshots; run after visible UI changes and commit the diff.
screenshots:
	./scripts/screenshots.sh

# HOST mode (the only supported deployment for now): wardynd runs as you and uses
# your existing Claude login (no re-login, no stale credential copy). Team/compose
# mode is a coming-soon feature. In a terminal this PROMPTS for each credential
# (staging, AWS, SCM); a headless run (no TTY) skips them unless WARDYN_STAGE_CLAUDE=1
# / WARDYN_IMPORT_AWS=1 / WARDYN_IMPORT_SCM=1 are set.
setup:
	@echo "Wardyn setup (host mode) — detects your host, prompts for each credential, launches + opens the UI..."
	./scripts/setup.sh

# Stage the resident Claude login for per-run subscription mounts, even headless.
# Re-runs setup with staging forced; a running wardynd is restarted so it loads
# the just-generated subscription ceiling. Idempotent — safe to re-run anytime
# (e.g. after a headless `make setup` skipped the staging prompt).
stage-claude:
	WARDYN_STAGE_CLAUDE=1 WARDYN_SETUP_MODE=local ./scripts/setup.sh

# Stop the background host-mode wardynd started by `make setup`.
# (Team/compose mode is stopped with `make compose-down`.)
stop-host:
	@pid=$$(cat $$HOME/.wardyn/host-wardynd.pid 2>/dev/null); \
	if [ -n "$$pid" ] && kill -0 $$pid 2>/dev/null; then \
	  kill $$pid && rm -f $$HOME/.wardyn/host-wardynd.pid && echo "Stopped host-mode wardynd (PID $$pid)."; \
	else \
	  echo "No running host-mode wardynd found (no live PID in ~/.wardyn/host-wardynd.pid)."; \
	  rm -f $$HOME/.wardyn/host-wardynd.pid; \
	fi

# Deliberate clean slate: `make compose-down` KEEPS the named volumes (runs +
# append-only audit + recordings survive by design); `make reset` wipes them and
# re-runs setup, so you land on an EMPTY Runs list.
reset:
	@echo "Resetting local Wardyn (wipes volumes: runs + audit + recordings, then re-up)..."
	./scripts/up.sh reset

# FULL undo across BOTH modes: stops the host daemon, removes the compose stack
# + volumes, the stray wardyn-internal network, wardyn-test-pg, and the
# ~/.wardyn install files (allowlist — staged Claude creds included; the rest of
# ~/.wardyn is preserved). Keeps the age key (.env) and built images unless
# ARGS='--purge-env' / '--purge-images'. Leaves the box clean — no re-up.
reset-all:
	./scripts/up.sh reset-all $(ARGS)

doctor:
	./scripts/up.sh doctor

dev-pg:
	./scripts/up.sh pg

compose-build:
	@echo "Building compose images..."
	docker compose -f $(COMPOSE_FILE) build
	docker compose -f $(COMPOSE_FILE) --profile build-only build proxy-image

compose-up:
	@echo "Starting docker-compose stack..."
	docker compose -f $(COMPOSE_FILE) up -d postgres dex wardynd

compose-down:
	@echo "Stopping docker-compose stack..."
	docker compose -f $(COMPOSE_FILE) down

demo:
	@echo "Running the Wardyn compose demo..."
	./scripts/demo.sh

test-drive:
	@echo "Running the Wardyn governance test-drive (brings the stack up; override with ARGS=...)..."
	./scripts/test-drive.sh $(if $(ARGS),$(ARGS),--up)

clean:
	@echo "Cleaning built binaries..."
	rm -f bin/*
	rm -rf dist/

# Validate every fenced mermaid diagram in the public docs: parses/renders via
# mermaid-cli and each load-bearing label still exists at its cited source.
diagrams:
	./scripts/check-diagrams.sh

# Check SPDX license headers on source files (or apply with: make license-headers ARGS=fix).
license-headers:
	@if [ "$(ARGS)" = "fix" ]; then ./scripts/add-license-headers.sh; else ./scripts/check-license-headers.sh; fi
