#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-rekey-fingerprint-check.sh — the fingerprint check in
# docs/operations/secrets-and-keys.md tells an operator whether a kept
# <key-file>.new is the only key that reads the store. A missing or failing
# tool, an empty recipient, a fingerprint that is not 16 hex characters, a failed
# query or an answer that is not a bare number must print "could not check, keep
# every key file", never a count: a hash of nothing matches no row and would
# read as 0.
#
# Runs the documented block as written, with HOME in a scratch directory and a
# PATH that holds only stand-ins and the text tools the block uses (each case
# may leave one of those out or replace it), so no real key command, key file
# or database is reachable. The stand-in recipient is the
# golden one in internal/secretstore/kek/kek_test.go, whose kek_id ends in
# 929a37ad7ab6608f.
#
# Daemon-free, network-free, key-free.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

doc="$ROOT/docs/operations/secrets-and-keys.md"
block="$(awk '/^staged=/{on=1} on&&/^```$/{exit} on' "$doc")"
[ -n "$block" ] || fail "no staged= block in $doc"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
SH="$(command -v sh)"
mkdir -p "$tmp/home"

# stub DIR NAME BODY: a stand-in tool in DIR.
stub() { printf '#!%s\n%s\n' "$SH" "$3" >"$1/$2"; chmod +x "$1/$2"; }

# check NAME AGE-KEYGEN-BODY DOCKER-BODY [TOOL [BODY]]: run the block against
# fresh stand-ins (an empty body leaves that tool missing) and print what it
# printed. TOOL is one text tool to leave out, or to replace by a stand-in BODY.
check() {
  local bin="$tmp/$1"
  mkdir -p "$bin"
  for t in sha256sum cut grep; do [ "$t" = "${4:-}" ] || ln -s "$(command -v "$t")" "$bin/$t"; done
  [ -z "${5:-}" ] || stub "$bin" "$4" "$5"
  [ -z "$2" ] || stub "$bin" age-keygen "$2"
  [ -z "$3" ] || stub "$bin" docker "$3"
  env -i HOME="$tmp/home" PATH="$bin" "$SH" -c "$block" 2>&1 || true
}

recipient='[ "$1" = -y ] && [ "$2" = "$HOME/.wardyn/age.key.new" ] || exit 9; echo "wardyn test vector recipient"'
# Answers only a query that counts both tables under the golden fingerprint.
count='case "$*" in *"FROM secrets WHERE kek_id LIKE '"'"'%:929a37ad7ab6608f'"'"'"*"FROM principal_keys WHERE kek_id LIKE '"'"'%:929a37ad7ab6608f'"'"'"*) echo 3 ;; *) exit 9 ;; esac'

closed() { # NAME OUTPUT: the block must refuse to answer, never print a count.
  case "$2" in *"could not check, keep every key file"*) ;; *) fail "$1: want the could-not-check line, got: $2" ;; esac
  case "$2" in *"rows under it"*) fail "$1: printed a count: $2" ;; esac
}

# A stand-in database that answers every query, so only the block's own
# checks can stop it from printing a count.
any='echo 0'
closed "no age-keygen" "$(check no-keygen '' "$any")"
closed "empty recipient" "$(check empty-recipient 'exit 0' "$any")"
closed "age-keygen fails" "$(check keygen-fails 'echo age1partial; exit 1' "$any")"
closed "no docker" "$(check no-docker "$recipient" '')"
closed "query fails" "$(check query-fails "$recipient" 'echo "psql: error" >&2; exit 2')"
closed "query prints a number and fails" "$(check query-number-then-fails "$recipient" 'echo 0; exit 1')"
closed "query prints no number" "$(check query-not-a-number "$recipient" 'echo ERROR')"
closed "query prints nothing" "$(check query-empty "$recipient" 'exit 0')"
closed "query prints a number inside other text" "$(check query-number-in-text "$recipient" 'echo "ERROR 42"')"
closed "query prints a number on a second line" "$(check query-multiline "$recipient" 'echo ERROR; echo 42')"
closed "no sha256sum" "$(check no-sha256sum "$recipient" "$any" sha256sum)"
closed "sha256sum fails" "$(check sha256sum-fails "$recipient" "$any" sha256sum 'echo 929a37ad7ab6608f; exit 1')"
closed "sha256sum prints no hash" "$(check sha256sum-no-hash "$recipient" "$any" sha256sum 'echo "usage: sha256sum"')"
closed "sha256sum prints the hash on a second line" "$(check sha256sum-multiline "$recipient" "$any" sha256sum 'echo x; echo 929a37ad7ab6608f')"
closed "no cut" "$(check no-cut "$recipient" "$any" cut)"
closed "no grep" "$(check no-grep "$recipient" "$any" grep)"

got="$(check answers "$recipient" "$count")"
case "$got" in *"fingerprint: 929a37ad7ab6608f"*"rows under it: 3"*) ;; *) fail "answers: want the fingerprint and the count, got: $got" ;; esac
case "$got" in *"could not check"*) fail "answers: also printed the could-not-check line: $got" ;; esac

echo "test-rekey-fingerprint-check: ok"
