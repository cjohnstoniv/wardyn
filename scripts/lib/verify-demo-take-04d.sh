# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-04d.sh — grader for V04d, "Your drive".
# Sourced by scripts/verify-demo-take.sh; uses the caller's ok/bad/head_ and
# REPO_ROOT, resolved at call time after the caller defines them.
#
# THE CLUSTER, NOT THE COMPOSE STACK. 04d is shot on the kind install 02c built
# (:8280 with the SSO overlay), which wants a bearer token — read out of its own
# wardyn-auth Secret the way verify-demo-take-13.sh reads it. A 200 on the port
# proves nothing on its own: quickstart and an operator's compose stack publish
# the same shape, so arm 1 asserts the substrate by name.
#
# THE HANDOFF. The spec writes ${WARDYN_DEMO_WORK_DIR}/04d-runs.json with both
# run ids, the sentinel, the control path and the object name. Without it there
# is no way to tell this episode's two runs from 04c's and 12b's on the same
# cluster, and "the newest two" is wrong the moment a rehearsal runs the same
# evening (V13's v13-run-id.txt precedent).
#
# THE `decision` FIELD IS DELIBERATELY ABSENT HERE, and its absence is an arm.
# 04d dials nothing: 04c left Egress hosts ENFORCED and this member holds no
# grant, so an approval on either of these runs would mean something reached the
# network that the episode never accounted for. Arm 12 asserts that NEGATIVE
# instead of a decision.
check_video_04d() {
head_ "Video 04d · the substrate"
V04D_CTX="${WARDYN_V04D_CONTEXT:-kind-wardyn-quickstart}"
V04D_NS="${WARDYN_V04D_NAMESPACE:-wardyn}"
V04D_API="${WARDYN_DEMO_BASE_URL:-http://localhost:8280}"
V04D_TOK="${WARDYN_ADMIN_TOKEN:-$(kubectl --context "${V04D_CTX}" -n "${V04D_NS}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)}"
V04D_WORK="${WARDYN_DEMO_WORK_DIR:-${REPO_ROOT}/ui/test-results/demo-video-04d}"
V04D_HANDOFF="${V04D_WORK}/04d-runs.json"
V04D_TL="${V04D_WORK}/narration.json"

V04D_HEALTH=$(curl -fsS --max-time 10 "${V04D_API}/healthz" 2>/dev/null || echo '{}')
V04D_RUNNER="$(jq -r '.runner // "-"' <<<"${V04D_HEALTH}" 2>/dev/null || echo '-')"
V04D_SSO="$(jq -r '.sso // false' <<<"${V04D_HEALTH}" 2>/dev/null || echo false)"
[[ "${V04D_RUNNER}" == "k8s" && "${V04D_SSO}" == "true" ]] \
  && ok "filmed against the SSO cluster (healthz runner=k8s sso=true)" \
  || bad "healthz runner=${V04D_RUNNER} sso=${V04D_SSO} — this take did not film the multi-user cluster the episode claims"

head_ "Video 04d · the two runs"
if [[ ! -s "${V04D_HANDOFF}" ]]; then
  bad "no run handoff at ${V04D_HANDOFF} — the beats never reached a booted run, so there is nothing this video could have filmed"
  return
fi
V04D_A="$(jq -r '.write // ""' "${V04D_HANDOFF}" 2>/dev/null)"
V04D_B="$(jq -r '.read // ""' "${V04D_HANDOFF}" 2>/dev/null)"
V04D_OBJ="$(jq -r '.object // ""' "${V04D_HANDOFF}" 2>/dev/null)"
V04D_CTRL="$(jq -r '.control // ""' "${V04D_HANDOFF}" 2>/dev/null)"
V04D_SENT="$(jq -r '.sentinel // ""' "${V04D_HANDOFF}" 2>/dev/null)"
if [[ -z "${V04D_A}" || -z "${V04D_B}" || -z "${V04D_OBJ}" ]]; then
  bad "the handoff is incomplete (write=${V04D_A:-–} read=${V04D_B:-–} object=${V04D_OBJ:-–}) — one of the two runs never booted"
  return
fi
if [[ -z "${V04D_TOK}" ]]; then
  bad "no admin token (WARDYN_ADMIN_TOKEN, or Secret wardyn-auth in ${V04D_CTX}/${V04D_NS}) — the trail on a k8s install cannot be read without one"
  return
fi
ok "run A ${V04D_A} (write) · run B ${V04D_B} (read-back), from the handoff the beats wrote"

v04d_get() { curl -fsS --max-time 20 -H "Authorization: Bearer ${V04D_TOK}" "${V04D_API}$1" 2>/dev/null || echo '[]'; }
V04D_AUD_A=$(v04d_get "/api/v1/audit?run_id=${V04D_A}&limit=1000")
V04D_AUD_B=$(v04d_get "/api/v1/audit?run_id=${V04D_B}&limit=1000")
# drive.write and drive.grant.write carry NO run id (an admin registered a drive,
# no run existed), so they are only reachable through the global feed — which is
# security-operator only, hence the token above.
V04D_AUD_G=$(v04d_get "/api/v1/audit?action_prefix=drive.&limit=1000")
V04D_STATE_A="$(v04d_get "/api/v1/runs/${V04D_A}" | jq -r '.state // "-"' 2>/dev/null || echo '-')"
V04D_CREATED_B="$(v04d_get "/api/v1/runs/${V04D_B}" | jq -r '.created_at // ""' 2>/dev/null || echo '')"

head_ "Video 04d · the drive, the allocation, and the two mounts"
# A temp file, NOT a pipe: `| while read` runs in a subshell and ok()/bad()
# would increment counters that vanish — the bug class this script exists for.
grade_py /tmp/_demo_v04d.$$ "V04d · the drive, the allocation and the mounts" \
  "$V04D_OBJ" "$V04D_STATE_A" "$V04D_CREATED_B" \
  3<<'PYEOF' < <(printf '%s\0' "${V04D_AUD_G}" "${V04D_AUD_A}" "${V04D_AUD_B}")
import sys, json
# The three FEEDS ride stdin NUL-separated, in the order the caller printf'd
# them; only the short arguments are argv (grade_py's own note says why).
feeds = sys.stdin.buffer.read().split(b"\0")
def rows(i):
    try: d = json.loads(feeds[i])
    except Exception: return []
    return d if isinstance(d, list) else (d.get("events") or d.get("items") or [])
glob, a, b = rows(0), rows(1), rows(2)
obj, state_a, created_b = sys.argv[1], sys.argv[2], sys.argv[3]
def data(e): return e.get("data") or {}

# 2 · the drive itself. The three fields that make it THIS episode's drive: the
# cloud backend, its name, and the writable switch act 1 threw on camera.
print("V04D_DRIVE", any(
    e.get("action") == "drive.write" and e.get("outcome") == "success"
    and data(e).get("backend") == "k8s_pvc"
    and data(e).get("name") == "notebook"
    and data(e).get("writable") is True
    for e in glob))

# 3 · the allocation. A PERSON's own row — the only tier whose home the preview
# and the run derive from the same claim.
print("V04D_GRANT", any(
    e.get("action") == "drive.grant.write" and e.get("outcome") == "success"
    and data(e).get("subject_type") == "user"
    and data(e).get("subject") == "member@wardyn.local"
    for e in glob))

mounts_a = [e for e in a if e.get("action") == "run.drive.mount" and e.get("outcome") == "success"]
mounts_b = [e for e in b if e.get("action") == "run.drive.mount" and e.get("outcome") == "success"]
print("V04D_MOUNTED", bool(mounts_a) and bool(mounts_b))

# 4 · THE SAME VOLUME, BOTH TIMES. If the second run mounted a different object
# the persistence claim is not merely unproven, it is a lie.
objs = sorted({str(data(e).get("object") or "") for e in mounts_a + mounts_b})
print("V04D_SAME_OBJECT", objs == [obj], ",".join(objs) or "-")

# 5 · THE RECEIPT ON CAMERA (§5 #6). The run page's Audit tab renders `target`
# and nothing from `data`, so beat 3.16 can only ring the object name if
# auditDriveMount spent Target on the storage. Without it the beat films a "—"
# under a caption naming a string the screen never showed.
print("V04D_RECEIPT", bool(mounts_a) and all(str(e.get("target") or "") == str(data(e).get("object") or "") for e in mounts_a))

# 6 · TWO MODES. The vocabulary is driveAuditMode's rw/ro, but the arm asserts
# "two rows, two DISTINCT modes" rather than literals: what beat 4.2 has to have
# reached the wire is the CHANGE, and a renamed value should not fail a correct
# take.
modes_a = {str(data(e).get("mode") or "") for e in mounts_a}
modes_b = {str(data(e).get("mode") or "") for e in mounts_b}
print("V04D_NARROWED", bool(modes_a) and bool(modes_b) and modes_a != modes_b,
      "|".join(sorted(modes_a)) + " vs " + "|".join(sorted(modes_b)))

# 7 · RUN A WAS KILLED BEFORE RUN B READ. The blocker the dialog round found:
# without this the take filmed two live pods sharing one claim (DESIGN.md §6.1
# residual 36) under the title "survives the sandbox".
kills = [e for e in a if e.get("action") == "run.kill"]
def when(e): return str(e.get("time") or "")
kill_t = min((when(e) for e in kills if when(e)), default="")
mount_b_t = min((when(e) for e in mounts_b if when(e)), default="") or created_b
print("V04D_KILLED", state_a == "KILLED", state_a)
print("V04D_KILL_FIRST", bool(kill_t) and bool(mount_b_t) and kill_t < mount_b_t, (kill_t or "-") + " < " + (mount_b_t or "-"))

# 12 · NOTHING REACHED THE NETWORK. 04d dials nothing on purpose, so an
# approval row on either run is traffic the episode never accounted for.
appr = [e for e in a + b if str(e.get("action") or "").startswith("approval.")]
print("V04D_NO_APPROVALS", not appr, str(len(appr)))
PYEOF
while read -r k v extra; do
  case "$k" in
    V04D_DRIVE)       [[ "$v" == True ]] && ok "drive.write: notebook, k8s_pvc, writable — act 1's Save landed" || bad "no drive.write for a writable k8s_pvc drive named notebook — act 1 never landed" ;;
    V04D_GRANT)       [[ "$v" == True ]] && ok "drive.grant.write: the user row for member@wardyn.local — act 2's Allocate landed" || bad "no user-tier drive.grant.write for member@wardyn.local — act 2 never landed" ;;
    V04D_MOUNTED)     [[ "$v" == True ]] && ok "both runs carry a run.drive.mount row" || bad "one of the two runs mounted no drive — the episode's whole subject is missing from a run it films" ;;
    V04D_SAME_OBJECT) [[ "$v" == True ]] && ok "both mounts name ONE volume (${V04D_OBJ})" || bad "the two runs mounted different objects (${extra}) — 'same directory' is then false" ;;
    V04D_RECEIPT)     [[ "$v" == True ]] && ok "run A's mount row spends target on the object — beat 3.16 had a name to ring" || bad "run A's run.drive.mount target is not the object name — the Audit tab rendered '—' and C47c narrated a name the screen never showed" ;;
    V04D_NARROWED)    [[ "$v" == True ]] && ok "the two mounts differ in mode (${extra}) — the read-only toggle reached the wire" || bad "both mounts recorded the same mode (${extra}) — beat 4.2's narrowing never left the browser" ;;
    V04D_KILLED)      [[ "$v" == True ]] && ok "run A is KILLED" || bad "run A state=${extra} (want KILLED) — beat 3.17 did not land" ;;
    V04D_KILL_FIRST)  [[ "$v" == True ]] && ok "run A was killed BEFORE run B mounted (${extra})" || bad "run A's kill does not precede run B (${extra}) — the take filmed two live sandboxes sharing one claim, so C47e and C48 were false when they landed" ;;
    V04D_NO_APPROVALS) [[ "$v" == True ]] && ok "zero approval rows on either run — nothing dialled out, as the episode claims" || bad "${extra} approval row(s) on these runs — something reached the network that 04d never accounted for" ;;
  esac
done < /tmp/_demo_v04d.$$
rm -f /tmp/_demo_v04d.$$

head_ "Video 04d · the narration"
if [[ -s "${V04D_TL}" ]]; then
  # 68 captions + 1 spoken chapter card = 69. Two silent cards speak nothing and
  # are not counted.
  #
  # 62, RAISED FROM the script's 55 (§5 arm 13). Act 5 is 8 of those 69 lines,
  # so a floor of 55 clears with the entire close missing — and the close is
  # where this episode's own headline claim lands. 62 is 69 less the act-5
  # block, so a take that stops at C57 fails here instead of grading green on
  # four acts of a five-act episode; it is still ~90% of the total, the margin
  # the 00/02/05/07 floors use.
  V04D_CUES="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1])).get("cues",[])))' "${V04D_TL}" 2>/dev/null || echo 0)"
  [[ "${V04D_CUES}" -ge 62 ]] && ok "the episode spoke ${V04D_CUES} lines (floor 62)" \
    || bad "only ${V04D_CUES} narration cues — the take died before the close (floor 62)"
  # Pixels are not gradeable; a silent or died-early take is. Each line below is
  # spoken ONLY after its pollScreen matched, so its presence in the timeline is
  # the receipt that the shell really answered.
  #
  # 8 · the control file absent (the new-box proof), and the handoff names it.
  if grep -q "Gone with its sandbox" "${V04D_TL}" && [[ -n "${V04D_CTRL}" ]]; then
    ok "narration reaches the absent control file (${V04D_CTRL}) — run B really was a new box"
  else
    bad "narration never reaches 'Gone with its sandbox' — beat 4.5b was cut, and 'brand-new sandbox' is unproven"
  fi
  # 9 · the read-back.
  if grep -q "And the file the last run left" "${V04D_TL}" && [[ -n "${V04D_SENT}" ]]; then
    ok "narration reaches the read-back (sentinel '${V04D_SENT}')"
  else
    bad "narration never reaches the read-back — the persistence payoff did not film"
  fi
  # 10 · the refusal at the enforcement point.
  grep -q "Refused by the filesystem" "${V04D_TL}" \
    && ok "narration reaches the EROFS refusal — act 4's climax filmed" \
    || bad "narration never reaches 'Refused by the filesystem' — act 4's climax did not film"
  # 11 · THE HONESTY SENTENCE, verbatim. Blocking, not cosmetic: DRIVES.HONESTY
  # is frozen canon and this episode is the only one that says a size out loud,
  # so a take that cut or reworded it makes a size claim the product does not
  # stand behind.
  grep -q "never enforces a drive's size itself" "${V04D_TL}" \
    && ok "the frozen honesty sentence is on the soundtrack, verbatim" \
    || bad "the honesty sentence is missing or reworded — the size claim shipped without its caveat"
  # 14 · ACT 5, THE OFFBOARDING CLOSE. Every arm above reads act 1, 3 or 4, so
  # without this one the reclaim beat — the episode's own headline claim, and
  # the only place it says what the release does NOT do — could be absent from
  # a green take. C64 is the last spoken line of the claim (C65 is the hand-off
  # to 05), and it lands after the preview, the hint and the silent command
  # card, so its presence is the receipt that the whole close filmed.
  grep -q "It hands you the name and stands aside" "${V04D_TL}" \
    && ok "narration reaches the reclaim close (C64) — act 5 filmed to its claim" \
    || bad "narration never reaches 'It hands you the name and stands aside' — act 5 was cut, and the offboarding claim this episode is titled for never landed"
else
  bad "no narration timeline at ${V04D_TL} — the take recorded silent or not at all"
fi
}
