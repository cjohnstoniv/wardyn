// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leaseReleaseStore is dispatchTestStore plus the uncreated-run lease release.
type leaseReleaseStore struct {
	*dispatchTestStore
	released atomic.Int32
}

func (s *leaseReleaseStore) ReleaseUncreatedRunWatcher(context.Context, uuid.UUID) error {
	s.released.Add(1)
	return nil
}

// A dispatch that returns with no sandbox created releases the run's lease owner, so a
// member-drive delete does not read it as a create in flight; one that created a sandbox does not.
func TestDispatch_ReleasesTheLeaseOwnerOnlyWhenNoSandboxWasCreated(t *testing.T) {
	for name, tc := range map[string]struct {
		createErr error
		want      int32
	}{
		"create failed":   {errors.New("image pull: manifest unknown"), 1},
		"sandbox created": {nil, 0},
	} {
		srv, st, _, run := dispatchTeardownFixture(t, &fakeRunner{createErr: tc.createErr}, types.RunPending)
		run.Task = ""
		rel := &leaseReleaseStore{dispatchTestStore: st}
		srv.cfg.Store = rel
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest",
		})
		if got := rel.released.Load(); got != tc.want {
			t.Errorf("%s: lease releases = %d, want %d", name, got, tc.want)
		}
	}
}
