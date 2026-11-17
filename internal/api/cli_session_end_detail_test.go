// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCLISessionEndDetail: a refused refresh names only an end of this session's own: a sign-out, its
// lifetime or its idle lifetime. A revoke, a cut, a deactivation and a reuse name nothing.
func TestCLISessionEndDetail(t *testing.T) {
	ended := time.Now()
	for _, c := range []struct{ name, reason, stored, want string }{
		{"signed out on its row", "ended", types.CLISessionEndSignedOut, types.CLISessionEndSignedOut},
		{"ended past its lifetime", "ended", types.CLISessionEndExpired, types.CLISessionEndExpired},
		{"ended idle", "ended", types.CLISessionEndIdle, types.CLISessionEndIdle},
		{"ended by a revoke", "ended", types.CLISessionEndRevoked, ""},
		{"ended by a sessions cut", "ended", types.CLISessionEndSessionsCut, ""},
		{"ended on a reuse", "ended", types.CLISessionEndReuseDetected, ""},
		{"ended on a deactivation", "ended", types.CLISessionEndDeactivated, ""},
		{"ended with no recorded reason", "ended", "", ""},
		{"past its lifetime now", types.CLISessionEndExpired, "", types.CLISessionEndExpired},
		{"idle now", types.CLISessionEndIdle, "", types.CLISessionEndIdle},
		{"revoked now", types.CLISessionEndRevoked, "", ""},
		{"deactivated now", types.CLISessionEndDeactivated, "", ""},
	} {
		sess := store.CLISessionRow{EndReason: c.stored}
		if c.reason == "ended" {
			sess.EndedAt = &ended
		}
		if got := cliSessionEndDetail(sess, c.reason); got != c.want {
			t.Errorf("%s: detail %q, want %q", c.name, got, c.want)
		}
	}
}
