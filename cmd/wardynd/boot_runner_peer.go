// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
)

// secretRunnerPeerKey is the install's runner peer key: 32 random bytes that authenticate one replica's request to
// another for a registered runner whose session that replica holds. Made once, under the boot-key create lock, and
// stored encrypted like every boot key.
const secretRunnerPeerKey = "wardyn-runner-peer-key"

// loadRunnerPeer is this replica's runner peer configuration under WARDYN_HA. Without WARDYN_HA it is nil. Without
// WARDYN_PEER_URL it is nil after one Warn: registered runners are then served only by the replica that holds their
// session, and the setup check says so. It never refuses to start, since whether runners are on is a stored
// setting read at run time.
func loadRunnerPeer(ctx context.Context, f *bootFlags, secrets bootKeyStore, hopRoots *x509.CertPool, controlURL string) (*remote.PeerConfig, error) {
	if !*f.ha {
		return nil, nil
	}
	if *f.peerURL == "" {
		slog.WarnContext(ctx, "wardynd: WARDYN_HA without WARDYN_PEER_URL: registered runners are served only by the replica that holds their session")
		return nil, nil
	}
	u, err := url.Parse(controlURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("WARDYN_PEER_URL needs a WARDYN_CONTROL_PLANE_URL with a host, the name the internal CA certifies")
	}
	key, err := loadOrCreateSecret(ctx, secrets, secretRunnerPeerKey, func(raw []byte) bool { return len(raw) == 32 }, func() ([]byte, error) {
		b := make([]byte, 32)
		_, err := rand.Read(b)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	return &remote.PeerConfig{URL: peerURLOf(*f.peerURL), Key: key, Roots: hopRoots, ServerName: u.Hostname()}, nil
}

// peerURLOf brackets an IPv6 pod address in https://<ip>:<port>, the form the chart builds from status.podIP,
// which cannot tell an IPv4 address from an IPv6 one. Any other value is kept as given.
func peerURLOf(raw string) string {
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok || strings.HasPrefix(rest, "[") || strings.Contains(rest, "/") {
		return raw
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return raw
	}
	host, port := rest[:i], rest[i+1:]
	if ip := net.ParseIP(host); ip == nil || ip.To4() != nil {
		return raw
	}
	return "https://" + net.JoinHostPort(host, port)
}

// hopRootsOf is the pool of this replica's internal CA, the one root a peer's serving certificate is verified by.
func hopRootsOf(caPEM string) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(caPEM))
	return pool
}
