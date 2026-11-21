#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# portal-artifacts.sh <seed|old-refusals|old-revoke|old-worker|old-callback>: the issuer-portal half of
# .github/workflows/mixed-version.yml. The published v0.10.1 replica (svc/old-wardyn) and this tree's replica
# (svc/new-wardyn) share one migrated database. seed writes synthetic issuer-portal state into it, in the shapes
# migration 0294 defines and this tree's store writes; the others ask the old replica's own entry points
# about that state:
#   old-refusals  each issuer-portal artifact is refused at the old server's door, beside a legacy portal it
#                 still serves;
#   old-revoke    the old server's DELETE /api/v1/admin/delegates/{id} revokes an issuer portal, and the new
#                 server then refuses that portal's grant (and still admits another portal's);
#   old-worker    the new server queues one run through an issuer portal's grant and one through a console
#                 portal's token (its own POST /api/v1/runs, on a full pool); with the new replica scaled to
#                 zero, the old worker ends the first before any authority is bound and never dispatches it,
#                 and binds and starts the second;
#   old-callback  the old server's SSO callback, against fake-oidc-issuer.py, refuses an issuer-portal
#                 person's subject and signs in an ordinary one.
# Needs NS, OLD_IP and ADMIN_TOKEN (the job's env), RUNNER_TEMP, kubectl and helm on the kind cluster, docker,
# curl, jq, openssl and python3.
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
		psqlc "SELECT id, state, failure_hint, status_detail, sandbox_ref, watcher_owner FROM agent_runs WHERE id IN ('$TICKET_RUN', '${QUEUE_RUN:-$TICKET_RUN}', '${LEGACY_RUN:-$TICKET_RUN}')"
		psqlc "SELECT run_id, pool_id, owner, authority FROM run_queue"
		psqlc "SELECT id, scope, revision, limits FROM runner_pools"
		psqlc "SELECT * FROM remote_runners"
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
-- Pinned to the root unit (store.DefaultUnitID): this tree refuses a launch through an unpinned issuer portal.
INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by,
                       issuer, internal_issuer, issuer_key, token_profile, user_type, portal_managed_admission, run_in_under)
VALUES (:'p1', 'mixed-version issuer portal 1', 'mixed-version-portal-1', '', :'p1_sha', 'mixed-version-job',
        :'issuer', '', :'k', 'okta_access_token', 'standard', true, '00000000-0000-0000-0000-000000000001'),
       (:'p2', 'mixed-version issuer portal 2', 'mixed-version-portal-2', '', :'p2_sha', 'mixed-version-job',
        :'issuer', '', :'k', 'okta_access_token', 'standard', true, '00000000-0000-0000-0000-000000000001');
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

# launch <name> <bearer>: one real POST /api/v1/runs at the new server; it must queue. Run it in this shell, so
# its failure ends the step; the run id is in $T/<name>.json.
launch() {
	local code
	code=$(req "$1" -X POST -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
		-d '{"experience":"background","workload":{"kind":"command","command":"true"}}' "$NEW/api/v1/runs")
	[ "$code" = 201 ] || fail "$1: the new server's POST /api/v1/runs answered $code, want 201: $(head -c 600 "$T/$1.json")"
	jq -e '.state == "QUEUED" and (.id | length) == 36' "$T/$1.json" >/dev/null || fail "$1: the launch did not queue: $(head -c 600 "$T/$1.json")"
}

# old_worker: two runs queue through this tree's own launch path while the organization pool is full, one through
# issuer portal 2's grant and one through the console portal's token. With the new replica gone, the old worker
# takes both: it ends the issuer-portal run before it binds any authority, with the start-failed hint, and
# dispatches nothing; it binds and starts the console-portal run, whose start rows carry the portal.
old_worker() {
	load
	pf new-wardyn 18080
	local pool prev rrev filler portal_run legacy_run g n code state
	# The organization pool and the server's own Kubernetes runner, both made at first boot. Their limits go in
	# through the admin API: the runner's (a runner with none never fits a run), and the pool's, whose cap makes
	# every launch queue. One slot holder (a row that names no runner and no sandbox) fills that cap.
	pool=$(psqlc "SELECT id FROM runner_pools WHERE scope = 'organization'")
	[ "$(printf '%s\n' "$pool" | grep -c .)" = 1 ] || fail "want one organization pool, have: $pool"
	rrev=$(psqlc "SELECT revision FROM remote_runners WHERE id = 'k8s'")
	[ -n "$rrev" ] || fail "the server's Kubernetes runner (remote runner k8s) has no row"
	code=$(req runner-limits -X PUT -H "$(admin)" -H 'Content-Type: application/json' "$NEW/api/v1/remote-runners/k8s" \
		-d "{\"revision\":$rrev,\"limits\":{\"runs_at_once\":4,\"total\":{\"cpu_millis\":4000,\"memory_mib\":8192,\"disk_mib\":20480},\"per_run\":{\"cpu_millis\":1000,\"memory_mib\":2048,\"disk_mib\":5120}}}")
	expect "new PUT /remote-runners/k8s limits" 200 "$code" runner-limits
	filler=$(uuid)
	psqlf -v pool="$pool" -v run="$filler" <<'SQL'
INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target,
                        runner_pool_id, runner_pool_selection)
VALUES (:'run', 'mixed-version-slot-holder', 'claude-code', '', 'CC1', 'RUNNING', 'spiffe://wardyn.mixed-version/run/' || :'run',
        'k8s', :'pool', 'organisation_default');
SQL
	prev=$(psqlc "SELECT revision FROM runner_pools WHERE id = '$pool'")
	code=$(req pool-limits -X PUT -H "$(admin)" -H 'Content-Type: application/json' "$NEW/api/v1/runner-pools/$pool" \
		-d "{\"revision\":$prev,\"limits\":{\"background\":{},\"barriers\":[],\"max_concurrent_runs\":1}}")
	expect "new PUT /runner-pools/$pool limits" 200 "$code" pool-limits

	launch launch-portal "$G2_TOKEN"
	launch launch-legacy "$LT_TOKEN"
	portal_run=$(jq -r .id "$T/launch-portal.json")
	legacy_run=$(jq -r .id "$T/launch-legacy.json")
	save QUEUE_RUN "$portal_run"
	save LEGACY_RUN "$legacy_run"
	echo "ok: the new server queued run $portal_run (issuer portal 2) and run $legacy_run (console portal)"
	# What this tree's producer stored. 0.10.1 decodes the grant into a uuid.UUID, whose text form is 32, 36, 38
	# or 45 characters; "portal:<uuid>" is 43.
	g=$(psqlc "SELECT authority->>'grant' FROM run_queue WHERE run_id = '$portal_run'")
	[ "$g" = "portal:$G2" ] || fail "run $portal_run's stored grant is '$g', want portal:$G2"
	[ "${#g}" -eq 43 ] || fail "the stored grant is ${#g} characters, not 43"
	[ "$(psqlc "SELECT authority->>'delegate' FROM run_queue WHERE run_id = '$portal_run'")" = "$P2" ] || fail "run $portal_run's stored delegate is not portal 2"
	echo "ok: run $portal_run's stored grant is $g"
	g=$(psqlc "SELECT authority->>'grant' FROM run_queue WHERE run_id = '$legacy_run'")
	[ "$g" = "$LT" ] || fail "run $legacy_run's stored grant is '$g', want the bare $LT"
	[ "$(psqlc "SELECT authority->>'delegate' FROM run_queue WHERE run_id = '$legacy_run'")" = "$LP" ] || fail "run $legacy_run's stored delegate is not the console portal"
	echo "ok: run $legacy_run's stored grant is the bare $g"

	# Only the old replica may take the rows: the new one is scaled to zero, its pods are gone, and no session
	# but the old replica's remains on the database before the slot frees.
	kubectl -n "$NS" scale deployment/new-wardyn --replicas=0
	local gone=0
	for _ in $(seq 1 60); do
		if [ -z "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=new -o name)" ] && [ "$(no_other_sessions)" = 0 ]; then gone=1; break; fi
		sleep 2
	done
	[ "$gone" = 1 ] || fail "the new replica's pods or database sessions did not go within 120 s"
	echo "ok: only the old replica ($OLD_IP) is connected"
	for r in "$portal_run" "$legacy_run"; do
		[ "$(psqlc "SELECT state FROM agent_runs WHERE id = '$r'")" = QUEUED ] || fail "run $r left the queue while the pool was full"
	done
	# Free the slot and raise the cap past both runs, so neither waits on the other.
	psqlf -v pool="$pool" -v run="$filler" <<'SQL'
BEGIN;
UPDATE agent_runs SET state = 'STOPPED' WHERE id = :'run';
UPDATE runner_pools SET limits = jsonb_set(limits, '{max_concurrent_runs}', '4'), revision = revision + 1 WHERE id = :'pool';
COMMIT;
SQL
	# The old worker's leader sweep promotes every ten seconds once it holds the lease the new replica left.
	state=""
	for _ in $(seq 1 60); do
		state=$(psqlc "SELECT state FROM agent_runs WHERE id = '$portal_run'")
		case "$state" in
		STARTING | RUNNING | WAITING_FOR_CONFIRMATION) fail "the old worker started run $portal_run ($state)" ;;
		QUEUED | PENDING) sleep 5 ;;
		*) break ;;
		esac
	done
	[ "$state" = FAILED ] || fail "run $portal_run is $state after 300 s, want FAILED"
	[ "$(no_other_sessions)" = 0 ] || fail "a session other than the old replica's joined the database during the wait"
	[ -z "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=new -o name)" ] || fail "a new-replica pod came back"
	# 0.10.1 writes the terminal state before the hint, and the refusal's audit row after both, so each is
	# waited for on its own.
	local hint=""
	for _ in $(seq 1 30); do
		hint=$(psqlc "SELECT failure_hint FROM agent_runs WHERE id = '$portal_run'")
		[ "$hint" = "$START_FAILED_HINT" ] && break
		sleep 2
	done
	[ "$hint" = "$START_FAILED_HINT" ] || fail "run $portal_run failure_hint is '$hint' after 60 s, want '$START_FAILED_HINT'"
	echo "ok: run $portal_run is FAILED with the start-failed hint"
	[ "$(psqlc "SELECT count(*) FROM run_queue WHERE run_id = '$portal_run'")" = 0 ] || fail "run $portal_run still has a queue row"
	[ "$(psqlc "SELECT sandbox_ref FROM agent_runs WHERE id = '$portal_run'")" = "" ] || fail "run $portal_run has a sandbox_ref"
	# Nothing was dispatched for it: no pod, job or other workload anywhere names the run.
	n=$(kubectl get pods,jobs,deployments,statefulsets,replicasets -A -o json | jq --arg r "$portal_run" '[.items[] | select(tostring | contains($r))] | length')
	[ "$n" = 0 ] || fail "$n cluster object(s) name run $portal_run"
	echo "ok: no pod or job names run $portal_run"
	pf old-wardyn 18081
	local refused=0
	for _ in $(seq 1 24); do
		code=$(req queue-audit -H "$(admin)" "$OLD/api/v1/audit?action=run.queue.start&target=$portal_run")
		if [ "$code" = 200 ] && jq -e 'any(.[]; .outcome == "failure" and .data.reason == "queue_start_refused")' "$T/queue-audit.json" >/dev/null; then
			refused=1
			break
		fi
		sleep 5
	done
	[ "$refused" = 1 ] || fail "the old server shows no run.queue.start refusal for run $portal_run within 120 s (last answer $code): $(head -c 600 "$T/queue-audit.json")"
	# Its one row is written before any authority is bound, so it carries no portal.
	jqok "one run.queue.start failure (queue_start_refused) with no portal on it, and no success" queue-audit \
		'([.[] | select(.outcome == "failure" and .data.reason == "queue_start_refused" and .data.via == null)] | length == 1)
		 and ([.[] | select(.outcome == "success" or .data.via != null)] | length == 0)'
	# The control: the old worker bound the console portal's run to its portal and started it.
	local bound=0
	for _ in $(seq 1 24); do
		code=$(req legacy-audit -H "$(admin)" "$OLD/api/v1/audit?action=run.queue.start&target=$legacy_run")
		if [ "$code" = 200 ] && jq -e 'length > 0' "$T/legacy-audit.json" >/dev/null; then bound=1; break; fi
		sleep 5
	done
	[ "$bound" = 1 ] || fail "the old worker wrote no run.queue.start row for run $legacy_run within 120 s"
	jqok "run $legacy_run's start succeeded, bound to the console portal and its token" legacy-audit --arg d "$LP" --arg g "$LT" \
		'([.[] | select(.outcome == "success" and .data.via.delegate == $d and .data.via.grant == $g)] | length == 1)
		 and ([.[] | select(.outcome == "failure")] | length == 0)'
	state=$(psqlc "SELECT state FROM agent_runs WHERE id = '$legacy_run'")
	[ "$state" != QUEUED ] || fail "run $legacy_run is still QUEUED after its start"
	[ "$(psqlc "SELECT failure_hint FROM agent_runs WHERE id = '$legacy_run'")" != "$START_FAILED_HINT" ] || fail "run $legacy_run ended with the start-failed hint"
	echo "ok: run $legacy_run left the queue as $state"
}

# sign_in <dir> <sub> <email>: one browser sign-in at the old server through the fake issuer, on the cookie jar
# <dir>/jar. Every header file stays in <dir>, the private directory beside the jar, never in $T, which the
# diagnostics print; an error names the status and path only, never a cookie or a redirect's query. Writes the
# callback's status and Location to <dir>/signin.out; run it in this shell, so its own failure ends the step.
sign_in() {
	local dir=$1 sub=$2 email=$3 loc code
	code=$(curl -sS -o /dev/null -D "$dir/login.hdr" -c "$dir/jar" -b "$dir/jar" -w '%{http_code}' "$OLD/auth/login")
	[ "$code" = 302 ] || fail "GET /auth/login answered $code, want 302"
	loc=$(sed -n 's/^[Ll]ocation: *//p' "$dir/login.hdr" | tr -d '\r')
	case "$loc" in "$ISSUER/authorize?"*) ;; *) fail "/auth/login sent the browser to ${loc%%\?*}, want $ISSUER/authorize" ;; esac
	loc="$loc&mv_sub=$(jq -rn --arg v "$sub" '$v|@uri')&mv_email=$(jq -rn --arg v "$email" '$v|@uri')"
	code=$(curl -sS -o /dev/null -D "$dir/authorize.hdr" -w '%{http_code}' "$loc")
	[ "$code" = 302 ] || fail "GET $ISSUER/authorize answered $code, want 302"
	loc=$(sed -n 's/^[Ll]ocation: *//p' "$dir/authorize.hdr" | tr -d '\r')
	case "$loc" in "$OLD/auth/callback?"*) ;; *) fail "the fake issuer sent the browser to ${loc%%\?*}, want $OLD/auth/callback" ;; esac
	code=$(curl -sS -o /dev/null -D "$dir/callback.hdr" -c "$dir/jar" -b "$dir/jar" -w '%{http_code}' "$loc")
	printf '%s %s\n' "$code" "$(sed -n 's/^[Ll]ocation: *//p' "$dir/callback.hdr" | tr -d '\r')" >"$dir/signin.out"
}

# live_session <jar>: the jar holds an unexpired wardyn_session cookie (curl drops one the server expired).
live_session() { awk -F'\t' '$6 == "wardyn_session" && $7 != "" { found = 1 } END { exit !found }' "$1"; }

# old_callback: the released server's SSO callback refuses an issuer-portal person's subject (a delegate:
# principal, reserved at v0.10.1) as 0.10.1 answers it: a redirect to the sign-in error page with
# auth_error=sign_in_refused, no live session, and an auth.fail row naming reserved_principal. An ordinary
# subject through the same issuer signs in. The old replica is upgraded in place with the issuer's settings,
# so this runs after every other check that asks it.
old_callback() {
	load
	local gw keydir jar code loc cid=mixed-version-console control=mixed-version-console-person
	gw=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -E '^[0-9]+(\.[0-9]+){3}$' | head -n1)
	[ -n "$gw" ] || fail "the kind network has no IPv4 gateway"
	ISSUER="http://$gw:5556"
	# The key, the cookie jar and every sign-in header file stay out of $T, which the diagnostics print.
	keydir=$(mktemp -d "$RUNNER_TEMP/oidc.XXXXXX")
	chmod 700 "$keydir"
	openssl genrsa -out "$keydir/key.pem" 2048 2>/dev/null
	python3 -I .github/mixed-version/fake-oidc-issuer.py "$gw" 5556 "$keydir/key.pem" "$cid" >"$T/fake-issuer.log" 2>&1 &
	pids+=("$!")
	for _ in $(seq 1 20); do
		if curl -fsS -o "$T/discovery.json" "$ISSUER/.well-known/openid-configuration" 2>/dev/null; then break; fi
		sleep 1
	done
	jqok "the fake issuer serves its discovery document" discovery --arg i "$ISSUER" '.issuer == $i'

	helm upgrade old ./deploy/helm/wardyn --namespace "$NS" --reuse-values \
		--set-string env.WARDYN_OIDC_ISSUER="$ISSUER" \
		--set-string env.WARDYN_OIDC_CLIENT_ID="$cid" \
		--set-string env.WARDYN_OIDC_REDIRECT_URL="$OLD/auth/callback" \
		--set-string env.WARDYN_OIDC_OPERATOR_EMAILS=ops@mixed-version.invalid \
		--set-json "networkPolicy.egress.extra=[{\"to\":[{\"ipBlock\":{\"cidr\":\"$gw/32\"}}],\"ports\":[{\"port\":5556,\"protocol\":\"TCP\"}]}]"
	kubectl -n "$NS" rollout status deployment/old-wardyn --timeout=300s || fail "the old replica did not come back with the issuer configured"
	pf old-wardyn 18081
	jar="$keydir/jar"

	: >"$jar"
	sign_in "$keydir" "$PERSON" person-1@mixed-version.invalid
	read -r code loc <"$keydir/signin.out"
	[ "$code" = 302 ] || fail "GET $OLD/auth/callback for the portal person's subject answered $code, want 302"
	[ "$loc" = "/?auth_error=sign_in_refused" ] || fail "GET $OLD/auth/callback for the portal person's subject sent the browser to ${loc%%\?*} (query withheld), want /?auth_error=sign_in_refused"
	echo "ok: callback with subject $PERSON -> 302 $loc"
	! live_session "$jar" || fail "the refused callback left a live wardyn_session cookie"
	echo "ok: no live session after the refused callback"
	code=$(req me-refused -b "$jar" "$OLD/api/v1/me")
	expect "old /me on the refused sign-in's cookies" 401 "$code" me-refused
	expect "old audit read of auth.fail" 200 "$(req audit-callback -H "$(admin)" "$OLD/api/v1/audit?action=auth.fail&actor=wardyn/oidcCallback")" audit-callback
	jqok "one auth.fail row from the callback names reserved_principal" audit-callback \
		'[.[] | select(.target == "/auth/callback" and .outcome == "failure" and .data.reason == "reserved_principal")] | length == 1'

	: >"$jar"
	sign_in "$keydir" "$control" person@mixed-version.invalid
	read -r code loc <"$keydir/signin.out"
	[ "$code" = 302 ] || fail "GET $OLD/auth/callback for an ordinary subject answered $code, want 302"
	case "$loc" in "" | *auth_error*) fail "GET $OLD/auth/callback for an ordinary subject sent the browser to ${loc%%\?*} (query withheld), want a page without auth_error" ;; esac
	echo "ok: callback with subject $control -> 302 ${loc%%\?*}"
	live_session "$jar" || fail "the ordinary sign-in set no wardyn_session cookie"
	code=$(req me-control -b "$jar" "$OLD/api/v1/me")
	expect "old /me on the ordinary sign-in's session" 200 "$code" me-control
	jqok "the ordinary sign-in's principal is its subject" me-control --arg p "$control" '.principal == $p'
	rm -rf "$keydir"
}

case "${1:-}" in
seed) seed ;;
old-refusals) old_refusals ;;
old-revoke) old_revoke ;;
old-worker) old_worker ;;
old-callback) old_callback ;;
*) fail "usage: $0 <seed|old-refusals|old-revoke|old-worker|old-callback>" ;;
esac
