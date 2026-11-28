# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-ha-peer-egress.awk: exit 0 when a rendered chart's control-plane NetworkPolicy has one egress rule whose only
# destination is this release's control-plane pods in the same namespace (a podSelector with the name, instance and
# control-plane component labels, and no namespaceSelector or ipBlock), on TCP port internal: the runner peer door.
# Exit 1 otherwise. helm-lint runs it on the ha.enabled render (rc 0) and the render without it (rc 1).

function close_rule() {
	if (rule && cp && name && inst && tcp && port && !other) found = 1
	rule = cp = name = inst = tcp = port = other = 0
}
function close_doc() {
	if (eg) close_rule()
	doc = cpdoc = top = eg = 0
}
/^---/ { close_doc(); next }
/^kind: NetworkPolicy$/ { doc = 1 }
doc && /^  podSelector:$/ { top = 1; next }
doc && top && /^      app\.kubernetes\.io\/component: control-plane$/ { cpdoc = 1 }
doc && top && /^  [a-zA-Z]/ { top = 0 }
doc && cpdoc && /^  egress:$/ { eg = 1; next }
eg && /^  [a-zA-Z]/ { close_rule(); eg = 0 }
eg && /^    - / { close_rule(); rule = 1 }
eg && rule && /namespaceSelector:|ipBlock:/ { other = 1 }
eg && rule && /^              app\.kubernetes\.io\/component: control-plane$/ { cp = 1 }
eg && rule && /^              app\.kubernetes\.io\/name: / { name = 1 }
eg && rule && /^              app\.kubernetes\.io\/instance: / { inst = 1 }
eg && rule && /^        - protocol: TCP$/ { tcp = 1 }
eg && rule && /^          port: internal$/ { port = 1 }
END { close_doc(); exit !found }
