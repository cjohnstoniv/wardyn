// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The facts secretspresent.Evaluate reads for one run, all from durable state: the run's grant rows, its
// run_components rows, its human_takeover approvals, its accepted support grants, its desktop sign-ins, its own
// task, its run_dispatch_secret_facts row, the credential.mint rows its broker wrote in each mint's own
// transaction, and the audit retention anchors. A best-effort audit row and server config read now are never a
// source: either can disagree with what the run was actually given.

// residentGrantKinds are the grant kinds whose credential lives inside the sandbox for the whole run.
var residentGrantKinds = map[types.GrantKind]bool{
	types.GrantEnvSecret: true, types.GrantFileSecret: true, types.GrantSSHKey: true,
}

// brokeredGrantKinds are the grant kinds minted per use, each with the longest time a minted credential of that
// kind stays valid: the broker caps every minted credential at an hour (broker.ttlFor). A git_pat grant's token
// lives in the sandbox only when the broker was off, which is the dispatch row's fact, not the grant's. A grant
// kind in neither map counts as resident, and a mint of a kind not here counts as valid for as long as the run
// lives.
var brokeredGrantKinds = map[types.GrantKind]time.Duration{
	types.GrantGitHubToken: time.Hour, types.GrantCloudSTS: time.Hour, types.GrantAPIKey: time.Hour, types.GrantGitPAT: time.Hour,
}

// componentDeliveryResident says, for each delivery mode of a component secret, whether the secret lives inside
// the sandbox. A mode not here counts as resident.
var componentDeliveryResident = map[string]bool{
	types.ComponentDeliveryHeader: false, types.ComponentDeliveryEnv: true, types.ComponentDeliveryFile: true,
}

// secretsPresent gathers the facts for runID and evaluates the rule. w is R4's window for brokered mints. Every
// fact that cannot be read, and a dispatch row that does not name the run's current token, answers Unknown.
func (s *Server) secretsPresent(ctx context.Context, runID uuid.UUID, w mintWindow) (bool, []secretspresent.Reason) {
	st, ok := s.cfg.Store.(store.RunSecretFacts)
	if !ok {
		return secretspresent.Evaluate(secretspresent.Inputs{Unreadable: true})
	}
	var upTo *time.Time
	if !w.to.IsZero() {
		upTo = &w.to
	}
	f, err := st.ReadRunSecretFacts(ctx, runID, harnessLoginTask, upTo)
	if err != nil {
		slog.ErrorContext(ctx, "api: read the run's secret facts", slog.String("run_id", runID.String()), slog.Any("err", err))
		return secretspresent.Evaluate(secretspresent.Inputs{Unreadable: true})
	}
	in := secretsInputs(f, w)
	if f.Dispatch != nil {
		jti, err := s.currentRunJTI(ctx, runID)
		if err != nil {
			slog.WarnContext(ctx, "api: read the run's current token", slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		in.Unreadable = in.Unreadable || err != nil || jti == "" || jti != f.Dispatch.RecordedJTI
	}
	return secretspresent.Evaluate(in)
}

// secretsInputs turns one run's facts into the rule's inputs. A run with no dispatch row (dispatched while the
// switch was off), and a run some of whose audit rows a retention drop removed, are unreadable.
func secretsInputs(f store.SecretFacts, w mintWindow) secretspresent.Inputs {
	in := secretspresent.Inputs{
		TakeoverHappened: f.TakeoverGiven,
		SigninBegun:      f.SigninBegun,
		HarnessLogin:     f.HarnessLogin,
		SupportSession:   f.SupportSessionAccepted,
		Unreadable:       f.Dispatch == nil || f.AuditRowsDropped,
	}
	for kind, n := range f.GrantKinds {
		if _, brokered := brokeredGrantKinds[types.GrantKind(kind)]; residentGrantKinds[types.GrantKind(kind)] || !brokered {
			in.ResidentGrants += n
		}
	}
	for _, c := range f.Components {
		if c.Org || c.Cleared || slices.ContainsFunc(c.Deliveries, func(mode string) bool {
			resident, known := componentDeliveryResident[mode]
			return resident || !known
		}) {
			in.ResidentGrants++
		}
	}
	if f.Dispatch != nil {
		if f.Dispatch.BedrockResident {
			in.ResidentGrants++
		}
		in.GitTokenResident = f.Dispatch.GitTokenResident
	}
	from := w.from
	if from.IsZero() {
		from = f.Now
	}
	for kind, at := range f.NewestMints {
		ttl, known := brokeredGrantKinds[types.GrantKind(kind)]
		in.RecentCredential = in.RecentCredential || !known || at.Add(ttl).After(from)
	}
	return in
}
