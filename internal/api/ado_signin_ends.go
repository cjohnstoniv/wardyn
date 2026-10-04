// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The sign-in end counter (ado_pat_console.go's adoSignInEnds), shared across replicas when the
// store keeps one (ado_signin_ends, migration 0121). A mint that redeemed a person's sign-in
// before it was ended revokes its own token, whichever replica ran the end: the begin and finish
// of an end are rows every replica reads. A store without the table counts in this process.

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// adoSignInEndFinishTimeout bounds the write that marks an end finished.
const adoSignInEndFinishTimeout = 10 * time.Second

func (s *Server) signInEndStore() store.ADOSignInEndStore {
	st, _ := s.cfg.Store.(store.ADOSignInEndStore)
	return st
}

// beginADOSignInEnd marks the start of an end of owner's sign-in; the returned func marks its
// finish and runs on every exit. An error means the start could not be recorded, and the end
// must not go ahead: a mint could not tell it from none.
func (s *Server) beginADOSignInEnd(ctx context.Context, owner, reason string) (func(), error) {
	st := s.signInEndStore()
	if st == nil || owner == "" {
		return s.adoSignInEnds.begin(owner, reason), nil
	}
	if err := st.BeginADOSignInEnd(ctx, owner, reason); err != nil {
		return nil, err
	}
	return func() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), adoSignInEndFinishTimeout)
		defer cancel()
		if err := st.FinishADOSignInEnd(fctx, owner); err != nil {
			// The end then reads as running until store.ADOSignInEndsStaleAfter, which only makes
			// a mint revoke its own token: the safe side.
			slog.WarnContext(ctx, "wardynd: an Azure DevOps sign-in end was not marked finished", slog.Any("err", err))
		}
	}, nil
}

// readADOSignInEnds is owner's end count now.
func (s *Server) readADOSignInEnds(ctx context.Context, owner string) (uint64, error) {
	st := s.signInEndStore()
	if st == nil || owner == "" {
		return s.adoSignInEnds.read(owner), nil
	}
	state, err := st.ADOSignInEnds(ctx, owner)
	return uint64(state.Gen), err
}

// adoSignInEndedSince reports whether an end of owner's sign-in started, finished or is still
// running since readADOSignInEnds returned gen, and that end's revoke reason.
func (s *Server) adoSignInEndedSince(ctx context.Context, owner string, gen uint64) (reason string, ended bool, err error) {
	st := s.signInEndStore()
	if st == nil || owner == "" {
		reason, ended = s.adoSignInEnds.since(owner, gen)
		return reason, ended, nil
	}
	state, err := st.ADOSignInEnds(ctx, owner)
	if err != nil {
		return "", false, err
	}
	if uint64(state.Gen) == gen && !state.Running {
		return "", false, nil
	}
	return state.Reason, true, nil
}
