// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// NoProxyCovers reports whether a bypass list (SiteConfig.UpstreamProxyNoProxy) sends host around the upstream
// proxy: the rule Proxy.bypassUpstream applies at dial time, for a reader that holds the list but no Proxy. It
// compiles the list as a Proxy does and matches through noProxyRulesCoverHost, so the rule keeps one spelling.
func NoProxyCovers(entries []string, host string) bool {
	return noProxyRulesCoverHost(compileNoProxy(entries), host)
}
