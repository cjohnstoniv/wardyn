#!/usr/bin/env bash
# Diagram gate: every fenced ```mermaid block in the public docs must (a) parse
# (rendered via mermaid-cli) and (b) pass the label-truth manifest — each
# load-bearing label string in a diagram must still exist at its cited source,
# so a renamed enum or status tag fails CI instead of silently going stale.
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
# Format: <needle>\t<file>   (needle greps the FILE; keeps diagrams code-true)
while IFS=$'\t' read -r needle src; do
  [ -z "$needle" ] && continue
  case "$needle" in \#*) continue;; esac
  if ! grep -qF -- "$needle" "$src"; then
    echo "  FAIL label-truth: '$needle' not found in $src"; fail=1
  fi
done <<'MANIFEST'
PENDING	internal/types/types.go
STARTING	internal/types/types.go
RUNNING	internal/types/types.go
COMPLETED	internal/types/types.go
FAILED	internal/types/types.go
KILLED	internal/types/types.go
STOPPED	internal/types/types.go
WAITING_FOR_CONFIRMATION	internal/types/types.go
minted_jti	internal/broker/broker.go
MintOnApproval	internal/broker/broker.go
UpdateRunStateIf	internal/api/runs.go
StopSandbox	internal/api/runs.go
wait_for_review	internal/types/types.go
kernel.sensor.blind	internal/api/runs.go
/api/v1/internal/groundtruth	internal/api/server.go
wardyn.run-id	cmd/wardyn-tetragon-ingest/main.go
MANIFEST

# status-tag consistency: the CC3 maturity word must match across docs
if ! grep -q 'CC3/Vault (Kata microVM) \*\*\[experimental\]\*\*' README.md; then
  echo "  FAIL consistency: README CC3 [experimental] tag missing/changed"; fail=1
fi
if ! grep -q 'Kata microVM \[experimental\]' threatmodel/THREAT-MODEL.md; then
  echo "  FAIL consistency: THREAT-MODEL ladder CC3 [experimental] tag missing/changed"; fail=1
fi

[ "$fail" -eq 0 ] && echo "diagram gate: PASS ($n blocks)" || { echo "diagram gate: FAIL"; exit 1; }
