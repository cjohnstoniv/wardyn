// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package loopback is the loopback host-name check the packages that let plain http:// or a no-auth surface reach
// this machine share, so they cannot drift apart on what counts as loopback.
package loopback

import "net"

// Host reports whether host is exactly "localhost" or a loopback IP literal (127.0.0.0/8, ::1, or an IPv4-mapped
// 127 address). It compares host as written: no case folding, no trailing dot, no port, no brackets and no DNS lookup,
// because a name that resolves to 127.0.0.1 is a resolver's claim, not the operator's. A caller that accepts
// "LOCALHOST" or "localhost." normalises host before the call.
func Host(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
