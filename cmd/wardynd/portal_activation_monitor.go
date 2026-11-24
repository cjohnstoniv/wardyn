// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "context"

// portalActivationMonitor is what startPortalActivationMonitor starts: api.Server's rolling sample of the
// database's other clients, which the issuer-portal activation reads and its hourly alarm reports from.
type portalActivationMonitor interface {
	RunPortalActivationMonitor(ctx context.Context)
}

// startPortalActivationMonitor starts the monitor once on this replica, with the replica's own context. Every
// replica runs it (not only the sweeper leader): the activation call is answered from the replica it reaches,
// and an old server is a client of the database whichever replica sees it.
func startPortalActivationMonitor(ctx context.Context, m portalActivationMonitor) {
	go goSafe("portal.activation.monitor", func() { m.RunPortalActivationMonitor(ctx) })
}
