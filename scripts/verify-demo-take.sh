#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Verify a finished demo take actually proves what the video claims.
#
# THE WHOLE POINT: `make record-demo` exiting 0 has been wrong three separate
# times — a run that never ran (interactive mode), a workspace that never got
# written (read-only mount), and an approval granted to the WRONG host
# (.first() picked up Claude Code's telemetry endpoint while the intended host
# sat pending). Every one produced a green run and a dishonest video. So the
# take is checked against the audit trail and the filesystem, never the exit
# code.
#
#   scripts/verify-demo-take.sh [video.mp4]

set -uo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}" || exit 1
. "${REPO_ROOT}/scripts/lib/common.sh" 2>/dev/null || true
command -v wardyn_pick_docker_host >/dev/null 2>&1 && wardyn_pick_docker_host

VIDEO="${1:-}"
WS="${WARDYN_DEMO_WORKSPACE:-${HOME}/wardyn-demo/slugify}"
PASS=0; FAIL=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
head_() { printf '\n\033[1;35m── %s\033[0m\n' "$*"; }

head_ "The real run (act 5)"
# Select act 5's MAIN run by title. Not "the newest workspace-backed run":
# act 5 now also launches a second, PROOF run (PROOF_RUN_TITLE) to show the
# permanent grant holds, and that one is newer — so "newest" picks the run that
# deliberately does no work and raises no approval, and every check below would
# fail against it.
DEMO_TITLE="${WARDYN_DEMO_TITLE:-Slugify + reach two hosts}"
RUN=$(./wardyn runs list --json 2>/dev/null | DEMO_TITLE="${DEMO_TITLE}" python3 -c '
import sys, json, os
want = os.environ["DEMO_TITLE"]
d = json.load(sys.stdin); rs = d if isinstance(d, list) else d.get("items", [])
for r in rs:
    if (r.get("title") or "").strip() == want:
        print(r["id"]); break
else:
    # No title (an older take, or the field did not round-trip). Match the MAIN
    # run by its task text rather than taking the newest workspace-backed run —
    # the newest is the PROOF run, which by design does no work and raises no
    # approval, so every check below would fail against it.
    for r in rs:
        if "slugify" in (r.get("task") or ""):
            print(r["id"]); break
    else:
        for r in rs:
            if r.get("workspace_path") or r.get("repo", "").startswith("local:"):
                print(r["id"]); break
' 2>/dev/null)
if [[ -z "${RUN}" ]]; then
  bad "no workspace-backed run found — act 5 never launched"
else
  ok "run ${RUN}"
  AUDIT=$(./wardyn audit --run "${RUN}" --json 2>/dev/null)
  printf '%s' "${AUDIT}" | python3 -c '
import sys, json
from collections import Counter
try: d = json.load(sys.stdin)
except Exception: print("PARSE_FAIL"); sys.exit()
ev = d if isinstance(d, list) else d.get("events", [])
seen = Counter()
scopes = []
for e in ev:
    dd = e.get("data") or {}
    h = dd.get("host") or ""
    a = e.get("action", "")
    # run.workspace.egress is the always write-back and does NOT start with
    # egress/approval - collect it explicitly or the always beat reads as absent.
    if a.startswith(("egress", "approval")) or a == "run.workspace.egress":
        seen[(h, a)] += 1
    if a == "approval.decide":
        scopes.append((h, dd.get("decision_scope", "(none)")))
def has(host, action): return seen.get((host, action), 0) > 0
print("MODEL_ALLOWED", has("api.anthropic.com", "egress.allow"))
# NOT egress.pending. The ceiling clamp is member-only (inline_policy.go gates
# it on !isOperator) and the demo runs as operator, so wait_for_review SURVIVES:
# example.com is genuinely HELD, and an approved hold returns apApproved and
# logs ONLY egress.allow. Requiring egress.pending therefore FAILS a correct
# take and passes a degraded one — exactly backwards.
# An always decision does NOT leave a run-scoped approval.decide - it lands as
# run.workspace.egress (the write-back) plus the approved_egress row on the
# workspace. Accept either, or a correct take fails on where the event is filed.
# NOTE: no apostrophes in this block - it lives inside a single-quoted python -c.
print("HELD_DECIDED", has("example.com", "approval.decide") or any(a == "run.workspace.egress" for (_h, a) in seen))
print("HELD_ALLOWED", has("example.com", "egress.allow"))
# Informational: present only if the hold lapsed or a re-raise happened.
print("HELD_PENDING_INFO", has("example.com", "egress.pending"))
dd_appr = any(h.endswith("datadoghq.com") and a == "approval.decide" for (h, a) in seen)
print("TELEMETRY_APPROVED", dd_appr)
print("SCOPES", json.dumps(scopes))
' > /tmp/_demo_audit.$$ 2>/dev/null
  # shellcheck disable=SC1090
  while read -r k v; do
    case "$k" in
      MODEL_ALLOWED)      if [[ "${WARDYN_DEMO_SHELL_ACT5:-}" == "1" ]]; then
                            printf '    (shell Act 5: no model is used, so no anthropic egress expected)\n'
                          elif [[ "$v" == True ]]; then ok "api.anthropic.com allowed (the model path works)"
                          else bad "no egress.allow for api.anthropic.com"; fi ;;
      HELD_DECIDED)       [[ "$v" == True ]] && ok "example.com was decided on camera" || bad "example.com never decided — the approval beat did not happen" ;;
      HELD_ALLOWED)       [[ "$v" == True ]] && ok "example.com allowed (the held request completed)" || bad "example.com decided but never allowed" ;;
      HELD_PENDING_INFO)  [[ "$v" == True ]] && printf '    note: an egress.pending exists — a hold lapsed or re-raised\n' || true ;;
      TELEMETRY_APPROVED) [[ "$v" == False ]] && ok "telemetry host NOT approved (the .first() trap)" || bad "A TELEMETRY HOST WAS APPROVED — wrong host decided on camera" ;;
      SCOPES)             printf '    decision scopes: %s\n' "$v" ;;
    esac
  done < /tmp/_demo_audit.$$
  rm -f /tmp/_demo_audit.$$
fi

head_ "Real work landed on disk"
if [[ -d "${WS}/.git" ]]; then
  CHANGED=$(git -C "${WS}" status --porcelain 2>/dev/null | wc -l)
  [[ "${CHANGED}" -gt 0 ]] && ok "${CHANGED} file(s) changed in the workspace" || bad "workspace is untouched — the agent's edits never reached the host"
  [[ -f "${WS}/NOTES.md" ]] && ok "NOTES.md written" || bad "NOTES.md missing"
  # `export function slugify` — NOT a bare grep for "slugify": the fixture's own
  # header comment says "the agent will add slugify() here", so a substring match
  # passes on an untouched workspace. A false PASS in the verifier is worse than
  # no check at all.
  #
  # Only an AGENT writes this. The model-free variant
  # (WARDYN_DEMO_SHELL_ACT5=1) runs the existing tests and writes NOTES.md
  # instead, so asserting it there would be a false failure.
  if [[ "${WARDYN_DEMO_SHELL_ACT5:-}" == "1" ]]; then
    printf '    (shell Act 5: no agent, so no slugify() — NOTES.md is the work proof)\n'
  else
    grep -qE '(export +function|const) +slugify' "${WS}/src/slug.js" 2>/dev/null \
      && ok "slugify() really added to src/slug.js" || bad "slugify() not defined in src/slug.js"
  fi
else
  bad "workspace ${WS} is not a git repo"
fi

head_ "Permanent (always) grant is scoped to the right host"
# Read straight from the API: the `wardyn workspace` CLI has no list subcommand,
# and this shows the denied list beside the approved one.
WS_JSON=$(curl -s -m5 "${WARDYN_URL:-http://localhost:8080}/api/v1/workspaces" 2>/dev/null)
if [[ -n "${WS_JSON}" ]]; then
  printf '%s' "${WS_JSON}" | python3 -c '
import sys, json
try: d = json.load(sys.stdin)
except Exception: print("BADJSON"); sys.exit()
ws = d if isinstance(d, list) else (d.get("items") or d.get("workspaces") or [])
bad = False
for w in ws:
    ae = w.get("approved_egress") or []
    de = w.get("denied_egress") or []
    name = w.get("name")
    print("    %s: approved=%s denied=%s" % (name, ae, de))
    # The always beat must grant the host the demo actually held on — and
    # nothing else. A telemetry host here means the wrong approval was scoped
    # permanently, on camera, in a governance demo.
    for h in ae:
        if "datadog" in h or "anthropic" in h:
            bad = True
            print("    UNEXPECTED permanent grant: %s" % h)
print("ALWAYS_CLEAN", not bad)
' > /tmp/_demo_ws.$$ 2>/dev/null
  # Via a temp file, NOT a pipe: `| while read` runs in a subshell, so ok()/bad()
  # would increment counters that vanish — the script could then report success
  # on a real failure, which is the exact bug class it exists to catch.
  while read -r line; do
    case "$line" in
      "ALWAYS_CLEAN True")  ok "no unexpected host permanently allowed" ;;
      "ALWAYS_CLEAN False") bad "an unexpected host is in approved_egress" ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < /tmp/_demo_ws.$$
  rm -f /tmp/_demo_ws.$$
else
  printf '    (workspaces API unreachable — stack down?)\n'
fi

head_ "Narration"
TL="${REPO_ROOT}/ui/test-results/demo-video/narration.json"
if [[ -s "${TL}" ]]; then
  python3 - "${TL}" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); c = d.get("cues", [])
ov = sum(1 for i, x in enumerate(c) if i and x["tMs"] < c[i-1]["tMs"] + c[i-1]["durMs"])
print(f"  cues: {len(c)}   speech: {sum(x['durMs'] for x in c)/1000:.0f}s   overlaps: {ov}")
sys.exit(0 if (c and ov == 0) else 1)
PY
  [[ $? -eq 0 ]] && ok "timeline complete, no overlapping lines" || bad "narration timeline has overlaps or is empty"
else
  bad "no narration timeline — the take is silent"
fi

head_ "The artifact"
if [[ -n "${VIDEO}" && -s "${VIDEO}" ]]; then
  FF="$(command -v ffmpeg.exe || ls /mnt/c/Users/*/AppData/Local/Microsoft/WinGet/Packages/Gyan.FFmpeg_*/ffmpeg-*/bin/ffmpeg.exe 2>/dev/null | head -1)"
  INFO=$("${FF}" -hide_banner -i "$(wslpath -w "${VIDEO}" 2>/dev/null || echo "${VIDEO}")" 2>&1)
  printf '%s\n' "${INFO}" | grep -E 'Duration|Stream #' | sed 's/^/    /'
  grep -q 'Audio:' <<<"${INFO}" && ok "has an audio track" || bad "NO AUDIO — narration never made it in"
  grep -q '1920x1080' <<<"${INFO}" && ok "1920x1080" || bad "unexpected resolution"
else
  printf '    (no video passed; skipping)\n'
fi

printf '\n\033[1m%s passed, %s failed\033[0m\n' "${PASS}" "${FAIL}"
[[ "${FAIL}" -eq 0 ]]
