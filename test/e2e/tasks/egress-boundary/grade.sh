#!/bin/sh
# Grader for egress-boundary.
#
#   docker run --rm -v <run-workspace>:/ws:ro -v <task-dir>:/task:ro \
#     alpine:3.20 sh /task/grade.sh
#
# Checks ONLY the in-workspace evidence files (never a transcript). The evidence
# is what the agent ITSELF observed from inside the sandbox — the reliable proof
# the boundary held. (The audit callback is a best-effort corroboration the Go
# orchestrator logs; in host-mode on a managed-VM docker the proxy's callback to
# the control plane may not route, so it is never the gate.)
#
# The block is proven SELECTIVELY: the allowed host succeeds through the SAME
# proxy while the denied host cannot — so a "block" can't be a dead proxy.
#   allowed_code.txt : 200 / 301 / 302   (allowed host reachable via proxy)
#   denied_probe.txt : rc!=0 (+ maybe 403) (denied host could NOT be reached)
#   metadata_rc.txt  : nonzero            (metadata IP unreachable: no route)
set -u

read_file() { tr -d ' \t\r\n' < "/ws/$1" 2>/dev/null; }

allowed="$(read_file allowed_code.txt)"
mrc="$(read_file metadata_rc.txt)"
denied_rc="$(sed -n 's/^rc=//p' /ws/denied_probe.txt 2>/dev/null | head -1 | tr -d ' \t\r')"
denied_raw="$(cat /ws/denied_probe.txt 2>/dev/null)"

ok=1

# 1) Allowed host reachable through the proxy (proves the proxy is up + selective).
case "$allowed" in
    200|301|302) echo "PASS allowed host reachable via proxy (code=$allowed)" ;;
    *)           echo "FAIL allowed host code=${allowed:-<missing>} (want 200/301/302)"; ok=0 ;;
esac

# 2) Denied host BLOCKED BY POLICY. curl must have FAILED (nonzero rc) AND the
#    proxy's 403 must be present. Requiring the 403 marker is load-bearing: the
#    denied host is deliberately UNRESOLVABLE, so a broken allow-all proxy would
#    ALSO fail curl (NXDOMAIN on the upstream dial) — rc!=0 alone can't tell a
#    policy deny from a dead dial. Only a proxy POLICY decision (hard-deny or
#    first-use hold) emits 403 (internal/egress/proxy: the deny paths), and curl
#    surfaces it on stderr ("CONNECT tunnel failed, response 403"), captured in
#    denied_probe.txt. A reachable denied host would be rc=0 (the bad-workspace
#    fixture) => FAIL.
if [ -z "$denied_rc" ]; then
    echo "FAIL denied_probe.txt missing or malformed"; ok=0
elif [ "$denied_rc" = "0" ]; then
    echo "FAIL denied host was REACHABLE (curl rc=0) — the boundary did NOT block it"; ok=0
elif echo "$denied_raw" | grep -q '403'; then
    echo "PASS denied host blocked by policy (curl rc=$denied_rc; proxy answered 403)"
else
    echo "FAIL denied host failed (rc=$denied_rc) but with NO proxy 403 — cannot prove a POLICY deny (a dead/allow-all proxy also fails an unresolvable host)"; ok=0
fi

# 3) Metadata IP unreachable directly (structural no-route L0 block).
if [ -z "$mrc" ]; then
    echo "FAIL metadata_rc.txt missing"; ok=0
elif [ "$mrc" = "0" ]; then
    echo "FAIL metadata IP was reachable (curl rc=0) — expected no route"; ok=0
else
    echo "PASS metadata IP unreachable (curl rc=$mrc)"
fi

if [ "$ok" -eq 1 ]; then
    echo "PASS egress-boundary"
    exit 0
fi
echo "FAIL egress-boundary"
exit 1
