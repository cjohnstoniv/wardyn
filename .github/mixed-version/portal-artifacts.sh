#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# portal-artifacts.sh <seed|old-refusals|old-revoke|old-worker>: the issuer-portal half of
# .github/workflows/mixed-version.yml. The published v0.10.1 replica (svc/old-wardyn) and this tree's replica
# (svc/new-wardyn) share one migrated database. seed writes synthetic issuer-portal state into it, in the shapes
# migration 0294 defines and this tree's store writes; the other three ask the old replica's own entry points
# about that state:
#   old-refusals  each issuer-portal artifact is refused at the old server's door, beside a legacy portal it
#                 still serves;
#   old-revoke    the old server's DELETE /api/v1/admin/delegates/{id} revokes an issuer portal, and the new
#                 server then refuses that portal's grant (and still admits another portal's);
#   old-worker    with the new replica scaled to zero, the old worker takes a queued issuer-portal run whose
#                 stored grant is "portal:<uuid>", ends it before any authority is bound, and never dispatches it.
# Needs NS, OLD_IP and ADMIN_TOKEN (the job's env), RUNNER_TEMP, kubectl on the kind cluster, curl, jq, openssl.
# Every wait is bounded; any failure prints the diagnostics below and exits non-zero.
set -euo pipefail

: "${NS:?}" "${OLD_IP:?}" "${ADMIN_TOKEN:?}" "${RUNNER_TEMP:?}"
STATE="$RUNNER_TEMP/mixed-version-portal.env"
T=$(mktemp -d "$RUNNER_TEMP/portal.XXXXXX")
OLD=http://127.0.0.1:18081
NEW=http://127.0.0.1:18080
# What 0.10.1's start writes on a promoted run it could not start (internal/api/run_queue_start.go at v0.10.1).
START_FAILED_HINT="This run left the queue, but could not be started. Start it again."
pids=()

diag() {
	set +eu
	echo "::group::diagnostics"
	kubectl get pods -A -o wide
	kubectl -n "$NS" logs deployment/old-wardyn --tail=150
	kubectl -n "$NS" logs deployment/new-wardyn --tail=150
	for f in "$T"/*; do [ -f "$f" ] && { echo "--- $f"; head -c 2000 "$f"; echo; }; done
	if [ -f "$STATE" ]; then
		# shellcheck disable=SC1090
		. "$STATE"
		psqlc "SELECT id, issuer <> '' AS issuer_portal, revoked_at FROM delegates"
		psqlc "SELECT id, state, failure_hint, status_detail, sandbox_ref, watcher_owner FROM agent_runs WHERE id IN ('$TICKET_RUN', '${QUEUE_RUN:-$TICKET_RUN}')"
		psqlc "SELECT run_id, pool_id, owner, authority FROM run_queue"
	fi
	psqlc "SELECT pid, client_addr, application_name, state, backend_start FROM pg_stat_activity WHERE datname = current_database()"
	echo "::endgroup::"
}

cleanup() {
	local rc=$?
	[ "${#pids[@]}" -eq 0 ] || kill "${pids[@]}" 2>/dev/null
	[ "$rc" -eq 0 ] || diag
	exit "$rc"
}
trap cleanup EXIT

fail() { echo "::error::$*"; exit 1; }
psqlc() { kubectl -n "$NS" exec deployment/postgres -- psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -tAqc "$1"; }
# psqlf runs SQL from stdin with psql variables (-v name=value, read as :'name'), so no value is spliced into SQL.
psqlf() { kubectl -n "$NS" exec -i deployment/postgres -- psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -q "$@" -f -; }
uuid() { cat /proc/sys/kernel/random/uuid; }
rnd() { openssl rand -hex 32; }
sha() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
b32() { base32 -w0 | tr -d '=' | tr '[:upper:]' '[:lower:]'; }

# pf <service> <local port>: a port-forward that answers /healthz within 30 s.
pf() {
	kubectl -n "$NS" port-forward "svc/$1" "$2:8080" >"$T/pf-$1.log" 2>&1 &
	pids+=("$!")
	for _ in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$2/healthz" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	fail "the port-forward to svc/$1 never answered /healthz"
}

# req <name> <curl args...>: the status code; the body is in $T/<name>.json.
req() { local n=$1; shift; curl -sS -o "$T/$n.json" -w '%{http_code}' "$@"; }
# expect <what> <want> <got> <name>
expect() {
	[ "$3" = "$2" ] || fail "$1: answered $3, want $2: $(head -c 600 "$T/$4.json")"
	echo "ok: $1 -> $3"
}
# jqok <what> <name> <jq args...>
jqok() {
	local what=$1 n=$2; shift 2
	jq -e "$@" "$T/$n.json" >/dev/null || fail "$what: $(head -c 600 "$T/$n.json")"
	echo "ok: $what"
}
admin() { printf 'Authorization: Bearer %s' "$ADMIN_TOKEN"; }
# shellcheck disable=SC1090
load() { [ -f "$STATE" ] || fail "no seeded state at $STATE"; . "$STATE"; }
save() { printf '%s=%q\n' "$1" "$2" >>"$STATE"; }
secret() { echo "::add-mask::$2"; save "$1" "$2"; }

EXCHANGE_FORM='grant_type=urn:ietf:params:oauth:grant-type:token-exchange&subject_token=mixed-version-subject&subject_token_type=urn:ietf:params:oauth:token-type:access_token'

seed() {
	: >"$STATE"
	local issuer=https://issuer.mixed-version.invalid/oauth2/default k person
	# The issuer key and the person's principal, as internal/types/portal_subject.go derives them.
	k=$(printf 'wardyn-portal-v2\n%s' "$issuer" | openssl dgst -sha256 -binary | b32 | cut -c1-26)
	person="delegate:person:$k:$(printf '%s' mixed-version-person-1 | b32)"
	[ "${#k}" -eq 26 ] || fail "issuer key $k is not 26 characters"
	save PERSON "$person"
	save LEGACY_PERSON mixed-version-legacy-person
	for v in P1 P2 LP IDENT G1 G2 LT TICKET_RUN; do save "$v" "$(uuid)"; done
	secret P1_CRED "wdi_$(rnd)"
	secret P2_CRED "wdi_$(rnd)"
	secret LP_CRED "wdp_$(rnd)"
	secret G1_TOKEN "wdg_$(rnd)"
	secret G2_TOKEN "wdg_$(rnd)"
	secret LT_TOKEN "wdg_$(rnd)"
	secret TICKET "$(rnd)"
	load
	# P1 and P2 are issuer portals on one issuer binding: P1 is revoked through the old server, P2 holds the
	# queued run. LP is a console portal with a legacy delegated token, the old server's working control. The
	# grants outlive the job's steps: no lookup reads the portal's lifetime ceiling, only expires_at.
	psqlf -v k="$k" -v issuer="$issuer" -v person="$person" -v legacy="$LEGACY_PERSON" \
		-v p1="$P1" -v p2="$P2" -v lp="$LP" -v ident="$IDENT" -v g1="$G1" -v g2="$G2" -v lt="$LT" -v trun="$TICKET_RUN" \
		-v p1_sha="$(sha "$P1_CRED")" -v p2_sha="$(sha "$P2_CRED")" -v lp_sha="$(sha "$LP_CRED")" \
		-v g1_sha="$(sha "$G1_TOKEN")" -v g2_sha="$(sha "$G2_TOKEN")" -v lt_sha="$(sha "$LT_TOKEN")" \
		-v tk_sha="$(sha "$TICKET")" <<'SQL'
BEGIN;
INSERT INTO portal_issuers (issuer_key, issuer, internal_issuer_norm, person_id_claim, created_by)
VALUES (:'k', :'issuer', :'issuer', 'uid', 'mixed-version-job');
INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by,
                       issuer, internal_issuer, issuer_key, token_profile, user_type, portal_managed_admission)
VALUES (:'p1', 'mixed-version issuer portal 1', 'mixed-version-portal-1', '', :'p1_sha', 'mixed-version-job',
        :'issuer', '', :'k', 'okta_access_token', 'standard', true),
       (:'p2', 'mixed-version issuer portal 2', 'mixed-version-portal-2', '', :'p2_sha', 'mixed-version-job',
        :'issuer', '', :'k', 'okta_access_token', 'standard', true);
INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by)
VALUES (:'lp', 'mixed-version console portal', 'mixed-version-console', 'mixed-version-group', :'lp_sha', 'mixed-version-job');
INSERT INTO principal_identities (id, principal, issuer, display_email, introduced_by, introduced_issuer,
                                  introduction_event_id, introduction_event_at, introduction_audited_at)
VALUES (:'ident', :'person', 'delegate:person:' || :'k', '', :'p1', :'issuer', gen_random_uuid(), now(), now());
INSERT INTO portal_person_portals (identity_id, delegate_id) VALUES (:'ident', :'p1'), (:'ident', :'p2');
INSERT INTO portal_grants (id, delegate_id, token_sha256, principal, user_type, chain_id, subject_issued_at, expires_at)
VALUES (:'g1', :'p1', :'g1_sha', :'person', 'standard', gen_random_uuid(), now(), now() + interval '30 minutes'),
       (:'g2', :'p2', :'g2_sha', :'person', 'standard', gen_random_uuid(), now(), now() + interval '30 minutes');
INSERT INTO delegated_tokens (id, delegate_id, token_sha256, principal, user_type, groups, groups_truncated, expires_at)
VALUES (:'lt', :'lp', :'lt_sha', :'legacy', 'standard', '[]'::jsonb, false, now() + interval '30 minutes');
-- The run an attach ticket names; terminal, so no sweep of either replica acts on it.
INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, created_via, user_type)
VALUES (:'trun', :'person', 'claude-code', '', 'CC1', 'STOPPED', 'spiffe://wardyn.mixed-version/run/' || :'trun', 'k8s', :'p1', 'standard');
-- An attach ticket minted through portal 1's grant, as this tree's ticket mint writes it.
INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, role, expires_at, via_delegate, via_grant, authorized_at)
VALUES (:'tk_sha', :'trun', 'human', :'person', 'user', now() + interval '15 minutes', :'p1', :'g1', now());
COMMIT;
SQL
	echo "seeded issuer portals $P1 $P2, console portal $LP, person $PERSON"
}

old_refusals() {
	load
	pf old-wardyn 18081
	local code
	# Control: the old server serves a legacy portal's delegated token.
	code=$(req legacy-me -H "Authorization: Bearer $LT_TOKEN" "$OLD/api/v1/me")
	expect "old /me with a legacy delegated token" 200 "$code" legacy-me
	jqok "it names the legacy person" legacy-me --arg p "$LEGACY_PERSON" '.principal == $p'
	# The attach ticket first, while the old server's auth-failure audit budget is untouched.
	# ticketAuthority: the terminal attach entry refuses the ticket minted through a portal grant.
	code=$(req ticket "$OLD/api/v1/runs/$TICKET_RUN/attach?ticket=$TICKET")
	expect "old attach with a ticket minted through a portal grant" 403 "$code" ticket
	# delegationLive by the portal grant's id: its verdict, delegation_ended, is the one the refusal records;
	# 0.10.1's ticketAuthority returns it only from delegationLive, so this is not a spent or unknown ticket.
	code=$(req ticket-audit -H "$(admin)" "$OLD/api/v1/audit?action=session.attach&target=$TICKET_RUN")
	expect "old audit read" 200 "$code" ticket-audit
	jqok "the refusal is delegationLive's delegation_ended for portal grant $G1" ticket-audit --arg g "$G1" \
		'[.[] | select(.outcome == "failure" and .data.reason == "delegation_ended" and .data.via.grant == $g)] | length == 1'
	# A wdi_ secret at the old wdp_ exchange: refused at client authentication.
	code=$(req exch-wdi -u "$P1:$P1_CRED" --data "$EXCHANGE_FORM" "$OLD/api/v1/token")
	expect "old token exchange with an issuer portal's wdi_ secret" 401 "$code" exch-wdi
	jqok "the refusal is invalid_client" exch-wdi '.error == "invalid_client"'
	# Control: the legacy wdp_ credential passes client authentication on the same door (the old server has
	# no SSO, so it then answers invalid_grant).
	code=$(req exch-wdp -u "$LP:$LP_CRED" --data "$EXCHANGE_FORM" "$OLD/api/v1/token")
	expect "old token exchange with the console portal's wdp_ credential" 400 "$code" exch-wdp
	jqok "past client authentication: invalid_grant" exch-wdp '.error == "invalid_grant"'
	# A wdg_ grant held in portal_grants: the whole fallback stack gives the single 401 an unknown bearer gets.
	code=$(req grant-me -H "Authorization: Bearer $G1_TOKEN" "$OLD/api/v1/me")
	expect "old /me with a portal_grants wdg_ token" 401 "$code" grant-me
	code=$(req unknown-me -H "Authorization: Bearer wdg_$(rnd)" "$OLD/api/v1/me")
	expect "old /me with an unknown wdg_ token" 401 "$code" unknown-me
	cmp -s "$T/grant-me.json" "$T/unknown-me.json" || fail "the portal grant's 401 differs from an unknown bearer's"
	echo "ok: the portal grant's 401 is the unknown bearer's, byte for byte"
}

old_revoke() {
	load
	pf old-wardyn 18081
	pf new-wardyn 18080
	local code
	# Before: the new server admits both portals' grants, so the refusal after the revoke is the revoke's.
	code=$(req new-g1 -H "Authorization: Bearer $G1_TOKEN" "$NEW/api/v1/me")
	expect "new /me with portal 1's grant, before the revoke" 200 "$code" new-g1
	jqok "it names the portal person" new-g1 --arg p "$PERSON" '.principal == $p'
	code=$(req new-g2 -H "Authorization: Bearer $G2_TOKEN" "$NEW/api/v1/me")
	expect "new /me with portal 2's grant, before the revoke" 200 "$code" new-g2
	code=$(req revoke -X DELETE -H "$(admin)" "$OLD/api/v1/admin/delegates/$P1")
	expect "old DELETE /api/v1/admin/delegates/{id} of issuer portal 1" 204 "$code" revoke
	[ "$(psqlc "SELECT revoked_at IS NOT NULL FROM delegates WHERE id = '$P1'")" = t ] || fail "portal 1 has no revoked_at"
	code=$(req revoke-audit -H "$(admin)" "$OLD/api/v1/audit?action=delegate.revoke&target=$P1")
	expect "old audit read" 200 "$code" revoke-audit
	jqok "one delegate.revoke success row for portal 1" revoke-audit '[.[] | select(.outcome == "success")] | length == 1'
	# After: the new server refuses portal 1's grant with the single 401, and still admits portal 2's.
	code=$(req new-g1-after -H "Authorization: Bearer $G1_TOKEN" "$NEW/api/v1/me")
	expect "new /me with the revoked portal's grant" 401 "$code" new-g1-after
	code=$(req new-unknown -H "Authorization: Bearer wdg_$(rnd)" "$NEW/api/v1/me")
	expect "new /me with an unknown wdg_ token" 401 "$code" new-unknown
	cmp -s "$T/new-g1-after.json" "$T/new-unknown.json" || fail "the revoked grant's 401 differs from an unknown bearer's"
	code=$(req new-g2-after -H "Authorization: Bearer $G2_TOKEN" "$NEW/api/v1/me")
	expect "new /me with portal 2's grant, after portal 1's revoke" 200 "$code" new-g2-after
}

# no_other_sessions: every client session on the database is the old replica's.
no_other_sessions() {
	psqlc "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND client_addr IS NOT NULL AND host(client_addr) <> '$OLD_IP'"
}

old_worker() {
	load
	# Only the old replica may take the row: the new one is scaled to zero, its pods are gone, and no session
	# but the old replica's remains on the database before the row exists.
	kubectl -n "$NS" scale deployment/new-wardyn --replicas=0
	local gone=0 n
	for _ in $(seq 1 60); do
		if [ -z "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=new -o name)" ] && [ "$(no_other_sessions)" = 0 ]; then gone=1; break; fi
		sleep 2
	done
	[ "$gone" = 1 ] || fail "the new replica's pods or database sessions did not go within 120 s"
	echo "ok: only the old replica ($OLD_IP) is connected"
	local pool run grant auth
	pool=$(uuid) run=$(uuid) grant="portal:$G2"
	save QUEUE_RUN "$run"
	# 0.10.1 decodes the field into a uuid.UUID, whose text form is 32, 36, 38 or 45 characters; this is 43.
	[ "${#grant}" -eq 43 ] || fail "the stored grant is ${#grant} characters, not 43"
	# The authority this tree's queueAuthority marshals for a portal launch, in its field order.
	auth=$(jq -nc --arg sub "$PERSON" --arg d "$P2" --arg g "$grant" \
		'{sub: $sub, role: "user", user_type: "standard", groups: [], delegate: $d, grant: $g}')
	# The pool is a person's pool the portal person cannot see. 0.10.1's admission counts such a pool as free
	# and leaves the run to its start, and the start is what this asks about: no runner, executor or k8s room
	# decides anything here.
	psqlf -v pool="$pool" -v run="$run" -v person="$PERSON" -v p2="$P2" -v auth="$auth" <<'SQL'
BEGIN;
INSERT INTO runner_pools (id, name, scope, owner, created_by)
VALUES (:'pool', 'mixed-version queue', 'user', 'mixed-version-pool-owner', 'mixed-version-job');
INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target,
                        created_via, user_type, runner_pool_id)
VALUES (:'run', :'person', 'claude-code', '', 'CC1', 'QUEUED', 'spiffe://wardyn.mixed-version/run/' || :'run', 'k8s',
        :'p2', 'standard', :'pool');
INSERT INTO run_queue (run_id, pool_id, owner, deadline_at, authority, launch)
VALUES (:'run', :'pool', :'person', now() + interval '1 hour', (:'auth')::jsonb, '{}'::jsonb);
COMMIT;
SQL
	echo "queued run $run on pool $pool with grant $grant"
	# The old worker's leader sweep promotes every ten seconds once it holds the lease the new replica left.
	local state=""
	for _ in $(seq 1 60); do
		state=$(psqlc "SELECT state FROM agent_runs WHERE id = '$run'")
		case "$state" in
		STARTING | RUNNING | WAITING_FOR_CONFIRMATION) fail "the old worker started run $run ($state)" ;;
		QUEUED | PENDING) sleep 5 ;;
		*) break ;;
		esac
	done
	[ "$state" = FAILED ] || fail "run $run is $state after 300 s, want FAILED"
	[ "$(no_other_sessions)" = 0 ] || fail "a session other than the old replica's joined the database during the wait"
	[ -z "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=new -o name)" ] || fail "a new-replica pod came back"
	local hint
	hint=$(psqlc "SELECT failure_hint FROM agent_runs WHERE id = '$run'")
	[ "$hint" = "$START_FAILED_HINT" ] || fail "run $run failure_hint is '$hint', want '$START_FAILED_HINT'"
	echo "ok: run $run is FAILED with the start-failed hint"
	[ "$(psqlc "SELECT count(*) FROM run_queue WHERE run_id = '$run'")" = 0 ] || fail "run $run still has a queue row"
	[ "$(psqlc "SELECT sandbox_ref FROM agent_runs WHERE id = '$run'")" = "" ] || fail "run $run has a sandbox_ref"
	# Nothing was dispatched for it: no pod, job or other workload anywhere names the run.
	n=$(kubectl get pods,jobs,deployments,statefulsets,replicasets -A -o json | jq --arg r "$run" '[.items[] | select(tostring | contains($r))] | length')
	[ "$n" = 0 ] || fail "$n cluster object(s) name run $run"
	echo "ok: no pod or job names run $run"
	pf old-wardyn 18081
	local code
	code=$(req queue-audit -H "$(admin)" "$OLD/api/v1/audit?action=run.queue.start&target=$run")
	expect "old audit read" 200 "$code" queue-audit
	jqok "one run.queue.start failure (queue_start_refused) and no success" queue-audit \
		'([.[] | select(.outcome == "failure" and .data.reason == "queue_start_refused")] | length == 1)
		 and ([.[] | select(.outcome == "success")] | length == 0)'
}

case "${1:-}" in
seed) seed ;;
old-refusals) old_refusals ;;
old-revoke) old_revoke ;;
old-worker) old_worker ;;
*) fail "usage: $0 <seed|old-refusals|old-revoke|old-worker>" ;;
esac
