.PHONY: test-gaps license-headers diagrams build build-docker build-k8s test test-docker lint ui compose-build compose-up compose-down demo clean test-conformance-docker test-conformance-k8s build-conformance-agent-image test-conformance-stub test-envbuild-integration govulncheck staticcheck agent-images test-drive help test-report test-report-pg test-report-docker test-report-k8s cover-check release-check ui-test ui-typecheck test-e2e test-e2e-concurrent test-e2e-live test-e2e-subscription test-e2e-byoi test-e2e-ssh test-e2e-ssh-k8s test-e2e-ui-sandbox test-e2e-ui screenshots record-demo setup stage-claude stop-host reset reset-all doctor dev-pg agent-images-core test-race tidy-check agent-image-full agent-image-vscode gitleaks licenses helm-lint helm-install-test kind-quickstart kind-down compose-config dco sbom npm-license npm-audit ci

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
# Native-binary agent installs (opt-in; npm stays the default). Behind a proxy
# where public npm is blocked, install the native CLI instead:
#   make agent-images-core CLAUDE_INSTALL=native            # checksum-verified download
#   make agent-images-core CLAUDE_INSTALL=native CLAUDE_CODE_VERSION=2.1.215
#   scripts/stage-agent-binary.sh codex-cli && make agent-images-core CODEX_INSTALL=native
CLAUDE_INSTALL      ?=
CODEX_INSTALL       ?=
CLAUDE_CODE_VERSION ?=
# Emit "--build-arg NAME=VALUE" only when VALUE is non-empty, so an unset knob
# never overrides a Dockerfile default with an empty string.
_build_arg = $(if $(2),--build-arg $(1)="$(2)",)
DOCKER_BUILD_ARGS = \
	$(call _build_arg,NPM_REGISTRY,$(NPM_REGISTRY)) \
	$(call _build_arg,HTTP_PROXY,$(HTTP_PROXY)) \
	$(call _build_arg,HTTPS_PROXY,$(HTTPS_PROXY)) \
	$(call _build_arg,NO_PROXY,$(NO_PROXY)) \
	$(call _build_arg,CLAUDE_INSTALL,$(CLAUDE_INSTALL)) \
	$(call _build_arg,CODEX_INSTALL,$(CODEX_INSTALL)) \
	$(call _build_arg,CLAUDE_CODE_VERSION,$(CLAUDE_CODE_VERSION))

# Self-describing help: the description lives on the target line as a `##`
# comment, so it cannot drift out of step with the target list the way the
# hand-echoed block it replaced did (that block silently omitted `ci`, the merge
# gate, plus 9 other targets). Operator detail too long for one line lives in the
# comment block above each recipe.
help:
	@echo "Wardyn governance control plane. Targets:"
	@awk -F':.*##' '/^[a-z0-9-]+:.*##/{printf "  %-24s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

# Core = the two real agent harnesses a user actually runs. The oracle image is
# a deterministic e2e stand-in (no LLM) — dev/e2e only, so setup paths build
# core and the e2e scripts build oracle themselves.
agent-images-core: ## Build the user-facing agent images (claude-code + codex-cli)
	@echo "Building agent images (build context: repo root)..."
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:local .
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/codex-cli/Dockerfile   -t wardyn/agent-codex-cli:local   .
	@echo "Agent images built: wardyn/agent-claude-code:local  wardyn/agent-codex-cli:local"

agent-images: agent-images-core ## Build all agent OCI images (core + oracle e2e stand-in + aws-sso)
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/oracle/Dockerfile      -t wardyn/agent-oracle:local      .
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/aws-sso/Dockerfile     -t wardyn/agent-aws-sso:local     .
	@echo "Oracle e2e image built: wardyn/agent-oracle:local"
	@echo "AWS SSO login image built: wardyn/agent-aws-sso:local"

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
agent-image-full: agent-images-core ## Build the fat toolchain agent image (Go/Python/Rust/JDK/pnpm)
	@echo "Building the fat toolchain image (Go/Python/Rust/JDK/pnpm)..."
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/full/Dockerfile -t wardyn/agent-full:local .
	@echo "Full toolchain image built: wardyn/agent-full:local"

# The claude-code agent image plus a pinned code-server, for the UI-sandbox
# relay's "vscode" app (Workstream D, deploy/images/vscode/Dockerfile). Not in
# agent-images-core/agent-images: it is +~300MB and only a run whose policy
# declares a ui_apps entry needs it. Register it under an agent name with:
#   WARDYN_AGENT_IMAGES='{"vscode":"wardyn/agent-vscode:local"}'
agent-image-vscode: agent-images-core ## Build the code-server UI-sandbox agent image
	@echo "Building the code-server UI-sandbox image..."
	docker build $(DOCKER_BUILD_ARGS) -f deploy/images/vscode/Dockerfile -t wardyn/agent-vscode:local .
	@echo "Vscode UI-sandbox image built: wardyn/agent-vscode:local"

build: ## Build Go binaries (default tags)
	@echo "Building Go binaries..."
	go build ./...

build-docker: ## Build Go binaries with -tags docker
	@echo "Building Go binaries (-tags docker)..."
	go build -tags docker ./...

build-k8s: ## Build Go binaries with -tags k8s
	@echo "Building Go binaries (-tags k8s)..."
	go build -tags k8s ./...

test: ## Run all Go tests
	@echo "Running Go tests..."
	go test ./...

# Race-detector sweep. The kill/dispatch FSM has dedicated concurrent tests
# (internal/api/kill_dispatch_race_test.go) that only mean something under -race;
# the rest of the tree rides along. Required green before restructuring runs.go.
# BOTH tag sets: the tagless pass alone never compiles the -tags docker runner
# tree (internal/runner/docker, internal/envbuild) — the concurrency-heavy
# sandbox lifecycle — so it would get zero race coverage. The docker-tagged pass
# needs no daemon (the real-Docker cases self-skip unless WARDYN_TEST_DOCKER=1).
test-race: ## Race-detector sweep over BOTH tag sets (tagless + -tags docker)
	@echo "Running Go tests under the race detector (tagless)..."
	go test -race ./...
	@echo "Running Go tests under the race detector (-tags docker)..."
	go test -race -tags docker ./...

test-docker: ## Run all Go tests with -tags docker
	@echo "Running Go tests (-tags docker)..."
	go test -tags docker ./...

# ── detailed test reports (JSON event stream + coverage) ────────────────────
# Regenerates docs/TEST-GAPS.md (the triaged untested-exported-func inventory)
# from the union coverage profile. Run after cover-check when the inventory
# should track a change; the doc states its own regeneration command, which is
# the wiring whose absence got the last copy deleted as unmaintainable.
test-gaps: ## Regenerate docs/TEST-GAPS.md from test/reports/go coverage output
	./scripts/test-gaps.sh

# Emits per-suite artifacts under test/reports/go/<suite>/. See
# scripts/test-report.sh.
test-report: ## Go unit suite with per-suite JSON + coverage artifacts
	@echo "Running Go unit suite with detailed reports..."
	./scripts/test-report.sh unit ./...

test-report-pg: ## Postgres-gated suite with reports (needs WARDYN_TEST_PG)
	@echo "Running Postgres-gated suite with reports (requires WARDYN_TEST_PG)..."
	./scripts/test-report.sh pg \
		./internal/store/... ./internal/db/... ./internal/secretstore/... ./internal/broker/... \
		./internal/api/... ./test/apie2e/... ./internal/recording/... ./cmd/wardynd/...

# The whole tree under -tags docker, so the container-hardening driver
# (internal/runner/docker), internal/envbuild and the wardynd wiring that calls
# them — none of which the tagless build can even compile — are actually tested
# and measured. No daemon needed: the real-Docker cases self-skip unless
# WARDYN_TEST_DOCKER=1, leaving the fakeDocker-backed tests to run anywhere.
test-report-docker: ## -tags docker suite with reports (fakeDocker; no daemon needed)
	@echo "Running docker-tagged suite with reports (fakeDocker; WARDYN_TEST_DOCKER=1 adds the real-daemon cases)..."
	./scripts/test-report.sh docker -tags docker ./...

# The whole tree under -tags k8s, so the k8s confinement substrate
# (internal/runner/k8s) and the wardynd wiring that calls it — none of which
# the tagless build can even compile — are actually tested and measured. No
# cluster needed: the real-cluster case (test/conformance's TestConformanceK8s)
# self-skips unless WARDYN_TEST_K8S=1, leaving the fake-clientset-backed unit
# tests (internal/runner/k8s/*_test.go) to run anywhere.
test-report-k8s: ## -tags k8s suite with reports (fake clientset; no cluster needed)
	@echo "Running k8s-tagged suite with reports (fake clientset; WARDYN_TEST_K8S=1 + a kubeconfig adds the real-cluster conformance case)..."
	./scripts/test-report.sh k8s -tags k8s ./...

# Coverage floor gate. Override with `make cover-check COVER_MIN=NN`.
# Enforced over the UNION of all three shipped builds (tagless + -tags docker +
# -tags k8s), not the tagless subset alone — measuring only the tagless build
# reported a number for code that is not what ships. Pulling the excluded
# packages in moved the honest total from 67.1% (tagless-only) to 66.1%
# (docker union); the floor sits just under that with a small margin for
# routine churn. Raise it as coverage climbs.
# scripts/cover-union.sh documents exactly what is and is not counted.
COVER_MIN ?= 65
cover-check: test-report test-report-docker test-report-k8s ## Enforce the COVER_MIN floor over ALL THREE shipped builds, unioned
	@./scripts/cover-union.sh --self-test
	@./scripts/cover-union.sh $(COVER_MIN) test/reports/go/union \
		test/reports/go/unit/cover.out test/reports/go/docker/cover.out test/reports/go/k8s/cover.out

# ── pre-tag release gate ────────────────────────────────────────────────────
# `ci` PLUS the release-only checks. It used to be a hand-copied subset of ci's
# prerequisites, which made the PRE-TAG gate WEAKER than the MERGE gate (it
# skipped diagrams, helm-lint, compose-config, dco, npm-license and every UI
# job). Depending on `ci` means adding a gate to ci automatically strengthens
# the release gate. It PUSHES NOTHING and TAGS NOTHING.
#
# WARDYN_TEST_PG adds the Postgres lane (CI always runs it; local runs say so
# loudly when it is skipped). Still not a full CI replica: five jobs need a
# live daemon or service — conformance, conformance-k8s, envbuild-integration,
# helm-install-test, and the Playwright ui-e2e — and are CI-only. See
# RELEASING.md.
release-check: ci ## Pre-tag gate: make ci + CHANGELOG (+ PG lane)
	@grep -q "## \[Unreleased\]" CHANGELOG.md || (echo "CHANGELOG missing [Unreleased]"; exit 1)
	@if [ -n "$$WARDYN_TEST_PG" ]; then \
	  echo "==> Postgres-gated suite"; $(MAKE) test-report-pg; \
	else \
	  echo ">> SKIPPED test-report-pg — set WARDYN_TEST_PG=postgres://... to run it (CI always does)"; \
	fi
	@echo ""
	@echo "release-check PASSED. NOT covered here: conformance, conformance-k8s,"
	@echo "envbuild-integration, helm-install-test, the Playwright ui-e2e, and"
	@echo "screenshot freshness (ci.yml's screenshots-fresh job owns that — a local"
	@echo "commit-timestamp test cannot be cleared once the PNGs re-render"
	@echo "byte-identical) — confirm CI is green on the commit before tagging."

test-conformance-docker: ## Run the conformance suite on Docker (needs WARDYN_TEST_DOCKER=1)
	@echo "Running conformance tests on Docker (WARDYN_TEST_DOCKER=1 required)..."
	WARDYN_TEST_DOCKER=1 go test -v -tags docker -timeout 10m ./test/conformance/...

test-conformance-k8s: ## Run the conformance suite on Kubernetes (needs WARDYN_TEST_K8S=1 + a kubeconfig context)
	@echo "Running conformance tests on Kubernetes (WARDYN_TEST_K8S=1 + WARDYN_PROXY_IMAGE + WARDYN_TEST_K8S_AGENT_IMAGE required; uses the current kubeconfig context)..."
	WARDYN_TEST_K8S=1 go test -v -tags k8s -timeout 10m ./test/conformance/...

# H1 (review round 2): the conformance agent image MUST carry wardyn-rec —
# k8s's SessionRecording is unconditionally true (exec.go's recordCmd has no
# opt-out, unlike docker's Config.Record), so a bare busybox took the
# never-exercises-recording path and the conformance gate never actually
# proved recording works. Stages a fresh wardyn-rec binary (not a
# builder-stage duplicate) into a throwaway build context — mirrors
# test-envbuild-integration's own tools-staging recipe.
CONFORMANCE_AGENT_IMAGE ?= wardyn/conformance-agent:local
build-conformance-agent-image: ## Build the busybox+wardyn-rec conformance agent image (deploy/kind/Dockerfile.conformance-agent)
	@echo "Building the conformance agent image ($(CONFORMANCE_AGENT_IMAGE))..."
	@set -eu; \
	tools_dir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tools_dir"' EXIT; \
	CGO_ENABLED=0 GOOS=linux go build -o "$$tools_dir/wardyn-rec" ./cmd/wardyn-rec; \
	docker build -f deploy/kind/Dockerfile.conformance-agent -t $(CONFORMANCE_AGENT_IMAGE) "$$tools_dir"

test-conformance-stub: ## Run the driver-agnostic conformance honesty stub (no cluster needed)
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
test-envbuild-integration: ## Real-daemon envbuild push/pull smoke test (needs Docker)
	@echo "Running real-daemon envbuild integration tests (U064; requires Docker)..."
	@set -eu; \
	tools_dir="$$(mktemp -d)"; \
	trap 'docker rm -f wardyn-envbuild-registry >/dev/null 2>&1 || true; rm -rf "$$tools_dir"' EXIT; \
	echo "==> staging runner tools into $$tools_dir"; \
	go build -o "$$tools_dir/" ./cmd/wardyn-rec ./cmd/wardyn-git-helper; \
	cp deploy/images/claude-code/agent-run "$$tools_dir/agent-run"; \
	cp deploy/images/common/agent-run-lib.sh "$$tools_dir/agent-run-lib.sh"; \
	chmod +x "$$tools_dir/agent-run" "$$tools_dir/agent-run-lib.sh"; \
	echo "==> starting throwaway registry $(ENVBUILD_REGISTRY_IMAGE) on :5000"; \
	docker rm -f wardyn-envbuild-registry >/dev/null 2>&1 || true; \
	docker run -d --name wardyn-envbuild-registry -p 5000:5000 $(ENVBUILD_REGISTRY_IMAGE) >/dev/null; \
	echo "==> running the real-daemon envbuild tests (git-clone smoke + agent-CLI bake proof)"; \
	WARDYN_TEST_DOCKER=1 \
	WARDYN_TEST_CACHE_REPO=localhost:5000/wardyn-envbuild-test \
	WARDYN_TEST_TOOLS_DIR="$$tools_dir" \
	go test -tags docker -run 'TestBuild_SmokeDockerd|TestBuildFromDevcontainerFiles_BakesAgentCLI' -timeout 40m -v ./internal/envbuild/

# Live full-stack security e2e (L0 egress, metadata block, kill cascade,
# brokered creds, recording). Heavy: stands up the compose stack. Guarded by
# WARDYN_TEST_DOCKER=1 inside the script. Runs in the nightly workflow.
test-e2e: ## Live security e2e: L0 egress, metadata block, kill cascade (needs Docker)
	@echo "Running live security e2e (requires Docker; WARDYN_TEST_DOCKER=1)..."
	WARDYN_TEST_DOCKER=1 ./test/e2e/e2e.sh

# Shared-host concurrency acceptance test: two project-scoped stacks up at once,
# proving no name/network/volume/port collision, cross-network isolation, and that
# one job's `down --volumes` never tears down another's. Needs wardyn/wardynd:local.
test-e2e-concurrent: ## Two project-scoped stacks up at once: no collision, net isolation
	@echo "Running 2-job shared-host concurrency test (requires Docker + wardyn/wardynd:local)..."
	@docker image inspect wardyn/wardynd:local >/dev/null 2>&1 || $(MAKE) -s compose-build
	./scripts/test-concurrent.sh

# Live TASK e2e: real sandboxes running the test/e2e/tasks corpus, graded on
# final workspace STATE (did the agent actually do the work?), plus per-tier
# allow/block confinement, interactive PTY, and recording-replay. The $0 oracle
# lane runs by default; add WARDYN_E2E_REAL_MODEL=1 (+ staged creds via
# scripts/stage-claude-creds.sh) for the real claude-code lane. Guarded by
# WARDYN_TEST_DOCKER=1 inside the script.
test-e2e-live: ## Live TASK e2e: real sandboxes run the corpus, graded on state
	@echo "Running live TASK e2e (real sandboxes + graders; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-live.sh

# Live SUBSCRIPTION e2e: proves proxy-side OAuth-token injection end-to-end. The
# driver RESTARTS wardynd with WARDYN_SUBSCRIPTION_INJECT flipped to run both the
# inject-on attach-walkthrough and the inject-off escape-hatch lane, then restores
# the safe default. Needs Docker + staged Claude creds (scripts/stage-claude-creds.sh).
test-e2e-subscription: ## Live SUBSCRIPTION e2e: inject-on attach + inject-off escape hatch
	@echo "Running live SUBSCRIPTION e2e (inject-on attach + inject-off escape hatch; restarts wardynd)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-subscription.sh

# Live BYOI e2e: an operator-supplied base image (stock/harness/hostile/
# nonexistent) is wrapped with the runner tools and every sandbox control is
# proven to hold, including the fail-closed agent-run --selftest launch gate.
# Needs Docker + the envbuild path; the script self-skips without
# WARDYN_TEST_DOCKER=1.
test-e2e-byoi: ## Live BYOI e2e: wrap stock/harness/hostile bases + selftest gate
	@echo "Running live BYOI e2e (wrap + selftest gate; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-byoi.sh

# Live SSH gateway e2e: exec exit-code propagation, sftp put/get byte-compare,
# a -L forward against an in-sandbox loopback listener, the ssh.exec/ssh.sftp/
# ssh.forward audit rows, a saved ssh-<session> recording, a foreign-key
# denial, and the concurrency case (sftp transfer + a second exec, same run).
# Brings up its OWN dedicated compose stack (project "wardynv05e2e", ports
# 18080/15432/12222) and tears it down after — never touches another stack.
# Needs Docker + real ssh/sftp/jq clients; the script self-skips without
# WARDYN_TEST_DOCKER=1.
test-e2e-ssh: ## Live SSH gateway e2e: exec/sftp/-L forward/recording/denial/concurrency
	@echo "Running live SSH gateway e2e (dedicated compose stack; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-ssh.sh

# The same gateway on the OTHER substrate: the sandbox is a Pod, reached
# through Runner.Attach/ExecStream's k8s implementation. Proves the exec lane
# ('id' exits 0 + exit-code propagation), the pty lane, and the ssh.exec +
# session.attach{transport:ssh} audit rows. sftp/-L are skipped OUT LOUD (an
# image contract, not a substrate one — the script says why, docs/SSH.md
# "Image contract (BYOI)" is canonical); the compose lane above proves those.
# Runs against the cluster `make kind-quickstart` leaves behind — it never
# creates or deletes one — and self-skips without WARDYN_TEST_K8S=1.
test-e2e-ssh-k8s: ## Live SSH gateway e2e on Kubernetes (needs a kind-quickstart cluster up)
	@echo "Running live SSH gateway e2e on Kubernetes (existing kind-quickstart cluster; shell lane only)..."
	WARDYN_TEST_K8S=1 ./scripts/run-e2e-ssh-k8s.sh

# Live UI-sandbox gateway e2e: the ticket -> enter -> cookie -> code-server
# handoff on the SECOND origin, the ui.* audit rows (and the session.attach row
# that must NOT appear), an undeclared app and a foreign run's ticket both
# refused 403, the inbound/outbound header strips asserted against an
# in-sandbox echo responder, and the exec baseline (relayed socat execs pooled,
# then reaped by the idle timeout). Brings up its OWN dedicated compose stack
# (project "wardynv06uisbx", ports 18081/15433/18083) with both listeners wired
# and tears it down after -- never touches another stack. Needs Docker + curl +
# jq and the code-server image (built on demand: make agent-image-vscode);
# self-skips without WARDYN_TEST_DOCKER=1.
test-e2e-ui-sandbox: ## Live UI-sandbox relay e2e: handoff/audit/403s/header strips/exec baseline
	@echo "Running live UI-sandbox gateway e2e (dedicated compose stack; requires Docker)..."
	WARDYN_TEST_DOCKER=1 ./scripts/run-e2e-ui-sandbox.sh

govulncheck: ## Scan for known vulnerabilities (tagless + -tags docker + -tags k8s)
	@echo "Running govulncheck (tagless + -tags docker + -tags k8s, the shipped builds)..."
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags docker ./...
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags k8s ./...

staticcheck: ## Static analysis (tagless + -tags docker + -tags k8s)
	@echo "Running staticcheck (tagless + -tags docker + -tags k8s)..."
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags docker ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags k8s ./...

# go.mod/go.sum must stay tidy: `go mod tidy` produces no diff. A stray require,
# or an indirect that a test/code now imports directly (e.g. moby/docker-image-spec
# used by internal/envbuild/fake_test.go), would otherwise drift uncaught. `-diff`
# prints the tidy diff and exits nonzero when untidy WITHOUT mutating go.mod/go.sum,
# so it never clobbers uncommitted edits (unlike a tidy + git checkout dance).
tidy-check: ## Fail if go.mod/go.sum are untidy (go mod tidy -diff)
	@echo "Checking go.mod/go.sum are tidy (go mod tidy -diff must be empty)..."
	go mod tidy -diff

lint: ## go vet (all tag sets) + golangci-lint size/complexity + file-size gate
	@echo "Running go vet (default + docker + k8s tags)..."
	go vet ./...
	go vet -tags docker ./...
	go vet -tags k8s ./...
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
gitleaks: ## Scan the FULL git history for committed secrets
	@echo "Scanning full git history for secrets with gitleaks $(GITLEAKS_VERSION)..."
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git -c .gitleaks.toml -v

# Forbid copyleft / non-permissive Go dependencies. go-licenses has no -tags
# flag, so the docker-tagged deps (moby/moby/*, containerd/errdefs) and the
# k8s-tagged deps (k8s.io/client-go et al) are covered by driving the tag
# through GOFLAGS on the second/third pass (U112).
licenses: ## Forbid copyleft/non-permissive Go dependencies (all three tag sets)
	@echo "Checking Go dependency licenses (tagless + -tags docker + -tags k8s)..."
	go run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check --disallowed_types=forbidden,restricted ./...
	GOFLAGS=-tags=docker go run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check --disallowed_types=forbidden,restricted ./...
	GOFLAGS=-tags=k8s go run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check --disallowed_types=forbidden,restricted ./...

# Helm chart lint + template-render (must render the load-bearing objects).
#
# EIGHT renders, because ONE render only ever exercises the default branch of
# every {{ if }} in templates/ — and every hardening switch this chart has is
# off/other-side by default:
#   1. defaults (+ an admin token, which the chart now requires): the
#      external-Secret / persistence-off / created-ServiceAccount /
#      k8s-runner-off / ssh-off side;
#   2. ci/all-on-values.yaml — the other side of each of those in one go,
#      INCLUDING k8s.enabled (a different runsNamespace, exercising the
#      control-plane NetworkPolicy's extra ingress peer) and ssh.enabled;
#   3-7. the five refusals, asserted BY MESSAGE: a render that fails for the
#      wrong reason is a false green, which is the whole point of these guards.
#   8. the replicas refusal's documented override, asserted to still RENDER —
#      a guard with no way past would be a wall, not a guard.
# No kubeconform: it resolves schemas at runtime from an unpinned upstream ref,
# which would trade a network-free gate for a flaky one and break the pinning
# discipline scripts/check-image-pins.sh exists to enforce.
helm-lint: ## Lint + template-render the Helm chart (default + all-on values + the refusals)
	@echo "Linting + rendering the Helm chart..."
	helm lint ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true
	@out=$$(helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true); \
	echo "$$out" | grep -q "kind: Deployment" || { echo "chart rendered no Deployment"; exit 1; }; \
	echo "$$out" | grep -q "kind: Service" || { echo "chart rendered no Service"; exit 1; }; \
	echo "$$out" | grep -q "kind: NetworkPolicy" || { echo "chart rendered no NetworkPolicy (default-on L0 egress control)"; exit 1; }; \
	echo "$$out" | grep -q "runAsNonRoot: true" || { echo "chart rendered no runAsNonRoot: true securityContext"; exit 1; }; \
	echo "$$out" | grep -q "readOnlyRootFilesystem: true" || { echo "chart rendered no readOnlyRootFilesystem: true securityContext"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_ADMIN_TOKEN" || { echo "chart rendered no WARDYN_ADMIN_TOKEN — the API would 401 every request"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_RECORDING_DIR" || { echo "chart left WARDYN_RECORDING_DIR unset — wardynd's default writes to the read-only root FS and the pod crash-loops"; exit 1; }; \
	echo "$$out" | grep -A1 "name: WARDYN_RECORDING_STORE" | grep -q 'value: "fs"' || { echo "chart no longer pins WARDYN_RECORDING_STORE=fs — with wardynd's pg default a stock install silently persists every PTY asciicast into Postgres, forever, while values.yaml/README say recording is off"; exit 1; }; \
	echo "$$out" | grep -q "podSelector: {}" || { echo "chart ingress default is not same-namespace"; exit 1; }; \
	[ "$$(echo "$$out" | grep -c 'namespaceSelector: {}')" = "1" ] || { echo "unexpected namespaceSelector: {} peer (only the DNS egress rule may be cluster-wide)"; exit 1; }; \
	[ "$$(echo "$$out" | grep -c 'automountServiceAccountToken: false')" = "2" ] || { echo "default render does not show automount:false exactly twice (the created ServiceAccount object + the pod spec) — k8s.enabled and ssh.enabled both default off, so both must still default-deny the API server token"; exit 1; }; \
	echo "$$out" | grep -A14 "readinessProbe:" | grep -q 'path: "/readyz"' || { echo "readinessProbe no longer targets /readyz — a dead Postgres would read healthy again (W28-S1-7)"; exit 1; }; \
	[ "$$(echo "$$out" | grep -c 'path: /healthz')" = "2" ] || { echo "expected exactly 2 probes still on /healthz (liveness + startup)"; exit 1; }
	@out=$$(helm template wardyn ./deploy/helm/wardyn -f deploy/helm/wardyn/ci/all-on-values.yaml); \
	echo "$$out" | grep -q "kind: PersistentVolumeClaim" || { echo "persistence.enabled rendered no PVC"; exit 1; }; \
	echo "$$out" | grep -q 'value: "/data/recordings"' || { echo "WARDYN_RECORDING_DIR does not follow the persistent mount"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_AGE_KEY" || { echo "inline secrets.ageKey is not injected — every stored secret dies on restart"; exit 1; }; \
	echo "$$out" | grep -q "name: wardyn-oidc" || { echo "extraEnv did not render (secret-bearing env has no secretKeyRef path)"; exit 1; }; \
	echo "$$out" | grep -q "name: regcred" || { echo "image.pullSecrets did not render"; exit 1; }; \
	echo "$$out" | grep -q "storageClassName: fast" || { echo "persistence.storageClass did not render"; exit 1; }; \
	echo "$$out" | grep -q "kubernetes.io/metadata.name: ingress-nginx" || { echo "networkPolicy.ingress.from did not render"; exit 1; }; \
	[ "$$(echo "$$out" | grep -c 'automountServiceAccountToken: true')" = "1" ] || { echo "pod spec does not honor the values-level automount override on the bring-your-own-SA path (true here: k8s.enabled requires it — see the fourth refusal)"; exit 1; }; \
	echo "$$out" | grep -A1 "name: WARDYN_RUNNER" | grep -q 'value: "k8s"' || { echo "k8s.enabled did not render WARDYN_RUNNER=k8s — the registry defaults to docker and would try to dial a nonexistent daemon"; exit 1; }; \
	echo "$$out" | grep -q "kubernetes.io/metadata.name: wardyn-runs" || { echo "k8s.runsNamespace (different from the release namespace) did not render its NetworkPolicy ingress peer"; exit 1; }; \
	echo "$$out" | grep -q "CC2=gvisor;CC3=kata-qemu" || { echo "k8s.runtimeClasses did not join into WARDYN_CONFINEMENT_MAP"; exit 1; }; \
	echo "$$out" | grep -q "port: 6443" || { echo "k8s.apiServer.ports did not render the control-plane NetworkPolicy's apiserver egress rule"; exit 1; }; \
	echo "$$out" | grep -q '^kind: Role$$' || { echo "k8s.enabled rendered no RBAC Role"; exit 1; }; \
	echo "$$out" | grep -q '^kind: ClusterRole$$' || { echo "k8s.enabled rendered no RBAC ClusterRole (runtimeclasses is cluster-scoped)"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_SSH_LISTEN" || { echo "ssh.enabled rendered no WARDYN_SSH_LISTEN"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_SSH_ADVERTISE" || { echo "ssh.enabled rendered no WARDYN_SSH_ADVERTISE"; exit 1; }; \
	echo "$$out" | grep -q "targetPort: ssh" || { echo "ssh.enabled rendered no ssh Service port"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_UI_SANDBOX_LISTEN" || { echo "uiSandbox.enabled rendered no WARDYN_UI_SANDBOX_LISTEN — the chart would publish a port with no gateway behind it"; exit 1; }; \
	echo "$$out" | grep -q "name: WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE" || { echo "uiSandbox.originTemplate did not render — every run would share one browser origin (threatmodel/THREAT-MODEL.md §5 #18)"; exit 1; }; \
	echo "$$out" | grep -q "targetPort: ui" || { echo "uiSandbox.enabled rendered no ui Service port"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn 2>&1 | grep -q "the public API would 401" || { echo "chart no longer refuses an install with neither an admin token nor an OIDC issuer"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set postgres.dsn.secretRef.name="" 2>&1 | grep -q "set either postgres.dsn" || { echo "chart no longer refuses an install with no DSN"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKey=fake 2>&1 | grep -q "secrets.ageKey applies to inline mode only" || { echo "chart no longer refuses an ageKey it would silently drop"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth 2>&1 | grep -q "no age identity is wired" || { echo "chart no longer refuses an external-DSN install with an ephemeral age key — boot 2 cannot decrypt what boot 1 wrote, so the pod crash-loops on its SECOND start and those rows are unrecoverable (W27-S1-5)"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.allowEphemeralAgeKey=true >/dev/null 2>&1 || { echo "secrets.allowEphemeralAgeKey no longer renders — the refusal has become a wall with no documented way past"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set env.WARDYN_AGE_KEY=AGE-SECRET-KEY-EXAMPLE >/dev/null 2>&1 || { echo "an age identity wired through .Values.env no longer satisfies the refusal — the chart refuses a render that is actually fine"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set readinessProbe.path=/healthz | grep -q 'path: "/healthz"' || { echo "readinessProbe.path no longer pins the probe back to /healthz — an image <= 0.5.0 serves no /readyz, so the pod would never become Ready and the rollout would hang"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set replicas=5 2>&1 | grep -q "replicas > 1 is refused" || { echo "chart no longer refuses replicas > 1 — the secret-masking registry is process-local and fails open, so a second replica can persist a recording with live credentials in cleartext"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set k8s.enabled=true --set k8s.proxyImage=example/wardyn-proxy:test --set serviceAccount.automount=false 2>&1 | grep -q "k8s.enabled requires serviceAccount.automount=true" || { echo "chart no longer refuses k8s.enabled with serviceAccount.automount=false — the k8s runner needs the API server"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set k8s.enabled=true --set serviceAccount.automount=true 2>&1 | grep -q "k8s.enabled requires k8s.proxyImage" || { echo "chart no longer refuses k8s.enabled with an empty k8s.proxyImage — the k8s runner substrate refuses to construct (errProxyImageUnset), a BOOT-time crash-loop, not a per-run one"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set replicas=5 --set allowMultiReplica=true >/dev/null 2>&1 || { echo "allowMultiReplica no longer renders — the refusal has become a wall with no documented way past"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set k8s.enabled=true --set k8s.proxyImage=example/wardyn-proxy:test --set serviceAccount.automount=true --set serviceAccount.create=false 2>&1 | grep -q "would bind the k8s-runner privileges" || { echo "chart no longer refuses k8s.enabled with serviceAccount.create=false and no serviceAccount.name — the RBAC binding would silently fall to the namespace default ServiceAccount"; exit 1; }
	@helm template wardyn ./deploy/helm/wardyn --set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true --set k8s.enabled=true --set k8s.proxyImage=example/wardyn-proxy:test --set serviceAccount.automount=true --set serviceAccount.create=false --set serviceAccount.name=my-existing-sa >/dev/null 2>&1 || { echo "serviceAccount.create=false with an explicit serviceAccount.name no longer renders — the refusal has become a wall with no documented way past"; exit 1; }

# ── kind Helm install-test (CI: ci.yml's helm-install-test job) ─────────────
# helm-lint above only proves the chart RENDERS; this proves an install
# actually CONVERGES to a booting, healthy control plane — not just valid
# YAML. Only the in-cluster kubectl/helm steps live here (repo convention:
# gate logic single-sourced in make); the kind cluster's own create/delete
# lifecycle is the CALLER's job (ci.yml's helm-install-test job, via
# helm/kind-action) — this target only needs a current kubeconfig context
# already pointed at a kind cluster with the target image already
# `kind load docker-image`-ed.
#
# Two install-time overrides below exist ONLY because real validation (a
# live kind install, not just `helm template`) surfaced two gaps helm-lint's
# render-only check cannot see — NEITHER is a chart DEFAULTS change
# (values.yaml itself is untouched):
#   - secrets.ageKeyFromSecret=true: the chart's own default (an ephemeral
#     age identity regenerated every boot) cannot survive ANY pod restart
#     once paired with a real (non-inline) Postgres — boot N encrypts
#     "wardyn-signing-key" under ephemeral key N, boot N+1 generates a
#     brand-new unrelated key and can never decrypt it again ("age decrypt:
#     no identity matched any of the recipients"), so the pod crash-loops
#     forever after its first restart. A stable key riding in the SAME
#     external Secret as the DSN (the chart's own documented mechanism for
#     exactly this case — see values.yaml) fixes it.
#   - (fixed at the image layer, deliberately NOT overridden here): the Go
#     -default-policy default is a RELATIVE path that never resolved from the
#     distroless image's WorkingDir — every default install crash-looped on
#     ENOENT until Dockerfile.wardynd gained
#     ENV WARDYN_DEFAULT_POLICY=/examples/policies/default.json. This target
#     installs WITHOUT a policy override precisely so it keeps proving the
#     default path boots.
#
# Two honest limits of this gate: (1) if the -gen-age-key docker run ever
# fails, the age-key literal degrades to "" (ephemeral identity) and the
# install still passes without proving key persistence; (2) kind's default
# CNI does not enforce NetworkPolicy, so the chart's default-deny NP is
# rendered but never exercised here.
HELM_TEST_NAMESPACE  ?= wardyn-test
HELM_TEST_RELEASE    ?= wardyn-test
HELM_TEST_IMAGE_REPO ?= wardyn/wardynd
HELM_TEST_IMAGE_TAG  ?= kind-test

helm-install-test: ## kind: postgres + helm install the loaded image + prove /healthz (needs a kind cluster up, see ci.yml)
	@echo "==> Postgres ($(HELM_TEST_NAMESPACE))"
	kubectl delete namespace $(HELM_TEST_NAMESPACE) --ignore-not-found --wait
	kubectl create namespace $(HELM_TEST_NAMESPACE)
	kubectl -n $(HELM_TEST_NAMESPACE) create secret generic wardyn-postgres-dsn \
		--from-literal=dsn="postgres://wardyn:wardyn@postgres:5432/wardyn?sslmode=disable" \
		--from-literal=age-key="$$(docker run --rm $(HELM_TEST_IMAGE_REPO):$(HELM_TEST_IMAGE_TAG) -gen-age-key)"
	kubectl -n $(HELM_TEST_NAMESPACE) create deployment postgres --image=postgres:16
	kubectl -n $(HELM_TEST_NAMESPACE) set env deployment/postgres POSTGRES_USER=wardyn POSTGRES_PASSWORD=wardyn POSTGRES_DB=wardyn
	kubectl -n $(HELM_TEST_NAMESPACE) expose deployment postgres --port=5432
	kubectl -n $(HELM_TEST_NAMESPACE) rollout status deployment/postgres --timeout=120s
	@echo "==> helm install $(HELM_TEST_RELEASE) (image $(HELM_TEST_IMAGE_REPO):$(HELM_TEST_IMAGE_TAG), already kind-loaded — no registry pull)"
	helm install $(HELM_TEST_RELEASE) ./deploy/helm/wardyn \
		--namespace $(HELM_TEST_NAMESPACE) \
		--set image.repository=$(HELM_TEST_IMAGE_REPO) \
		--set image.tag=$(HELM_TEST_IMAGE_TAG) \
		--set secrets.ageKeyFromSecret=true \
		--set auth.adminToken.value="$$(openssl rand -hex 20)"
	kubectl -n $(HELM_TEST_NAMESPACE) rollout status deployment/$(HELM_TEST_RELEASE) --timeout=180s || { \
		echo "FAIL: wardynd rollout never converged — pod state + logs follow"; \
		kubectl -n $(HELM_TEST_NAMESPACE) describe pod -l app.kubernetes.io/name=wardyn; \
		kubectl -n $(HELM_TEST_NAMESPACE) logs -l app.kubernetes.io/name=wardyn --tail=100 --all-containers || true; \
		exit 1; \
	}
	@echo "==> asserting /healthz through the Service (kubectl port-forward + curl)"
	@set -eu; \
	kubectl -n $(HELM_TEST_NAMESPACE) port-forward svc/$(HELM_TEST_RELEASE) 18080:8080 >/tmp/wardyn-kind-test-portforward.log 2>&1 & \
	pf_pid=$$!; \
	trap 'kill $$pf_pid 2>/dev/null || true' EXIT; \
	ok=0; \
	for i in $$(seq 1 15); do \
		code=$$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/healthz || true); \
		if [ "$$code" = "200" ]; then ok=1; break; fi; \
		sleep 1; \
	done; \
	if [ "$$ok" != "1" ]; then \
		echo "FAIL: /healthz never returned 200 (last code: $$code)"; \
		cat /tmp/wardyn-kind-test-portforward.log; \
		exit 1; \
	fi; \
	echo "/healthz OK (200) via kubectl port-forward -> Service -> Pod"
	@echo "==> teardown"
	helm uninstall $(HELM_TEST_RELEASE) --namespace $(HELM_TEST_NAMESPACE)
	kubectl delete namespace $(HELM_TEST_NAMESPACE) --wait=false

# ── kind quickstart (the k8s "one command to a real install" path) ──────────
# What helm-install-test above proves in CI, an operator can run on their own
# box: build wardynd/proxy/one agent image, stand up a throwaway kind cluster
# with a NetworkPolicy-enforcing CNI, helm install with the k8s runner
# substrate ON, and print a URL + token. Both targets are thin on purpose —
# the cluster name, the ports and every install flag live in ONE place
# (deploy/kind/quickstart.sh), so `kind-down` can never drift from what
# `kind-quickstart` created.
kind-quickstart: ## kind: build + throwaway cluster + helm install, k8s runner on (prints URL + token)
	deploy/kind/quickstart.sh

kind-down: ## Delete the kind-quickstart cluster
	deploy/kind/quickstart.sh --down

# Validate the compose files parse (does NOT need a running daemon).
# Both invocations, since scripts/ci-run.sh runs the base + the CI overlay together.
# The overlay interpolates ${WARDYN_CI_TOOLS_DIR:?...}; a dummy value is enough to
# parse (compose does not stat the bind-mount source at `config` time).
compose-config: ## Validate the compose files parse (no daemon needed)
	@echo "Validating docker-compose config..."
	docker compose -f $(COMPOSE_FILE) config >/dev/null
	WARDYN_CI_TOOLS_DIR=/tmp docker compose -f $(COMPOSE_FILE) -f deploy/compose/docker-compose.ci.yaml config >/dev/null

# DCO sign-off: every non-merge commit in DCO_RANGE carries a Signed-off-by.
# CI passes the PR range (BASE..HEAD); default is origin/main..HEAD for local use.
#
# git parses trailers itself (%(trailers:...) since 2.13), so there is no
# hand-rolled regex and no `git log -1` fork per commit. `separator=` is
# load-bearing: without it each trailer value is terminated by a line feed, which
# would emit a phantom blank record per commit. valueonly + the key filter also
# means a "Signed-off-by:" typed mid-body no longer counts — only a real trailer.
# The awk shape assertion is the other half: DCO is a claim about an identifiable
# human, so a present-but-shapeless trailer (`Signed-off-by: nobody`) must fail
# exactly like a missing one. `.+ <.+@.+>` is the whole contract — name, space,
# angle-bracketed address with an @ — and it also covers the empty case.
DCO_RANGE ?= origin/main..HEAD
dco: ## Every non-merge commit in DCO_RANGE carries a Signed-off-by trailer
	@echo "Checking DCO sign-off (Signed-off-by) over: $(DCO_RANGE)..."
	@signoffs=$$(git log --no-merges $(DCO_RANGE) --format='%H%x09%(trailers:key=Signed-off-by,valueonly,separator=%x2C)') \
	  || { echo "ERROR: git log failed for DCO_RANGE=$(DCO_RANGE) (bad/unreachable range) — failing closed"; exit 1; }; \
	bad=$$(printf '%s\n' "$$signoffs" | awk -F'\t' '$$2 !~ /.+ <.+@.+>/ {print $$1}'); \
	[ -z "$$bad" ] || { echo "ERROR: commit(s) lack a well-formed 'Signed-off-by: Name <email>' trailer:"; echo "$$bad"; echo "Add it with: git commit --signoff (or git commit -s)"; exit 1; }; \
	echo "All commits carry Signed-off-by. DCO check passed."

# CycloneDX SBOM via syft (installs syft if absent, pinned). Consumed by two
# callers: ci.yml's sbom-stub job (continuous freshness check on main, no
# signing) and .github/workflows/release.yml (the real release asset, cosign-
# signed, on a vX.Y.Z tag) — this target earned dropping its former "release
# stub" self-description once the second caller landed; it IS the release
# SBOM step now, not a placeholder for one.
sbom: ## Generate a CycloneDX SBOM via syft
	@echo "Generating CycloneDX SBOM via syft $(SYFT_VERSION)..."
	@command -v syft >/dev/null 2>&1 || curl -sSfL https://raw.githubusercontent.com/anchore/syft/$(SYFT_VERSION)/install.sh | sh -s -- -b /usr/local/bin $(SYFT_VERSION)
	syft . -o cyclonedx-json > wardyn-sbom.cdx.json

# Fail closed on a copyleft license in a SHIPPED (prod) UI dependency.
npm-license: ## Fail closed on copyleft in a SHIPPED (prod) UI dependency
	@echo "Checking UI production dependency licenses (no copyleft)..."
	./scripts/check-ui-licenses.sh

# The npm half of govulncheck: every Go dep was blocked on advisories at merge
# while the browser-delivered bundle had no CVE gate at all (dependabot opens
# PRs, it never fails a build). --prod scopes it to what actually ships, so
# vitest/playwright/puppeteer devDependency noise never gates a merge. Resolves
# straight from ui/pnpm-lock.yaml — no node_modules needed, hence no install.
# Deliberately NO --ignore-registry-errors: a registry blip must fail red, not
# silently pass (security invariant 5, fail-closed).
#
# NOTHING is suppressed: this gate runs against the real advisory set. The last
# suppression (GHSA-qwww-vcr4-c8h2, react-router "RSC Mode CSRF Bypass") is gone
# — it was carried on the belief that only the 7.x -> 8.x major fixed it, which
# was never true of the 7.x line: the advisory patches at BOTH 7.18.2 and 8.3.0,
# and we take 7.18.2. Read a GHSA's full patched-version list before suppressing;
# a stale "no patch exists" note outlives the release that refutes it.
# A future ignore goes in ui/package.json's pnpm.auditConfig.ignoreGhsas (pnpm's
# native mechanism) with its reason here — per-id, never per-package, so a
# different advisory on the same package still fails the gate.
npm-audit: ## Fail closed on a high/critical advisory in a SHIPPED (prod) UI dependency
	@echo "Auditing UI production dependencies for advisories (high+)..."
	cd ui && pnpm audit --prod --audit-level=high

# ── daemon-free merge gate ───────────────────────────────────────────────────
# green `make ci` != CI is green. This runs the merge-gating checks
# that need NO Docker daemon and NO live service — it deliberately EXCLUDES
# test-conformance-docker, every WARDYN_TEST_DOCKER e2e lane, the Postgres suite
# (test-pg), the Playwright UI e2e (ui-e2e), and the push-only sbom stub. CI
# remains the authority; use this locally to catch most failures before pushing.
ci: build build-docker build-k8s tidy-check lint cover-check test-race staticcheck govulncheck license-headers licenses gitleaks helm-lint compose-config dco diagrams npm-license npm-audit ui-typecheck ui-test ui test-conformance-stub ## Daemon-free merge gate: every CI check that needs no daemon or service
	@echo ""
	@echo "make ci PASSED (daemon-free merge gate). NOT covered here:"
	@echo "  test-conformance-docker, the WARDYN_TEST_DOCKER e2e lanes, the"
	@echo "  Postgres suite (test-pg), the Playwright UI e2e (ui-e2e), and the"
	@echo "  push-only SBOM stub — confirm CI is green before merging."

ui: ## Build the embedded web UI
	@echo "Building embedded web UI..."
	cd ui && pnpm install --frozen-lockfile && pnpm build

ui-typecheck: ## Typecheck the web UI (tsc --noEmit)
	@echo "Typechecking web UI (tsc --noEmit)..."
	cd ui && pnpm install --frozen-lockfile && pnpm typecheck

ui-test: ## Web UI vitest unit/component tests + coverage
	@echo "Running web UI unit/component tests (vitest + coverage)..."
	cd ui && pnpm install --frozen-lockfile && pnpm test:coverage

# Playwright UI e2e against a seeded none-runner backend. Each spec runs against a
# FRESHLY SEEDED backend (deterministic isolation). Requires Docker (Postgres) and
# the Playwright chromium browser (`cd ui && pnpm exec playwright install chromium`).
test-e2e-ui: ## Playwright UI e2e vs a seeded backend (needs Docker + chromium)
	@echo "Running Playwright UI e2e (fresh seed per spec)..."
	cd ui && pnpm install --frozen-lockfile
	./scripts/run-ui-e2e.sh

# regenerates docs/img UI screenshots; run after visible UI changes and commit the diff.
screenshots: ## Regenerate docs/img UI screenshots (run after visible UI changes)
	./scripts/screenshots.sh

# Re-shoots the demo video: virgin host -> make setup -> the funnel -> a real
# governed run -> the audit trail. DESTRUCTIVE (wipes the compose stack first).
# Needs a Windows ffmpeg (winget.exe install Gyan.FFmpeg) and a Claude subscription token
# at ~/.wardyn-demo-token. Beat sheet + re-shoot notes: docs/DEMO-SCRIPT.md.
record-demo: ## Record the demo video (DESTRUCTIVE: resets the stack; ARGS: --no-reset, --no-record)
	./scripts/record-demo.sh $(ARGS)

# ONE front door: asks containerized (default, recommended — the compose stack) vs
# host (advanced escape hatch — wardynd runs as you, using your resident Claude
# login). Enter / headless = containerized; a packaged one-command team setup does not
# exist, but admin/member RBAC + SSO shipped in v0.5 — see docs/OPERATIONS.md §Multi-user
# and deploy/compose/README.md for the recipe.
# In host mode a terminal PROMPTS for each credential (staging, AWS, SCM); a headless
# run (no TTY) skips them unless WARDYN_STAGE_CLAUDE=1 / WARDYN_IMPORT_AWS=1 /
# WARDYN_IMPORT_SCM=1 / WARDYN_FORCE_RESET=1 are set. Scripts that must not
# prompt at all pick the mode up front with WARDYN_SETUP_MODE=local|container.
setup: ## One-command Wardyn: containerized (default) or host; builds, ups, opens the UI
	@echo "Wardyn setup — asks containerized (default) vs host, then launches + opens the UI..."
	./scripts/setup.sh

# HOST MODE ONLY — it pins WARDYN_SETUP_MODE=local, and the ceiling it writes
# (~/.wardyn/claude-subscription.json) is read by scripts/run-host.sh, never
# by the containerized wardynd. Re-runs setup with staging forced; a running HOST
# wardynd is restarted so it loads the just-generated ceiling. Idempotent — safe to
# re-run anytime (e.g. after a headless `make setup` skipped the staging prompt).
# Containerized (the default) stages model access at the CLI instead:
# `claude setup-token | wardyn subscription connect`.
stage-claude: ## Stage your Claude login for HOST-mode subscription mounts (restarts the host wardynd)
	WARDYN_STAGE_CLAUDE=1 WARDYN_SETUP_MODE=local ./scripts/setup.sh

# Stop the background host-mode wardynd started by `make setup`.
# (Team/compose mode is stopped with `make compose-down`.)
stop-host: ## Stop the host-mode wardynd started by make setup (pidfile under ~/.wardyn)
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
reset: ## Clean slate: wipe local volumes (runs + audit + recordings) then setup
	@echo "Resetting local Wardyn (wipes volumes: runs + audit + recordings, then re-up)..."
	./scripts/up.sh reset

# FULL undo across BOTH modes: stops the host daemon, removes the compose stack
# + volumes, the stray wardyn-internal network, wardyn-test-pg, and the
# ~/.wardyn install files (allowlist — staged Claude creds included; the rest of
# ~/.wardyn is preserved). Keeps the age key (.env) and built images unless
# ARGS='--purge-env' / '--purge-images'; ARGS='--dry-run' only audits what would
# go. Leaves the box clean — no re-up.
reset-all: ## FULL undo: host daemon + compose + ~/.wardyn files (ARGS: --dry-run, --purge-*)
	./scripts/up.sh reset-all $(ARGS)

doctor: ## Read-only preflight — creates/changes nothing (docker, ports, confinement classes, WSL/Windows)
	./scripts/up.sh doctor

dev-pg: ## Start/ensure the dockerized dev/e2e Postgres (wardyn-test-pg :55432)
	./scripts/up.sh pg

compose-build: ## Build compose images (wardynd -tags docker + proxy)
	@echo "Building compose images..."
	docker compose -f $(COMPOSE_FILE) build
	docker compose -f $(COMPOSE_FILE) --profile build-only build proxy-image

compose-up: ## Start the docker-compose stack (postgres + dex + wardynd)
	@echo "Starting docker-compose stack..."
	docker compose -f $(COMPOSE_FILE) up -d postgres dex wardynd

compose-down: ## Stop the docker-compose stack
	@echo "Stopping docker-compose stack..."
	docker compose -f $(COMPOSE_FILE) down

demo: ## End-to-end compose demo (build, up, run, audit)
	@echo "Running the Wardyn compose demo..."
	./scripts/demo.sh

# Walks every major governance feature against a compose stack. ARGS defaults to
# '--up' so it brings its own stack up; override for a stack that is already
# running (ARGS=''), a single section (ARGS='--section 3'), or ARGS='--keep'.
test-drive: ## Guided governance test-drive (ARGS defaults to --up: brings the stack up)
	@echo "Running the Wardyn governance test-drive (brings the stack up; override with ARGS=...)..."
	./scripts/test-drive.sh $(if $(ARGS),$(ARGS),--up)

# Every place a build actually lands: bin/ (scripts/setup.sh and the e2e scripts
# build there with an explicit -o; `make build` is `go build ./...`, which writes
# no executables at all), .e2e-bin/ (the e2e scripts) and .local-bin/ (stale —
# nothing writes it anymore), ui/dist, and the repo-root binaries a bare `go build ./cmd/<name>`
# drops. That root list is DERIVED from cmd/ — the directory names ARE the binary
# names — so it can never drift from the package set again: the hand-typed list it
# replaced named 6 of the 10 and left ~74 MB of stale root binaries behind,
# precisely the state where a re-run of setup uses old code.
clean: ## Remove built binaries, the e2e/local bin dirs and the UI bundle
	@echo "Cleaning built binaries and generated output..."
	rm -rf bin .e2e-bin .local-bin ui/dist wardyn-sbom.cdx.json $(patsubst cmd/%/,%,$(wildcard cmd/*/))

# Validate every fenced mermaid diagram in the public docs: parses/renders via
# mermaid-cli and each load-bearing label still exists at its cited source.
diagrams: ## Validate the mermaid diagrams in the public docs (syntax + label-truth)
	./scripts/check-diagrams.sh

# Check SPDX license headers on source files (or apply with: make license-headers ARGS=fix).
license-headers: ## Check SPDX headers on source files; ARGS=fix to apply them
	./scripts/license-headers.sh $(if $(filter fix,$(ARGS)),--fix)
