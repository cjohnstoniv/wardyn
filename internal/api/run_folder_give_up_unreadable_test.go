// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unreadFolderStore is keptFolderStore with a folder record that cannot be read (unreadable) or answers record, a run
// the database answers is inside or past its keep (past), the run's folder on a folder runner, seeded, both ways or
// only copied in (copiedIn), and a stored record that, read without decoding it, holds a resume (resumeHeld) or
// fails that read (resumeErr). A give-up statement applies past the keep and makes the record final, answers Done
// once it is final, and otherwise applies nothing. Every read and statement fails on a context that is done, and
// only a statement made on a live one is recorded (giveUps). A record read hangs until its context ends while hang
// answers true (hung counts those).
type unreadFolderStore struct {
	*keptFolderStore
	fmu          sync.Mutex
	unreadable   bool
	record       types.RunLocalFolderStatus
	past         bool
	copiedIn     bool
	resumeHeld   bool
	resumeErr    error
	final        bool
	folderRunner uuid.UUID
	giveUps      []store.GiveUpParams
	hang         func() bool
	hung         atomic.Int32
}

func (s *unreadFolderStore) RunLocalFolderStatus(ctx context.Context, _ uuid.UUID) (types.RunLocalFolderStatus, error) {
	if s.hang != nil && s.hang() {
		s.hung.Add(1)
		<-ctx.Done()
		return types.RunLocalFolderStatus{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return types.RunLocalFolderStatus{}, err
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if s.unreadable {
		return types.RunLocalFolderStatus{}, errors.New("the record does not decode")
	}
	return s.record, nil
}

func (s *unreadFolderStore) RunLocalFolderHoldsResume(ctx context.Context, _ uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return s.resumeHeld, s.resumeErr
}

func (s *unreadFolderStore) GiveUpRunLocalFolder(ctx context.Context, _ uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	if err := ctx.Err(); err != nil {
		return store.GiveUpOutcome{}, err
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.giveUps = append(s.giveUps, p)
	switch {
	case s.final:
		return store.GiveUpOutcome{Done: true}, nil
	case s.past:
		s.final = true
		return store.GiveUpOutcome{Applied: true, Gen: 4}, nil
	}
	return store.GiveUpOutcome{}, nil
}

func (s *unreadFolderStore) RunDueFlags(ctx context.Context, _ uuid.UUID, _ time.Duration) (store.RunDue, error) {
	if err := ctx.Err(); err != nil {
		return store.RunDue{}, err
	}
	return store.RunDue{Due: true, CapPassed: s.past}, nil
}

func (s *unreadFolderStore) RunLocalFolderSeed(ctx context.Context, _ uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	if err := ctx.Err(); err != nil {
		return nil, store.LocalFolderSeed{}, err
	}
	dir, at := types.ComputerFolderBoth, time.Now().UTC()
	if s.copiedIn {
		dir = types.ComputerFolderIn
	}
	return &types.RunLocalFolder{Name: "work", Direction: dir, RunnerID: &s.folderRunner}, store.LocalFolderSeed{SeededAt: &at}, nil
}

func (s *unreadFolderStore) SetRunLocalFolderSeed(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

func (s *unreadFolderStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

func (s *unreadFolderStore) MarkGivenUpAudited(context.Context, uuid.UUID, int) (bool, error) {
	return true, nil
}

// liveGiveUps is the statements made on a live context so far.
func (s *unreadFolderStore) liveGiveUps() []store.GiveUpParams {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return slices.Clone(s.giveUps)
}

// unreadFolderServer is a server over an unreadFolderStore holding one terminal run in state, on the server's
// Docker with its sandbox "sbx", and rn as its runner.
func unreadFolderServer(t *testing.T, state types.RunState, rn runner.Runner) (*Server, *unreadFolderStore, *recRecorder, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
		ConfinementClass: types.CC1, State: state, RunnerTarget: "docker", SandboxRef: "sbx"}
	st := &unreadFolderStore{keptFolderStore: &keptFolderStore{orphanSweepStore: &orphanSweepStore{runs: []types.AgentRun{run}}},
		folderRunner: uuid.New()}
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Audit = audit
	return testServer(t, cfg), st, audit, run
}

// unreadRows counts run's given_up rows that say its record could not be read.
func unreadRows(audit *recRecorder, run types.AgentRun) int {
	return endGiveUpRows(audit, run.ID, `"status_unreadable":true`)
}

// A stopped sandbox's folder record that cannot be read is decided without decoding it, at once and whatever the
// keep. A run that owes nothing (a killed run whose stored record holds no resume, a folder copied in only) is final
// with no give-up and no row. Any other (a run the record-free checks say is owed, a killed run whose stored record
// holds a resume, a killed run whose resume read fails) has its copy back given up with the keep as its cap, which the
// database applies only past the keep, with its given_up row. Catches: a killed run's resume dropped with no
// given_up row because its record does not decode; an unreadable record of a run that owes nothing left not final
// inside the keep, so its caller tries it again (the forced retry stopping the sandbox on every tick); and an owed
// copy back given up before its keep has run out.
func TestGiveUpFolderStopped_AnUnreadableRecordIsDecidedWithoutDecodingIt(t *testing.T) {
	readFails := errors.New("the database did not answer")
	for _, tc := range []struct {
		name                 string
		state                types.RunState
		past, copiedIn, held bool
		heldErr              error
		giveUps, rows        int
		final                bool
	}{
		{"a killed run holding no resume, inside its keep", types.RunKilled, false, false, false, nil, 0, 0, true},
		{"a killed run holding no resume, past its keep", types.RunKilled, true, false, false, nil, 0, 0, true},
		{"a killed run holding a resume, inside its keep", types.RunKilled, false, false, true, nil, 1, 0, false},
		{"a killed run holding a resume, past its keep", types.RunKilled, true, false, true, nil, 1, 1, true},
		{"a killed run whose resume read fails, inside its keep", types.RunKilled, false, false, false, readFails, 1, 0, false},
		{"a killed run whose resume read fails, past its keep", types.RunKilled, true, false, false, readFails, 1, 1, true},
		{"a completed run inside its keep", types.RunCompleted, false, false, false, nil, 1, 0, false},
		{"a completed run past its keep", types.RunCompleted, true, false, false, nil, 1, 1, true},
		{"a folder copied in only, past its keep", types.RunCompleted, true, true, false, nil, 0, 0, true},
		{"a folder copied in only, inside its keep", types.RunCompleted, false, true, false, nil, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, audit, run := unreadFolderServer(t, tc.state, &orphanSweepRunner{fakeRunner: &fakeRunner{}})
			st.unreadable, st.past, st.copiedIn, st.resumeHeld, st.resumeErr = true, tc.past, tc.copiedIn, tc.held, tc.heldErr
			final, err := s.giveUpFolderStopped(context.Background(), run)
			gs := st.liveGiveUps()
			if final != tc.final || (err == nil) != tc.final || len(gs) != tc.giveUps || unreadRows(audit, run) != tc.rows {
				t.Fatalf("final %v, err %v, %d give-ups, %d rows; want final %v with no error only then, %d give-ups, %d rows",
					final, err, len(gs), unreadRows(audit, run), tc.final, tc.giveUps, tc.rows)
			}
			for _, p := range gs {
				if p.Cap != s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, folderStatusUnreadable) {
					t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and the unreadable record noted", p)
				}
			}
		})
	}
}
