# Wardyn e2e fixture "agent" image.
#
# This image stands in for a real coding-agent image (e.g.
# ghcr.io/cjohnstoniv/agent-claude-code) during end-to-end validation. The
# public agent images do not exist yet, so the e2e harness creates a run with
# agent name "e2e-fixture" and pre-tags this locally as
# ghcr.io/cjohnstoniv/agent-e2e-fixture:latest. The docker driver's
# ensureImage() then finds it present and skips the (failing) registry pull.
#
# The image ships the tools the e2e assertions exercise from INSIDE the sandbox
# (via `docker exec` against the running agent container):
#   - curl        : drive egress through the wardyn-proxy sidecar
#   - iproute2    : `ip route` to prove the sandbox has NO default route (L0)
#   - busybox wget: a second egress client
#
# The docker driver holds the agent open by setting the container Cmd to
# `sleep infinity` and launches the real agent via Exec; e2e.sh runs its own
# inline docker-exec curl probes rather than a fixture script.
#
# CRITICAL: this image MUST NOT declare an ENTRYPOINT. The Wardyn docker driver
# sets only the container *Cmd* (["sleep","infinity"]); an ENTRYPOINT would
# wrap that Cmd as arguments (e.g. `entrypoint sleep infinity`) instead of
# replacing it, so an entrypoint that exits would tear the sandbox down
# immediately. Real agent images follow the same contract: no blocking
# ENTRYPOINT that swallows the driver-supplied Cmd.
FROM alpine:3.20

RUN apk add --no-cache curl iproute2 ca-certificates

# No ENTRYPOINT on purpose (see note above). The default CMD is a harmless
# self-documenting message for standalone `docker run` invocations; the Wardyn
# driver replaces it with `sleep infinity`.
CMD ["/bin/sh", "-c", "echo 'wardyn e2e fixture: probes run via docker exec' && sleep infinity"]
