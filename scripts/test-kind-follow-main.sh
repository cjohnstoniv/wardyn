#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-kind-follow-main.sh — 06-kind-follow-main.sh must not roll back a failed
# deploy that adds migrations: the new wardynd may already have migrated the
# database, and the older one refuses to boot on it (internal/db/db.go).
# Drives adds_migrations() on a throwaway git repo; needs no cluster.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/deploy/azure-entra-sso/06-kind-follow-main.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }

body="$(sed -n '/^adds_migrations() {/,/^}/p' "${SCRIPT}")"
[[ -n "${body}" ]] || fail "06-kind-follow-main.sh has no adds_migrations()"
grep -q 'if adds_migrations "${DEPLOYED}" "${SHA}"' "${SCRIPT}" \
  || fail "the rollback block no longer asks adds_migrations before helm rollback"

tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
git -C "${tmp}" init -q
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
mkdir -p "${tmp}/internal/db/migrations"
echo a >"${tmp}/internal/db/migrations/0001_a.sql"; echo x >"${tmp}/other"
git -C "${tmp}" add -A; git -C "${tmp}" commit -q -m one
echo y >"${tmp}/other"; git -C "${tmp}" commit -q -am two
echo b >"${tmp}/internal/db/migrations/0002_b.sql"; git -C "${tmp}" add -A; git -C "${tmp}" commit -q -m three
c1="$(git -C "${tmp}" rev-parse HEAD~2)"; c2="$(git -C "${tmp}" rev-parse HEAD~1)"; c3="$(git -C "${tmp}" rev-parse HEAD)"

ROOT="${tmp}"
eval "${body}"
adds_migrations "${c1}" "${c2}" && fail "a range with no new migration file was treated as adding one"
adds_migrations "${c2}" "${c3}" || fail "a range that adds 0002_b.sql was not treated as adding a migration"
adds_migrations "${c1}" "${c3}" || fail "a range spanning an added migration was not treated as adding one"
echo "test-kind-follow-main: PASS"
