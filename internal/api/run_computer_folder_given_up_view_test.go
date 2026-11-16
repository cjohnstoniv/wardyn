// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// givenUpViewStore serves one run whose computer folder is on a folder runner, seeded, both ways; its folder record
// read answers statusErr (nil: an empty record), and its give-up read (store.RunLocalFolderGivenUps) answers given
// or givenErr and is counted.
type givenUpViewStore struct {
	store.Store
	store.RunLocalFolderStatuses
	run        types.AgentRun
	runner     uuid.UUID
	statusErr  error
	given      store.FolderGivenUp
	givenErr   error
	givenReads int
}

func (s *givenUpViewStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

func (s *givenUpViewStore) RunLocalFolderSeed(context.Context, uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	at := time.Now().UTC()
	return &types.RunLocalFolder{Name: "work", RemoteDir: "/home/agent/work", RunnerID: &s.runner, Direction: types.ComputerFolderBoth},
		store.LocalFolderSeed{SeededAt: &at}, nil
}

func (s *givenUpViewStore) SetRunLocalFolderSeed(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

func (s *givenUpViewStore) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	return types.RunLocalFolderStatus{}, s.statusErr
}

func (s *givenUpViewStore) RunLocalFolderGivenUp(context.Context, uuid.UUID) (store.FolderGivenUp, error) {
	s.givenReads++
	return s.given, s.givenErr
}

func (s *givenUpViewStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

// A run whose folder record cannot be decoded still shows a give-up the record holds, read from the stored JSON
// alone: given up, with its count when one was kept, its sandbox copy as the run's ref says, and record_unreadable,
// as the run's given_up row says status_unreadable. A record not given up, or a give-up read that fails, shows no
// fate, and a live run none either. A record that reads, or a run with no record, has no flag and no give-up read.
// Catches: a record that never reads showing only the neutral line on the run page after its give-up.
func TestComputerFolderRunView_ShowsAGiveUpItsRecordCannotShow(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	three := 3
	undecodable := errors.New("store: unmarshal run local folder status: the record does not decode")
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for _, tc := range []struct {
		name              string
		state             types.RunState
		ref               string
		statusErr         error
		given             store.FolderGivenUp
		givenErr          error
		unreadable        bool
		reads             int
		fate, sandboxCopy string
		count             *int
	}{
		{"given up and counted, the sandbox deleted", types.RunCompleted, "", undecodable, store.FolderGivenUp{At: &at, Count: &three}, nil, true, 1, folderFateGivenUp, folderCopyDeleted, &three},
		{"given up, not counted, the sandbox kept", types.RunCompleted, "sbx", undecodable, store.FolderGivenUp{At: &at}, nil, true, 1, folderFateGivenUp, folderCopyKept, nil},
		{"not given up", types.RunCompleted, "sbx", undecodable, store.FolderGivenUp{}, nil, true, 1, "", "", nil},
		{"the give-up read fails", types.RunCompleted, "", undecodable, store.FolderGivenUp{}, errors.New("connection reset"), true, 1, "", "", nil},
		{"a live run", types.RunRunning, "sbx", undecodable, store.FolderGivenUp{At: &at, Count: &three}, nil, true, 1, "", "", nil},
		{"the record reads", types.RunCompleted, "", nil, store.FolderGivenUp{At: &at}, nil, false, 0, "", folderCopyDeleted, nil},
		{"no record", types.RunCompleted, "", store.ErrNotFound, store.FolderGivenUp{At: &at}, nil, false, 0, "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &givenUpViewStore{run: types.AgentRun{ID: uuid.New(), State: tc.state, SandboxRef: tc.ref, UpdatedAt: at},
				runner: uuid.New(), statusErr: tc.statusErr, given: tc.given, givenErr: tc.givenErr}
			s := &Server{cfg: Config{Store: st, Now: time.Now}}
			v := s.computerFolderRunView(context.Background(), st.run.ID)
			if v == nil {
				t.Fatal("no view for a run with a computer folder")
			}
			if v.RecordUnreadable != tc.unreadable || st.givenReads != tc.reads || str(v.Fate) != tc.fate ||
				str(v.SandboxCopy) != tc.sandboxCopy || v.GiveUpAt != nil || v.Back != nil {
				t.Fatalf("view %+v after %d give-up reads; want record_unreadable %v, %d reads, fate %q, sandbox copy %q, no give-up time, no pass",
					v, st.givenReads, tc.unreadable, tc.reads, tc.fate, tc.sandboxCopy)
			}
			r := v.LastReport
			switch {
			case tc.fate != folderFateGivenUp && r != nil:
				t.Errorf("last report %+v; want none", r)
			case tc.fate == folderFateGivenUp && (r == nil || !r.At.Equal(at) || (r.Unsynced == nil) != (tc.count == nil) ||
				(r.Unsynced != nil && *r.Unsynced != *tc.count)):
				t.Errorf("last report %+v; want the give-up's time and count %v", r, tc.count)
			}
			b, err := json.Marshal(v)
			if err != nil || strings.Contains(string(b), `"record_unreadable":true`) != tc.unreadable {
				t.Errorf("view JSON %s (%v); want record_unreadable %v on the wire", b, err, tc.unreadable)
			}
		})
	}
}
