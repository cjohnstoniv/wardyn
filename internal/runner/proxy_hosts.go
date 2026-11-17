// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"net/netip"
	"net/url"
	"strings"
)

// ProxyHostnames is every name a container or pod sandbox's hosts must map to its run's proxy: wardyn-proxy, and the
// host of env's WARDYN_PROXY_URL when that names another. The sandbox's only nameserver is its proxy, which answers no
// query, so a name its hosts do not map resolves to nothing. An address literal, an unreadable URL and a URL with no
// host add nothing. The name is lower-cased: a hosts lookup ignores case, and Kubernetes refuses capitals in a host
// alias. A VM run's relay maps the same name (relayProxyHosts in internal/runner/docker).
func ProxyHostnames(env map[string]string) []string {
	names := []string{"wardyn-proxy"}
	u, err := url.Parse(env["WARDYN_PROXY_URL"])
	if err != nil {
		return names
	}
	h := strings.ToLower(u.Hostname())
	if _, err := netip.ParseAddr(h); h == "" || h == names[0] || err == nil {
		return names
	}
	return append(names, h)
}
