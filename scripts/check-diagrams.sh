#!/usr/bin/env bash
# Diagram gate: every fenced ```mermaid block in the public docs must (a) parse
# (rendered via mermaid-cli), (b) pass the label-truth manifest — each
# load-bearing label string must exist BOTH at its cited source and in a
# diagram, so a renamed enum AND a stale/typo'd diagram label each fail CI —
# and (c) pass the diagram style rules below.
#
# Usage: scripts/check-diagrams.sh [--render-png DIR]   (PNGs for visual review)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DOCS=(README.md ARCHITECTURE.md threatmodel/THREAT-MODEL.md)
RENDER_DIR=""
[ "${1:-}" = "--render-png" ] && RENDER_DIR="${2:?--render-png needs a dir}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── extract fences ────────────────────────────────────────────────────────────
n=0
for doc in "${DOCS[@]}"; do
  awk -v doc="$(basename "$doc" .md)" -v tmp="$TMP" '
    /^```mermaid[[:space:]]*$/ { inblock=1; i++; f=sprintf("%s/%s-%02d.mmd", tmp, doc, i); next }
    /^```[[:space:]]*$/ && inblock { inblock=0; next }
    inblock { print > f }
  ' "$doc"
done
n=$(ls "$TMP"/*.mmd 2>/dev/null | wc -l)
[ "$n" -gt 0 ] || { echo "no mermaid fences found — check the DOCS list"; exit 1; }
echo "extracted $n mermaid blocks"

# ── syntax gate: render each with mermaid-cli ────────────────────────────────
# mmdc comes from ui devDependencies with puppeteer's chromium download skipped;
# point it at a browser that's already here: $CHROME_PATH, the playwright
# cache (the ui e2e suite installs one), or a system chrome/chromium.
MMDC="${MMDC:-ui/node_modules/.bin/mmdc}"
[ -x "$MMDC" ] || { echo "mmdc not found — run: cd ui && PUPPETEER_SKIP_DOWNLOAD=1 pnpm install"; exit 1; }
CHROME="${CHROME_PATH:-}"
if [ -z "$CHROME" ]; then
  CHROME="$(ls -t "$HOME"/.cache/ms-playwright/chromium-*/chrome-linux*/chrome 2>/dev/null | head -1 || true)"
fi
if [ -z "$CHROME" ]; then
  CHROME="$(command -v google-chrome || command -v chromium || command -v chromium-browser || true)"
fi
[ -n "$CHROME" ] || { echo "no chrome/chromium found for mermaid render"; exit 1; }
MMDC_ARGS=()
if [ -n "$CHROME" ]; then
  printf '{"executablePath": "%s", "args": ["--no-sandbox", "--disable-setuid-sandbox"]}\n' "$CHROME" > "$TMP/puppeteer.json"
  MMDC_ARGS+=(-p "$TMP/puppeteer.json")
fi

fail=0
for f in "$TMP"/*.mmd; do
  out="$TMP/$(basename "$f" .mmd).svg"
  if "$MMDC" "${MMDC_ARGS[@]}" -q -t neutral -i "$f" -o "$out" >/dev/null 2>"$TMP/err"; then
    echo "  ok  $(basename "$f")"
    if [ -n "$RENDER_DIR" ]; then
      mkdir -p "$RENDER_DIR"
      "$MMDC" "${MMDC_ARGS[@]}" -q -t neutral -b white -s 2 -i "$f" \
        -o "$RENDER_DIR/$(basename "$f" .mmd).png" >/dev/null 2>&1 || true
    fi
  else
    echo "  FAIL $(basename "$f") — mermaid parse/render error:"; sed 's/^/       /' "$TMP/err"; fail=1
  fi
done

# ── label-truth manifest: diagram label -> the source that must contain it ───
# Format: <needle>\t<file>. Both sides are required: the needle must appear at
# the cited source AND in some extracted diagram, so neither can drift alone.
while IFS=$'\t' read -r needle src; do
  [ -z "$needle" ] && continue
  case "$needle" in \#*) continue;; esac
  grep -qF -- "$needle" "$src" || { echo "  FAIL label-truth: '$needle' not found in $src"; fail=1; }
  grep -qF -- "$needle" "$TMP"/*.mmd || { echo "  FAIL label-truth: '$needle' is in no mermaid diagram"; fail=1; }
done <<'MANIFEST'
PENDING	internal/types/types.go
STARTING	internal/types/types.go
RUNNING	internal/types/types.go
COMPLETED	internal/types/types.go
FAILED	internal/types/types.go
KILLED	internal/types/types.go
STOPPED	internal/types/types.go
minted_jti	internal/broker/broker.go
kernel.sensor.blind	internal/api/runs_dispatch.go
/api/v1/internal/groundtruth	internal/api/server.go
wardyn.run-id	cmd/wardyn-tetragon-ingest/main.go
MANIFEST

# ── prose manifest: same both-sides rule for strings the docs name in TEXT ───
# Format: <needle>\t<source file>\t<doc>. These are not diagram labels, so they
# would fail the .mmd half above; they still pin doc prose to the code.
while IFS=$'\t' read -r needle src doc; do
  [ -z "$needle" ] && continue
  case "$needle" in \#*) continue;; esac
  for f in "$src" "$doc"; do
    grep -qF -- "$needle" "$f" || { echo "  FAIL prose-truth: '$needle' not found in $f"; fail=1; }
  done
done <<'PROSE'
WAITING_FOR_CONFIRMATION	internal/types/types.go	ARCHITECTURE.md
UpdateRunStateIf	internal/api/runs_lifecycle.go	ARCHITECTURE.md
wait_for_review	internal/types/types.go	ARCHITECTURE.md
PROSE

# ── style rules: a diagram nobody can read is worse than the table it replaced ─
# R0 flowcharts only (state/sequence diagrams have their own shape)
# R1 ≤12 nodes  R2 ≤12 edges  R6 ≤4 labeled edges — past that, use a table
# R3 edge label ≤32 chars — long labels render as free-floating boxes
#    (ponytail: 32 is today's ceiling, tighten to 24 once the overview fences shrink)
# R4 no edge may target a subgraph id — mermaid resolves it to an arbitrary member
# R5 no `direction` inside a subgraph — silently discarded once an edge crosses it
# R7 ≤3 `<br/>` per node label — more is a bullet list drawn as one blob
# R8 every fence needs an edge, unless marked `%% nesting` (containment, not flow)
for f in "$TMP"/*.mmd; do
  awk -v name="$(basename "$f")" -v maxlabel=32 '
    function bad(m) { printf "  FAIL style %s: %s\n", name, m; rc=1 }
    NR==1 { if ($0 !~ /^[[:space:]]*(flowchart|graph)/) { skip=1; exit } ; next }
    /%%[[:space:]]*nesting/ { nest=1 }
    /^[[:space:]]*%%/ { next }
    {
      l=$0
      if (gsub(/<br\/>/,"",l) > 3) bad("R7 >3 <br/> in one node label: " $0)
      while (match(l, /\|"[^"]*"\|/)) {
        lab=substr(l, RSTART+2, RLENGTH-4); labeled++
        if (length(lab) > maxlabel) bad("R3 edge label >" maxlabel " chars: " lab)
        l=substr(l, 1, RSTART-1) substr(l, RSTART+RLENGTH)
      }
      gsub(/"[^"]*"/, "", l)                                   # drop remaining label text
      while (gsub(/\[[^][]*\]|\([^()]*\)|\{[^{}]*\}/, "", l)) ; # drop node shapes
      if (l ~ /^[[:space:]]*direction[[:space:]]/) { if (depth > 0) bad("R5 direction inside a subgraph"); next }
      e = gsub(/-\.[^>]*->|-->|==>|---/, " ", l); edges += e
      if (sub(/^[[:space:]]*subgraph[[:space:]]+/, "", l)) { split(l, t, /[[:space:]]+/); sg[t[1]]=1; depth++; next }
      if (l ~ /^[[:space:]]*end[[:space:]]*$/) { depth--; next }
      n=split(l, t, /[[:space:]|&]+/)
      for (i=1; i<=n; i++) if (t[i] ~ /^[A-Za-z_][A-Za-z0-9_-]*$/) {
        if (!(t[i] in node)) { node[t[i]]=1; nodes++ }
        if (e) ep[t[i]]=1
      }
    }
    END {
      if (skip) exit 0
      for (x in ep) if (x in sg) bad("R4 edge targets subgraph id: " x)
      if (nodes > 12) bad("R1 " nodes " nodes (max 12)")
      if (edges > 12) bad("R2 " edges " edges (max 12)")
      if (labeled > 4) bad("R6 " labeled " labeled edges (max 4)")
      if (edges == 0 && !nest) bad("R8 no edges and no `%% nesting` marker")
      exit rc ? 1 : 0
    }
  ' "$f" || fail=1
done

# status-tag consistency: the CC3 maturity word must match across docs
if ! grep -q 'CC3/Vault (Kata microVM) \*\*\[experimental\]\*\*' README.md; then
  echo "  FAIL consistency: README CC3 [experimental] tag missing/changed"; fail=1
fi
if ! grep -q 'Kata microVM \[experimental\]' threatmodel/THREAT-MODEL.md; then
  echo "  FAIL consistency: THREAT-MODEL ladder CC3 [experimental] tag missing/changed"; fail=1
fi

[ "$fail" -eq 0 ] && echo "diagram gate: PASS ($n blocks)" || { echo "diagram gate: FAIL"; exit 1; }
