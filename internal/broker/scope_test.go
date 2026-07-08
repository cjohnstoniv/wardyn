// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These are PURE unit tests for scope.go's clamp / no-widening primitives and
// the small helpers (ttlFor, newJTI, spiffeForRun). They complement the
// DB-backed broker_test.go (which only spot-checks clampGitHubPermissions and
// ttlFor at a coarse level) by exercising the underlying minLevel ranking and
// the full no-widening matrix for clampGitHubPermissions.

// minLevel is the primitive every clamp leans on: it must return the WEAKER of
// two access levels and fail closed (read) for anything it does not recognize.
func TestMinLevel(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want string
	}{
		{"read_read", "read", "read", "read"},
		{"write_write", "write", "write", "write"},
		{"admin_admin", "admin", "admin", "admin"},
		// Asymmetric pairs both directions: the weaker wins regardless of order.
		{"read_write", "read", "write", "read"},
		{"write_read", "write", "read", "read"},
		{"write_admin", "write", "admin", "write"},
		{"admin_write", "admin", "write", "write"},
		{"read_admin", "read", "admin", "read"},
		{"admin_read", "admin", "read", "read"},
		// Fail closed: an unknown level on either side collapses to read.
		{"unknown_a_fails_closed", "superuser", "admin", "read"},
		{"unknown_b_fails_closed", "admin", "superuser", "read"},
		{"both_unknown_fails_closed", "", "", "read"},
		{"empty_vs_write", "", "write", "read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := minLevel(tc.a, tc.b); got != tc.want {
				t.Fatalf("minLevel(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// clampGitHubPermissions is the no-widening enforcement point. The matrix below
// asserts the full contract: requested access is intersected DOWN to the
// ceiling, out-of-ceiling keys are dropped (fail closed), narrowing is honored,
// and metadata:read is always present so clone/fetch keeps working.
func TestClampGitHubPermissions_NoWideningMatrix(t *testing.T) {
	tests := []struct {
		name      string
		requested map[string]string
		want      map[string]string
	}{
		{
			// Empty request: only the implicit metadata:read survives.
			name:      "empty_request_yields_metadata_only",
			requested: map[string]string{},
			want:      map[string]string{"metadata": "read"},
		},
		{
			// admin contents is the classic over-claim: clamp DOWN to write.
			name:      "admin_contents_clamped_to_write",
			requested: map[string]string{"contents": "admin"},
			want:      map[string]string{"metadata": "read", "contents": "write"},
		},
		{
			// Narrowing is allowed: a read request must stay read, never widen.
			name:      "read_contents_preserved_not_widened",
			requested: map[string]string{"contents": "read"},
			want:      map[string]string{"metadata": "read", "contents": "read"},
		},
		{
			// pull_requests admin is also clamped to the write ceiling.
			name:      "admin_pull_requests_clamped_to_write",
			requested: map[string]string{"pull_requests": "admin"},
			want:      map[string]string{"metadata": "read", "pull_requests": "write"},
		},
		{
			// Out-of-ceiling keys are dropped entirely (fail closed): a request
			// for administration/workflows/secrets cannot widen the grant.
			name: "out_of_ceiling_keys_dropped",
			requested: map[string]string{
				"administration": "admin",
				"workflows":      "write",
				"secrets":        "write",
			},
			want: map[string]string{"metadata": "read"},
		},
		{
			// A caller cannot widen metadata above read even by asking for write.
			name:      "metadata_cannot_be_widened_above_read",
			requested: map[string]string{"metadata": "write"},
			want:      map[string]string{"metadata": "read"},
		},
		{
			// Unknown access level on an in-ceiling key fails closed to read.
			name:      "unknown_level_fails_closed_to_read",
			requested: map[string]string{"contents": "superuser"},
			want:      map[string]string{"metadata": "read", "contents": "read"},
		},
		{
			// Mixed: in-ceiling clamp + out-of-ceiling drop in one request.
			name: "mixed_clamp_and_drop",
			requested: map[string]string{
				"contents":       "admin", // -> write
				"pull_requests":  "write", // -> write
				"administration": "admin", // -> dropped
			},
			want: map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clampGitHubPermissions(tc.requested)
			if len(got) != len(tc.want) {
				t.Fatalf("clamped = %v, want %v (size mismatch)", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("clamped[%q] = %q, want %q (full: %v)", k, got[k], v, got)
				}
			}
			// metadata:read must ALWAYS be present so clone/fetch works.
			if got["metadata"] != "read" {
				t.Fatalf("clamped is missing metadata:read: %v", got)
			}
		})
	}
}

// clampGitHubPermissions must never mutate the caller's input map. A clamp that
// edited the request in place could leak a widened value back to the caller.
func TestClampGitHubPermissions_DoesNotMutateInput(t *testing.T) {
	requested := map[string]string{"contents": "admin", "administration": "admin"}
	_ = clampGitHubPermissions(requested)
	if requested["contents"] != "admin" {
		t.Fatalf("input contents mutated to %q, want admin (clamp must not edit caller map)", requested["contents"])
	}
	if requested["administration"] != "admin" {
		t.Fatalf("input administration mutated to %q, want admin", requested["administration"])
	}
	if len(requested) != 2 {
		t.Fatalf("input map size changed to %d, want 2", len(requested))
	}
}

// ttlFor caps minted-credential lifetime at defaultMaxTTL: zero/negative/over-max
// all clamp to the cap, while a sub-cap value is honored exactly (narrowing).
func TestTTLFor_Edges(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want time.Duration
	}{
		{"zero_clamps_to_max", 0, defaultMaxTTL},
		{"negative_clamps_to_max", -1, defaultMaxTTL},
		{"over_max_clamps_to_max", 99999, defaultMaxTTL},
		{"exactly_max_preserved", int(defaultMaxTTL / time.Second), defaultMaxTTL},
		{"one_second_over_max_clamps", int(defaultMaxTTL/time.Second) + 1, defaultMaxTTL},
		{"one_second_under_max_honored", int(defaultMaxTTL/time.Second) - 1, defaultMaxTTL - time.Second},
		{"narrow_5m_honored", 300, 5 * time.Minute},
		{"narrow_1s_honored", 1, time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ttlFor(types.GrantSpec{TTLSeconds: tc.secs})
			if got != tc.want {
				t.Fatalf("ttlFor(%d) = %v, want %v", tc.secs, got, tc.want)
			}
			// Invariant: a minted TTL is never widened beyond the cap.
			if got > defaultMaxTTL {
				t.Fatalf("ttlFor(%d) = %v exceeds cap %v", tc.secs, got, defaultMaxTTL)
			}
		})
	}
}

// newJTI returns a fresh, opaque, 16-byte (32 hex char) credential id on every
// call. The audit/revocation join depends on these being unique per mint.
func TestNewJTI_UniqueAndOpaque(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		jti := newJTI()
		if len(jti) != 32 {
			t.Fatalf("jti %q has len %d, want 32 hex chars (16 bytes)", jti, len(jti))
		}
		if _, err := hex.DecodeString(jti); err != nil {
			t.Fatalf("jti %q is not valid hex: %v", jti, err)
		}
		if _, dup := seen[jti]; dup {
			t.Fatalf("newJTI returned a duplicate id %q after %d calls", jti, i)
		}
		seen[jti] = struct{}{}
	}
}

// spiffeForRun builds the canonical run SPIFFE id used as the audit actor. It
// must be stable for a given run and embed the run uuid under the expected
// trust-domain path so audit/attribution joins line up.
func TestSpiffeForRun(t *testing.T) {
	runID := uuid.New()
	got := spiffeForRun(runID)
	want := "spiffe://wardyn.local/agent-run/" + runID.String()
	if got != want {
		t.Fatalf("spiffeForRun = %q, want %q", got, want)
	}
	// Deterministic: same run id -> same SPIFFE id.
	if again := spiffeForRun(runID); again != got {
		t.Fatalf("spiffeForRun not deterministic: %q != %q", again, got)
	}
	// Distinct runs -> distinct ids.
	if other := spiffeForRun(uuid.New()); other == got {
		t.Fatalf("spiffeForRun collided across distinct runs: %q", other)
	}
}
