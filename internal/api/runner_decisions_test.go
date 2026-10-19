// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// spoolIngestStub answers IngestRunnerDecision with a fixed first-time verdict.
type spoolIngestStub struct {
	store.Store
	first bool
}

// GetSiteConfig fails the read: countReauthTimeout then counts the hold, as it does with no site config.
func (spoolIngestStub) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("no site config in this stub")
}

func (s spoolIngestStub) IngestRunnerDecision(context.Context, uuid.UUID, runnerspool.Record, func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	return s.first, nil
}

// A spooled deny moves wardyn_egress_denies_total once, as the direct path does, and a replayed
// row (not first-time) moves nothing; a non-policy deny source is not counted either.
func TestDecisionIngestFromRunner_RunsPostAuditHooksOnFirstRowOnly(t *testing.T) {
	runID := uuid.New()
	deny := func(rule string) runnerwire.Event {
		var d egress.DecisionLog
		d.Decision, d.RuleSource, d.Request.RunID = egress.Deny, rule, runID
		b, err := json.Marshal(runnerspool.Record{Decision: d})
		if err != nil {
			t.Fatal(err)
		}
		return runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: b}
	}
	for _, c := range []struct {
		name  string
		first bool
		rule  string
		want  int64
	}{
		{"first policy deny", true, "policy", 1},
		{"replayed row", false, "policy", 0},
		{"reauth timeout is not a policy deny", true, ruleSourceCredentialReauthTimeout, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := &Server{cfg: Config{
				Store:              spoolIngestStub{first: c.first},
				RunnerAuditPrepare: func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil },
			}}
			if err := srv.DecisionIngestFromRunner(context.Background(), uuid.New(), deny(c.rule)); err != nil {
				t.Fatal(err)
			}
			if got := srv.metrics.egressDenies; got != c.want {
				t.Errorf("egressDenies = %d, want %d", got, c.want)
			}
		})
	}
}
