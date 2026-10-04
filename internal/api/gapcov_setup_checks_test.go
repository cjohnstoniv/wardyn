// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

func TestGapCovKeyCustodyChecksSingularWording(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	rows := keyCustodyChecks(keyCustody{
		Domains: []string{"finance"},
		Changes: keydomain.Changes{Count: 1, Latest: now.Add(-30 * time.Hour), LatestBy: "admin@example.test"},
		Now:     now,
	})
	byID := map[string]SetupCheck{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if c := byID["key_domains"]; c.Status != "ok" || c.Detail != "1 key domain, proven at boot: finance." {
		t.Errorf("one domain: %+v", c)
	}
	want := "1 key-domain assignment change in the last 30 days, the latest 1 day ago by admin@example.test. Each one moves where that person's next keys are made."
	if c := byID["key_domain_changes"]; c.Status != "warn" || c.Detail != want {
		t.Errorf("one change a day and a half ago: %+v", c)
	}
}

// When Postgres does not answer the key-custody reads, the key-custody rows are left out, not shown
// from a guess.
func TestGapCovKeyCustodyRowsAreOmittedWhenTheCountCannotBeRead(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	s := &Server{cfg: Config{
		KeyDomains:    keydomain.NewService(pool, []string{"finance"}),
		PrincipalKeys: true,
		Now:           func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) },
	}}
	if rows := s.keyCustodyRows(t.Context()); rows != nil {
		t.Fatalf("keyCustodyRows = %+v, want none when Postgres does not answer", rows)
	}
}
