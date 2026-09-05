// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// quotaRecordStore is ceilingRecordStore plus CountActiveRunsBy, the read the
// MaxConcurrentRuns limit makes. Real rather than a 0 stub, so the quota arm
// below exercises the comparison instead of a canned answer.
type quotaRecordStore struct {
	ceilingRecordStore
	active int
}

func (s quotaRecordStore) CountActiveRunsBy(context.Context, string) (int, error) {
	return s.active, nil
}

// TestRecordRunHonorsCeilingLimits is F153.
//
// launchRecordRun resolved the FULL governanceCeiling and applied only its deny
// axis. The Limits axis was read at POST /runs and nowhere else, so a profile
// setting deny_interactive or max_concurrent_runs bound a member's ordinary run
// and NOT the interactive, attachable, allow-all, credentialed session
// POST /workspaces/{id}/record opens for that same principal — while the code's
// own comment claimed "the same walls as their ordinary runs".
//
// The proof that this was live rather than theoretical is in the tree:
// TestLaunchRecordRun_ThreadsActingPrincipalCeiling launched a record run under
// govProfile("walled"), whose Limits carry DenyInteractive:true, and passed.
func TestRecordRunHonorsCeilingLimits(t *testing.T) {
	newSrv := func(t *testing.T, profile *types.GovernanceProfile, active int) (*Server, *fakeRunner) {
		t.Helper()
		h := newHarness(t)
		ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
		fr := &fakeRunner{}
		cfg := baseTestConfig(h, quotaRecordStore{
			ceilingRecordStore: ceilingRecordStore{recordLLMModeStore: newRecordLLMModeStore(ws), profile: profile},
			active:             active,
		})
		cfg.Runner = fr
		cfg.Broker = h.broker
		return New(cfg), fr
	}
	t.Run("deny_interactive refuses the record launch", func(t *testing.T) {
		p := govProfile("no-terminals")
		p.Limits = types.GovernanceLimits{DenyInteractive: true}
		srv, fr := newSrv(t, p, 0)
		_, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
			"walled@corp.example", types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false)
		if !errors.Is(err, errRecordCeilingLimit) {
			t.Fatalf("launchRecordRun err = %v, want a governance-limit refusal — a deny_interactive profile "+
				"must not get a server-authored, allow-all, attachable sandbox", err)
		}
		// A REFUSAL COSTS NO STATE: the check runs before the CAS claim, so
		// nothing was launched and nothing has to be aborted.
		if fr.lastSpec.RunID != uuid.Nil {
			t.Errorf("a refused record launch still reached the runner (run %s) — the limit must be applied "+
				"before the import-step claim, or a refusal leaves a claimed workspace behind", fr.lastSpec.RunID)
		}
	})

	t.Run("max_concurrent_runs refuses at the cap and admits under it", func(t *testing.T) {
		p := govProfile("two-at-a-time")
		p.Limits = types.GovernanceLimits{MaxConcurrentRuns: 2}

		srv, _ := newSrv(t, p, 2)
		if _, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
			"walled@corp.example", types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false); !errors.Is(err, errRecordCeilingLimit) {
			t.Fatalf("at the cap: err = %v, want a governance-limit refusal", err)
		}
		// The control that makes the arm above mean something: under the cap the
		// same profile launches normally, so the test is not simply asserting
		// that record is broken.
		srv, _ = newSrv(t, p, 1)
		if _, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
			"walled@corp.example", types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false); err != nil {
			t.Fatalf("under the cap: err = %v, want a normal launch", err)
		}
	})

	// An unassigned principal has no profile, so there is no limit to read and
	// no deployment-wide default to fall back on — Record Mode's moat workflow
	// is byte-for-byte what it was.
	t.Run("an unassigned principal is unaffected", func(t *testing.T) {
		srv, _ := newSrv(t, nil, 99)
		if _, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
			"alice@corp.example", types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false); err != nil {
			t.Fatalf("unassigned principal refused: %v", err)
		}
	})
}
