// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// watchClosed starts watchSigninDisplay on f's run at a short interval and returns the channel it closes.
func watchClosed(t *testing.T, f *signinFixture) chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	closed := make(chan struct{})
	go f.srv.watchSigninDisplay(ctx, f.run, signinOwner, 10*time.Millisecond, func() { close(closed) })
	return closed
}

// askEnd records the end of f's open sign-in, as a stop that failed does.
func askEnd(t *testing.T, f *signinFixture) {
	t.Helper()
	si, err := f.st.OpenDesktopSignin(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.RequestDesktopSigninEnd(context.Background(), si.ID, "person", "stop:1"); err != nil || !ok {
		t.Fatalf("RequestDesktopSigninEnd = %v, %v", ok, err)
	}
}

// An open sign-in display's relay closes once its sign-in is no longer the holder's open one, and not before.
func TestSigninDisplay_ClosesOnceItsEndIsAsked(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	closed := watchClosed(t, f)
	select {
	case <-closed:
		t.Fatal("the relay closed while its sign-in was open")
	case <-time.After(100 * time.Millisecond):
	}
	askEnd(t, f)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the relay stayed open after its end was asked for")
	}
}

// signinReadFailStore fails the sign-in read while fail is set.
type signinReadFailStore struct {
	*signinTestStore
	fail atomic.Bool
}

func (s *signinReadFailStore) OpenDesktopSignin(ctx context.Context, runID uuid.UUID) (types.DesktopSignin, error) {
	if s.fail.Load() {
		return types.DesktopSignin{}, errors.New("store down")
	}
	return s.signinTestStore.OpenDesktopSignin(ctx, runID)
}

// A sign-in read that fails is not an end: the relay stays open until a read answers that the sign-in is not held.
func TestSigninDisplay_AFailedReadKeepsTheRelay(t *testing.T) {
	f := newSigninFixture(t)
	st := &signinReadFailStore{signinTestStore: f.st}
	f.srv.cfg.Store = st
	f.begin(t)
	st.fail.Store(true)
	closed := watchClosed(t, f)
	select {
	case <-closed:
		t.Fatal("the relay closed on a failed read")
	case <-time.After(100 * time.Millisecond):
	}
	st.fail.Store(false)
	askEnd(t, f)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the relay stayed open once a read answered")
	}
}
