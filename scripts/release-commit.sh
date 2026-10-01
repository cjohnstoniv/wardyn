#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# release-commit.sh --dry-run | --apply --tree DIR --from X.Y.Z --to X.Y.Z [--date YYYY-MM-DD] [--notes FILE]
#                                      [--highlights TEXT] [--expect-tip SHA-PREFIX]
#                    | --test-gaps --tree DIR --to X.Y.Z
# X.Y.Z may also be X.Y.Z-rc.N (rehearsals). Commits under the caller's own git identity.
# The X.Y.Z release commit, RELEASING.md steps 1/1a/1b in one place: rename [Unreleased] → [X.Y.Z] — DATE, put the
# resolved release-notes paste block under it, restore a fresh [Unreleased], bump every shipped version string
# (version.go, Chart.yaml version+appVersion, ui/package.json, README + install.sh installer pins, the two docs/ci
# checkout pins, the threat-model currency line), then `git commit -s -m "release: X.Y.Z"`.
# NOT here (RELEASING step 5, after the asset upload): the demo-videos.ts / README video rows and tag flips.
# NOTE 0.7.4: docs/DESKTOP.md's two MDM image pins joined the guarded set; they are edited here.
# NOTE 0.7.0: deploy/helm/wardyn/values.yaml carries the version in a comment that TestShippedVersionStringsAgree checks — bump it by hand.
# Refuses: dirty tree, unexpected tip, a notes file that still carries a "> **NOTE" block, any target not found once
# (a pin already at --to, 0 old matches and the expected count of new ones, passes as "already"), a missing ROADMAP row without --highlights.
# After --apply: run `make release-check` (RELEASING says it must run AFTER this commit) before anyone tags.
# #171 v0.8.0 fork closes the two gaps the 0.7.7/0.7.8 tooling left open:
#   - ROADMAP.md Shipped row: --dry-run/--apply check for the row's Status cell (RELEASING.md step 1b).
#     Highlights prose is a human call, so a missing row is added from --highlights TEXT and refused
#     without it, never invented text.
#   - `make test-gaps`: a separate --test-gaps mode, run AFTER `make release-check` (it reads the
#     coverage profile that only a full `make ci` run produces), commits docs/TEST-GAPS.md on its own
#     if it changed — matching the two-commit shape the 0.7.13 forward-port actually used.
set -euo pipefail
MODE=""; DATE=$(date +%Y-%m-%d); NOTES=""; EXPECT=""; TREE=""; FROM=""; TO=""; HIGHLIGHTS=""
while [ $# -gt 0 ]; do case "$1" in
  --dry-run) MODE=dry ;; --apply) MODE=apply ;; --test-gaps) MODE=testgaps ;; --date) DATE=$2; shift ;; --notes) NOTES=$2; shift ;; --expect-tip) EXPECT=$2; shift ;; --highlights) HIGHLIGHTS=$2; shift ;;
  --tree) TREE=$2; shift ;; --from) FROM=$2; shift ;; --to) TO=$2; shift ;;
  *) echo "unknown arg $1" >&2; exit 2 ;; esac; shift; done
VERSION_RE='^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
for v in "$FROM" "$TO"; do
  [ -z "$v" ] || [[ "$v" =~ $VERSION_RE ]] || { echo "bad version '$v': want X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }
done
[ -n "$MODE" ] && [ -n "$TREE" ] || { sed -n '5,8p' "$0"; exit 2; }
if [ "$MODE" = testgaps ]; then
  [ -n "$TO" ] || { echo "--test-gaps needs --to X.Y.Z" >&2; exit 2; }
  cd "$TREE" || exit 2
  [ -z "$(git status --porcelain)" ] || { echo "REFUSE: worktree not clean"; git status --short | head; exit 3; }
  make test-gaps
  if git diff --quiet -- docs/TEST-GAPS.md && [ -z "$(git status --porcelain -- docs/TEST-GAPS.md)" ]; then
    echo "docs/TEST-GAPS.md unchanged — nothing to commit"; exit 0
  fi
  git add docs/TEST-GAPS.md
  git commit -q -s -m "release: regenerate docs/TEST-GAPS.md for $TO"
  git log --format='%h %an %s' -1
  exit 0
fi
[ -n "$FROM" ] && [ -n "$TO" ] || { sed -n '5,8p' "$0"; exit 2; }
cd "$TREE" || exit 2
[ -z "$(git status --porcelain)" ] || { echo "REFUSE: worktree not clean"; git status --short | head; exit 3; }
if [ -n "$EXPECT" ]; then
  [[ "$EXPECT" =~ ^[0-9a-f]{7,40}$ ]] || { echo "--expect-tip wants 7-40 hex characters of the commit id, got '$EXPECT'" >&2; exit 2; }
  if [ "$(git rev-parse --verify -q "$EXPECT^{commit}" 2>/dev/null)" != "$(git rev-parse HEAD)" ]; then
    if [ "$(git rev-parse --disambiguate="$EXPECT" | wc -l)" -gt 1 ]; then echo "REFUSE: --expect-tip $EXPECT is ambiguous (several objects share that prefix); give more characters"
    else echo "REFUSE: HEAD $(git rev-parse --short=8 HEAD) != --expect-tip $EXPECT"; fi
    exit 3
  fi
fi
if [ -n "$NOTES" ]; then
  [ -s "$NOTES" ] || { echo "REFUSE: notes file missing: $NOTES"; exit 3; }
  if grep -q '> \*\*NOTE' "$NOTES"; then echo "REFUSE: $NOTES still carries $(grep -c '> \*\*NOTE' "$NOTES") '> **NOTE' block(s) — resolve them first"; exit 3; fi
fi
python3 - "$MODE" "$DATE" "$NOTES" "$FROM" "$TO" "$HIGHLIGHTS" <<'PY'
import re,sys,io
from pathlib import Path
mode,date,notes,V_OLD,V_NEW,highlights=sys.argv[1:7]
edits=[  # (path, exact old, new, expected count)
 ("internal/version/version.go", f'const Version = "{V_OLD}"', f'const Version = "{V_NEW}"', 1),
 ("deploy/helm/wardyn/Chart.yaml", f"version: {V_OLD}\nappVersion: {V_OLD}\n", f"version: {V_NEW}\nappVersion: {V_NEW}\n", 1),
 ("ui/package.json", f'"version": "{V_OLD}"', f'"version": "{V_NEW}"', 1),
 ("README.md", f"releases/download/v{V_OLD}/install.sh", f"releases/download/v{V_NEW}/install.sh", 1),
 ("install.sh", f"releases/download/v{V_OLD}/install.sh", f"releases/download/v{V_NEW}/install.sh", 1),
 ("docs/ci/github-actions.yml", f"ref: v{V_OLD}\n", f"ref: v{V_NEW}\n", 1),
 ("docs/ci/azure-pipelines.yml", f"--branch v{V_OLD} ", f"--branch v{V_NEW} ", 1),
 ("threatmodel/THREAT-MODEL.md", f"last reviewed at v{V_OLD})", f"last reviewed at v{V_NEW})", 1),
 ("deploy/helm/wardyn/values.yaml", f"resolves to .Chart.AppVersion, which is {V_OLD} ", f"resolves to .Chart.AppVersion, which is {V_NEW} ", 1),  # TestShippedVersionStringsAgree's tenth pin
 # 0.7.4: DESKTOP.md's real-hardware smoke recipe pins both image tags by hand (the
 # desktop tier's MDM config has no $WARDYN_VERSION to interpolate). X1a-F10 found
 # this stale for a whole cycle, so TestShippedVersionStringsAgree now pins the pair.
 ("docs/DESKTOP.md", f"wardynd:{V_OLD}|", f"wardynd:{V_NEW}|", 1),
 ("docs/DESKTOP.md", f"wardyn-proxy:{V_OLD}|", f"wardyn-proxy:{V_NEW}|", 1),
]
ok=True
for path,old,new,n in edits:
    s=Path(path).read_text(); c=s.count(old)
    if c==0 and s.count(new)==n:
        print(f"OK  {path}: OK (already {V_NEW}) for {new!r}")
        continue
    print(f"{'OK ' if c==n else 'BAD'} {path}: {c} match(es) for {old!r}")
    ok&= c==n
cl=Path("CHANGELOG.md").read_text()
c=cl.count("\n## [Unreleased]\n"); print(f"{'OK ' if c==1 else 'BAD'} CHANGELOG.md: {c} '## [Unreleased]' heading(s)"); ok&= c==1
# Gap #1 (issue #171): a maintainer could tag a release and never add its ROADMAP.md Shipped row. A present,
# correctly-formatted Status cell passes; a missing row needs --highlights (prose only a human writes) and is
# refused without it, before anything is edited.
roadmap_path=Path("ROADMAP.md")
roadmap=roadmap_path.read_text()
status_cell=f"**Shipped (pre-alpha)** — `v{V_NEW}`, {date} (see [CHANGELOG.md](CHANGELOG.md))"
roadmap_has_row = status_cell in roadmap
if roadmap_has_row:
    print(f"OK  ROADMAP.md: 1 match(es) for Shipped row {status_cell!r}")
else:
    print(f"PENDING ROADMAP.md: no Shipped row yet for {status_cell!r} — --highlights TEXT supplies the row")
block=None
if notes:
    t=Path(notes).read_text()
    m=re.search(r"<!-- =+ BEGIN PASTE =+ -->\n(.*?)<!-- =+ END PASTE =+ -->", t, re.S)
    block=(m.group(1) if m else t).strip("\n")
    block=re.sub(r"<!--.*?-->\n?", "", block, flags=re.S)         # drop the source comments
    if not block.startswith(f"## [{V_NEW}]"): print(f"BAD notes: paste block must start with '## [{V_NEW}]'"); ok=False
    block=re.sub(r"^## \[[^\]\n]+\][^\n]*", f"## [{V_NEW}] — {date}", block, count=1)
    print(f"OK  notes: {len(block.splitlines())} lines, heading -> '## [{V_NEW}] — {date}'")
else:
    print("(no --notes: the section is renamed only; the lead/hardening block is NOT inserted)")
if f"\n## [{V_NEW}]" in cl:
    print(f"BAD CHANGELOG.md: ## [{V_NEW}] already exists — this release was already cut"); ok=False
if not ok: sys.exit(4)
if not roadmap_has_row:
    if not highlights: print(f"REFUSE: ROADMAP.md has no Shipped row for v{V_NEW}; pass --highlights TEXT"); sys.exit(3)
    if "|" in highlights or "\n" in highlights: print("REFUSE: --highlights must be one line without '|'"); sys.exit(3)
if mode=="dry": print("DRY RUN — nothing written"); sys.exit(0)
for path,old,new,n in edits: Path(path).write_text(Path(path).read_text().replace(old,new))
head,rest=cl.split("\n## [Unreleased]\n",1)
# R1/0.7.2: with --notes the block IS the resolved section (heading + body), so the
# working [Unreleased] body it resolves must be CONSUMED, not left below the paste —
# otherwise every lead paragraph and every ### entry ships twice. Without --notes the
# 0.7.1 shape is unchanged: the heading is inserted above the body that becomes it.
if block:
    j = rest.find("\n## [")
    rest = ("\n" + rest[j:].lstrip("\n")) if j >= 0 else "\n"
new_section = (block+"\n") if block else f"## [{V_NEW}] — {date}\n"
cl2=head+"\n## [Unreleased]\n\n"+new_section+rest
Path("CHANGELOG.md").write_text(cl2)
# internal/db/migrations_documented_test.go keeps a SHRINKING allowlist of migrations no shipped markdown names and
# fails when a listed one becomes documented — so every stem the pasted block documents leaves the list here.
tp=Path("internal/db/migrations_documented_test.go"); t=tp.read_text(); dropped=[]
for m in re.finditer(r'\n\t"(\d{4})_[a-z0-9_]+":[^\n]*', t):
    if f"**{m.group(1)}**" in cl2: dropped.append(m.group(0)); 
for d in dropped: t=t.replace(d,"")
if dropped: tp.write_text(t); print("undocumentedMigrations: dropped", [d.split('"')[1] for d in dropped])
if not roadmap_has_row:
    marker="\n## Shipped\n\n| Milestone | Highlights | Status |\n|---|---|---|\n"
    i=roadmap.index(marker)+len(marker)
    j=roadmap.index("\n## ", i)  # end of the Shipped table = the next ## heading
    row=f"| **v{V_NEW}** | {highlights} | {status_cell} |\n"
    roadmap_path.write_text(roadmap[:j]+row+roadmap[j:])
    print(f"ROADMAP.md: inserted Shipped row for v{V_NEW}")
print("edits applied")
PY
[ "$MODE" = apply ] || exit 0
git add docs/DESKTOP.md deploy/helm/wardyn/values.yaml internal/db/migrations_documented_test.go internal/version/version.go deploy/helm/wardyn/Chart.yaml ui/package.json README.md install.sh docs/ci/github-actions.yml docs/ci/azure-pipelines.yml threatmodel/THREAT-MODEL.md CHANGELOG.md ROADMAP.md
git commit -q -s -m "release: $TO"
git log --format='%h %an %s' -1
echo "NEXT: nice -n 10 make release-check   (must be green AFTER this commit, before any tag/push)"
