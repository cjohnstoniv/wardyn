# wardyn/agent-oracle: deterministic "oracle" agent image for the e2e task corpus.
#
# This is NOT a real coding agent. It runs a task's scripted, known-good SOLUTION
# (solution.sh) as the agent process so the e2e harness can prove each task in
# test/e2e/tasks/ is actually solvable and that its grader scores the intended
# final workspace state as PASS. It is also the agent for tasks that ARE a script
# (e.g. egress-boundary), where the "solution" is the probe itself.
#
# IMAGE CONTRACT (mirrors deploy/images/README.md — the docker driver enforces it):
#   - NO ENTRYPOINT: the Wardyn docker driver sets Cmd=["sleep","infinity"] at
#     create and launches the agent later via Exec. An ENTRYPOINT would wrap that
#     Cmd as arguments, so an entrypoint that exits would tear the sandbox down
#     immediately.
#   - /usr/local/bin/agent-run "<task>": runs one task; agent-run --selftest
#     verifies tools and exits 0 (used by e2e to confirm the image contract).
#   - /usr/local/bin/wardyn-rec: the docker driver WRAPS every agent argv with the
#     session recorder (recorder.go), so the binary MUST be present even though the
#     oracle never calls a model — without it the wrapped exec fails 127. It also
#     gives oracle runs a real recording, which the recording-replay e2e consumes.
#   - USER agent (uid 1000), home /home/agent, workspace /home/agent/work.
#
# The oracle needs NO LLM wiring and NO git credential broker (it never calls a
# model). Because it ships wardyn-rec, the build context is the REPO ROOT (Go
# source), so build with:
#   docker build -f deploy/images/oracle/Dockerfile -t wardyn/agent-oracle:local .

# ── Stage 1: Go builder (wardyn-rec + wardyn-scan) ────────────────────────────
FROM golang:1.26@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS builder
WORKDIR /build

# Corporate build support (no-ops on an OSS build): GOTOOLCHAIN=local skips the
# pinned-toolchain self-upgrade fetch (blocked behind a TLS-MITM proxy); the
# optional corp CA (host-staged deploy/images/corp-ca.pem, gitignored) is added to
# the system trust so `go mod download` can verify the module proxy through
# corporate MITM. The README.md companion makes the COPY succeed when the
# corp-ca.pem* glob matches nothing (mirrors deploy/images/claude-code/Dockerfile).
ENV GOTOOLCHAIN=local
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-rec ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-scan ./cmd/wardyn-scan

# ── Stage 2: oracle runtime ───────────────────────────────────────────────────
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# curl + ca-certificates: the egress-boundary oracle probes the proxy.
# python3: several task solutions rewrite files via a python heredoc.
# git: parity with the real images (some solutions may init/inspect a repo).
RUN apk add --no-cache curl ca-certificates python3 git

# Create user `agent` (uid 1000, /home/agent, work /home/agent/work) to satisfy
# the sandbox contract. alpine's adduser/addgroup (no useradd) create it cleanly.
RUN addgroup -g 1000 agent \
 && adduser -D -u 1000 -G agent -h /home/agent agent \
 && mkdir -p /home/agent/work \
 && chown -R agent:agent /home/agent

# The session recorder the driver wraps the agent argv with. No asciinema in this
# image, so wardyn-rec falls back to a plain .log capture (see cmd/wardyn-rec).
COPY --from=builder /out/wardyn-rec /usr/local/bin/wardyn-rec
# wardyn-scan: in-sandbox workspace scanner run by a governed scan run.
COPY --from=builder /out/wardyn-scan /usr/local/bin/wardyn-scan

# agent-run launcher (repo-root context now, so COPY the full path).
COPY deploy/images/oracle/agent-run /usr/local/bin/agent-run
RUN chmod +x /usr/local/bin/agent-run /usr/local/bin/wardyn-rec /usr/local/bin/wardyn-scan

WORKDIR /home/agent/work
USER agent

# No ENTRYPOINT on purpose (see contract above). The default CMD is a harmless
# self-documenting message for a standalone `docker run`; the Wardyn driver
# replaces it with `sleep infinity`.
CMD ["/bin/sh", "-c", "echo 'wardyn oracle agent: run via agent-run <solution-path-relative-to-workspace>' && sleep infinity"]
