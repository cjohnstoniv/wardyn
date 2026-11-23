#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# kind-readiness-db-cut.sh NAMESPACE RELEASE: the chart's readiness default on a real cluster.
# `make helm-install-test` runs it on its default leg, after the install, against the postgres
# Deployment and Service that target made in NAMESPACE (Makefile helm-install-test). It needs that
# cluster, kubectl, helm and curl, and is run from the repository root.
#
# The cut: the wardyn database is set to refuse new connections (ALLOW_CONNECTIONS false), so
# Postgres answers each one with an error at once, and wardynd's open sessions are ended. Postgres
# keeps running and keeps its data; allowing connections again ends the cut. A Service pointed at no
# pod is not used: on kind it left wardynd's reads hanging, the stalled-database case that
# docs/operations/high-availability.md says restarts the pod.
#
# 1. The release as installed (readiness on /healthz, the chart default): through a CUT_SECS cut the
#    pod stays Ready and is not restarted, /healthz answers 200 and /readyz answers 503, which is
#    what the console's banner reads (ui/src/app/App.tsx refreshHealth).
# 2. The release upgraded with --reuse-values --set readinessProbe.path=/readyz: the Deployment keeps
#    that path, and the same cut takes the pod out of Ready, so the cut in 1 was long enough to matter.
set -euo pipefail

NS=${1:?usage: kind-readiness-db-cut.sh NAMESPACE RELEASE}
REL=${2:?usage: kind-readiness-db-cut.sh NAMESPACE RELEASE}
# Longer than three failed readiness checks 10 s apart plus their 3 s timeout (templates/deployment.yaml).
CUT_SECS=${CUT_SECS:-40}
PORT=${DB_CUT_PORT:-18090}
samples=$(mktemp)
pf_log=$(mktemp)
pf_pid=""

k() { kubectl -n "$NS" "$@"; }
pg() { k exec deploy/postgres -- psql -U wardyn -d postgres -Atc "$1" >/dev/null; }
restore() { pg "ALTER DATABASE wardyn WITH ALLOW_CONNECTIONS true"; }
cleanup() {
	if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; fi
	restore 2>/dev/null || true
	rm -f "$samples" "$pf_log"
}
trap cleanup EXIT
fail() {
	echo "kind-readiness-db-cut: FAIL: $*" >&2
	k get pods -o wide >&2 || true
	k logs -l app.kubernetes.io/name=wardyn --tail=50 --all-containers >&2 || true
	exit 1
}

pod() { k get pod -l app.kubernetes.io/name=wardyn --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}'; }
ready() { k get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'; }
restarts() { k get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="wardynd")].restartCount}'; }
probe_path() { k get deploy "$REL" -o jsonpath='{.spec.template.spec.containers[?(@.name=="wardynd")].readinessProbe.httpGet.path}'; }
code() { curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:${PORT}$1" || true; }

# forward POD: a port-forward to the pod itself, which answers whether or not the pod is Ready.
forward() {
	if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; fi
	kubectl -n "$NS" port-forward "pod/$1" "${PORT}:8080" >"$pf_log" 2>&1 &
	pf_pid=$!
	for _ in $(seq 1 30); do
		[ "$(code /healthz)" = 200 ] && return 0
		sleep 1
	done
	cat "$pf_log" >&2
	fail "a port-forward to $1 never answered /healthz"
}

# wait_ready POD SECS
wait_ready() {
	for _ in $(seq 1 "$2"); do
		[ "$(ready "$1")" = True ] && return 0
		sleep 1
	done
	fail "$1 was not Ready within $2 s"
}

cut() {
	pg "ALTER DATABASE wardyn WITH ALLOW_CONNECTIONS false"
	for _ in 1 2; do
		pg "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = 'wardyn' AND pid <> pg_backend_pid()"
		sleep 2
	done
}

# watch POD: one line a second for CUT_SECS, "<Ready> <healthz code> <readyz code>", into $samples.
watch() {
	local end=$((SECONDS + CUT_SECS))
	: >"$samples"
	while ((SECONDS < end)); do
		echo "$(ready "$1") $(code /healthz) $(code /readyz)" >>"$samples"
		sleep 1
	done
}
summary() { sort "$samples" | uniq -c | tr '\n' ';'; }

echo "==> leg 1: the chart default"
[ "$(probe_path)" = /healthz ] || fail "the default install's readiness path is '$(probe_path)', want /healthz"
p=$(pod)
wait_ready "$p" 60
forward "$p"
before=$(restarts "$p")
cut
watch "$p"
restore
if grep -qv '^True ' "$samples"; then fail "readiness on /healthz: $p left Ready during the cut: $(summary)"; fi
if [ -n "$(awk '$2 != 200' "$samples")" ]; then fail "/healthz did not answer 200 throughout the cut: $(summary)"; fi
n503=$(awk '$3 == 503' "$samples" | wc -l)
((n503 >= 5)) || fail "/readyz answered 503 $n503 times during the cut, so the cut did not reach wardynd: $(summary)"
[ "$(restarts "$p")" = "$before" ] || fail "wardynd restarted during the cut (restartCount $before -> $(restarts "$p"))"
for _ in $(seq 1 60); do
	[ "$(code /readyz)" = 200 ] && break
	sleep 1
done
[ "$(code /readyz)" = 200 ] || fail "/readyz did not answer 200 within 60 s of the cut ending"
echo "leg 1: $p stayed Ready, /healthz 200, /readyz 503 x$n503: $(summary)"

echo "==> leg 2: readinessProbe.path=/readyz kept through --reuse-values"
helm upgrade "$REL" ./deploy/helm/wardyn --namespace "$NS" --reuse-values --set readinessProbe.path=/readyz --wait --timeout 240s >/dev/null
k rollout status "deployment/$REL" --timeout=240s >/dev/null
[ "$(probe_path)" = /readyz ] || fail "--set readinessProbe.path=/readyz was not kept: the Deployment reads '$(probe_path)'"
p=$(pod)
wait_ready "$p" 90
forward "$p"
cut
watch "$p"
restore
grep -q '^False ' "$samples" || fail "readiness on /readyz: $p stayed Ready through a ${CUT_SECS} s cut, so leg 1 proves nothing: $(summary)"
wait_ready "$p" 90
echo "leg 2: $p left Ready during the cut and came back: $(summary)"
echo "kind-readiness-db-cut: PASS"
