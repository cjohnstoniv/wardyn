#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Episode 03's four arms — 03a (core, "What it stops") plus the optional
# detours 03b (the network, three more ways), 03c (authorized, then issued) and
# 03d (the kinds that can't use a header).
#
# OUT OF LINE ON PURPOSE. scripts/verify-demo-take.sh sits within ~20 lines of
# scripts/check-file-size.sh's 1000-line threshold; 0.6's check_video_13 lives in
# its own lib (scripts/lib/verify-demo-take-13.sh, arriving with prep→main) for
# the same reason. Everything here is a FUNCTION, never a subshell
# or a pipeline: ok()/bad() increment PASS/FAIL counters in the caller that must
# survive to the summary, and a subshell is exactly how this script once
# reported success over a real failure.
#
# WHAT THESE CHECK, AND WHY NOT THE EXIT CODE. Every demo card starts its own
# interactive exec run (demo-runner.tsx), and a demo run carries NO title, no
# task and no workspace — so "the newest interactive run" is the only other
# handle and it is wrong the moment two episodes rehearse in one evening. The
# specs therefore write {demo-id: run-id} into
# ${WARDYN_DEMO_WORK_DIR}/demo-runs.json (noteDemoRun, ui/e2e/demo/demos.ts),
# and every assertion below reads the audit trail of THOSE EXACT RUNS. The
# expectations are the catalog cards' own claims (demo-catalog.ts) — a card that
# promises "deny → allow → deny" is graded on rows, not on a green terminal.

V03_API="http://localhost:${WARDYN_UP_PORT:-8080}"

# ---------------------------------------------------------------------------
# Shared plumbing for all four arms.
# ---------------------------------------------------------------------------

_v03_work() { printf '%s' "${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-${WARDYN_DEMO_VIDEO:-03a}}"; }

# The run id the take recorded for one demo card, or "" when it never started one.
_v03_run_id() {
  python3 -c 'import json,sys
try: m = json.load(open(sys.argv[1]))
except Exception: m = {}
print((m or {}).get(sys.argv[2], "") if isinstance(m, dict) else "")' "$(_v03_work)/demo-runs.json" "$1" 2>/dev/null
}

# Resolve every expected demo id to its run and pull that run's audit trail into
# ${V03_DIR}/<demo-id>.json — one curl per run, so the per-episode python below
# is pure logic over files. Returns 1 (arm bails) only when the handoff file
# itself is missing; a single missing demo is a `bad`, not a bail.
_v03_load() {
  local f id rid
  f="$(_v03_work)/demo-runs.json"
  V03_DIR="/tmp/_demo_v03aud.$$"
  if [[ ! -s "${f}" ]]; then
    bad "no ${f} — the take never recorded which runs it launched (noteDemoRun, ui/e2e/demo/demos.ts)"
    return 1
  fi
  rm -rf "${V03_DIR}"; mkdir -p "${V03_DIR}"
  for id in "$@"; do
    rid="$(_v03_run_id "${id}")"
    if [[ -z "${rid}" ]]; then
      bad "no run recorded for demo '${id}' — that card never started a sandbox in this take"
      printf '[]' > "${V03_DIR}/${id}.json"
      continue
    fi
    ok "${id} → ${rid}"
    curl -fsS "${V03_API}/api/v1/audit?run_id=${rid}&limit=1000" 2>/dev/null > "${V03_DIR}/${id}.json" \
      || printf '[]' > "${V03_DIR}/${id}.json"
  done
  return 0
}

# The demos whose lesson IS that no run exists (a closed gate, a 422 at
# run-create). A run id here means the take filmed the wrong thing.
_v03_absent() {
  local rid; rid="$(_v03_run_id "$1")"
  [[ -z "${rid}" ]] && ok "no run for '$1' — $2" || bad "'$1' STARTED a run (${rid}) — $2"
}

# The canary sentinels demos.ts stores as secret VALUES all share this prefix.
# The audit log is append-only and fans to every SIEM sink, so a secret value
# reaching it is a defect no episode gets to ship — checked once, for every
# episode, against every row of every run this take launched.
_v03_no_canary() {
  if grep -qs 'WARDYN-V03-' "${V03_DIR}"/*.json; then
    bad "a secret VALUE (WARDYN-V03-* canary) reached the AUDIT LOG — an append-only sink must never carry one"
  else
    ok "no secret value anywhere in the audit trail"
  fi
}

# The cue floor, as the 01) arm does it: a take that dies mid-episode still
# leaves a narration.json, just a short one. Each floor is ~85% of the episode's
# full cue count = local/episode-03-stanza-check.py's spec-string count, plus
# chapter() cards and the lines spoken from shared helpers (startAndBoot's
# "Start it."). 03a 109+5=114 -> 95, 03b 36+5=41 -> 34, 03c 51+5=56 -> 47,
# 03d 38+3=41 -> 34. 03a and 03c are confirmed against real rehearsals (114 and
# 56 cues, 2026-08-24). Pacing drift never fails a take; a died-early one always
# does.
_v03_cues() {
  local tl n; tl="$(_v03_work)/narration.json"
  if [[ -s "${tl}" ]]; then
    n=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${tl}" 2>/dev/null || echo 0)
    [[ "${n}" -ge "$1" ]] && ok "the episode spoke ${n} lines (floor $1)" \
      || bad "only ${n} narration cues — the take died before the end of the episode (floor $1)"
  else
    bad "no narration timeline at ${tl}"
  fi
}

# The python preamble every arm shares: rows(demo, action..., host=, port=)
# over the per-run audit files, in the API's own ascending time order.
_v03_py_head() {
  cat <<'PY'
import json, os, re, sys
D = sys.argv[1]
def ev(demo):
    try:
        x = json.load(open(os.path.join(D, demo + ".json")))
    except Exception:
        return []
    return x if isinstance(x, list) else (x.get("events") or x.get("items") or [])
def host_of(e):
    d = e.get("data") or {}
    return d.get("host") or e.get("target") or ""
def rows(demo, *actions, **kw):
    out = []
    for e in ev(demo):
        if actions and e.get("action") not in actions:
            continue
        d = e.get("data") or {}
        if "host" in kw and host_of(e) != kw["host"]:
            continue
        if "port" in kw and d.get("port") != kw["port"]:
            continue
        if "path" in kw and d.get("path") != kw["path"]:
            continue
        out.append(e)
    return out
def at(e):
    # RFC3339Nano as a comparable string: Go trims trailing zeros, so pad the
    # fraction to nine digits ("…36.26063Z" must sort after "…36.260631Z"'s peers).
    t = e.get("time") or ""
    return re.sub(r"\.(\d+)(Z|[+-]\d\d:\d\d)$", lambda m: "." + m.group(1).ljust(9, "0") + m.group(2), t)
def t0(l):
    return at(l[0]) if l else ""
def decides(demo, host, decision):
    return [e for e in rows(demo, "approval.decide")
            if (e.get("data") or {}).get("host") == host
            and (e.get("data") or {}).get("decision") == decision]
def out(k, v):
    print(k, v)
PY
}

# Compose the preamble and this arm's body into one program and run it, K V
# lines to $1. Deliberately not a pipeline: the `while read` that follows in
# each arm calls ok()/bad(), whose counters must survive to the summary.
# stderr is NOT silenced, and a non-zero exit is a bad() of its own — a broken
# program here would otherwise emit zero (or fewer) lines and therefore zero
# (or fewer) verdicts, which is the failure mode this whole script exists to prevent.
_v03_run_py() {
  local prog="/tmp/_demo_v03prog.$$"
  { _v03_py_head; cat; } > "${prog}"
  python3 "${prog}" "${V03_DIR}" > "$1" || bad "the audit program crashed (exit $?) — every verdict after the crash is silently missing"
  rm -f "${prog}"
}

# ---------------------------------------------------------------------------
# 03a — the core episode. Six cards: the egress quartet, then the two secrets
# basics. Every expectation below is a claim one of those cards makes out loud.
# ---------------------------------------------------------------------------
check_video_03a() {
head_ "Video 03a · narration"
_v03_cues 95

head_ "Video 03a · the runs this take launched"
_v03_load sealed-box fail-then-approve held-at-the-door lines-that-cant-be-crossed \
          write-only-by-design key-never-in-the-box || return
_v03_no_canary

head_ "Video 03a · the audit trail"
_v03_run_py /tmp/_demo_v03a.$$ <<'PYEOF'
# --- sealed-box: always_deny + an empty allowlist. Refused the instant it is
# dialed — so a DENY, and nothing that ever asked a human.
sealed_deny = rows("sealed-box", "egress.deny", host="example.com")
out("V03A_SEALED_DENY", bool(sealed_deny))
out("V03A_SEALED_QUIET", not rows("sealed-box", "egress.pending", "approval.decide")
                         and not rows("sealed-box", "egress.allow", host="example.com"))

# --- fail-then-approve: deny_with_review. The arc IS the order — held, decided
# on camera, and the retry actually landed.
p = t0(rows("fail-then-approve", "egress.pending", host="example.com"))
d = t0(decides("fail-then-approve", "example.com", "APPROVED"))
a = t0(rows("fail-then-approve", "egress.allow", host="example.com"))
out("V03A_FTA_PENDING", bool(p))
out("V03A_FTA_DECIDE", bool(d))
out("V03A_FTA_ALLOW", bool(a))
out("V03A_FTA_ORDER", bool(p and d and a and p < d < a))

# --- held-at-the-door: wait_for_review. NOTE — verified against the live
# rehearsal (2026-08-24): a wait_for_review request is held IN FLIGHT and emits
# NO egress.pending row; the first row is the approval.decide. So the beat is
# graded on decide → allow, not on a pending that never exists.
# Two audit writers (the proxy's egress row, the approver's decide row) land within
# ~300 ms of each other in either order — join them causally instead: the allow row
# carries the decide's approval_id.
hd = decides("held-at-the-door", "example.com", "APPROVED")
ha = rows("held-at-the-door", "egress.allow", host="example.com")
out("V03A_HELD_ORDER", bool(hd and ha and any((a.get("data") or {}).get("approval_id") == (hd[0].get("data") or {}).get("approval_id") for a in ha)))
out("V03A_HELD_WIKI_DENIED", bool(decides("held-at-the-door", "wikipedia.org", "DENIED"))
                              and bool(rows("held-at-the-door", "egress.deny", host="wikipedia.org")))
out("V03A_HELD_WIKI_CLEAN", not rows("held-at-the-door", "egress.allow", host="wikipedia.org"))

# --- lines-that-can't-be-crossed: allow_all_egress. The public host proves the
# door really is open; the hard lines are the lesson.
PRIV = ("169.254.", "192.168.", "10.", "127.", "172.16.")
def hard(h):
    return any(h.startswith(x) for x in PRIV)
out("V03A_LINES_OPEN", any((e.get("data") or {}).get("rule_source") == "policy:allowed"
                           for e in rows("lines-that-cant-be-crossed", "egress.allow", host="example.com")))
out("V03A_LINES_HARD", not [e for e in rows("lines-that-cant-be-crossed", "egress.allow") if hard(host_of(e))])
# Two honest shapes, both a pass. docs/DEMO-SCRIPT.md records the historical one
# (the probe never reaches the proxy, so NOTHING logs it); the live 2026-08-24
# rehearsal of THIS card produced the stronger one — an egress.deny with
# rule_source=builtin:private-ip, i.e. refused beneath the policy. Only an
# ALLOW is a failure.
meta = rows("lines-that-cant-be-crossed", "egress.deny", host="169.254.169.254")
out("V03A_LINES_META", ((meta[0].get("data") or {}).get("rule_source") or "?") if meta else "none")
out("V03A_LINES_QUIET", not rows("lines-that-cant-be-crossed", "egress.pending", "approval.decide"))

# --- write-only-by-design: the in-sandbox read-back probe is UNAUDITED — it
# goes --noproxy '*' straight at the proxy's brokered router, which 404s an
# unknown route before any decision is logged. So the audit-side evidence is
# the ABSENCE of a grant: this policy carries no eligible_grants, so nothing was
# ever minted and no secret was ever read on its behalf. That absence is
# exactly what the card claims.
out("V03A_WO_RAN", bool(rows("write-only-by-design", "run.create"))
                    and bool(rows("write-only-by-design", "session.attach")))
out("V03A_WO_NOGRANT", not rows("write-only-by-design", "credential.mint", "secret.read"))

# --- key-never-in-the-box: the box is credentialed BEFORE it opens. The card's
# words are "stamped at STARTUP, before you ran anything" — so the mint and the
# secret read must both precede the attach.
att = t0(rows("key-never-in-the-box", "session.attach"))
mint = t0(rows("key-never-in-the-box", "credential.mint"))
sread = t0([e for e in rows("key-never-in-the-box", "secret.read")
            if e.get("target") == "wardyn-demo-key"])
out("V03A_KEY_MINT", bool(mint) and bool(sread))
out("V03A_KEY_STARTUP", bool(att and mint and sread and mint < att and sread < att))
out("V03A_KEY_ALLOW", bool(rows("key-never-in-the-box", "egress.allow", host="example.com")))
out("V03A_KEY_WIKI", bool(rows("key-never-in-the-box", "egress.deny", host="wikipedia.org")))
PYEOF
while read -r k v; do
  case "$k" in
    V03A_SEALED_DENY)  [[ "$v" == True ]] && ok "sealed-box: example.com denied on the record" || bad "sealed-box: no egress.deny for example.com — test one's refusal never happened" ;;
    V03A_SEALED_QUIET) [[ "$v" == True ]] && ok "sealed-box: nothing asked, nothing allowed — always-deny means no prompt" || bad "sealed-box: an approval was raised or example.com got through — always_deny asks nobody, ever" ;;
    V03A_FTA_PENDING)  [[ "$v" == True ]] && ok "fail-then-approve: example.com went pending" || bad "fail-then-approve: no egress.pending for example.com — deny_with_review never raised the question" ;;
    V03A_FTA_DECIDE)   [[ "$v" == True ]] && ok "fail-then-approve: approved on camera" || bad "fail-then-approve: no approval.decide APPROVED for example.com — the decision beat never happened" ;;
    V03A_FTA_ALLOW)    [[ "$v" == True ]] && ok "fail-then-approve: the retry got through" || bad "fail-then-approve: no egress.allow for example.com — the retry after the approval never landed" ;;
    V03A_FTA_ORDER)    [[ "$v" == True ]] && ok "fail-then-approve: pending → approved → allowed, in that order" || bad "fail-then-approve: the three rows are out of order — 'the only thing that changed was the decision' is not what the trail says" ;;
    V03A_HELD_ORDER)   [[ "$v" == True ]] && ok "held-at-the-door: approved, then the held request completed" || bad "held-at-the-door: no approval.decide APPROVED → egress.allow for example.com — the in-flight request was never released" ;;
    V03A_HELD_WIKI_DENIED) [[ "$v" == True ]] && ok "held-at-the-door: wikipedia.org denied on camera and refused" || bad "held-at-the-door: wikipedia.org was not DENIED and refused — test four never happened" ;;
    V03A_HELD_WIKI_CLEAN)  [[ "$v" == True ]] && ok "held-at-the-door: wikipedia.org never got through" || bad "held-at-the-door: WIKIPEDIA.ORG WAS ALLOWED — the denied host reached the network anyway" ;;
    V03A_LINES_OPEN)   [[ "$v" == True ]] && ok "lines: example.com allowed by policy — the door really was wide open" || bad "lines: no policy-allowed egress.allow for example.com — allow_all_egress never took effect, so 'and yet' has nothing to contrast with" ;;
    V03A_LINES_HARD)   [[ "$v" == True ]] && ok "lines: no link-local or private address was ever allowed" || bad "A HARD LINE WAS CROSSED — a link-local/private address got an egress.allow with the policy wide open" ;;
    V03A_LINES_META)   case "$v" in
                         none) ok "lines: nothing logged for 169.254.169.254 — the probe died at the network layer (the older build's shape, per docs/DEMO-SCRIPT.md)" ;;
                         builtin:*) ok "lines: 169.254.169.254 refused beneath the policy (rule_source ${v})" ;;
                         *) bad "lines: 169.254.169.254 was decided by '${v}' — the metadata address must be refused by a builtin guard, not by policy (a policy line is a line you could change)" ;;
                       esac ;;
    V03A_LINES_QUIET)  [[ "$v" == True ]] && ok "lines: no approval was ever raised — these are not decisions a human gets to make" || bad "lines: an approval was raised — the take narrates 'there's no approval prompt' over a pending row" ;;
    V03A_WO_RAN)       [[ "$v" == True ]] && ok "write-only: the sandbox really booted and attached" || bad "write-only: no run.create + session.attach — the read-back probe was never typed into anything" ;;
    V03A_WO_NOGRANT)   [[ "$v" == True ]] && ok "write-only: no mint, no secret read — nothing was ever going to hand it the key" || bad "write-only: this run MINTED or READ a secret — its policy carries no eligible_grants, so the demo's whole claim is false" ;;
    V03A_KEY_MINT)     [[ "$v" == True ]] && ok "key-never: credential.mint + secret.read wardyn-demo-key on the record" || bad "key-never: no credential.mint/secret.read for wardyn-demo-key — the grant never minted (missing secret, or the role clamped it away)" ;;
    V03A_KEY_STARTUP)  [[ "$v" == True ]] && ok "key-never: both stamped BEFORE the attach — the box was credentialed before it opened" || bad "key-never: the mint did not precede session.attach — 'that happened outside the box, before the request crossed the boundary' is not what the trail says" ;;
    V03A_KEY_ALLOW)    [[ "$v" == True ]] && ok "key-never: example.com allowed — the injected header rode a real request" || bad "key-never: no egress.allow for example.com — the credentialed request never went out" ;;
    V03A_KEY_WIKI)     [[ "$v" == True ]] && ok "key-never: wikipedia.org denied — the grant is scoped to one host" || bad "key-never: no egress.deny for wikipedia.org — the off-scope contrast never ran" ;;
  esac
done < /tmp/_demo_v03a.$$
rm -f /tmp/_demo_v03a.$$
rm -rf "${V03_DIR}"
}

# ---------------------------------------------------------------------------
# 03b — the network, three more ways. The optional detour episodes 08/09/10
# later own deeply. agent-in-the-box is the series' only model-quota-bound act.
# ---------------------------------------------------------------------------
check_video_03b() {
head_ "Video 03b · narration"
_v03_cues 34

head_ "Video 03b · the runs this take launched"
_v03_load agent-in-the-box record-a-policy once-or-for-good || return
_v03_no_canary

head_ "Video 03b · the audit trail"
_v03_run_py /tmp/_demo_v03b.$$ <<'PYEOF'
# `wardynd` is the in-sandbox broker route (the mint endpoint), not a network
# destination — never counted as a host the agent "reached".
def dests(demo, *actions):
    return sorted({host_of(e) for e in rows(demo, *actions) if host_of(e) != "wardynd"})

# --- agent-in-the-box: allowed_domains is api.anthropic.com + *.anthropic.com,
# first_use_approval always_deny. The card's claim is that the model host is the
# ONLY host it reached.
allowed = dests("agent-in-the-box", "egress.allow")
out("V03B_AGENT_ANTHROPIC", any(h == "api.anthropic.com" or h.endswith(".anthropic.com") for h in allowed))
out("V03B_AGENT_ONLY", bool(allowed) and all(h == "api.anthropic.com" or h.endswith(".anthropic.com") for h in allowed))
out("V03B_AGENT_HOSTS", ",".join(allowed) or "-")
out("V03B_AGENT_DENIED", bool(rows("agent-in-the-box", "egress.deny", host="example.com")))

# --- record-a-policy: allow_all_egress, so three real hosts are RECORDED, not
# blocked, and the payoff is the synthesis reading them back.
rec = dests("record-a-policy", "egress.allow")
want = ["pypi.org", "registry.npmjs.org", "example.com"]
out("V03B_REC_ALL", all(h in rec for h in want))
out("V03B_REC_MISSING", ",".join(h for h in want if h not in rec) or "-")
out("V03B_REC_QUIET", not rows("record-a-policy", "egress.pending", "approval.decide"))
syn = rows("record-a-policy", "run.record.synthesize")
out("V03B_REC_SYNTH", bool(syn))
out("V03B_REC_PROPOSED", ",".join(sorted((syn[-1].get("data") or {}).get("allowed_domains") or [])) if syn else "-")

# --- once-or-for-good: THE re-raise. A `once` grant spends itself on the single
# connection it was raised for, so the THIRD attempt must ask again — a second
# egress.pending strictly after the decision. Its absence means the demo taught
# the opposite of its own lesson on camera.
op = [at(e) for e in rows("once-or-for-good", "egress.pending", host="example.com")]
od = decides("once-or-for-good", "example.com", "APPROVED")
odt = t0(od)
oa = t0(rows("once-or-for-good", "egress.allow", host="example.com"))
out("V03B_ONCE_SCOPE", any((e.get("data") or {}).get("decision_scope") == "once" for e in od))
out("V03B_ONCE_ORDER", bool(op and odt and oa and op[0] < odt < oa))
out("V03B_ONCE_RERAISE", bool(odt) and any(x > odt for x in op))
out("V03B_ONCE_PENDINGS", str(len(op)))
PYEOF
while read -r k v; do
  case "$k" in
    V03B_AGENT_ANTHROPIC) [[ "$v" == True ]] && ok "agent-in-the-box: the model host was reached" || bad "agent-in-the-box: no egress.allow for api.anthropic.com — the agent never called the model (quota? the act is the only quota-bound one in the series)" ;;
    V03B_AGENT_ONLY)      [[ "$v" == True ]] && ok "agent-in-the-box: anthropic was the ONLY host allowed" || bad "agent-in-the-box: a non-anthropic host was allowed — 'the agent in the box' reached somewhere its allowlist does not name" ;;
    V03B_AGENT_HOSTS)     printf '    allowed hosts: %s\n' "$v" ;;
    V03B_AGENT_DENIED)    [[ "$v" == True ]] && ok "agent-in-the-box: example.com denied — the contrast landed" || bad "agent-in-the-box: no egress.deny for example.com — the off-list command never ran" ;;
    V03B_REC_ALL)         [[ "$v" == True ]] && ok "record-a-policy: all three hosts were recorded, not blocked" || bad "record-a-policy: hosts missing from the recording — the raw material for the policy is incomplete" ;;
    V03B_REC_MISSING)     [[ "$v" == "-" ]] || printf '    never recorded: %s\n' "$v" ;;
    V03B_REC_QUIET)       [[ "$v" == True ]] && ok "record-a-policy: nothing was held — you can't record what a policy already blocks" || bad "record-a-policy: an approval was raised during the recording — allow_all_egress never took effect" ;;
    V03B_REC_SYNTH)       [[ "$v" == True ]] && ok "record-a-policy: run.record.synthesize on the record — the payoff really ran" || bad "record-a-policy: no run.record.synthesize row — 'Turn this into a policy' never reached the server, so the proposed allowlist on screen came from nowhere" ;;
    V03B_REC_PROPOSED)    printf '    proposed allowlist: %s\n' "$v" ;;
    V03B_ONCE_SCOPE)      [[ "$v" == True ]] && ok "once-or-for-good: approved with decision_scope=once" || bad "once-or-for-good: no approval.decide with decision_scope=once — the caret menu picked the wrong scope, so the whole demo is a plain run-scope approval" ;;
    V03B_ONCE_ORDER)      [[ "$v" == True ]] && ok "once-or-for-good: held → approved once → through" || bad "once-or-for-good: pending → decide → allow are not in that order — the Once-approved retry never landed" ;;
    V03B_ONCE_RERAISE)    [[ "$v" == True ]] && ok "once-or-for-good: a SECOND egress.pending after the decision — Once spent itself" || bad "THE RE-RAISE NEVER HAPPENED — no second egress.pending for example.com after the once decision; the demo silently taught that Once lasts the run" ;;
    V03B_ONCE_PENDINGS)   printf '    example.com pendings: %s (expect >= 2)\n' "$v" ;;
  esac
done < /tmp/_demo_v03b.$$
rm -f /tmp/_demo_v03b.$$
rm -rf "${V03_DIR}"
}

# ---------------------------------------------------------------------------
# 03c — authorized, then issued. Three api_key/git_pat cards whose whole subject
# is WHEN a credential is issued, and to whom.
# ---------------------------------------------------------------------------
check_video_03c() {
head_ "Video 03c · narration"
_v03_cues 47

head_ "Video 03c · the runs this take launched"
_v03_load authorized-not-issued rest-api-token pat-stdout-only || return
_v03_no_canary

head_ "Video 03c · the audit trail"
_v03_run_py /tmp/_demo_v03c.$$ <<'PYEOF'
MINT_PATH = "/wardyn/v1/credentials/mint"

# --- authorized-not-issued: the card promises, verbatim, "Audit panel: deny →
# allow → deny, in that order". Verified against the live rehearsal: those three
# rows are egress decisions on the BROKERED MINT ROUTE (host wardynd,
# rule_source brokered:mint) — the first refused pending an approval, the second
# the approved single-use mint, the third the 409 already_minted.
seq = [e.get("action").split(".")[1]
       for e in rows("authorized-not-issued", "egress.deny", "egress.allow", path=MINT_PATH)]
out("V03C_MINT_SEQ", ",".join(seq) or "-")
out("V03C_MINT_SEQ_OK", seq == ["deny", "allow", "deny"])
out("V03C_AUTH_DECIDE", bool(decides("authorized-not-issued", "example.com", "APPROVED")))
minted = [e for e in rows("authorized-not-issued", "credential.mint") if e.get("outcome") == "success"]
out("V03C_AUTH_ONE_MINT", len(minted) == 1)
out("V03C_AUTH_MINT_APPROVED", bool(minted) and bool((minted[0].get("data") or {}).get("approval_id")))

# --- rest-api-token: requires_approval false, so the mint is stamped at
# STARTUP — "before you typed anything". Same shape as key-never, a real host
# and a real Authorization header this time.
att = t0(rows("rest-api-token", "session.attach"))
m = t0(rows("rest-api-token", "credential.mint"))
s = t0([e for e in rows("rest-api-token", "secret.read") if e.get("target") == "wardyn-demo-api-token"])
out("V03C_REST_MINT", bool(m) and bool(s))
out("V03C_REST_STARTUP", bool(att and m and s and m < att and s < att))
out("V03C_REST_ALLOW", bool(rows("rest-api-token", "egress.allow", host="example.org")))

# --- pat-stdout-only: the OTHER half of the contrast — a git_pat is minted when
# YOU ask, not at startup. (The ungated first call is refused inside
# wardyn-git-helper, client-side, and never reaches the server: no audit row is
# expected for it. The one mint that exists must land AFTER the attach.)
patt = t0(rows("pat-stdout-only", "session.attach"))
pm = rows("pat-stdout-only", "credential.mint")
out("V03C_PAT_MINTS", str(len(pm)))
out("V03C_PAT_ONE", len(pm) == 1)
out("V03C_PAT_ONDEMAND", bool(patt and pm and t0(pm) > patt))
PYEOF
while read -r k v; do
  case "$k" in
    V03C_MINT_SEQ)          printf '    mint-route decisions: %s (expect deny,allow,deny)\n' "$v" ;;
    V03C_MINT_SEQ_OK)       [[ "$v" == True ]] && ok "authorized-not-issued: deny → allow → deny on the mint route, in that order" || bad "authorized-not-issued: the mint route's decisions are not deny → allow → deny — the card names that exact sequence, so either the approval gate or the single-use spend did not happen" ;;
    V03C_AUTH_DECIDE)       [[ "$v" == True ]] && ok "authorized-not-issued: the mint was approved on camera" || bad "authorized-not-issued: no approval.decide APPROVED for example.com — the requires_approval gate never opened" ;;
    V03C_AUTH_ONE_MINT)     [[ "$v" == True ]] && ok "authorized-not-issued: exactly one successful mint — single-use held" || bad "authorized-not-issued: not exactly one successful credential.mint — 'single-use, already spent' is not what the trail says" ;;
    V03C_AUTH_MINT_APPROVED)[[ "$v" == True ]] && ok "authorized-not-issued: the mint carries its approval_id — authorized, THEN issued" || bad "authorized-not-issued: the credential.mint carries no approval_id — it was issued without the approval the whole demo is about" ;;
    V03C_REST_MINT)         [[ "$v" == True ]] && ok "rest-api-token: credential.mint + secret.read wardyn-demo-api-token" || bad "rest-api-token: no mint/secret.read for wardyn-demo-api-token — the grant never minted (is the secret staged?)" ;;
    V03C_REST_STARTUP)      [[ "$v" == True ]] && ok "rest-api-token: stamped at STARTUP, before the attach" || bad "rest-api-token: the mint did not precede session.attach — 'before you typed anything' is not what the trail says" ;;
    V03C_REST_ALLOW)        [[ "$v" == True ]] && ok "rest-api-token: example.org allowed — the Bearer header rode a real request" || bad "rest-api-token: no egress.allow for example.org — the credentialed call never went out" ;;
    V03C_PAT_MINTS)         printf '    pat mints: %s (expect 1 — the ungated first call is refused client-side and logs nothing)\n' "$v" ;;
    V03C_PAT_ONE)           [[ "$v" == True ]] && ok "pat-stdout-only: exactly one mint" || bad "pat-stdout-only: not exactly one credential.mint — the gated/ungated pair did not behave as the card describes" ;;
    V03C_PAT_ONDEMAND)      [[ "$v" == True ]] && ok "pat-stdout-only: minted AFTER the attach — when you asked, not at startup" || bad "pat-stdout-only: the mint did not follow session.attach — the card's entire contrast with an injected api_key is false" ;;
  esac
done < /tmp/_demo_v03c.$$
rm -f /tmp/_demo_v03c.$$
rm -rf "${V03_DIR}"
}

# ---------------------------------------------------------------------------
# 03d — the kinds that can't use a header. One card runs; the other TWO are
# graded on the run they must NOT have created.
# ---------------------------------------------------------------------------
check_video_03d() {
head_ "Video 03d · narration"
_v03_cues 34

head_ "Video 03d · the runs this take launched"
_v03_load ssh-briefly-resident || return
_v03_absent github-app-broker "Start stays disabled without a configured GitHub App, so no sandbox is ever built (the closed door IS the beat)"
_v03_absent sts-fail-closed "the 422 fires at run-create, before any sandbox exists (the refusal IS the demo)"
_v03_no_canary

head_ "Video 03d · the audit trail"
_v03_run_py /tmp/_demo_v03d.$$ <<'PYEOF'
D2 = "ssh-briefly-resident"
# The ssh_key grant's scope names github.com, but ssh is re-originated through
# the proxy's CONNECT lane: the server UNIONS ssh.github.com:443 onto the run's
# allowlist (run.ssh.egress) and the sandbox dials THAT. Port 22 is never
# reached — which is the point of "briefly resident", and the reason this
# episode can film a real ssh handshake at all.
un = rows(D2, "run.ssh.egress")
added = (un[0].get("data") or {}).get("added_domains") if un else []
out("V03D_SSH_UNION", "ssh.github.com:443" in (added or []))
out("V03D_SSH_ADDED", ",".join(added or []) or "-")
out("V03D_SSH_ALLOW", bool(rows(D2, "egress.allow", host="ssh.github.com", port=443)))
# No EGRESS row may name github.com itself (the mint's scope legitimately does).
bare = [e for e in rows(D2, "egress.allow", "egress.deny", "egress.pending")
        if host_of(e) == "github.com" or (e.get("data") or {}).get("port") == 22]
out("V03D_SSH_NO_22", not bare)
out("V03D_SSH_BARE", ",".join(sorted({host_of(e) for e in bare})) or "-")
out("V03D_SSH_MINT", any((e.get("data") or {}).get("scope", {}).get("key_secret_ref") == "wardyn-demo-ssh-key"
                         for e in rows(D2, "credential.mint")))
PYEOF
while read -r k v; do
  case "$k" in
    V03D_SSH_UNION) [[ "$v" == True ]] && ok "ssh: run.ssh.egress unioned ssh.github.com:443 onto the allowlist" || bad "ssh: no run.ssh.egress adding ssh.github.com:443 — the CONNECT lane was never opened, so the handshake beat could not have run" ;;
    V03D_SSH_ADDED) printf '    unioned domains: %s\n' "$v" ;;
    V03D_SSH_ALLOW) [[ "$v" == True ]] && ok "ssh: ssh.github.com:443 allowed — the re-originated handshake really went out" || bad "ssh: no egress.allow for ssh.github.com:443 — the ssh command never reached GitHub" ;;
    V03D_SSH_NO_22) [[ "$v" == True ]] && ok "ssh: nothing ever dialed github.com:22 — port 22 stayed shut" || bad "ssh: an egress decision names github.com / port 22 — the sandbox dialed the bare ssh port, which the allowlist does not carry" ;;
    V03D_SSH_BARE)  [[ "$v" == "-" ]] || printf '    bare-host rows: %s\n' "$v" ;;
    V03D_SSH_MINT)  [[ "$v" == True ]] && ok "ssh: credential.mint for the wardyn-demo-ssh-key grant" || bad "ssh: no credential.mint naming wardyn-demo-ssh-key — the key that 'touches disk, briefly' was never issued" ;;
  esac
done < /tmp/_demo_v03d.$$
rm -f /tmp/_demo_v03d.$$
rm -rf "${V03_DIR}"
}
