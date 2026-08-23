// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestConfigureHoldOverridesAndDefaults pins D8 through the REAL seam: a
// policy's FirstUseHoldSeconds/MaxHolds reach the running proxy's
// approvalClient via NewServer -> configureHold, not via a direct call to
// configureHold itself (that only proves configureHold works, not that
// production ever feeds it the policy's values — see NewServer in proxy.go,
// which builds the approvalClient and plumbs cfg.Policy into it).
func TestConfigureHoldOverridesAndDefaults(t *testing.T) {
	newSrv := func(t *testing.T, holdSeconds, maxHolds int) *Server {
		t.Helper()
		cfg := &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://127.0.0.1:1", // never dialed by this test
			RunToken:        "token",
			Listen:          "127.0.0.1:0",
			Policy: types.RunPolicySpec{
				AllowedDomains:      []string{"example.com"},
				FirstUseApproval:    types.FirstUseWaitForReview,
				FirstUseHoldSeconds: holdSeconds,
				MaxHolds:            maxHolds,
			},
		}
		if err := cfg.applyDefaultsAndValidate(); err != nil {
			t.Fatalf("config: %v", err)
		}
		srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		return srv
	}

	// 0/absent: NewServer's plumbing keeps configureHold's built-in 30s/16.
	def := newSrv(t, 0, 0)
	if got := def.proxy.approval.holdTimeout; got != defaultHoldTimeout {
		t.Fatalf("default holdTimeout = %v, want %v", got, defaultHoldTimeout)
	}
	if got := cap(def.proxy.approval.holdSem); got != defaultMaxHolds {
		t.Fatalf("default holdSem cap = %d, want %d", got, defaultMaxHolds)
	}

	// A non-default policy value reaches the running approvalClient.
	over := newSrv(t, 90, 4)
	if got := over.proxy.approval.holdTimeout; got != 90*time.Second {
		t.Fatalf("override holdTimeout = %v, want 90s", got)
	}
	if got := cap(over.proxy.approval.holdSem); got != 4 {
		t.Fatalf("override holdSem cap = %d, want 4", got)
	}
}
