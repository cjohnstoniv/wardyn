// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A host-scoped secret that went in as its value, read back from the run's run.secret_placeholder.resolve rows
// (#3741), so the policy view says which and why. The newest row per secret decides. A revive places nothing (it
// reuses the stored proxy config), so a failure stays for the run's life. The rows are the record: nothing is stored
// twice.

// secretsAsValueMax is how many entries the policy view lists; secrets_as_value_truncated says more matched.
const secretsAsValueMax = 50

type runPolicySecretAsValue struct {
	Kind       string `json:"kind"`                  // env_secret | file_secret
	SecretName string `json:"secret_name,omitempty"` // withheld below the security admin tier
	Host       string `json:"host"`
	Cause      string `json:"cause,omitempty"`    // a placeholderCause*; absent on a row written before causes
	Reason     string `json:"reason"`             // the row's sentence
	Delivery   string `json:"delivery,omitempty"` // runner_resident for a value the runner got
}

// placeholderRowData is the data of a run.secret_placeholder.resolve row.
type placeholderRowData struct {
	Kind       string `json:"kind"`
	SecretName string `json:"secret_name"`
	Host       string `json:"host"`
	Reason     string `json:"reason"`
	Cause      string `json:"cause"`
	Delivery   string `json:"delivery"`
}

// fillSecretsAsValue sets the policy view's list. The view is read-only and never fails for it: on an error it logs
// and leaves the field empty.
func (s *Server) fillSecretsAsValue(ctx context.Context, runID uuid.UUID, tier specReadTier, resp *runPolicyResponse) {
	var err error
	if resp.SecretsAsValue, resp.SecretsAsValueTruncated, err = s.runSecretsAsValue(ctx, runID, tier); err != nil {
		slog.WarnContext(ctx, "api: run policy view without the secrets that went in as their value", "run_id", runID, "err", err)
	}
}

// runSecretsAsValue is each (kind, secret, host) whose newest resolve row is a failure, at most secretsAsValueMax;
// truncated is true when more matched.
func (s *Server) runSecretsAsValue(ctx context.Context, runID uuid.UUID, tier specReadTier) (rows []runPolicySecretAsValue, truncated bool, err error) {
	f := store.AuditFilter{Action: "run.secret_placeholder.resolve"}
	var evs []types.AuditEvent
	if p, ok := s.cfg.Store.(store.Pager); ok {
		evs, err = p.QueryAuditEventsFilteredPage(ctx, &runID, f, store.Page{Limit: 200, Newest: true})
	} else {
		var all []types.AuditEvent
		all, err = s.cfg.Store.QueryAuditEvents(ctx, runID, 0)
		evs = f.Keep(all)
	}
	if err != nil {
		return nil, false, err
	}
	// QueryAuditEvents is oldest first, so sort in both paths: the newest row of a secret decides.
	slices.SortStableFunc(evs, func(a, b types.AuditEvent) int { return b.Time.Compare(a.Time) })
	type key struct{ kind, name, host string }
	seen := map[key]bool{}
	for _, ev := range evs {
		var d placeholderRowData
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		k := key{d.Kind, d.SecretName, d.Host}
		if seen[k] {
			continue
		}
		seen[k] = true
		if ev.Outcome == "success" {
			continue
		}
		cause := d.Cause
		if cause == "" {
			for c, sentence := range placeholderCauseSentence {
				if sentence == d.Reason {
					cause = c
				}
			}
		}
		row := runPolicySecretAsValue{Kind: d.Kind, Host: d.Host, Cause: cause, Reason: d.Reason, Delivery: d.Delivery}
		if tier != specReadMember {
			row.SecretName = d.SecretName
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b runPolicySecretAsValue) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Host, b.Host), cmp.Compare(a.SecretName, b.SecretName))
	})
	if len(rows) > secretsAsValueMax {
		rows, truncated = rows[:secretsAsValueMax], true
	}
	return rows, truncated, nil
}
