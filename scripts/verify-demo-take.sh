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
DEMO_TITLE="${WARDYN_DEMO_TITLE:-Add slugify — one off-list host}"
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

# --- video 08: policies & confinement -------------------------------------------
# The take authors ONE policy (`nightly-triage`) by PASTING a prepared CC1 spec
# over the editor's CC2-floored starter, then launches a shell run BY REFERENCE
# to it. Three ways that goes green while filming a lie, and each gets a check:
#   - the paste silently failed and the STARTER spec was saved (CC2, one host),
#     so the narrator describes a policy that is not the one on screen;
#   - the run shipped `inline_policy` instead of `policy_id` (the radio-vs-
#     payload bug this campaign fixed), so the Identity widget's Policy row is
#     someone else's id — or absent;
#   - the run never actually ran, so "every run after it, already governed" is
#     narrated over a sandbox that failed to start.
# Deliberately NOT checked here: which barrier the run got. That is the host's
# answer, not the video's claim (the policy floors at CC1 and the wizard picks
# the strongest tier available), and the spec asserts it against /healthz.
check_video_08_policies() {
head_ "Video 08 · the policy"
V08_API="http://localhost:${WARDYN_UP_PORT:-8080}"
V08_POLICY="${WARDYN_DEMO_V08_POLICY:-nightly-triage}"
V08_TITLE="${WARDYN_DEMO_V08_TITLE:-Nightly triage, governed by policy}"
V08_POLJ=$(curl -fsS "${V08_API}/api/v1/policies" 2>/dev/null || echo '[]')
# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish — the bug class this script exists for.
python3 - "$V08_POLJ" "$V08_POLICY" <<'PYEOF' > /tmp/_demo_v08.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
pols = d if isinstance(d, list) else d.get("items") or d.get("policies") or []
name = sys.argv[2]
p = next((x for x in pols if (x or {}).get("name") == name), None)
print("V08_POL_EXISTS", bool(p))
print("V08_POL_ID", (p or {}).get("id") or "-")
spec = (p or {}).get("spec") or {}
# The four fields the narrator says out loud, in the order the SAY lines say
# them. allowed_domains is compared as a SET: the server is free to sort it.
print("V08_POL_HOSTS", sorted(spec.get("allowed_domains") or []) == ["github.com", "npmjs.org"])
print("V08_POL_HOLD", (spec.get("first_use_approval") or "") == "wait_for_review")
# The STARTER_SPEC trap (policies.tsx): its floor is CC2 and it allows exactly
# one host. A CC2 here means the paste never replaced the prefilled editor.
print("V08_POL_FLOOR", (spec.get("min_confinement_class") or ""))
# -1, not 0, when there is no policy at all: an absent row must never read as
# "no grants" and score a PASS on a take that never created anything.
print("V08_POL_GRANTS", len(spec.get("eligible_grants") or []) if p else -1)
PYEOF
V08_POL_ID="-"
while read -r k v; do
  case "$k" in
    V08_POL_EXISTS) [[ "$v" == True ]] && ok "policy '${V08_POLICY}' exists" || bad "no '${V08_POLICY}' policy — beat 2's Create never landed" ;;
    V08_POL_ID)     V08_POL_ID="$v" ;;
    V08_POL_HOSTS)  [[ "$v" == True ]] && ok "two allowed hosts (github.com, npmjs.org)" || bad "allowed_domains is not the pasted pair — the spec on camera is not the spec that saved" ;;
    V08_POL_HOLD)   [[ "$v" == True ]] && ok "first_use_approval=wait_for_review (unlisted hosts are HELD)" || bad "first_use_approval is not wait_for_review — 'held for your approval' is false" ;;
    V08_POL_FLOOR)  [[ "$v" == CC1 ]] && ok "barrier floor CC1 (Fence) — the pasted spec, not the CC2 starter" || bad "min_confinement_class=${v}, want CC1 — the CC2 STARTER_SPEC was saved instead of the paste" ;;
    V08_POL_GRANTS) case "$v" in
                      0)  ok "no credential grants" ;;
                      -1) bad "no policy to read grants from" ;;
                      *)  bad "the policy carries ${v} grant(s) — the take authored more than it described" ;;
                    esac ;;
  esac
done < /tmp/_demo_v08.$$
rm -f /tmp/_demo_v08.$$

head_ "Video 08 · the run that referenced it"
V08_RUNS=$(curl -fsS "${V08_API}/api/v1/runs" 2>/dev/null || echo '[]')
python3 - "$V08_RUNS" "$V08_TITLE" "$V08_POL_ID" <<'PYEOF' > /tmp/_demo_v08b.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
rs = d if isinstance(d, list) else d.get("items") or d.get("runs") or []
title, pol = sys.argv[2], sys.argv[3]
m = [r for r in rs if (r.get("title") or "").strip() == title]
r = m[0] if m else None
print("V08_RUN_ID", (r or {}).get("id") or "-")
# policy_id is omitempty on the wire (types.go AgentRun) — absent means the run
# shipped an inline_policy instead, which is the radio-vs-payload bug in the
# direction that matters: the video claims it launched BY REFERENCE.
print("V08_RUN_POLICY", ((r or {}).get("policy_id") or "-") == pol and pol != "-")
print("V08_RUN_POLICY_SAW", (r or {}).get("policy_id") or "(none)")
print("V08_RUN_STATE", (r or {}).get("state") or "MISSING")
PYEOF
V08_RUN="-"
while read -r k v; do
  case "$k" in
    V08_RUN_ID)         V08_RUN="$v"
                        [[ "$v" == "-" ]] && bad "no run titled '${V08_TITLE}' — beat 4 never launched" || ok "run ${v}" ;;
    V08_RUN_POLICY)     [[ "$v" == True ]] && ok "the run carries policy_id=${V08_POL_ID}" || bad "the run's policy_id is not '${V08_POLICY}' — it did not launch by reference" ;;
    V08_RUN_POLICY_SAW) printf '    run.policy_id: %s (policy row: %s)\n' "$v" "${V08_POL_ID}" ;;
    V08_RUN_STATE)      case "$v" in
                          COMPLETED)                    ok "run COMPLETED" ;;
                          STOPPED|ARCHIVED|FAILED|KILLED) bad "run reached ${v}, not COMPLETED — the take narrates a governed run that worked" ;;
                          *)                            bad "run state=${v} — it never reached a terminal state on camera" ;;
                        esac ;;
  esac
done < /tmp/_demo_v08b.$$
rm -f /tmp/_demo_v08b.$$

head_ "Video 08 · the run's own audit trail"
if [[ "${V08_RUN}" == "-" ]]; then
  printf '    (no run to audit)\n'
else
  V08_AUD=$(curl -fsS "${V08_API}/api/v1/audit?run_id=${V08_RUN}&limit=1000" 2>/dev/null || echo '[]')
  python3 - "$V08_AUD" <<'PYEOF' > /tmp/_demo_v08c.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
acts = [e.get("action") for e in ev]
# The two rows every governed launch writes (runs.go handleCreateRun,
# runs_dispatch.go). A run row with no audit behind it would mean the trail the
# whole product sells did not record the one launch this video is about.
print("V08_AUD_CREATE", "run.create" in acts)
print("V08_AUD_DISPATCH", "run.exec" in acts)
# No spaces in the payload: the reader below is `read -r k v`, which would
# otherwise keep only the first word of a multi-element list.
print("V08_AUD_FAILED", json.dumps(sorted({
    e.get("action") for e in ev if e.get("outcome") == "failure"}), separators=(",", ":")))
PYEOF
  while read -r k v; do
    case "$k" in
      V08_AUD_CREATE)   [[ "$v" == True ]] && ok "run.create is on the audit trail" || bad "no run.create for ${V08_RUN} — the launch left no record" ;;
      V08_AUD_DISPATCH) [[ "$v" == True ]] && ok "run.exec is on the audit trail" || bad "no run.exec for ${V08_RUN} — the sandbox was never scheduled" ;;
      V08_AUD_FAILED)   [[ "$v" == "[]" ]] || printf '    note: failure-outcome rows present: %s\n' "$v" ;;
    esac
  done < /tmp/_demo_v08c.$$
  rm -f /tmp/_demo_v08c.$$
fi
}

# Video 09 · the CI take. THE ARTIFACTS ARE THE EVIDENCE, NOT THE STACK. Every
# other check in this file interrogates the live :8080 console, because that
# stack is still up when a take finishes. Video 09's is not: beats 1-4 stand up
# a SEPARATE compose project (wardyn-ci-demo on :8099) with WARDYN_CI_KEEP=1 —
# kept only long enough for beats 5-6 to film it — and the take's own staging
# tells the operator to tear it down right after beat 6. Verification frequently
# runs after that, and on a retake the :8099 stack answering may well be the NEXT
# take's. So what is checked is ci-artifacts/, which is (a) durable, (b) exactly
# what beat 4 puts on camera and calls "the receipts", and (c) the same file the
# browser spec reads to learn which run it is filming. The live stack is probed
# last and best-effort: present and correct is a bonus, absent is a note.
check_video_09() {
head_ "Video 09 · the pipeline's receipts"
V09_OUT="${WARDYN_CI_OUT:-${REPO_ROOT}/ci-artifacts}"
V09_TASK="${WARDYN_CI_TASK:-echo hello from a governed sandbox}"
for f in run.json run.log audit.json; do
  [[ -s "${V09_OUT}/${f}" ]] && ok "ci-artifacts/${f}" \
    || bad "no ${V09_OUT}/${f} — beat 4 films three files and narrates them as the receipts"
done

# waitForRun's own verdict, in the CLI's words (cmd/wardyn/commands.go:434 —
# `run %s finished: state %s, agent exit code %d`). run.log is that command's
# STDERR, tee'd live by ci-run.sh; the agent's own stdout never lands here, it
# goes to the run's log/recording. So this line, not the task's echo, is what
# run.log has to prove — and it happens to carry both numbers the video films.
if [[ -s "${V09_OUT}/run.log" ]]; then
  grep -qE 'finished: state COMPLETED, agent exit code 0$' "${V09_OUT}/run.log" \
    && ok "run.log carries 'finished: state COMPLETED, agent exit code 0'" \
    || bad "run.log has no COMPLETED/exit-0 verdict from --wait — beat 3 films that number as zero"
fi

# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish (see check_video_08_policies).
python3 - "${V09_OUT}/run.json" "${V09_OUT}/audit.json" "${V09_TASK}" <<'PYEOF' > /tmp/_demo_v09.$$ 2>/dev/null
import sys, json
def load(p):
    try:
        with open(p) as f: return json.load(f)
    except Exception: return None
run = load(sys.argv[1]) or {}
aud = load(sys.argv[2])
print("V09_RUN_ID", run.get("id") or "-")
print("V09_RUN_STATE", run.get("state") or "MISSING")
# The board card's headline is runHeadline(run) = title || task, and a CLI run
# carries no title — so the browser half locates its card by this exact string.
# A drifted WARDYN_CI_TASK means beats 5-6 filmed a card nobody can find again.
print("V09_RUN_TASK", "MATCH" if (run.get("task") or "") == sys.argv[3] else "DRIFT")
ev = aud if isinstance(aud, list) else (aud or {}).get("events") or []
print("V09_AUD_COUNT", len(ev))
acts = {e.get("action") for e in ev}
print("V09_AUD_CREATE", "run.create" in acts)
print("V09_AUD_COMPLETE", "run.complete" in acts)
# The ONE field the video films twice: beat 3 as the shell's `echo $?`, beat 6
# as the console's "agent exit 0" chip (exitCodeFromAudit reads run.complete's
# data.exit_code, and so does the CLI's own --wait). Two surfaces, one number —
# which is the whole point of filming both.
codes = [(e.get("data") or {}).get("exit_code") for e in ev if e.get("action") == "run.complete"]
codes = [c for c in codes if c is not None]
print("V09_AUD_EXIT", codes[-1] if codes else "MISSING")
PYEOF
V09_RUN="-"
while read -r k v; do
  case "$k" in
    V09_RUN_ID)       V09_RUN="$v"
                      [[ "$v" == "-" ]] && bad "run.json carries no run id — the pipeline never finished collecting" || ok "run ${v}" ;;
    V09_RUN_STATE)    case "$v" in
                        COMPLETED) ok "run.json state=COMPLETED" ;;
                        MISSING)   bad "run.json has no state — beat 4's 'jq .state' filmed nothing" ;;
                        *)         bad "run.json state=${v}, not COMPLETED — beats 4-5 narrate 'completed' over a run that did not" ;;
                      esac ;;
    V09_RUN_TASK)     [[ "$v" == MATCH ]] && ok "run task is the filmed one ('${V09_TASK}')" \
                        || bad "run.json's task is not '${V09_TASK}' — the board card beats 5-6 clicked is not the one this take launched" ;;
    V09_AUD_COUNT)    [[ "$v" -gt 0 ]] && ok "audit.json carries ${v} events" || bad "audit.json is empty — beat 6 narrates 'the whole audit trail'" ;;
    V09_AUD_CREATE)   [[ "$v" == True ]] && ok "run.create on the trail" || bad "no run.create — beat 6 narrates 'create through complete'" ;;
    V09_AUD_COMPLETE) [[ "$v" == True ]] && ok "run.complete on the trail" || bad "no run.complete — beat 6 narrates 'create through complete'" ;;
    V09_AUD_EXIT)     case "$v" in
                        0)       ok "run.complete's exit_code is 0 — the same 0 beat 3 echoes and beat 6 chips" ;;
                        MISSING) bad "run.complete carries no exit_code — the 'agent exit 0' chip has nothing to render" ;;
                        *)       bad "run.complete's exit_code is ${v} — beat 6 films a chip that says 'agent exit 0'" ;;
                      esac ;;
  esac
done < /tmp/_demo_v09.$$
rm -f /tmp/_demo_v09.$$

# The terminal lane's cues. Checked HERE and not with the shared narration block
# below, which only knows the browser lane's narration.json: V09 is a hybrid, and
# a missing terminal timeline ships beats 1-4 SILENT under a fully narrated
# browser half with every step of the recorder still exiting 0.
V09_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-09}/narration-terminal.json"
[[ -s "${V09_TL}" ]] && ok "terminal narration timeline present" \
  || bad "no ${V09_TL} — beats 1-4 are silent (demo-typist.sh's default path vs record-demo.sh's per-video dir)"

head_ "Video 09 · the stack beats 5-6 filmed (best-effort)"
V09_URL="http://127.0.0.1:${WARDYN_UP_PORT:-8099}"
V09_LIST=$(curl -fsS --max-time 10 -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN:-demo-admin-token}" \
  "${V09_URL}/api/v1/runs" 2>/dev/null || true)
if [[ -z "${V09_LIST}" ]]; then
  printf '    (nothing answering at %s — the KEEPed stack is torn down, which the staging asks for after beat 6; the artifacts above are the durable evidence)\n' "${V09_URL}"
elif [[ "${V09_RUN}" == "-" ]]; then
  printf '    (%s is up, but there is no run id to look for)\n' "${V09_URL}"
elif [[ "${V09_LIST}" == *"${V09_RUN}"* ]]; then
  ok "run ${V09_RUN} is listed at ${V09_URL} — KEEP held and the browser half had the right board"
else
  bad "${V09_URL} answers but does not list ${V09_RUN} — beats 5-6 filmed a DIFFERENT stack"
fi
}

# Video 10 · the finale. Same artifact-first shape as 09 — start from the file
# the two lanes agreed on, not from "the newest run" — but the evidence lives on
# the LIVE stack, not on disk: beats 1-3 are three real ssh sessions whose whole
# durable trace is ssh.auth / session.attach / session.detach / session.recording
# on :8080, and unlike 09's KEEPed stack this one is still up when a take ends.
#
# The run's STATE is deliberately not asserted. An interactive run outlives the
# beats by design, but auto_stop, a reaper or an operator tidying up between the
# take and the check may all have ended it — none of which makes the filmed
# footage dishonest. What must still be true is who OWNED it: sshAuth is a plain
# `run.CreatedBy == key.Principal`, so a run created by `admin-token` would have
# refused the owner's own key and beat 1 could not have happened at all.
check_video_10() {
head_ "Video 10 · the run the terminals attacked"
V10_API="http://localhost:${WARDYN_UP_PORT:-8080}"
# The handoff the beat script writes and the browser spec reads (both resolve
# this one fixed path) — the same "two lanes agree via the file the take wrote"
# rule 09's ci-artifacts/run.json follows.
V10_HANDOFF="${REPO_ROOT}/ui/test-results/demo-video/v10-run-id.txt"
V10_RUN="${WARDYN_DEMO_RUN_ID:-}"
[[ -z "${V10_RUN}" && -s "${V10_HANDOFF}" ]] && V10_RUN="$(tr -d '[:space:]' <"${V10_HANDOFF}")"
if [[ -z "${V10_RUN}" ]]; then
  bad "no run id at ${V10_HANDOFF} — the terminal lane never got past preflight, so there is nothing this video could have filmed"
else
ok "run ${V10_RUN} (from the handoff both lanes read)"
V10_RUNJ=$(curl -fsS "${V10_API}/api/v1/runs/${V10_RUN}" 2>/dev/null || echo '{}')
V10_ME=$(curl -fsS "${V10_API}/api/v1/me" 2>/dev/null || echo '{}')
V10_AUD=$(curl -fsS "${V10_API}/api/v1/audit?run_id=${V10_RUN}&limit=1000" 2>/dev/null || echo '[]')

# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish — the bug class this script exists for.
python3 - "$V10_RUNJ" "$V10_ME" "$V10_AUD" "$V10_RUN" <<'PYEOF' > /tmp/_demo_v10.$$ 2>/dev/null
import sys, json
def load(s):
    try: return json.loads(s)
    except Exception: return {}
run, me = load(sys.argv[1]), load(sys.argv[2])
d = load(sys.argv[3]); run_id = sys.argv[4]
ev = d if isinstance(d, list) else (d or {}).get("events") or (d or {}).get("items") or []
owner = run.get("created_by") or "-"
print("V10_OWNER", owner)
print("V10_OWNER_ME", me.get("principal") or "-")
# admin-token is the ONE value that makes the whole video impossible; matching
# the caller's own principal is the positive form of the same fact.
print("V10_OWNER_OK", owner not in ("-", "admin-token") and owner == (me.get("principal") or ""))
print("V10_STATE", run.get("state") or "MISSING")

def data(e): return e.get("data") or {}
auth = [e for e in ev if e.get("action") == "ssh.auth"]
okfp = {e.get("target") for e in auth if e.get("outcome") == "success" and e.get("target")}
# The registered-but-foreign branch (sshgateway.go:246). A key that was never
# registered logs "unregistered key" with actor "unknown" instead, and the
# finale's money row would then be about the wrong refusal (SV13).
refused = [e for e in auth
           if e.get("outcome") == "failure" and data(e).get("reason") == "not the run owner"]
print("V10_AUTH_OK_FPS", len(okfp))
# Every success attributed to the run's owner: two keys, ONE principal (DA1).
print("V10_AUTH_OK_ACTOR", all((e.get("actor") or "") == owner
                               for e in auth if e.get("outcome") == "success") and bool(okfp))
print("V10_AUTH_FAIL", len(refused))
# FOREIGN, not merely refused: a fingerprint none of the successes used, under a
# principal that is not the run's owner. Both halves are what beat 3 narrates.
print("V10_AUTH_FAIL_FOREIGN", bool(refused) and all(
    e.get("target") not in okfp and (e.get("actor") or "") not in ("", "unknown", owner)
    for e in refused))
print("V10_AUTH_FAIL_FP", (refused[0].get("target") or "-") if refused else "-")

def transport_ssh(e): return data(e).get("transport") == "ssh"
print("V10_ATTACH_SSH", sum(1 for e in ev if e.get("action") == "session.attach"
                            and e.get("outcome") == "success" and transport_ssh(e)))
# The observer's detach. read_only rides the detach row (sshgateway_channels.go
# :508) and is the only place the trail records which client was DRIVING.
print("V10_DETACH_RO", sum(1 for e in ev if e.get("action") == "session.detach"
                           and transport_ssh(e) and data(e).get("read_only") is True))
# recording.CastKey(run, "ssh-<uuid>") — the composite key beat 6's Session
# picker lists and its player replays. Written at DETACH, so its absence also
# means an ssh client was still connected when the browser half filmed.
print("V10_REC_SSH", sum(1 for e in ev if e.get("action") == "session.recording"
                         and e.get("outcome") == "success"
                         and (e.get("target") or "").startswith(run_id + "~ssh-")))
PYEOF
while read -r k v; do
  case "$k" in
    V10_OWNER)        printf '    run.created_by: %s\n' "$v" ;;
    V10_OWNER_ME)     printf '    this caller:    %s\n' "$v" ;;
    V10_OWNER_OK)     [[ "$v" == True ]] && ok "the run is owned by the signed-in human, not admin-token" \
                        || bad "the run's created_by is not this caller's principal — sshAuth is owner-only, so beat 1 could not have attached (see the two lines above)" ;;
    V10_STATE)        printf '    run state:      %s (not asserted — an interactive run may be stopped by verify time)\n' "$v" ;;
    V10_AUTH_OK_FPS)  [[ "$v" -ge 2 ]] && ok "${v} distinct fingerprints authenticated over ssh — the holder and the observer" \
                        || bad "only ${v} distinct ssh.auth success fingerprint(s) — beats 1-2 film TWO keys of one person" ;;
    V10_AUTH_OK_ACTOR)[[ "$v" == True ]] && ok "both successes are attributed to the run's owner (two keys, one principal)" \
                        || bad "an ssh.auth success is attributed to someone other than the run's owner — 'same person' is false" ;;
    V10_AUTH_FAIL)    case "$v" in
                        0) bad "no ssh.auth failure with reason 'not the run owner' — beat 3's refusal never happened, or the key was UNREGISTERED and logged 'unregistered key' (the wrong branch, SV13)" ;;
                        1) ok "exactly one refusal, reason 'not the run owner'" ;;
                        *) ok "${v} refusals with reason 'not the run owner' (>1 = earlier takes; audit is append-only and reset-all belongs to V01)" ;;
                      esac ;;
    V10_AUTH_FAIL_FOREIGN) [[ "$v" == True ]] && ok "the refused key is genuinely foreign — a fingerprint no success used, under another principal" \
                        || bad "the refusal is not attributable to a second identity — the psql INSERT did not land under a distinct principal (SV19), so beat 3 filmed the owner refusing themself" ;;
    V10_AUTH_FAIL_FP) printf '    refused key:    %s\n' "$v" ;;
    V10_ATTACH_SSH)   [[ "$v" -ge 2 ]] && ok "${v} session.attach rows with transport=ssh" \
                        || bad "only ${v} successful session.attach with transport=ssh — the video shows two ssh clients in the same session" ;;
    V10_DETACH_RO)    [[ "$v" -ge 1 ]] && ok "an ssh detach recorded read_only=true — the observer really was one" \
                        || bad "no read_only ssh session.detach — nothing in the trail says the second client was an observer, which is beat 2's whole claim" ;;
    V10_REC_SSH)      [[ "$v" -ge 1 ]] && ok "${v} session recording(s) keyed ${V10_RUN}~ssh-* — beat 6 has a tape to play" \
                        || bad "no session.recording keyed ${V10_RUN}~ssh-* — beat 6's Session picker had no ssh session, or a client never detached" ;;
  esac
done < /tmp/_demo_v10.$$
rm -f /tmp/_demo_v10.$$
fi

# The terminal lane's cues, for the same reason 09 checks its own: the shared
# narration block below only knows narration.json (the browser lane), and a
# missing terminal timeline ships beats 1-3 SILENT under a fully narrated
# browser half with every step of the recorder still exiting 0.
V10_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-10}/narration-terminal.json"
[[ -s "${V10_TL}" ]] && ok "terminal narration timeline present" \
  || bad "no ${V10_TL} — beats 1-3 are silent (the driver runs inside tmux; WARDYN_DEMO_WORK_DIR has to reach it)"
}

case "${WARDYN_DEMO_VIDEO:-}" in
  00)
    head_ "Video 00 · the primer (slides lane)"
    # No product state to check — the take is a slide deck plus narration; the
    # spec itself asserts the deck rendered and the exhibit image decoded. What
    # can still break silently is the take dying mid-deck, which the cue floor
    # catches (a full read is ~28 lines; a died-early take leaves a fraction).
    TL00="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-00}/narration.json"
    if [[ -s "${TL00}" ]]; then
      N00=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${TL00}" 2>/dev/null || echo 0)
      [[ "${N00}" -ge 20 ]] && ok "the deck spoke ${N00} lines (floor 20)" \
        || bad "only ${N00} narration cues — the deck died early (floor 20)"
    else
      bad "no narration timeline at ${TL00}"
    fi
    ;;
  ""|05) check_video_02 ;;
  02) check_video_02_workspace ;;
  03) check_video_03_first_run ;;
  06) check_video_06_record ;;
  07) check_video_07_approvals ;;
  08) check_video_08_policies ;;
  09) check_video_09 ;;
  10) check_video_10 ;;
  01|04)
    head_ "Video ${WARDYN_DEMO_VIDEO}"
    printf '    video-specific checks TBD by spec\n'
    ;;
  *) head_ "Video ${WARDYN_DEMO_VIDEO}"
     bad "unknown WARDYN_DEMO_VIDEO=${WARDYN_DEMO_VIDEO} — expected 00..10, or unset for the walkthrough" ;;
esac

# --- shared: every take, every video -----------------------------------------

head_ "Narration"
TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video}/narration.json"
if [[ -s "${TL}" ]]; then
  python3 - "${TL}" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); c = d.get("cues", [])
# The mux slides sub-1500ms collisions (narrate-mux.py's CLAMP_MS — the
# accounting-bias class); only a bigger one ships as two voices at once, so
# only a bigger one fails the take. Keep this threshold equal to the mux's.
ov = sum(1 for i, x in enumerate(c) if i and x["tMs"] < c[i-1]["tMs"] + c[i-1]["durMs"] - 1500)
slid = sum(1 for i, x in enumerate(c) if i and x["tMs"] < c[i-1]["tMs"] + c[i-1]["durMs"])
print(f"  cues: {len(c)}   speech: {sum(x['durMs'] for x in c)/1000:.0f}s   hard overlaps: {ov}   (mux-slid: {slid - ov})")
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
