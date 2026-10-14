// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// ownerUploads is a server over real run rows, so the upload route reads each
// run's owner as it does in production.
type ownerUploads struct {
	t   *testing.T
	h   *harness
	st  store.PG
	srv *Server
}

func newOwnerUploads(t *testing.T, wrap func(*recording.PGStore) recording.Store) *ownerUploads {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.RecordingStore = wrap(recording.NewPGStore(pool))
	return &ownerUploads{t: t, h: h, st: st, srv: New(cfg)}
}

// run stores a live run of owner and returns its id and upload token.
func (o *ownerUploads) run(owner string, operatorOwned bool) (uuid.UUID, string) {
	o.t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.OperatorOwned = owner, operatorOwned
	run, err := o.st.CreateRun(context.Background(), run)
	if err != nil {
		o.t.Fatal(err)
	}
	return run.ID, mintRunTokenAs(o.t, o.h, run.ID, owner)
}

// admittedStore reports the admission group of each save, and each stream save
// once it is admitted and reading. Neither report ever blocks a save.
type admittedStore struct {
	*recording.PGStore
	groups  chan string
	reading chan string
}

type admittedRead struct {
	io.Reader
	key  string
	ch   chan<- string
	seen bool
}

func (a *admittedRead) Read(p []byte) (int, error) {
	if !a.seen {
		a.seen = true
		select {
		case a.ch <- a.key:
		default:
		}
	}
	return a.Reader.Read(p)
}

func (s admittedStore) SaveCast(ctx context.Context, key string, r io.Reader) error {
	select {
	case s.groups <- recording.AdmissionGroup(ctx, key):
	default:
	}
	return s.PGStore.SaveCast(ctx, key, &admittedRead{Reader: r, key: key, ch: s.reading})
}

func (s admittedStore) SaveCastNamed(ctx context.Context, run, suffix string, r io.Reader) error {
	return s.SaveCast(ctx, recording.CastKey(run, suffix), r)
}

func newAdmittedStore(pg *recording.PGStore) admittedStore {
	return admittedStore{PGStore: pg, groups: make(chan string, 64), reading: make(chan string, 64)}
}

// Both upload doors queue a person's run under its owner, and a run with no
// person owner under itself.
func TestUploadRecording_QueuesByTheRunsOwner(t *testing.T) {
	var as admittedStore
	o := newOwnerUploads(t, func(pg *recording.PGStore) recording.Store { as = newAdmittedStore(pg); return as })
	owned, ownedToken := o.run("pat@example.com", false)
	unowned, unownedToken := o.run("ops@example.com", true)
	for _, tc := range []struct {
		name, path, token, want string
	}{
		{"a person's upload", "/api/v1/internal/recordings/" + owned.String(), ownedToken, "owner:pat@example.com"},
		{"a person's part", "/api/v1/internal/recordings/" + owned.String() + "/parts/2", ownedToken, "owner:pat@example.com"},
		{"a run with no person owner", "/api/v1/internal/recordings/" + unowned.String(), unownedToken, unowned.String()},
	} {
		if w := do(t, o.srv, http.MethodPut, tc.path, tc.token, "cast"); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if got := <-as.groups; got != tc.want {
			t.Errorf("%s queued as %q, want %q", tc.name, got, tc.want)
		}
	}
}

// stalledLoops keeps each run re-sending an upload whose body never arrives,
// sending again after each answer, until the returned stop is called.
func stalledLoops(t *testing.T, addr string, runs map[uuid.UUID]string) (stop func()) {
	t.Helper()
	quit := make(chan struct{})
	var wg sync.WaitGroup
	for run, token := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-quit:
					return
				default:
				}
				c, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "PUT /api/v1/internal/recordings/%s HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n", run, token)
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.ReadFull(c, make([]byte, 12)) // the answer's status line
				_ = c.Close()
			}
		}()
	}
	return func() { close(quit); wg.Wait() }
}

// awaitStalledHolder returns once a stalled upload has just been admitted and
// is holding the upload slot.
func awaitStalledHolder(as admittedStore) {
	for {
		select {
		case <-as.reading: // drop older reports
		default:
			<-as.reading
			return
		}
	}
}

// Two runs of one owner keep re-sending stalled uploads. They queue behind
// each other, so another owner's upload is behind one of them at most and gets
// 204 in each of three consecutive cycles.
func TestPGUploadRecording_OneOwnersStalledUploadsDoNotRefuseAnother(t *testing.T) {
	shortenUploadWindow(t)
	var as admittedStore
	o := newOwnerUploads(t, func(pg *recording.PGStore) recording.Store { as = newAdmittedStore(pg); return as })
	srv := httptest.NewServer(o.srv.Handler())
	defer srv.Close()
	a1, a1Token := o.run("busy@example.com", false)
	a2, a2Token := o.run("busy@example.com", false)
	other, otherToken := o.run("other@example.com", false)
	stop := stalledLoops(t, strings.TrimPrefix(srv.URL, "http://"), map[uuid.UUID]string{a1: a1Token, a2: a2Token})
	defer stop()
	for cycle := 1; cycle <= 3; cycle++ {
		awaitStalledHolder(as)
		if code := boundedPut(t, http.DefaultClient, srv.URL+"/api/v1/internal/recordings/"+other.String(), otherToken, strings.NewReader("cast"), 4); code != http.StatusNoContent {
			t.Fatalf("cycle %d: another owner's upload = %d, want 204", cycle, code)
		}
	}
}

// The residual: the same two stalled runs under two different owners each
// hold a place of their own, so a third owner's upload can wait behind both.
// It may be refused once the waits outgrow the admission bound; here it is
// only required to be answered, never left hanging.
func TestPGUploadRecording_SeveralOwnersStalledUploadsAnswerWithinBounds(t *testing.T) {
	shortenUploadWindow(t)
	var as admittedStore
	o := newOwnerUploads(t, func(pg *recording.PGStore) recording.Store { as = newAdmittedStore(pg); return as })
	srv := httptest.NewServer(o.srv.Handler())
	defer srv.Close()
	a, aToken := o.run("one@example.com", false)
	b, bToken := o.run("two@example.com", false)
	third, thirdToken := o.run("three@example.com", false)
	stop := stalledLoops(t, strings.TrimPrefix(srv.URL, "http://"), map[uuid.UUID]string{a: aToken, b: bToken})
	defer stop()
	for cycle := 1; cycle <= 3; cycle++ {
		awaitStalledHolder(as)
		code := boundedPut(t, http.DefaultClient, srv.URL+"/api/v1/internal/recordings/"+third.String(), thirdToken, strings.NewReader("cast"), 4)
		if code != http.StatusNoContent && code != http.StatusInternalServerError {
			t.Fatalf("cycle %d: a third owner's upload = %d, want 204 or the 500", cycle, code)
		}
	}
}
