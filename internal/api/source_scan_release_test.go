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

// scanGrantDownStore fails every grant write: the clone grant a scan run needs.
type scanGrantDownStore struct{ *scanLaneStore }

func (scanGrantDownStore) CreateGrant(context.Context, types.CredentialGrant) (types.CredentialGrant, error) {
	return types.CredentialGrant{}, errors.New("store down")
}

// A scan whose clone grant cannot be written after its run row exists fails
// that PENDING run, instead of leaving it to hold the member's run slot and its
// run token until the undispatched sweep (#2277).
func TestSourceScan_GrantFailureFailsThePendingRun(t *testing.T) {
	srv, st, _ := govEscapeFixture(t, &capStore{})
	srv.cfg.Store = scanGrantDownStore{&scanLaneStore{govEscapeStore: st}}
	src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: govWorkspaceRepo}
	if _, err := srv.launchSourceScanRun(govMemberCtx([]string{"eng"}, false), "bob@corp.example", src, nil); err == nil {
		t.Fatal("the launch succeeded although the clone grant could not be written")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.states) != 1 {
		t.Fatalf("runs created = %d, want 1", len(st.states))
	}
	for id, state := range st.states {
		if state != types.RunFailed {
			t.Errorf("scan run %s is %s after its launch failed, want %s", id, state, types.RunFailed)
		}
	}
}
