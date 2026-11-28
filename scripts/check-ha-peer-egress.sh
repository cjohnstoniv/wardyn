#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-ha-peer-egress.sh < MANIFEST: exit 0 when the rendered chart's control-plane NetworkPolicy has exactly one
# egress rule whose only destination is a podSelector matching exactly this release's control-plane pods (the
# policy's own name and instance labels, and component control-plane), and whose ports are exactly one entry, TCP on
# port internal: the runner peer door. Exit 1 otherwise. helm-lint runs it on the ha.enabled render (rc 0) and on the
# render without it (rc 1). `check-ha-peer-egress.sh --self-test` runs it on fixed fixtures.
set -euo pipefail
die() { echo "check-ha-peer-egress: $*" >&2; exit 2; }
for tool in yq jq; do
  command -v "$tool" >/dev/null || die "$tool is required (yq must be Mike Farah v4)"
done

check() {
  yq eval-all -o=json '[select(.kind == "NetworkPolicy")]' - | jq -e '
    [.[] | select(.spec.podSelector.matchLabels["app.kubernetes.io/component"] == "control-plane")] as $nps
    | ($nps | length) == 1
      and ($nps[0] as $np | $np.spec.podSelector.matchLabels as $sel
        | [($np.spec.egress // [])[] | select(
            (.to | type) == "array" and (.to | length) == 1
            and .to[0] == {podSelector: {matchLabels: {
              "app.kubernetes.io/name": $sel["app.kubernetes.io/name"],
              "app.kubernetes.io/instance": $sel["app.kubernetes.io/instance"],
              "app.kubernetes.io/component": "control-plane"}}}
            and .ports == [{protocol: "TCP", port: "internal"}])]
        | length == 1)' >/dev/null
}

[ "${1:-}" = --self-test ] || { check; exit; }

policy() { # policy TO_ENTRIES PORT_ENTRIES: a control-plane policy whose one extra egress rule has these to and ports
  cat <<EOF
kind: NetworkPolicy
spec:
  podSelector:
    matchLabels: {app.kubernetes.io/name: wardyn, app.kubernetes.io/instance: rel, app.kubernetes.io/component: control-plane}
  egress:
    - to: [{namespaceSelector: {}}]
      ports: [{protocol: UDP, port: 53}]
    - to: $1
      ports: $2
---
kind: Service
EOF
}
own='{podSelector: {matchLabels: {app.kubernetes.io/name: wardyn, app.kubernetes.io/instance: rel, app.kubernetes.io/component: control-plane}}}'
db='{podSelector: {matchLabels: {app.kubernetes.io/name: wardyn, app.kubernetes.io/instance: rel, app.kubernetes.io/component: database}}}'
foreign='{podSelector: {matchLabels: {app.kubernetes.io/name: wardyn, app.kubernetes.io/instance: other, app.kubernetes.io/component: control-plane}}}'
tcp='[{protocol: TCP, port: internal}]'
fail=0
expect() { # expect WANT_RC NAME: the check on stdin must exit WANT_RC; runs in this shell, so fail is kept
  local rc=0
  check || rc=$?
  echo "fixture $2: rc=$rc (want $1)"
  [ "$rc" = "$1" ] || fail=1
}
expect 0 intended < <(policy "[$own]" "$tcp")
expect 1 ha-off < <(policy "[{namespaceSelector: {}}]" '[{protocol: UDP, port: 53}]')
expect 1 wrong-component < <(policy "[$db]" "$tcp")
expect 1 foreign-instance < <(policy "[$foreign]" "$tcp")
expect 1 extra-open-podselector < <(policy "[$own, {podSelector: {}}]" "$tcp")
expect 1 split-protocol-port < <(policy "[$own]" '[{protocol: TCP, port: 53}, {protocol: UDP, port: internal}]')
expect 1 extra-port < <(policy "[$own]" '[{protocol: TCP, port: internal}, {protocol: TCP, port: 22}]')
exit "$fail"
