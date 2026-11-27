#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# make-testdata.sh writes the git objects scan_test.go reads, from real repositories made by git itself:
#   loose-<id>     a loose blob holding the test value (its file under objects/xx/ is named by <id>)
#   whole.pack     a pack after `git gc` whose blob holding the value is stored whole
#   delta.pack     a pack after `git gc --aggressive` where the value is only in a delta's inserts
#   clean.pack     a pack after `git gc` that holds no value
# Run it from anywhere; it writes beside itself. The value is scan_test.go's `secret`.
set -euo pipefail
out=$(cd "$(dirname "$0")" && pwd)
value=wdn-scan-value-7f3a9c2e1b
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z

repo() { rm -rf "$work/r" && git init -q -b main "$work/r" && cd "$work/r"; }
body() { for i in $(seq 1 400); do echo "line $i of a file long enough that git stores a change to it as a delta"; done; }
only_pack() { ls .git/objects/pack/*.pack; }
# put copies a git object file, which git makes read-only, as an ordinary file.
put() { rm -f "$2" && cat "$1" > "$2"; }

# A loose blob holding the value.
repo
id=$(printf 'k: %s\n' "$value" | git hash-object -w --stdin)
rm -f "$out"/loose-*
put ".git/objects/${id:0:2}/${id:2}" "$out/loose-$id"

# The value in a blob stored whole.
repo
{ body; echo "k: $value"; } > f
git add f && git commit -q -m one
git gc -q
put "$(only_pack)" "$out/whole.pack"

# The value only in a delta's inserts: the older version holds it, and git stores the newer, larger one whole.
repo
{ body; echo "k: $value"; body; } > f
git add f && git commit -q -m one
{ body; echo "k: a longer line in its place, which makes this version the larger one"; body; echo end; } > f
git add f && git commit -q -m two
git gc -q --aggressive
old=$(git rev-parse HEAD~1:f)
git verify-pack -v .git/objects/pack/*.idx | awk -v id="$old" '$1 == id && NF >= 7 { found = 1 } END { exit !found }' ||
  { echo "make-testdata: the blob holding the value is not a delta" >&2; exit 1; }
put "$(only_pack)" "$out/delta.pack"

# A pack that holds no value.
repo
body > f
git add f && git commit -q -m one
git gc -q
put "$(only_pack)" "$out/clean.pack"
