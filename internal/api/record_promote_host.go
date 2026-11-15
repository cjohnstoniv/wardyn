// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/egress/proxy"

// notPromotableText is the refusal of a promote that names host outside the promotable set: why the proxy
// refuses its spelling in every request when it does (promotableHosts never offers such a host), or else
// that the recording never allowed it.
func notPromotableText(host string) string {
	if err := proxy.ValidConfigHost(host); err != nil {
		return "host " + err.Error()
	}
	return "host " + host + " was not observed+allowed in this recording — cannot promote"
}
