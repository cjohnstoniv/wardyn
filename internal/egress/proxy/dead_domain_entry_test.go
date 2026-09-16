// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBootWarnsOnceForEachDeadDomainEntry (W6-S6) pins the runtime signal for a
// policy entry that can never match.
//
// ValidDomainEntry runs at the API WRITE doors only, so it refuses a bad entry
// on the way in and says nothing about one that is already stored. A
// `denied_domains` entry written before 0.7.4 with a non-ASCII host therefore
// compiles silently and the deny FAILS OPEN — the request reaches the punycode
// form, which no compiled entry spells. That is the one direction where a dead
// entry is not merely useless.
//
// A warning, never a refusal: failing closed on a legacy entry would take every
// run on the estate down on upgrade day, which is a worse outcome than the deny
// this is reporting. The policy still compiles byte-identically.
func TestBootWarnsOnceForEachDeadDomainEntry(t *testing.T) {
	newSrv := func(t *testing.T, spec types.RunPolicySpec) string {
		t.Helper()
		cfg := &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://127.0.0.1:1", // never dialed by this test
			RunToken:        "token",
			Listen:          "127.0.0.1:0",
			Policy:          spec,
		}
		if err := cfg.applyDefaultsAndValidate(); err != nil {
			t.Fatalf("config: %v", err)
		}
		var logged bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
		defer slog.SetDefault(prev)

		srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		return logged.String()
	}

	t.Run("a dead deny entry is named, and the policy still compiles", func(t *testing.T) {
		got := newSrv(t, types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"ëxample.com"}})
		if n := strings.Count(got, deadDomainEntryWarning); n != 1 {
			t.Fatalf("boot logged the dead-entry warning %d time(s), want exactly 1:\n%s", n, got)
		}
		if !strings.Contains(got, "ëxample.com") {
			t.Errorf("the warning does not name the entry it is about:\n%s", got)
		}
		if !strings.Contains(got, "denied_domains") {
			t.Errorf("the warning does not say WHICH list the entry is on — an allow-side dead entry is "+
				"merely useless, a deny-side one fails open:\n%s", got)
		}
	})

	// NEGATIVE CONTROL: a well-formed policy — including the port-qualified and
	// wildcard forms — logs nothing new at all.
	t.Run("a well-formed policy logs nothing", func(t *testing.T) {
		got := newSrv(t, types.RunPolicySpec{
			AllowedDomains: []string{"example.com", "*.example.com", "m.corp:443"},
			DeniedDomains:  []string{"blocked.example.com", "*.blocked.example.com:8443"},
		})
		if strings.Contains(got, deadDomainEntryWarning) {
			t.Errorf("a well-formed policy logged a dead-entry warning:\n%s", got)
		}
	})
}
