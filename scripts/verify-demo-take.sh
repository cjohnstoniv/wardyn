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
# WHICH take is on the bench comes from WARDYN_DEMO_VIDEO (record-demo.sh
# --video exports it). Unset means the legacy end-to-end walkthrough, whose
# checks ARE video 02's — so the default path is byte-for-byte what this script
# always did. The narration and artifact checks at the bottom are SHARED: they
# run for every take of every video, because "it recorded" and "it has a voice"
# are claims no video gets to skip.
#
#   scripts/verify-demo-take.sh [video.mp4]
#   WARDYN_DEMO_VIDEO=05 scripts/verify-demo-take.sh video.mp4

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

# Video 02's own checks — the act-5 governance beats, exactly as this script has
# always run them, now behind a name so the dispatch below can pick them.
#
# A FUNCTION, and deliberately not a subshell or a pipeline: ok()/bad() increment
# counters that must survive to the summary, and a subshell is precisely how this
# script once reported success over a real failure (see the temp-file note further
# down). The body is also deliberately NOT re-indented — those `python3 -c '...'`
# blocks are single-quoted PYTHON, where leading whitespace is syntax.
check_video_02() {
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
}

# --- which video is on the bench ---------------------------------------------
#
# Unset is the legacy walkthrough: it films act 5, so it gets act 5's checks.
# The other nine videos are stubs until their spec lands — each one's assertions
# ship WITH the spec that films them, because a check written before the beat
# exists is a guess, and a guess that passes is worse than no check at all. What
# they do get today is every SHARED check below, which is already enough to
# catch a take that recorded nothing, recorded silently, or recorded at the
# wrong size.
# --- video 02: add a workspace ------------------------------------------------
# The take onboards the slugify workspace WRITABLE and stores one write-only
# secret with a canary value. Checks are against the API and the audit trail,
# never the driver's exit code.
check_video_02_workspace() {
head_ "Video 02 · the workspace"
WS_JSON=$(curl -fsS "http://localhost:${WARDYN_UP_PORT:-8080}/api/v1/workspaces" 2>/dev/null || echo '[]')
python3 - "$WS_JSON" <<'PYEOF'
import sys, json
d = json.loads(sys.argv[1]); items = d if isinstance(d, list) else d.get("items", d.get("workspaces", []))
ws = next((w for w in items if w.get("name") == "slugify"), None)
assert ws, "no slugify workspace — beat 2 never landed"
mounts = ws.get("mounts") or ws.get("workspace_mounts") or []
sys.exit(0)
PYEOF
[[ $? -eq 0 ]] && ok "slugify workspace exists" || bad "slugify workspace missing — beat 2 never landed"

head_ "Video 02 · the secret"
NAMES=$(curl -fsS "http://localhost:${WARDYN_UP_PORT:-8080}/api/v1/secrets" 2>/dev/null | python3 -c 'import sys,json;print("\n".join(json.load(sys.stdin).get("names",[])))' 2>/dev/null)
grep -qx "deploy-webhook-token" <<<"${NAMES}" && ok "deploy-webhook-token stored" || bad "secret missing — beat 4 never saved"
# The canary must not appear in the audit trail (a write-only store that logs
# the value would be the leak the video denies).
AUD=$(curl -fsS "http://localhost:${WARDYN_UP_PORT:-8080}/api/v1/audit?limit=500" 2>/dev/null || true)
grep -q "WARDYN-V02-CANARY" <<<"${AUD}" && bad "the canary VALUE appears in the audit trail" || ok "canary value nowhere in the audit trail"
}

# --- video 03: your first run ---------------------------------------------------
# One background shell run, COMPLETED, whose inventory file really landed in
# the writable workspace.
check_video_03_first_run() {
head_ "Video 03 · the run"
V03_TITLE="${WARDYN_DEMO_V03_TITLE:-Take inventory — first governed run}"
RUNS=$(curl -fsS "http://localhost:${WARDYN_UP_PORT:-8080}/api/v1/runs" 2>/dev/null || echo '[]')
STATE=$(python3 - "$RUNS" "$V03_TITLE" <<'PYEOF'
import sys, json
d = json.loads(sys.argv[1]); rs = d if isinstance(d, list) else d.get("items", d.get("runs", []))
m = [r for r in rs if r.get("title") == sys.argv[2]]
print(m[0]["state"] if m else "MISSING")
PYEOF
)
[[ "${STATE}" == "COMPLETED" ]] && ok "run '${V03_TITLE}' COMPLETED" || bad "run state=${STATE} (want COMPLETED)"

head_ "Video 03 · the artifact on disk"
INV="${WARDYN_DEMO_WORKSPACE:-${HOME}/wardyn-demo/slugify}/NOTES-INVENTORY.txt"
[[ -s "${INV}" ]] && ok "inventory file landed: ${INV}" || bad "no ${INV} — the write never reached the host"
}

# --- video 06: record a run -----------------------------------------------------
# The take records ONE session on its OWN workspace (`record-demo`), saves the
# synthesized policy under the derived name, promotes the two observed hosts,
# then replays the same session CONFINED and denies a host the recording never
# saw. Every claim below is read from the control plane, because a take that
# promoted nothing, saved nothing, or replayed unconfined exits 0 all the same.
check_video_06_record() {
head_ "Video 06 · the recorded session"
V06_API="http://localhost:${WARDYN_UP_PORT:-8080}"
V06_WS="${WARDYN_DEMO_RECORD_WS_NAME:-record-demo}"
V06_KEY="build-test"                  # sessionKeyOf("build & test"), session-helpers.ts
V06_POLICY="record-demo-build-test"   # policyNameFor(workspace, session) — the spec asserts this pre-fill
V06_WSJ=$(curl -fsS "${V06_API}/api/v1/workspaces" 2>/dev/null || echo '[]')
V06_POLJ=$(curl -fsS "${V06_API}/api/v1/policies" 2>/dev/null || echo '[]')
# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish — the bug class this script exists for.
python3 - "$V06_WSJ" "$V06_POLJ" "$V06_WS" "$V06_KEY" "$V06_POLICY" <<'PYEOF' > /tmp/_demo_v06.$$ 2>/dev/null
import sys, json
def items(raw):
    try: d = json.loads(raw)
    except Exception: return []
    if isinstance(d, list): return d
    return d.get("items") or d.get("workspaces") or d.get("policies") or []
ws_all, pol_all = items(sys.argv[1]), items(sys.argv[2])
name, key, policy = sys.argv[3], sys.argv[4], sys.argv[5]
ws = next((w for w in ws_all if w.get("name") == name), None)
print("V06_WS_EXISTS", bool(ws))
rr = (ws or {}).get("record_results") or {}
opened = rr.get(key) or {}
# A confined replay lands under its OWN key (recordVerifyKeyPrefix, record.go)
# so it never clobbers the open capture it replays.
confined = rr.get("verify:" + key) or {}
print("V06_RECORDED", opened.get("status") == "recorded")
print("V06_PROMOTED", bool(opened.get("egress_promoted")))
print("V06_REPLAY_RUN", confined.get("run_id") or "-")
# Promotion writes egress: REQUIREMENT rows on the workspace overlay
# (handlePromoteRecordEgress) and leaves the legacy approved_egress lane
# read-only — accept either, since a host either grants egress or it does not.
reqs = (ws or {}).get("effective_requirements") or (ws or {}).get("requirements") or {}
appr = set((ws or {}).get("approved_egress") or [])
def granted(h):
    return h in appr or (reqs.get("egress:" + h) or {}).get("level") == "required"
missing = [h for h in ("pypi.org", "files.pythonhosted.org") if not granted(h)]
print("V06_HOSTS", not missing, ",".join(missing) or "-")
print("V06_POLICY", any((p or {}).get("name") == policy for p in pol_all))
PYEOF
V06_REPLAY="-"
while read -r k v extra; do
  case "$k" in
    V06_WS_EXISTS)  [[ "$v" == True ]] && ok "workspace ${V06_WS} exists" || bad "no ${V06_WS} workspace — the take never staged" ;;
    V06_RECORDED)   [[ "$v" == True ]] && ok "session '${V06_KEY}' status=recorded" || bad "no recorded session '${V06_KEY}' — the capture never settled (empty capture = shoot in containerized mode)" ;;
    V06_PROMOTED)   [[ "$v" == True ]] && ok "observed hosts promoted onto the workspace" || bad "egress_promoted is not set — beat 5's 'Approve 2 observed hosts' never landed" ;;
    V06_HOSTS)      [[ "$v" == True ]] && ok "pypi.org + files.pythonhosted.org granted on ${V06_WS}" || bad "canon hosts missing from ${V06_WS}: ${extra}" ;;
    V06_POLICY)     [[ "$v" == True ]] && ok "policy '${V06_POLICY}' saved" || bad "no '${V06_POLICY}' policy — beat 4's save failed on camera" ;;
    V06_REPLAY_RUN) V06_REPLAY="$v" ;;
  esac
done < /tmp/_demo_v06.$$
rm -f /tmp/_demo_v06.$$

head_ "Video 06 · the confined replay"
if [[ "${V06_REPLAY}" == "-" ]]; then
  bad "no confined replay on ${V06_WS} — beats 5/6 never ran"
else
  ok "replay run ${V06_REPLAY}"
  V06_AUD=$(curl -fsS "${V06_API}/api/v1/audit?run_id=${V06_REPLAY}&limit=1000" 2>/dev/null || echo '[]')
  python3 - "$V06_AUD" <<'PYEOF' > /tmp/_demo_v06b.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
def hits(host, *actions):
    out = []
    for e in ev:
        dd = e.get("data") or {}
        if (dd.get("host") or e.get("target") or "").split(":")[0] != host: continue
        if e.get("action") in actions: out.append(e)
    return out
# The unseen host was HELD and DENIED on camera. Either lane proves it: the
# decision itself (approval.decide/DENIED) or the refusal the proxy logged.
denied = [e for e in hits("example.com", "approval.decide") if ((e.get("data") or {}).get("decision") == "DENIED")]
print("V06_UNSEEN_DENIED", bool(denied) or bool(hits("example.com", "egress.deny")))
print("V06_UNSEEN_ALLOWED", bool(hits("example.com", "egress.allow")))
# "Default-deny now. Same commands, same two hosts, and nothing to approve."
both = all(hits(h, "egress.allow") for h in ("pypi.org", "files.pythonhosted.org"))
asked = any(hits(h, "approval.decide") for h in ("pypi.org", "files.pythonhosted.org"))
print("V06_CONFINED_OK", both and not asked)
PYEOF
  while read -r k v; do
    case "$k" in
      V06_UNSEEN_DENIED)  [[ "$v" == True ]] && ok "example.com denied in the confined replay" || bad "no deny for example.com — beat 6's on-camera refusal is not on the record" ;;
      V06_UNSEEN_ALLOWED) [[ "$v" == False ]] && ok "example.com never allowed" || bad "EXAMPLE.COM WAS ALLOWED in a replay the video calls confined" ;;
      V06_CONFINED_OK)    [[ "$v" == True ]] && ok "both recorded hosts reached with nothing to approve" || bad "the replay did not reach both canon hosts silently — the promotion never reached its policy" ;;
    esac
  done < /tmp/_demo_v06b.$$
  rm -f /tmp/_demo_v06b.$$
fi
}

# --- video 07: approvals & egress scopes ----------------------------------------
# The take decides FOUR things on camera: crates.io approved at the default
# scope inside a demo sandbox, ingest.sentry.io denied there, crates.io approved
# with `always` on the real `egress-lab` workspace, and then a fresh run that
# reaches the same host with nothing to click. The two hosts are this video's
# alone (DA5), so a stack-wide approval.decide query is unambiguous.
check_video_07_approvals() {
head_ "Video 07 · the decisions"
V07_API="http://localhost:${WARDYN_UP_PORT:-8080}"
V07_WS="${WARDYN_DEMO_EGRESS_WS_NAME:-egress-lab}"
V07_HELD="crates.io"
V07_TELE="ingest.sentry.io"
V07_PROOF_TITLE="${WARDYN_DEMO_V07_PROOF_TITLE:-Same host, no approval}"
V07_DEC=$(curl -fsS "${V07_API}/api/v1/audit?action=approval.decide&limit=1000" 2>/dev/null || echo '[]')
python3 - "$V07_DEC" "$V07_HELD" "$V07_TELE" <<'PYEOF' > /tmp/_demo_v07.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
held, tele = sys.argv[2], sys.argv[3]
# approval.decide carries the host at the TOP level of data (approval.go lifts it
# out of requested_scope) plus the raw decision_scope: "" means "no scope
# recorded", which Normalize() reads as today's default, run.
dec = []
for e in ev:
    dd = e.get("data") or {}
    dec.append((dd.get("host") or "", dd.get("decision") or "", dd.get("decision_scope") or ""))
def any_(host, state, scopes): return any(h == host and s == state and sc in scopes for h, s, sc in dec)
# Beat 2: the bare split-button Approve — today's default scope, this run.
print("V07_HELD_RUN", any_(held, "APPROVED", ("", "run")))
# Beat 6: the whole point of the video.
print("V07_HELD_ALWAYS", any_(held, "APPROVED", ("always",)))
# Beat 5: the host nobody asked for.
print("V07_TELE_DENIED", any_(tele, "DENIED", ("", "run", "once", "until", "always")))
# The .first() trap, in the direction that matters for THIS video: the telemetry
# host must never have been approved at any scope, on camera or otherwise.
print("V07_TELE_APPROVED", any_(tele, "APPROVED", ("", "run", "once", "until", "always")))
print("V07_SCOPES", json.dumps([(h, s, sc) for h, s, sc in dec if h in (held, tele)]))
PYEOF
while read -r k v; do
  case "$k" in
    V07_HELD_RUN)      [[ "$v" == True ]] && ok "${V07_HELD} approved at the default (this run) scope" || bad "no default-scope approve for ${V07_HELD} — beat 2 never happened" ;;
    V07_HELD_ALWAYS)   [[ "$v" == True ]] && ok "${V07_HELD} approved with scope=always" || bad "no always decision for ${V07_HELD} — beat 6, the point of the video, never landed" ;;
    V07_TELE_DENIED)   [[ "$v" == True ]] && ok "${V07_TELE} denied on camera" || bad "${V07_TELE} was never denied — beat 5 never happened" ;;
    V07_TELE_APPROVED) [[ "$v" == False ]] && ok "${V07_TELE} never approved (the wrong-host trap)" || bad "THE TELEMETRY HOST WAS APPROVED — the wrong row was decided on camera" ;;
    V07_SCOPES)        printf '    decisions: %s\n' "$v" ;;
  esac
done < /tmp/_demo_v07.$$
rm -f /tmp/_demo_v07.$$

head_ "Video 07 · the receipt on ${V07_WS}"
V07_WSJ=$(curl -fsS "${V07_API}/api/v1/workspaces" 2>/dev/null || echo '[]')
python3 - "$V07_WSJ" "$V07_WS" "$V07_HELD" "$V07_TELE" <<'PYEOF' > /tmp/_demo_v07b.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ws_all = d if isinstance(d, list) else d.get("items") or d.get("workspaces") or []
name, held, tele = sys.argv[2], sys.argv[3], sys.argv[4]
ws = next((w for w in ws_all if w.get("name") == name), None)
print("V07_WS_EXISTS", bool(ws))
appr = set((ws or {}).get("approved_egress") or [])
reqs = (ws or {}).get("effective_requirements") or (ws or {}).get("requirements") or {}
def granted(h):
    # approve-always writes approved_egress (persistWorkspaceEgressDecision);
    # the requirements lane is read too so the check survives a lane change.
    return h in appr or (reqs.get("egress:" + h) or {}).get("level") == "required"
print("V07_HELD_GRANTED", granted(held))
print("V07_TELE_GRANTED", granted(tele))
print("V07_ALLOWED", json.dumps(sorted(appr)))
PYEOF
while read -r k v; do
  case "$k" in
    V07_WS_EXISTS)    [[ "$v" == True ]] && ok "workspace ${V07_WS} exists" || bad "no ${V07_WS} workspace — beforeAll never staged it" ;;
    V07_HELD_GRANTED) [[ "$v" == True ]] && ok "${V07_HELD} is permanently allowed on ${V07_WS}" || bad "${V07_HELD} is NOT on ${V07_WS} — beat 7's 'Allowed hosts · 1' receipt is not real" ;;
    V07_TELE_GRANTED) [[ "$v" == False ]] && ok "${V07_TELE} is not permanently allowed" || bad "${V07_TELE} IS PERMANENTLY ALLOWED — a denied host was granted for good" ;;
    V07_ALLOWED)      printf '    approved_egress: %s\n' "$v" ;;
  esac
done < /tmp/_demo_v07b.$$
rm -f /tmp/_demo_v07b.$$

head_ "Video 07 · the run that never had to ask"
V07_RUNS=$(curl -fsS "${V07_API}/api/v1/runs" 2>/dev/null || echo '[]')
V07_PROOF=$(python3 - "$V07_RUNS" "$V07_PROOF_TITLE" <<'PYEOF'
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
rs = d if isinstance(d, list) else d.get("items") or d.get("runs") or []
m = [r for r in rs if (r.get("title") or "").strip() == sys.argv[2]]
print(m[0]["id"] if m else "-")
PYEOF
)
if [[ "${V07_PROOF}" == "-" || -z "${V07_PROOF}" ]]; then
  bad "no run titled '${V07_PROOF_TITLE}' — beat 8 never launched"
else
  ok "proof run ${V07_PROOF}"
  V07_AUD=$(curl -fsS "${V07_API}/api/v1/audit?run_id=${V07_PROOF}&limit=1000" 2>/dev/null || echo '[]')
  python3 - "$V07_AUD" "$V07_HELD" <<'PYEOF' > /tmp/_demo_v07c.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
host = sys.argv[2]
def acts(*names):
    return [e for e in ev
            if e.get("action") in names
            and ((e.get("data") or {}).get("host") or e.get("target") or "").split(":")[0] == host]
print("V07_PROOF_ALLOWED", bool(acts("egress.allow")))
# The whole beat is that NOTHING was raised: a pending decision (or a fresh
# approval) here means the permanent grant never reached this run's allowlist,
# and the outro's "the decision outlived the run that raised it" is false.
print("V07_PROOF_SILENT", not acts("egress.pending", "approval.decide"))
PYEOF
  while read -r k v; do
    case "$k" in
      V07_PROOF_ALLOWED) [[ "$v" == True ]] && ok "${V07_HELD} allowed on the proof run" || bad "no egress.allow for ${V07_HELD} — beat 8's command never got through" ;;
      V07_PROOF_SILENT)  [[ "$v" == True ]] && ok "no approval raised on the proof run" || bad "the proof run RAISED an approval for ${V07_HELD} — 'nothing to click' is false" ;;
    esac
  done < /tmp/_demo_v07c.$$
  rm -f /tmp/_demo_v07c.$$
fi
}

case "${WARDYN_DEMO_VIDEO:-}" in
  ""|05) check_video_02 ;;
  02) check_video_02_workspace ;;
  03) check_video_03_first_run ;;
  06) check_video_06_record ;;
  07) check_video_07_approvals ;;
  01|04|08|09|10)
    head_ "Video ${WARDYN_DEMO_VIDEO}"
    printf '    video-specific checks TBD by spec\n'
    ;;
  *) head_ "Video ${WARDYN_DEMO_VIDEO}"
     bad "unknown WARDYN_DEMO_VIDEO=${WARDYN_DEMO_VIDEO} — expected 01..10, or unset for the walkthrough" ;;
esac

# --- shared: every take, every video -----------------------------------------

head_ "Narration"
TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video}/narration.json"
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
