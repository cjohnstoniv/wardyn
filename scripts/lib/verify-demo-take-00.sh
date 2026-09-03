# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-00.sh — grader for V00, the front door.
# Sourced by scripts/verify-demo-take.sh; uses the caller's ok/bad/head_ and
# REPO_ROOT, resolved at call time after the caller defines them.
#
# EVERYTHING IS READ OFF THE AUDIT TRAIL, and that is forced rather than
# stylistic: this episode's CLOSE deletes its own workspace and its own secret
# ON CAMERA ("Nothing here outlives your say-so"), so by the time this runs
# there is no workspace row to inspect, no approved_egress list to count and no
# secret to look up. The append-only trail is the only thing that survives the
# teardown the episode is partly about — and the teardown itself is one of the
# claims checked below.
#
# The three questions the video promises to answer are the three blocks here:
# what a run may REACH (the always decision + both allowlist writes), what it
# may HOLD (the secret written, never leaked, and refused a read-back), and WHO
# DECIDED (approval.decide's own `decision` field, never the action name — a
# deny is an approval.decide too, so the action alone cannot tell an approval
# from a refusal, which is exactly the trap that shipped a dishonest take once).
check_video_00() {
head_ "Video 00 · the workspace, and its own teardown"
V00_API="http://localhost:${WARDYN_UP_PORT:-8080}"
V00_WS="${WARDYN_DEMO_MEET_WS_NAME:-meet-wardyn}"
V00_SECRET="${WARDYN_DEMO_MEET_SECRET:-metrics-push-token}"
V00_REACH="example.net"
V00_JOB="www.iana.org"
V00_VILLAIN="pastebin.com"
V00_CANARY="WARDYN-V00-CANARY"

# One request for the whole workspace namespace, correlated on the workspace id
# the create row carries as its target — workspace.create, the two
# workspace.egress.approve writers (the `always` decision at approvals.go and
# the record-mode promote at record.go both file under that one action, with
# {"domains":[…],"source":…}) and workspace.delete all target the same id.
V00_WSA=$(curl -fsS "${V00_API}/api/v1/audit?action_prefix=workspace.&limit=1000" 2>/dev/null || echo '[]')
# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish — the bug class this script exists for.
python3 - "$V00_WSA" "$V00_WS" "$V00_REACH" "$V00_JOB" <<'PYEOF' > /tmp/_demo_v00.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
name, reach, job = sys.argv[2], sys.argv[3], sys.argv[4]

created = [e for e in ev if e.get("action") == "workspace.create"
           and ((e.get("data") or {}).get("name") or "") == name]
print("V00_ONBOARDED", bool(created))
ws_id = created[-1].get("target") if created else ""
print("V00_WS_ID", ws_id or "-")

def domains(e):
    dd = e.get("data") or {}
    v = dd.get("domains")
    return [str(x).lower() for x in v] if isinstance(v, list) else []

# Scoped to THIS workspace's id, so a grant another episode wrote onto its own
# workspace can never stand in for one this take earned on camera.
approves = [e for e in ev
            if e.get("action") == "workspace.egress.approve"
            and e.get("outcome") == "success"
            and e.get("target") == ws_id]
granted = {h for e in approves for h in domains(e)}
srcs = sorted({str((e.get("data") or {}).get("source") or "") for e in approves})
# The `always` write-back: source is "approval:<id>" (persistWorkspaceEgressDecision).
print("V00_ALWAYS_WRITTEN", reach in granted and any(s.startswith("approval:") for s in srcs))
# The promote: source is "record:<session key>" (handlePromoteRecordEgress).
print("V00_PROMOTED", job in granted and any(s.startswith("record:") for s in srcs))
print("V00_GRANTED", json.dumps(sorted(granted)))
# The close's own promise. The delete has no name in its payload, so it is
# matched on the id the create row named.
print("V00_TORN_DOWN", bool(ws_id) and any(e.get("action") == "workspace.delete" and e.get("target") == ws_id for e in ev))
PYEOF
V00_WS_ID="-"
while read -r k v extra; do
  case "$k" in
    V00_ONBOARDED)     [[ "$v" == True ]] && ok "${V00_WS} was onboarded on camera (workspace.create)" || bad "no workspace.create for ${V00_WS} — act 1 never landed" ;;
    V00_WS_ID)         V00_WS_ID="$v" ;;
    V00_ALWAYS_WRITTEN) [[ "$v" == True ]] && ok "${V00_REACH} written onto ${V00_WS} by an approval (source approval:…)" || bad "${V00_REACH} never reached ${V00_WS}'s allowlist — act 2's 'Always' did not persist, so beat 2.18's receipt is not real" ;;
    V00_PROMOTED)      [[ "$v" == True ]] && ok "${V00_JOB} promoted onto ${V00_WS} from the recording (source record:…)" || bad "${V00_JOB} was never promoted — act 4's 'Approve 1 observed host' did not land" ;;
    V00_GRANTED)       printf '    allowlist writes: %s\n' "$v" ;;
    # NOT "on camera", deliberately. The spec keeps a best-effort afterAll net
    # (00-meet-wardyn.spec.ts's teardown()), and a take that filmed only ONE of
    # its two deletes still lets that net write the other's row — so this arm
    # can tell that the ROW exists and no more. That the close actually filmed
    # is the C70 arm's job, in the narration block at the bottom.
    V00_TORN_DOWN)     [[ "$v" == True ]] && ok "${V00_WS} was deleted (workspace.delete)" || bad "no workspace.delete for ${V00_WS} — neither the close nor its teardown net removed it, and 'nothing here outlives your say-so' went unproven" ;;
  esac
  : "${extra:-}"
done < /tmp/_demo_v00.$$
rm -f /tmp/_demo_v00.$$

head_ "Video 00 · the decisions"
# READ THE `decision` FIELD, NEVER THE ACTION. A deny is an approval.decide too,
# so the action name alone cannot tell the approve at 2.11 from the deny at
# 4.16 — and cannot catch the .first() trap, where the driver decides a row it
# did not mean to.
V00_DEC=$(curl -fsS "${V00_API}/api/v1/audit?action=approval.decide&limit=1000" 2>/dev/null || echo '[]')
python3 - "$V00_DEC" "$V00_REACH" "$V00_VILLAIN" <<'PYEOF' > /tmp/_demo_v00b.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
reach, villain = sys.argv[2], sys.argv[3]
# approval.decide carries the host at the TOP level of data (approval.go lifts
# it out of requested_scope) plus the raw decision_scope: "" means "no scope
# recorded", which Normalize() reads as today's default, run.
dec = []
for e in ev:
    dd = e.get("data") or {}
    dec.append((str(dd.get("host") or ""), str(dd.get("decision") or ""), str(dd.get("decision_scope") or "")))
def any_(host, state, scopes): return any(h == host and s == state and sc in scopes for h, s, sc in dec)
ANY = ("", "run", "once", "until", "always")
# Beat 2.11-2.13 — the widest yes, and the one the workspace remembers.
print("V00_REACH_ALWAYS", any_(reach, "APPROVED", ("always",)))
# Beat 4.16 — the host that was never the job, held at the door and refused.
print("V00_VILLAIN_DENIED", any_(villain, "DENIED", ANY))
# The .first() trap, in the direction that matters: in a governance demo,
# approving something you did not mean to approve is the worst possible frame.
print("V00_VILLAIN_APPROVED", any_(villain, "APPROVED", ANY))
print("V00_DECISIONS", json.dumps([t for t in dec if t[0] in (reach, villain)]))
PYEOF
while read -r k v; do
  case "$k" in
    V00_REACH_ALWAYS)     [[ "$v" == True ]] && ok "${V00_REACH} approved with decision=APPROVED scope=always" || bad "no always-scoped APPROVED decision for ${V00_REACH} — act 2, the episode's first answer, never landed" ;;
    V00_VILLAIN_DENIED)   [[ "$v" == True ]] && ok "${V00_VILLAIN} decided DENIED on camera" || bad "${V00_VILLAIN} was never denied — act 4's caught-one payoff has nothing behind it" ;;
    V00_VILLAIN_APPROVED) [[ "$v" == False ]] && ok "${V00_VILLAIN} never approved (the wrong-host trap)" || bad "THE VILLAIN HOST WAS APPROVED — the wrong row was decided on camera" ;;
    V00_DECISIONS)        printf '    decisions: %s\n' "$v" ;;
  esac
done < /tmp/_demo_v00b.$$
rm -f /tmp/_demo_v00b.$$

head_ "Video 00 · what the run may hold"
V00_SEC=$(curl -fsS "${V00_API}/api/v1/audit?action_prefix=secret.&limit=1000" 2>/dev/null || echo '[]')
python3 - "$V00_SEC" "$V00_SECRET" <<'PYEOF' > /tmp/_demo_v00c.$$ 2>/dev/null
import sys, json
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
name = sys.argv[2]
def has(action): return any(e.get("action") == action and e.get("target") == name for e in ev)
print("V00_SECRET_WRITTEN", has("secret.write"))
print("V00_SECRET_DELETED", has("secret.delete"))
# There is no read-back ACTION to look for, and that IS the claim: the store has
# no read-back route at all, so a `secret.read` row for this name would mean the
# episode's central sentence is false.
print("V00_SECRET_READ", has("secret.read"))
PYEOF
while read -r k v; do
  case "$k" in
    V00_SECRET_WRITTEN) [[ "$v" == True ]] && ok "${V00_SECRET} stored on camera (secret.write)" || bad "no secret.write for ${V00_SECRET} — act 3's save never landed" ;;
    V00_SECRET_READ)    [[ "$v" == False ]] && ok "no secret.read for ${V00_SECRET} — nothing read that value back" || bad "A secret.read ROW EXISTS for ${V00_SECRET} — 'no screen and no run can read it back' is false" ;;
    # NOT "on camera", for the same reason V00_TORN_DOWN is not — see there.
    V00_SECRET_DELETED) [[ "$v" == True ]] && ok "${V00_SECRET} was deleted (secret.delete)" || bad "no secret.delete for ${V00_SECRET} — neither the close nor its teardown net removed it" ;;
  esac
done < /tmp/_demo_v00c.$$
rm -f /tmp/_demo_v00c.$$

# A write-only store that logged the value would be the leak this episode
# spends a whole act denying. Read across the whole feed, not one action.
#
# THE FEED IS ASSERTED BEFORE THE GREP, and that is the whole point of the two
# steps: `curl … || echo '[]'` hands a down stack, a wrong port and a 500 the
# same body a clean feed's grep misses on, so "the canary appears nowhere"
# would otherwise be this arm's verdict on a stack that answered nothing at
# all — a green leak check over no evidence.
V00_AUD=$(curl -fsS "${V00_API}/api/v1/audit?limit=1000" 2>/dev/null || echo '[]')
V00_AUD_N="$(python3 -c 'import json,sys
try: d = json.loads(sys.argv[1])
except Exception: d = []
ev = d if isinstance(d, list) else d.get("events") or d.get("items") or []
print(len(ev))' "${V00_AUD}" 2>/dev/null || echo 0)"
if [[ "${V00_AUD_N:-0}" -le 0 ]]; then
  bad "the audit feed came back empty — the leak check proved nothing (is the stack up on ${V00_API}?)"
elif grep -q "${V00_CANARY}" <<<"${V00_AUD}"; then
  bad "THE CANARY VALUE APPEARS IN THE AUDIT TRAIL — the secret leaked into the record"
else
  ok "the canary value appears nowhere in the audit trail (${V00_AUD_N} rows read)"
fi

head_ "Video 00 · the narration"
V00_TL="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-00}/narration.json"
if [[ -s "${V00_TL}" ]]; then
  # 75 captions + 2 spoken chapter cards = 77 lines (00-script.md §3); the floor
  # is ~85% of that, the same margin the 02/05/07 floors use. A take that dies
  # mid-episode still leaves a narration.json, just a short one.
  V00_CUES="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${V00_TL}" 2>/dev/null || echo 0)"
  [[ "${V00_CUES}" -ge 65 ]] && ok "the episode spoke ${V00_CUES} lines (floor 65)" \
    || bad "only ${V00_CUES} narration cues — the take died before the end of the episode (floor 65)"
  # THE READ-BACK REFUSAL IS THE ONE CLAIM WITH NO ROW BEHIND IT: the proxy's
  # 404 for an unknown brokered route is not an audited event (there is no route
  # to audit), so what is checkable is that the spec REACHED that beat — it
  # speaks this line only after pollScreen matched /404|unknown brokered route/
  # on the sandbox's own screen.
  grep -q "Four-oh-four" "${V00_TL}" \
    && ok "narration reaches the read-back refusal (act 3's 404 filmed)" \
    || bad "narration never reaches 'Four-oh-four' — the in-sandbox read-back probe did not film"
  grep -q "Caught one" "${V00_TL}" \
    && ok "narration reaches the confined replay's verdict (act 4 filmed to the end)" \
    || bad "narration never reaches 'Caught one' — act 4's payoff did not film"
  # THE CLOSE, AND WHY THE AUDIT ROWS CANNOT STAND IN FOR IT. The spec's
  # afterAll issues the same two DELETEs the close does and writes the same
  # two rows, so a take that died at the `Delete this workspace` click grades
  # green on workspace.delete/secret.delete alone — and still clears the 65-cue
  # floor, because 68 of the 77 lines are already spoken by then. These two
  # arms are the ones the net cannot satisfy.
  #
  # C65a is the line the close OPENS on, ahead of the delete click.
  grep -q "Nothing here outlives your say-so" "${V00_TL}" \
    && ok "narration reaches the close's own promise (C65a)" \
    || bad "narration never reaches 'Nothing here outlives your say-so' — the take ended before its close, and the two delete rows are the afterAll net's, not the episode's"
  # C70 is the LAST line before the outro and lands after BOTH on-camera
  # deletes, so its presence is the only thing here that says the teardown
  # beats really filmed.
  grep -q "open source, Apache-2.0" "${V00_TL}" \
    && ok "narration reaches the fourth word (C70) — both teardown beats filmed" \
    || bad "narration never reaches 'open source, Apache-2.0' — the close was cut short, so 'deleted on camera' is not something this take can claim"
else
  bad "no narration timeline at ${V00_TL} — the take recorded silent or not at all"
fi
}
