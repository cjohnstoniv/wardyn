// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cryptoRequire matches go.mod's golang.org/x/crypto requirement.
var cryptoRequire = regexp.MustCompile(`(?m)^\s*golang\.org/x/crypto\s+(v\S+)`)

// TestSSHCryptoAdvisoryFloor pins the golang.org/x/crypto floor the SSH gateway
// depends on, because the gateway's own shape is what makes the advisories
// below it unrecoverable rather than merely slow.
//
// handleSSHConn calls ssh.NewServerConn and then CLEARS the handshake deadline
// for the life of the session (`nc.SetDeadline(time.Time{})`), so a connection
// the ssh mux deadlocks has no timer left to reap it and the deferred Close
// never runs: one malicious peer holds a gateway goroutine and its connection
// forever. golang.org/x/crypto/ssh before v0.56.0 deadlocks on exactly that —
// GO-2026-6354 (a flood of incomingRequests on a channel that is registered but
// not yet established) and GO-2026-6355 (crafted messages after establishment).
// Both fixed in v0.56.0.
//
// `make govulncheck` catches a regression here too, but only with a network and
// the live Go vulnerability database, and only on the branch where CI runs it.
// This test fails offline, in a plain `go test`, the moment the pin slips back.
func TestSSHCryptoAdvisoryFloor(t *testing.T) {
	// The first release carrying the GO-2026-6354 / GO-2026-6355 fixes.
	const floor = "v0.56.0"

	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := cryptoRequire.FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no golang.org/x/crypto requirement; the SSH gateway imports golang.org/x/crypto/ssh")
	}
	got := string(m[1])
	if compareModVersion(t, got, floor) < 0 {
		t.Errorf("go.mod pins golang.org/x/crypto %s, below the %s floor: GO-2026-6354 and GO-2026-6355 "+
			"deadlock a connection through ssh.NewServerConn, which handleSSHConn calls with no deadline left to reap it",
			got, floor)
	}
}

// compareModVersion orders two vMAJOR.MINOR.PATCH module versions numerically.
// Pre-release and build suffixes are ignored: this is a floor check, and a
// v0.56.0-rc pin should be argued about upstream, not silently accepted here.
func compareModVersion(t *testing.T, a, b string) int {
	t.Helper()
	pa, pb := modParts(t, a), modParts(t, b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func modParts(t *testing.T, v string) [3]int {
	t.Helper()
	trimmed := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(trimmed, "-+"); i >= 0 {
		trimmed = trimmed[:i]
	}
	fields := strings.Split(trimmed, ".")
	if len(fields) != 3 {
		t.Fatalf("unparseable module version %q", v)
	}
	var out [3]int
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("unparseable module version %q: %v", v, err)
		}
		out[i] = n
	}
	return out
}
