#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/kek-conformance.sh — the live Vault Transit KEK and Vault KV suite
# (internal/secretstore/vaultkv TestLive_Transit*, TestLive_VaultKV) against the
# official dev-mode servers, one run each: hashicorp/vault and openbao/openbao,
# plus a throwaway postgres:17. Each server writes a file audit device into a
# directory mounted at the same path on both sides, so the test reads the log
# the server wrote; the outage cases `docker pause` the server's container.
#
# Reports land in test/reports/go/kek-<server>/. The skip floor
# (scripts/test-report.sh) fails a run in which any of these tests skipped.
#
# Usage: scripts/kek-conformance.sh      (needs docker; `make test-kek-conformance`)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

VAULT_IMAGE="${VAULT_IMAGE:-hashicorp/vault:2.1.1}"
OPENBAO_IMAGE="${OPENBAO_IMAGE:-openbao/openbao:2.7.0}"
PG_IMAGE="${PG_IMAGE:-postgres:17}"
TAG="kekconf-$$"
WORK="$(mktemp -d)"
chmod 0777 "$WORK" # each server writes its audit log as its own user

cleanup() {
  docker rm -f "$TAG-pg" "$TAG-vault" "$TAG-openbao" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

port() { docker port "$1" "$2" | head -1 | sed 's/.*://'; }

wait_vault() {
  local url="$1"
  for _ in $(seq 1 60); do
    curl -fsS -m 3 "$url/v1/sys/health" >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "no answer from $url/v1/sys/health"
}

log "starting $PG_IMAGE"
docker run -d --name "$TAG-pg" -e POSTGRES_PASSWORD=wardyn -p 127.0.0.1::5432 "$PG_IMAGE" >/dev/null
for _ in $(seq 1 60); do
  docker exec "$TAG-pg" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$TAG-pg" pg_isready -U postgres -h 127.0.0.1 >/dev/null || die "postgres did not come up"
PG="postgres://postgres:wardyn@127.0.0.1:$(port "$TAG-pg" 5432)/postgres?sslmode=disable"

echo root >"$WORK/root-token"

log "starting $VAULT_IMAGE (dev mode)"
docker run -d --name "$TAG-vault" --cap-add IPC_LOCK -v "$WORK:$WORK" -p 127.0.0.1::8200 \
  -e VAULT_DEV_ROOT_TOKEN_ID=root -e VAULT_DEV_LISTEN_ADDRESS=0.0.0.0:8200 "$VAULT_IMAGE" >/dev/null
VAULT_URL="http://127.0.0.1:$(port "$TAG-vault" 8200)"
wait_vault "$VAULT_URL"
# Vault enables an audit device over the API; OpenBao only from its config.
curl -fsS -X PUT -H 'X-Vault-Token: root' "$VAULT_URL/v1/sys/audit/wardyn" \
  -d "{\"type\":\"file\",\"options\":{\"file_path\":\"$WORK/vault-audit.log\",\"mode\":\"0644\"}}" >/dev/null

log "starting $OPENBAO_IMAGE (dev mode)"
cat >"$WORK/openbao.hcl" <<EOF
audit "file" "wardyn" {
  options {
    file_path = "$WORK/openbao-audit.log"
    mode      = "0644"
  }
}
EOF
chmod 0644 "$WORK/openbao.hcl"
docker run -d --name "$TAG-openbao" -v "$WORK:$WORK" -p 127.0.0.1::8200 \
  -e BAO_DEV_ROOT_TOKEN_ID=root -e BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200 "$OPENBAO_IMAGE" \
  server -dev -config="$WORK/openbao.hcl" >/dev/null
OPENBAO_URL="http://127.0.0.1:$(port "$TAG-openbao" 8200)"
wait_vault "$OPENBAO_URL"

rc=0
for server in vault openbao; do
  url="$VAULT_URL"
  [ "$server" = openbao ] && url="$OPENBAO_URL"
  log "live suite against $server at $url"
  WARDYN_TEST_VAULT="$url" \
    WARDYN_TEST_VAULT_TOKEN_FILE="$WORK/root-token" \
    WARDYN_TEST_VAULT_CONTAINER="$TAG-$server" \
    WARDYN_TEST_VAULT_AUDIT_FILE="$WORK/$server-audit.log" \
    WARDYN_TEST_PG="$PG" \
    "$ROOT/scripts/test-report.sh" "kek-$server" -count=1 -run '^TestLive_(Transit|VaultKV)' ./internal/secretstore/vaultkv/ || rc=1
done
exit "$rc"
