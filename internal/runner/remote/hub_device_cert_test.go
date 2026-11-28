// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A claimed runner's AUTH_OK says device_cert exactly when the organisation's answer says an MDM device CA is set,
// and says nothing when no answer is wired or the answer could not be read.
func TestHubAuthOKCarriesDeviceCert(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, c := range []struct {
			name   string
			answer func(context.Context) (bool, error)
			want   bool
		}{
			{"no answer", nil, false},
			{"no CA", func(context.Context) (bool, error) { return false, nil }, false},
			{"CA set", func(context.Context) (bool, error) { return true, nil }, true},
			{"unreadable", func(context.Context) (bool, error) { return true, errors.New("site config unreadable") }, false},
		} {
			f := newInventoryFixture(t, realWire)
			f.hub.cfg.DeviceCert = c.answer
			if _, ok, _ := f.open(t, nil); ok.DeviceCert != c.want {
				t.Fatalf("%s: AUTH_OK device_cert = %v, want %v", c.name, ok.DeviceCert, c.want)
			}
		}
	})
}

// The stored-row recheck ends a Mac's session whose AUTH_OK answered device_cert otherwise than the organisation
// answers now, a CA set or cleared on any replica, and no other session: not a Mac's whose answer still holds, not
// another platform's, and none on a tick whose answer cannot be read. The answer is read once per tick.
func TestHubRecheckAllEndsAMacSessionWhoseDeviceCertAnswerChanged(t *testing.T) {
	setRecheckEvery(t, time.Hour)
	alice, bob, carol := personRunner("alice"), personRunner("bob"), personRunner("carol")
	alice.Posture.OS, bob.Posture.OS, carol.Posture.OS = "darwin", "darwin", "linux"
	rows := newRows(alice, bob, carol)
	const unreadable, caSet, noCA = 0, 1, 2
	var answer, reads atomic.Int32
	answer.Store(unreadable)
	hub := NewHub(t.Context(), HubConfig{LookupMany: rows.lookupMany,
		DeviceCert: func(context.Context) (bool, error) {
			reads.Add(1)
			switch answer.Load() {
			case caSet:
				return true, nil
			case noCA:
				return false, nil
			}
			return false, errors.New("site config unreadable")
		}})
	t.Cleanup(hub.Close)
	no, yes := false, true
	fa, fb, fc := addLiveSession(t, hub, alice, nil), addLiveSession(t, hub, bob, nil), addLiveSession(t, hub, carol, nil)
	fa.s.deviceCert.Store(&no)  // a Mac told no CA
	fb.s.deviceCert.Store(&yes) // a Mac told a CA is set
	fc.s.deviceCert.Store(&no)  // a Linux runner told no CA
	tick := func() {
		t.Helper()
		check(t, hub.recheckAll(t.Context()), "recheckAll reported a closed Hub")
		waitRecheckIdle(t, hub)
	}

	tick() // the answer cannot be read: no session ends
	requireOnline(t, fa)
	requireOnline(t, fb)
	requireOnline(t, fc)

	answer.Store(caSet)
	tick()
	requireOffline(t, fa)
	requireOnline(t, fb)
	requireOnline(t, fc)

	answer.Store(noCA)
	tick()
	requireOffline(t, fb)
	requireOnline(t, fc)
	check(t, reads.Load() == 3, "the device_cert answer must be read once per tick")
}

// An MDM device CA set through another replica (this one is never told) reaches a Mac's session through the
// stored-row recheck: the hub ends it with GOAWAY and the fresh session's AUTH_OK asks for the device certificate.
func TestHubDeviceCertSetFromAnotherReplicaEndsTheMacSession(t *testing.T) {
	recheck := storedRowRecheck
	storedRowRecheck = 20 * time.Millisecond
	t.Cleanup(func() { storedRowRecheck = recheck })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// Every Serve has returned before the cleanup restores storedRowRecheck, which Serve reads.
	var serving sync.WaitGroup
	defer func() { cancel(); serving.Wait() }()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"),
		Posture: types.RunnerPosture{OS: "darwin"}}
	var caSet atomic.Bool
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example",
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
		DeviceCert: func(context.Context) (bool, error) { return caSet.Load(), nil }})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	heard := make(chan bool, 4)
	hooks := sessionHooks(online, resumed)
	hooks.DeviceCert = func(on bool) { heard <- on }
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			serving.Go(func() { _ = hub.Serve(ctx, org, row.ID) })
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, online)
	waitOnline(t, ctx, link)
	if got := awaitValue(t, ctx, heard); got {
		t.Fatal("the first session, with no CA set, was asked for the device certificate")
	}
	caSet.Store(true)
	if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrGoaway) {
		t.Fatalf("the recheck ended the session with %v, want a GOAWAY", err)
	}
	// As in TestHubStoredLimitsChangeFromAnotherReplicaEndsTheSession: a runner's Run retries a refused
	// reconnect after its delay, this test reconnects once every Serve has returned.
	serving.Wait()
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, online)
	if got := awaitValue(t, ctx, heard); !got {
		t.Fatal("the session after the GOAWAY was not asked for the device certificate")
	}
}
