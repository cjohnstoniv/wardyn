#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-ingress-ports.sh — no Ingress the chart renders routes to wardynd's `internal` port: the TLS listener
# run proxies dial for the run-token internal API (cmd/wardynd/internal_tls.go). It is for sandboxes inside the
# cluster (templates/networkpolicy.yaml); an Ingress would put it on the edge. Renders the all-on values, where both
# Ingresses are on, and requires every Ingress backend to be the console's `http` or the UI gateway's `ui`; then
# plants an `internal` backend in a scratch copy of the chart and requires the same check to refuse it.
#
# Needs helm (the hosted `helm` job, via `make helm-lint`). Daemon-free, network-free.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"

VALUES="$ROOT/deploy/helm/wardyn/ci/all-on-values.yaml"

# ingress_ports <chart dir>: the backend port (name or number) of every rendered Ingress, one per line.
ingress_ports() {
  helm template wardyn "$1" -f "$VALUES" \
    | awk '/^---/ { ing = 0 } /^kind: Ingress$/ { ing = 1 } ing && /^ *port:$/ { p = 1; next } ing && p && /^ *(name|number):/ { print $2; p = 0 }'
}

# edge_only <chart dir>: rc 0 when at least one Ingress renders and every backend is http or ui; prints offenders.
edge_only() {
  local ports
  ports="$(ingress_ports "$1")"
  [[ -n "$ports" ]] || { echo "no Ingress backend rendered from the all-on values" >&2; return 1; }
  ! grep -vxE 'http|ui' <<<"$ports"
}

edge_only "$ROOT/deploy/helm/wardyn" || die "an Ingress routes to a port other than http or ui (above): only the console and the UI gateway may face the edge"
echo "ok  every Ingress backend is http or ui"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cp -R "$ROOT/deploy/helm/wardyn" "$TMP/wardyn"
ING="$TMP/wardyn/templates/ingress.yaml"
sed 's/^\( *\)name: http$/\1name: internal/' "$ING" >"$TMP/ingress.yaml" && mv "$TMP/ingress.yaml" "$ING"
grep -q '^ *name: internal$' "$ING" || die "the plant did not apply: templates/ingress.yaml no longer names its backend port 'http'"
if edge_only "$TMP/wardyn" >/dev/null 2>&1; then
  die "an Ingress routed to the internal port and the check passed it"
fi
echo "ok  an Ingress routed to the internal port is refused"
