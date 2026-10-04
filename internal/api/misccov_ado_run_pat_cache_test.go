// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/db"
)

// miscCovFailingKeys is a subject-key service that is down.
type miscCovFailingKeys struct{ err error }

func (k miscCovFailingKeys) Current(context.Context, string, string) (int, []byte, error) {
	return 0, nil, k.err
}

func (k miscCovFailingKeys) Key(context.Context, string, string, int) ([]byte, error) {
	return nil, k.err
}

// miscCovPATServer is a server whose run-token state lives in a store over a closed pool, so every read
// and write of it fails, and a server with no such store beside it.
func miscCovPATServer(t *testing.T, keys adorunpat.Keys) (withStore *Server, closedErr error) {
	t.Helper()
	pool, closedErr := miscCovClosedPool(t)
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.ADORunPATs = adorunpat.New(pool, keys)
	return New(cfg), closedErr
}

func TestMiscCovLoadRunPATRefusesWhenTheStateCannotBeRead(t *testing.T) {
	srv, closedErr := miscCovPATServer(t, nil)
	e, err := srv.loadRunPAT(t.Context(), uuid.New())
	if e != nil {
		t.Errorf("an entry came back with the error: %+v", e)
	}
	if !errors.Is(err, errADOPATStateUnavailable) || !errors.Is(err, closedErr) {
		t.Fatalf("err = %v, want errADOPATStateUnavailable wrapping the store's error", err)
	}
}

func TestMiscCovSaveRunPAT(t *testing.T) {
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000b1")
	keysDown := errors.New("key service down")
	srv, closedErr := miscCovPATServer(t, miscCovFailingKeys{keysDown})
	valid := time.Now().UTC().Truncate(time.Second)

	t.Run("a store-less server's save is a no-op", func(t *testing.T) {
		plain := newHarness(t).srv
		if err := plain.saveRunPAT(t.Context(), run, &adoRunPATEntry{owner: "bob", cur: adoPAT{Token: "t"}}); err != nil {
			t.Fatalf("saveRunPAT with no store = %v, want nil", err)
		}
	})
	t.Run("an entry with no owner is deleted, not saved", func(t *testing.T) {
		err := srv.saveRunPAT(t.Context(), run, &adoRunPATEntry{cur: adoPAT{Token: "t"}})
		if !errors.Is(err, closedErr) || errors.Is(err, errADOPATStateUnavailable) {
			t.Errorf("err = %v, want the delete's own error from the closed pool", err)
		}
	})
	t.Run("an entry with nothing in it is deleted, not saved", func(t *testing.T) {
		err := srv.saveRunPAT(t.Context(), run, &adoRunPATEntry{owner: "bob"})
		if !errors.Is(err, closedErr) || errors.Is(err, errADOPATStateUnavailable) {
			t.Errorf("err = %v, want the delete's own error from the closed pool", err)
		}
	})
	t.Run("a token the key service cannot seal is not saved", func(t *testing.T) {
		e := &adoRunPATEntry{owner: "bob", cur: adoPAT{Token: "t", ValidTo: valid}, caps: []adoscope.Capability{adoscope.Capability("repo.read")}}
		err := srv.saveRunPAT(t.Context(), run, e)
		if !errors.Is(err, errADOPATStateUnavailable) || !errors.Is(err, keysDown) {
			t.Errorf("err = %v, want errADOPATStateUnavailable wrapping the key error", err)
		}
	})
	t.Run("a paused entry with no token is saved, and its failure is the state's", func(t *testing.T) {
		err := srv.saveRunPAT(t.Context(), run, &adoRunPATEntry{owner: "bob", paused: true})
		if !errors.Is(err, errADOPATStateUnavailable) || !errors.Is(err, closedErr) {
			t.Errorf("err = %v, want errADOPATStateUnavailable wrapping the pool's error", err)
		}
	})
}

func TestMiscCovDropRunPAT(t *testing.T) {
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000b2")
	srv, closedErr := miscCovPATServer(t, nil)
	if err := srv.dropRunPAT(t.Context(), run); !errors.Is(err, closedErr) {
		t.Errorf("dropRunPAT over a failing store = %v, want its error", err)
	}

	plain := newHarness(t).srv
	e, err := plain.loadRunPAT(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	e.cur.Token = "kept-in-memory"
	if err := plain.dropRunPAT(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	again, err := plain.loadRunPAT(t.Context(), run)
	if err != nil || again.cur.Token != "" || again == e {
		t.Errorf("after a drop the run's entry = %+v (same entry %v), err %v; want a fresh empty one", again, again == e, err)
	}
}

func TestMiscCovLockRunPAT(t *testing.T) {
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000b3")

	t.Run("a lock that cannot be taken refuses the work", func(t *testing.T) {
		srv := newHarness(t).srv
		srv.locks.override = &miscCovLocker{refuse: db.ErrLockBusy}
		ctx, e, unlock, err := srv.lockRunPAT(t.Context(), run)
		if !errors.Is(err, db.ErrLockBusy) || e != nil || unlock != nil {
			t.Fatalf("got entry %v, unlock %v, err %v; want the lock's refusal and nothing to release", e, unlock != nil, err)
		}
		if ctx == nil {
			t.Error("no context returned")
		}
	})

	t.Run("state that cannot be read releases the lock it took", func(t *testing.T) {
		srv, closedErr := miscCovPATServer(t, nil)
		locker := &miscCovLocker{}
		srv.locks.override = locker
		_, e, unlock, err := srv.lockRunPAT(t.Context(), run)
		if !errors.Is(err, errADOPATStateUnavailable) || !errors.Is(err, closedErr) || e != nil || unlock != nil {
			t.Fatalf("got entry %v, unlock %v, err %v; want errADOPATStateUnavailable", e, unlock != nil, err)
		}
		if locks, unlocks := locker.counts(); locks != 1 || unlocks != 1 {
			t.Errorf("locks taken %d, released %d; want the one lock released", locks, unlocks)
		}
	})
}
