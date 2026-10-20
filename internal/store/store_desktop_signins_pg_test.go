// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Desktop sign-ins (migration 0200) against a real Postgres. Guarded by
// WARDYN_TEST_PG; each test works in its own throwaway database.
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_DesktopSigninLifecycle(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sum := sha256.Sum256([]byte("cred"))
	si := types.DesktopSignin{ID: uuid.New(), RunID: run.ID, Holder: "alice", CredentialHash: sum[:],
		BeganAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	if had, _ := st.RunHadDesktopSignin(ctx, run.ID); had {
		t.Fatal("a run with no sign-in reports one")
	}
	if err := st.BeginDesktopSignin(ctx, si); err != nil {
		t.Fatal(err)
	}
	second := si
	second.ID = uuid.New()
	if err := st.BeginDesktopSignin(ctx, second); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second open sign-in = %v, want ErrConflict", err)
	}
	if err := st.SetDesktopSigninPauseRecord(ctx, si.ID, json.RawMessage(`{"already_stopped":[7]}`)); err != nil {
		t.Fatal(err)
	}
	open, err := st.OpenDesktopSignin(ctx, run.ID)
	if err != nil || open.ID != si.ID || string(open.PauseRecord) != `{"already_stopped": [7]}` && string(open.PauseRecord) != `{"already_stopped":[7]}` {
		t.Fatalf("open = %+v %v", open, err)
	}
	for _, tc := range []struct {
		hash []byte
		at   time.Time
		want bool
	}{
		{sum[:], now, true},
		{[]byte("other"), now, false},
		{sum[:], now.Add(31 * time.Minute), false},
	} {
		if got, err := st.DesktopSigninCredentialOpen(ctx, run.ID, tc.hash, tc.at); err != nil || got != tc.want {
			t.Errorf("credential open at %v = %v %v, want %v", tc.at, got, err, tc.want)
		}
	}
	if m, err := st.RunsWithOpenDesktopSignin(ctx); err != nil || !m[run.ID] {
		t.Fatalf("open set = %v %v", m, err)
	}
	if err := st.RecordDesktopSigninShare(ctx, types.DesktopSigninShare{ID: uuid.New(), SigninID: si.ID, RunID: run.ID,
		Site: "site.test", CookieCount: 2, SharedBy: "alice", SharedAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountDesktopSigninShares(ctx, si.ID); err != nil || n != 1 {
		t.Fatalf("shares = %d %v", n, err)
	}
	if changed, err := st.RequestDesktopSigninEnd(ctx, si.ID, "done", "stop"); err != nil || !changed {
		t.Fatalf("first end request = %v %v, want a state change", changed, err)
	}
	if changed, _ := st.RequestDesktopSigninEnd(ctx, si.ID, "expired", "stop"); changed {
		t.Fatal("the same stuck stage again reported a state change")
	}
	if changed, _ := st.RequestDesktopSigninEnd(ctx, si.ID, "expired", "resume"); !changed {
		t.Fatal("a new stuck stage reported no state change")
	}
	if open, _ := st.OpenDesktopSignin(ctx, run.ID); open.EndRequested != "done" || open.EndStage != "resume" {
		t.Fatalf("row = requested %q stage %q, want the first reason and the latest stage", open.EndRequested, open.EndStage)
	}
	if got, _ := st.DesktopSigninCredentialOpen(ctx, run.ID, sum[:], now); got {
		t.Fatal("a sign-in with an end requested still admits its credential")
	}
	if ended, err := st.EndDesktopSignin(ctx, si.ID, "done", now); err != nil || !ended {
		t.Fatalf("end = %v %v", ended, err)
	}
	if ended, _ := st.EndDesktopSignin(ctx, si.ID, "done", now); ended {
		t.Fatal("a second end reported ending it")
	}
	if _, err := st.OpenDesktopSignin(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open after end = %v, want ErrNotFound", err)
	}
	if err := st.RecordDesktopSigninShare(ctx, types.DesktopSigninShare{ID: uuid.New(), SigninID: si.ID, RunID: run.ID,
		Site: "site.test", SharedBy: "alice", SharedAt: now}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a share of an ended sign-in = %v, want ErrNotFound", err)
	}
	if had, _ := st.RunHadDesktopSignin(ctx, run.ID); !had {
		t.Fatal("an ended sign-in no longer counts for the secrets-present rule")
	}
	if err := st.BeginDesktopSignin(ctx, second); err != nil {
		t.Fatalf("a new sign-in after the end = %v", err)
	}
	if _, found, err := st.DesktopSigninRun(ctx, run.ID); err != nil || found {
		t.Fatalf("an unmarked run = %v %v, want no decision", found, err)
	}
	for _, off := range []string{"image_not_granted", ""} {
		if err := st.MarkDesktopSigninRun(ctx, run.ID, off); err != nil {
			t.Fatal(err)
		}
		if got, found, err := st.DesktopSigninRun(ctx, run.ID); err != nil || !found || got != off {
			t.Fatalf("a run marked %q = %q %v %v", off, got, found, err)
		}
	}
}
