// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretsPresentAuditScan bounds the run's audit rows the gatherer reads. A
// run with this many or more cannot be proven free of a resident component or
// AWS session, so it counts as unreadable.
const secretsPresentAuditScan = 2000

// errTakeoverUnrecorded: nothing records a "Needs you" takeover yet.
var errTakeoverUnrecorded = errors.New("takeovers are not recorded yet")

// takeoverHappened reports whether a "Needs you" takeover was ever taken on
// the run. SEAM for DSK-B (#2963): its takeover holder replaces this body.
// Until then it cannot answer, so it errs and secretsPresent fails closed
// (Unknown): a stub that answered false would let a snapshot through.
var takeoverHappened = func(context.Context, *Server, uuid.UUID) (bool, error) {
	return false, errTakeoverUnrecorded
}

// secretsPresent gathers the facts secretspresent.Evaluate reads for one run.
// Callers in 0.10.0 are tests; a snapshot or fork calls it before it copies a
// sandbox and refuses when present. Every read error fails closed.
func (s *Server) secretsPresent(ctx context.Context, runID uuid.UUID) (bool, []secretspresent.Reason) {
	var in secretspresent.Inputs
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		in.Unreadable = true
	}
	for _, g := range grants {
		switch g.Spec.Kind {
		case types.GrantEnvSecret, types.GrantFileSecret, types.GrantSSHKey:
			in.ResidentGrants++
		}
	}
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, secretsPresentAuditScan)
	if err != nil || len(events) >= secretsPresentAuditScan {
		in.Unreadable = true
	}
	for _, ev := range events {
		if residentByAudit(ev) {
			in.ResidentGrants++
		}
	}
	if in.TakeoverHappened, err = takeoverHappened(ctx, s, runID); err != nil {
		in.Unreadable = true
	}
	if st, ok := s.cfg.Store.(store.DesktopSigninStore); ok {
		if in.SigninBegun, err = st.RunHadDesktopSignin(ctx, runID); err != nil {
			in.Unreadable = true
		}
	}
	return secretspresent.Evaluate(in)
}

// residentByAudit reports whether one audit row records a credential that
// lives in the sandbox: a Bedrock AWS SSO session (its role credentials are
// resident in every mode but bearer), or a component that delivers a secret
// as an environment variable or file, or an organisation's custom component,
// which may be an AWS CLI whose role credentials are resident (its row does
// not say, so it counts).
func residentByAudit(ev types.AuditEvent) bool {
	if ev.Outcome != "success" {
		return false
	}
	switch canonicalAction(ev.Action) {
	case "run.bedrock.configure":
		var d struct {
			Mode string `json:"mode"`
		}
		return json.Unmarshal(ev.Data, &d) != nil || d.Mode != "bearer"
	case "run.component.attach":
		var d struct {
			Kind    string `json:"kind"`
			Source  string `json:"source"`
			Secrets []struct {
				Delivery string `json:"delivery"`
			} `json:"secrets"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			return true
		}
		if d.Kind == string(types.ComponentCustom) && d.Source == componentSourceOrg {
			return true
		}
		for _, sec := range d.Secrets {
			if sec.Delivery == types.ComponentDeliveryEnv || sec.Delivery == types.ComponentDeliveryFile {
				return true
			}
		}
	}
	return false
}
