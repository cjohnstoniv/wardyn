// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// defaultProxyHost is the per-run proxy sidecar's alias on the run's internal network.
const defaultProxyHost = "wardyn-proxy"

// defaultGuestProxy is the proxy as a guest's proxy variables name it when the operator sets no
// WARDYN_SANDBOX_PROXY_URL.
var defaultGuestProxy = net.JoinHostPort(defaultProxyHost, strconv.Itoa(runner.ProxyListenPort))

// guestProxyAddr is the proxy host:port the guest's proxy variables name (WARDYN_PROXY_URL), which
// the VM's network relay splices to the run's proxy (-proxy). Unset gives defaultGuestProxy; an
// override that is not http://host:port is refused, since the relay can only match a host and a port.
func guestProxyAddr(env map[string]string) (string, error) {
	raw := env["WARDYN_PROXY_URL"]
	if raw == "" {
		return defaultGuestProxy, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("docker: a VM run needs the sandbox proxy URL (WARDYN_SANDBOX_PROXY_URL) as http://host:port")
	}
	return net.JoinHostPort(u.Hostname(), u.Port()), nil
}

// relayProxyHosts is the relay container's extra hosts: the proxy sidecar's IP under the default
// alias, and under the guest proxy's own name when an override names another (an IP literal needs
// no entry).
func relayProxyHosts(guestProxy, proxyIP string) []string {
	hosts := []string{defaultProxyHost + ":" + proxyIP}
	if h, _, err := net.SplitHostPort(guestProxy); err == nil && h != defaultProxyHost {
		if _, err := netip.ParseAddr(h); err != nil {
			hosts = append(hosts, h+":"+proxyIP)
		}
	}
	return hosts
}
