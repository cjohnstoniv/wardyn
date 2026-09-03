// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The LISTEN-ADDRESS CLASSIFIERS: the one place that answers "what does this
// bind actually expose", split out of main.go for the 1000-line file-size gate
// (scripts/check-file-size.sh) when hostname resolution was added.
//
// They are together because they are ONE question asked three ways — is this
// loopback, is it a specific routable interface, is it publicly routable — and
// three boot refusals rest on the answers: -local-mode on a public bind,
// -local-trust-forwarder on a LAN bind, and plaintext HTTP on a LAN bind. Kept
// in one file so a future fourth caller finds the whole rule rather than the
// arm nearest to it.
package main

import (
	"context"
	"net"
	"strings"
	"time"
)

// listenHost extracts the host portion of a listen address, tolerating a bare
// host, a bare ":port", or "host:port".
func listenHost(listen string) string {
	if host, _, err := net.SplitHostPort(listen); err == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(listen)
}

// listenIsLoopback reports whether the listen address binds ONLY the loopback
// interface (127.0.0.0/8, ::1, or host "localhost"). An empty host (":8080") or
// 0.0.0.0/[::] binds all interfaces and is NOT loopback.
func listenIsLoopback(listen string) bool {
	ips, ok := listenHostIPs(listenHost(listen))
	if !ok {
		return false
	}
	// EVERY address, not any: "binds only loopback" is a claim about the whole
	// set, so a name that resolves to 127.0.0.1 AND a LAN address is not
	// loopback-only. The routable twin below asks the opposite question and so
	// uses ANY — both fail closed, in opposite directions.
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// listenIsRoutablePublic reports whether the listen address binds a SPECIFIC,
// globally-routable public IP (not loopback, not private/RFC1918, not link-local,
// and not the unspecified all-interfaces bind). It is the fail-closed gate for
// LocalMode: a no-auth public API must never be served on a public IP. The
// unspecified bind (":8080"/0.0.0.0) is treated as non-public here — it MIGHT
// include a public IP, so it earns a loud warning rather than a refusal (refusing
// it would block the common docker-bridge/compose single-host case).
func listenIsRoutablePublic(listen string) bool {
	host := listenHost(listen)
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we can't classify — don't refuse
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return ip.IsGlobalUnicast()
}

// listenBindsSpecificRoutable reports whether the listen address binds a
// SPECIFIC non-loopback interface — a private/RFC1918, link-local, or public IP
// a LAN peer can reach directly. It EXCLUDES loopback (peers are already local)
// and the unspecified all-interfaces bind (0.0.0.0/[::]), which from inside a
// container is indistinguishable from the safe compose 127.0.0.1-publish
// topology. It is the fail-closed gate for -local-trust-forwarder, which
// disables the loopback-PEER check and is therefore safe ONLY on a loopback or
// unspecified/compose bind. Unlike listenIsRoutablePublic this DELIBERATELY
// catches private and link-local too: with the peer gate disabled, those are
// LAN-reachable no-auth surfaces as well.
func listenBindsSpecificRoutable(listen string) bool {
	ips, ok := listenHostIPs(listenHost(listen))
	if !ok {
		return false // nothing to classify — see listenHostIPs on why that is not a refusal
	}
	// ANY address, not all: one LAN-reachable address is enough to re-open the
	// surface these two callers refuse for, so a dual-stack name whose A record
	// is a LAN address and whose AAAA is ::1 must still refuse.
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// lookupListenIPs is net.DefaultResolver.LookupIPAddr, indirected so the boot
// classifier can be tested without depending on the test host's DNS or /etc/hosts.
var lookupListenIPs = net.DefaultResolver.LookupIPAddr

// listenHostIPs resolves a listen host to the addresses it actually binds: the
// literal for an IP, and the RESOLVED set for a hostname. ok=false means "cannot
// be classified", which both callers read as "do not refuse".
//
// RESOLVING THE HOSTNAME is the fix. Both callers ask the same question — does
// this address bind a specific non-loopback interface a LAN peer can reach —
// and the answer for `lan-host.corp:8080` is yes, identically to the literal it
// resolves to. The classifier used to return false for ANY hostname ("a
// hostname we can't classify — don't refuse"), so the -local-trust-forwarder
// refusal, whose entire job is catching a LAN bind with the loopback-peer gate
// disabled, was skipped by naming the interface instead of numbering it. The
// plaintext-listen refusal had the same hole.
//
// A FAILED LOOKUP STAYS QUIET, deliberately, and this is where the line is
// drawn: an unresolvable name is not a broken configuration this can diagnose —
// the bind itself will fail seconds later with a better message — and refusing
// boot on a transient resolver blip would be the false alarm that gets a boot
// guard filtered out of the logs, so it is not there for the deployment that
// needed it. Same for the empty host (an unspecified bind) and for a name that
// resolves only to loopback, which is safe and merely unusual.
//
// Bounded so a slow or dead resolver cannot hang boot: a hung daemon is a worse
// outcome than a missed refusal, and the loud error-level log below still fires
// for the unclassified case.
func listenHostIPs(host string) ([]net.IP, bool) {
	if host == "" {
		return nil, false
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, true
	}
	// "localhost" is loopback BY DEFINITION (RFC 6761) and is answered without a
	// lookup, exactly as it was before this function existed. Resolving it would
	// make the most common local bind depend on the host's resolver — and a
	// deployment whose /etc/hosts is odd would silently lose the loopback
	// classification rather than gain a better one.
	if strings.EqualFold(host, "localhost") {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), listenLookupTimeout)
	defer cancel()
	addrs, err := lookupListenIPs(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, false
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, true
}

// listenLookupTimeout bounds the one boot-time DNS lookup above.
const listenLookupTimeout = 2 * time.Second
