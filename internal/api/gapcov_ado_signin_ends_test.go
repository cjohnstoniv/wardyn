// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gapCovEndStore is a store that keeps the shared sign-in end counter, recording what it was asked
// and answering as told.
type gapCovEndStore struct {
	store.Store
	state                       store.ADOSignInEndState
	beginErr, finishErr, readEr error

	begun    [][2]string
	finished []string
}

func (s *gapCovEndStore) BeginADOSignInEnd(_ context.Context, owner, reason string) error {
	s.begun = append(s.begun, [2]string{owner, reason})
	return s.beginErr
}

func (s *gapCovEndStore) FinishADOSignInEnd(_ context.Context, owner string) error {
	s.finished = append(s.finished, owner)
	return s.finishErr
}

func (s *gapCovEndStore) ADOSignInEnds(context.Context, string) (store.ADOSignInEndState, error) {
	return s.state, s.readEr
}

func TestGapCovBeginADOSignInEndRecordsTheStartAndTheFinish(t *testing.T) {
	st := &gapCovEndStore{}
	s := &Server{cfg: Config{Store: st}}

	finish, err := s.beginADOSignInEnd(t.Context(), "alice", "signed_out")
	if err != nil || finish == nil {
		t.Fatalf("begin = %v, %v; want a finish func", finish != nil, err)
	}
	if len(st.begun) != 1 || st.begun[0] != [2]string{"alice", "signed_out"} || len(st.finished) != 0 {
		t.Fatalf("after begin: begun %v finished %v, want only the start", st.begun, st.finished)
	}
	finish()
	if len(st.finished) != 1 || st.finished[0] != "alice" {
		t.Fatalf("after finish: finished %v, want alice", st.finished)
	}
}

// An end whose start cannot be recorded does not go ahead, and no finish is owed.
func TestGapCovBeginADOSignInEndFailureStopsTheEnd(t *testing.T) {
	boom := errors.New("gapcov: begin refused")
	st := &gapCovEndStore{beginErr: boom}
	s := &Server{cfg: Config{Store: st}}

	finish, err := s.beginADOSignInEnd(t.Context(), "alice", "signed_out")
	if !errors.Is(err, boom) || finish != nil {
		t.Fatalf("begin = finish %v, err %v; want the store's error and no finish func", finish != nil, err)
	}
	if len(st.finished) != 0 {
		t.Errorf("finished %v after a start that failed", st.finished)
	}
}

// A finish that cannot be recorded is logged and does not panic or return an error: the end reads as
// running until it goes stale.
func TestGapCovFinishADOSignInEndFailureIsLogged(t *testing.T) {
	st := &gapCovEndStore{finishErr: errors.New("gapcov: finish refused")}
	s := &Server{cfg: Config{Store: st}}
	logs := miscCovCaptureLogs(t)

	finish, err := s.beginADOSignInEnd(t.Context(), "alice", "signed_out")
	if err != nil {
		t.Fatal(err)
	}
	finish()
	if _, ok := logs.find("an Azure DevOps sign-in end was not marked finished"); !ok {
		t.Error("the failed finish was not logged")
	}
	if len(st.finished) != 1 {
		t.Errorf("finish attempted %d time(s), want once", len(st.finished))
	}
}

func TestGapCovReadADOSignInEndsReturnsTheSharedCount(t *testing.T) {
	boom := errors.New("gapcov: read refused")
	s := &Server{cfg: Config{Store: &gapCovEndStore{state: store.ADOSignInEndState{Gen: 7}}}}
	if n, err := s.readADOSignInEnds(t.Context(), "alice"); err != nil || n != 7 {
		t.Fatalf("read = %d, %v; want 7", n, err)
	}
	s = &Server{cfg: Config{Store: &gapCovEndStore{readEr: boom}}}
	if _, err := s.readADOSignInEnds(t.Context(), "alice"); !errors.Is(err, boom) {
		t.Fatalf("read err = %v, want the store's", err)
	}
}

// A mint asks whether an end began since it read the count: nothing happened and the count is
// unchanged is "no"; a changed count or a running end is "yes" with that end's reason; a failed read
// is an error, not a "no".
func TestGapCovADOSignInEndedSince(t *testing.T) {
	boom := errors.New("gapcov: read refused")
	for _, tc := range []struct {
		name       string
		st         *gapCovEndStore
		gen        uint64
		wantReason string
		wantEnded  bool
		wantErr    error
	}{
		{"count unchanged and nothing running", &gapCovEndStore{state: store.ADOSignInEndState{Gen: 3}}, 3, "", false, nil},
		{"count moved", &gapCovEndStore{state: store.ADOSignInEndState{Gen: 4, Reason: "signed_out"}}, 3, "signed_out", true, nil},
		{"an end is running at the same count", &gapCovEndStore{state: store.ADOSignInEndState{Gen: 3, Running: true, Reason: "revoked"}}, 3, "revoked", true, nil},
		{"the read fails", &gapCovEndStore{readEr: boom}, 3, "", false, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: tc.st}}
			reason, ended, err := s.adoSignInEndedSince(t.Context(), "alice", tc.gen)
			if reason != tc.wantReason || ended != tc.wantEnded || !errors.Is(err, tc.wantErr) {
				t.Fatalf("= %q, %v, %v; want %q, %v, %v", reason, ended, err, tc.wantReason, tc.wantEnded, tc.wantErr)
			}
		})
	}
}
