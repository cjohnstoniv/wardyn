// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package api wires Wardyn's control-plane REST surface (the wardynd binary).
// It contains ZERO target-specific code (the parity rule): it talks to runners
// only through the runner.Runner interface, and to identity/secrets/broker only
// through their contract interfaces. Every security decision fails closed.
//
// Routes: see routes.go for the table. The authoritative, always-current
// classification of every one of them — which role gate each sits behind — is
// authz_test.go's chi.Walk-enumerated matrix, which cannot go stale because it
// walks the router itself.
package api
