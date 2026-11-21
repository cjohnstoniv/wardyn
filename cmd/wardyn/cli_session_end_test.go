// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestServerEndWhy: the CLI names the end the server's detail names, and for any other refusal keeps the
// reason code, so a revoke or a reuse reads as it did before.
func TestServerEndWhy(t *testing.T) {
	generic := fmt.Sprintf(endReasonServer, "cli_session_invalid")
	for body, want := range map[string]string{
		`{"error":"x","reason":"cli_session_invalid","detail":"signed_out"}`: endReasonSignedOut,
		`{"error":"x","reason":"cli_session_invalid","detail":"expired"}`:    endReasonExpired,
		`{"error":"x","reason":"cli_session_invalid","detail":"idle"}`:       endReasonIdle,
		`{"error":"x","reason":"cli_session_invalid","detail":"revoked"}`:    generic,
		`{"error":"x","reason":"cli_session_invalid"}`:                       generic,
	} {
		if got := serverEndWhy(sdk.NewAPIError(http.StatusUnauthorized, []byte(body))); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}
