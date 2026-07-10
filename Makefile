.PHONY: license-headers diagrams build build-docker test test-docker lint ui compose-build compose-up compose-down demo clean test-conformance-docker test-conformance-stub govulncheck staticcheck agent-images test-drive help test-report test-report-pg test-report-docker cover-check ui-test ui-typecheck test-e2e test-e2e-live test-e2e-subscription test-e2e-ui setup setup-host setup-team stop-host reset doctor dev-pg

COMPOSE_FILE := deploy/compose/docker-compose.yaml

help:
	@echo "Wardyn governance control plane"
	@echo ""
	@echo "Targets:"
	@echo "  setup                 - One-command Wardyn: prompts for mode + each credential, builds, up, opens browser"
	@echo "                          (non-interactive opt-ins: WARDYN_SETUP_MODE=local|team [required headless],"
	@echo "                           WARDYN_STAGE_CLAUDE=1, WARDYN_IMPORT_AWS=1, WARDYN_IMPORT_SCM=1, WARDYN_FORCE_RESET=1)"
	@echo "  reset                 - Clean slate: wipe local volumes (runs + audit + recordings) then setup"
	@echo "  doctor                - Read-only preflight (docker, ports, confinement classes, WSL/Windows)"
	@echo "  dev-pg                - Start/ensure the dockerized dev/e2e Postgres (wardyn-test-pg :55432)"
	@echo "  build                 - Build Go binaries (default tags)"
	@echo "  build-docker          - Build Go binaries with -tags docker"
	@echo "  test                  - Run all Go tests"
	@echo "  test-docker           - Run all Go tests with -tags docker"
	@echo "  test-conformance-docker - Run conformance suite on Docker (requires WARDYN_TEST_DOCKER=1)"
	@echo "  test-conformance-stub - Run the driver-agnostic conformance honesty stub (no cluster required)"
	@echo "  govulncheck           - Run govulncheck for known vulnerabilities"
	@echo "  staticcheck           - Run staticcheck static analysis"
	@echo "  diagrams              - Validate the mermaid diagrams in the public docs (syntax + label-truth)"
	@echo "  license-headers       - Check (CI) SPDX headers on source files; ARGS=fix to apply"
	@echo "  lint                  - Run go vet (both tag sets)"
	@echo "  ui                    - Build embedded web UI"
	@echo "  ui-typecheck          - Typecheck the web UI (tsc --noEmit)"
	@echo "  ui-test               - Run web UI vitest unit/component tests + coverage"
	@echo "  test-e2e-ui           - Playwright UI e2e vs a seeded backend (needs Docker + chromium)"
	@echo "  test-e2e-live         - Live TASK e2e: real sandboxes run the corpus, graded on state (needs Docker)"
	@echo "  test-e2e-subscription - Live SUBSCRIPTION e2e: proxy-side inject-on attach + inject-off escape hatch (restarts wardynd)"
	@echo "  test-report           - Go unit tests with detailed md/coverage reports"
	@echo "  test-report-pg        - Postgres-gated suite with reports (needs WARDYN_TEST_PG)"
	@echo "  test-report-docker    - docker-tagged suite with reports (needs WARDYN_TEST_DOCKER=1)"
	@echo "  cover-check           - Run test-report and enforce COVER_MIN coverage floor"
	@echo "  compose-build         - Build compose images (wardynd -tags docker + proxy)"
	@echo "  compose-up            - Start docker-compose stack (postgres + dex + wardynd)"
	@echo "  compose-down          - Stop docker-compose stack"
	@echo "  demo                  - End-to-end compose demo (build, up, run, audit)"
	@echo "  agent-images          - Build the agent OCI images (claude-code + codex-cli + oracle)"
	@echo "  test-drive            - Guided governance test-drive against a running compose stack"
	@echo "                          (ARGS='--up' to bring the stack up first)"
	@echo "  clean                 - Remove built binaries"

agent-images:
	@echo "Building agent images (build context: repo root)..."
	docker build -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:demo .
	docker build -f deploy/images/codex-cli/Dockerfile   -t wardyn/agent-codex-cli:demo   .
	docker build -f deploy/images/oracle/Dockerfile      -t wardyn/agent-oracle:demo      .
	@echo "Agent images built: wardyn/agent-claude-code:demo  wardyn/agent-codex-cli:demo  wardyn/agent-oracle:demo"

build:
	@echo "Building Go binaries..."
	go build ./...

build-docker:
	@echo "Building Go binaries (-tags docker)..."
	go build -tags docker ./...

test:
	@echo "Running Go tests..."
	go test ./...

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

test-report-docker:
	@echo "Running docker-tagged suite with reports (requires WARDYN_TEST_DOCKER=1)..."
	./scripts/test-report.sh docker "Wardyn docker integration tests" \
		-tags docker ./internal/runner/... ./internal/envbuild/... ./cmd/wardyn-runner/...

# Coverage floor gate. Override with `make cover-check COVER_MIN=NN`.
# Ratcheted to lock in the deep-review test build-out (total was 47.7% at the
# start of that effort, 60.3% after). Keep a small margin below the current total
# so routine churn doesn't flake CI; raise this as coverage climbs.
COVER_MIN ?= 58
cover-check: test-report
	@total=$$(grep -E '^total:' test/reports/go/unit/coverage-func.txt | awk '{print $$NF}' | tr -d '%'); \
	echo "Total Go coverage: $${total}% (floor $(COVER_MIN)%)"; \
	awk -v t=$${total} -v m=$(COVER_MIN) 'BEGIN{exit !(t+0 >= m+0)}' || \
		{ echo "coverage $${total}% below floor $(COVER_MIN)%"; exit 1; }

test-conformance-docker:
	@echo "Running conformance tests on Docker (WARDYN_TEST_DOCKER=1 required)..."
	WARDYN_TEST_DOCKER=1 go test -v -tags docker -timeout 10m ./test/conformance/...

test-conformance-stub:
	@echo "Running driver-agnostic conformance honesty-stub tests (no cluster required)..."
	go test -v -timeout 2m ./test/conformance/...

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

govulncheck:
	@echo "Running govulncheck..."
	go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

staticcheck:
	@echo "Running staticcheck..."
	go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

lint:
	@echo "Running go vet (default + docker tags)..."
	go vet ./...
	go vet -tags docker ./...

ui:
	@echo "Building embedded web UI..."
	cd ui && pnpm install && pnpm build

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

setup:
	@echo "Wardyn setup — detects your host, asks how you're running it, launches + opens the UI..."
	./scripts/setup.sh

# Force local (host) mode: wardynd runs as you, uses your existing Claude login
# (no re-login, no stale credential copy). Best for sandboxing your own machine.
# In a terminal this still PROMPTS for each credential (staging, AWS, SCM); only a
# headless run (no TTY) skips them unless WARDYN_STAGE_CLAUDE=1 / WARDYN_IMPORT_AWS=1
# / WARDYN_IMPORT_SCM=1 are set.
setup-host:
	@echo "Wardyn setup (local/host mode: your Claude login, proxy-injected)..."
	WARDYN_SETUP_MODE=local ./scripts/setup.sh

# Force team (compose) mode: sealed containerized control plane, brokered per-user
# keys. Best for running Wardyn as a shared service.
setup-team:
	@echo "Wardyn setup (team/compose: sealed control plane, brokered keys)..."
	WARDYN_SETUP_MODE=team ./scripts/setup.sh

# Stop the background host-mode wardynd started by `make setup` / setup-host.
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
