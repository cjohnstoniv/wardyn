// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signInClockRows are the sign-in gate's rows. The database stamps requested_at and a server stamps the capture.
// capturer and reader are the capturing and the reading server's clocks less the database's, and capture is when the
// sign-in came on the database's clock, from the raise. stamped is a capture that carries its server's database stamp
// (CapturedDBAt); one without it is an older capture, which the reader moves onto the database's clock itself. readGap
// is how far the reader's clock moves while its database clock read is in flight. A capture after the raise answers it
// whatever the clocks; one before never does.
var signInClockRows = []struct {
	name                      string
	capturer, reader, capture time.Duration
	stamped                   bool
	readGap                   time.Duration
	clockErr                  error
	want                      types.ApprovalState
}{
	{"older capture, server 30s behind, 5s after the raise", -30 * time.Second, -30 * time.Second, 5 * time.Second, false, 0, nil, types.ApprovalApproved},
	{"older capture, server 30s behind, 5s before the raise", -30 * time.Second, -30 * time.Second, -5 * time.Second, false, 0, nil, types.ApprovalPending},
	{"older capture, clocks agree, 5s after the raise", 0, 0, 5 * time.Second, false, 0, nil, types.ApprovalApproved},
	{"older capture, clocks agree, 5s before the raise", 0, 0, -5 * time.Second, false, 0, nil, types.ApprovalPending},
	{"older capture, server 30s ahead, 5s after the raise", 30 * time.Second, 30 * time.Second, 5 * time.Second, false, 0, nil, types.ApprovalApproved},
	{"older capture, server 30s ahead, 5s before the raise", 30 * time.Second, 30 * time.Second, -5 * time.Second, false, 0, nil, types.ApprovalPending},
	{"older capture, database clock unreadable, 5s after the raise", 0, 0, 5 * time.Second, false, 0, errClockRead, types.ApprovalPending},
	// The database's clock is read before the server's: a capture inside the read's delay reads as before the raise.
	{"older capture 1s after the raise, a 2s clock read", 0, 0, time.Second, false, 2 * time.Second, nil, types.ApprovalPending},
	{"stamped, both servers 30s ahead, 5s before the raise", 30 * time.Second, 30 * time.Second, -5 * time.Second, true, 0, nil, types.ApprovalPending},
	{"stamped, both servers 30s ahead, 5s after the raise", 30 * time.Second, 30 * time.Second, 5 * time.Second, true, 0, nil, types.ApprovalApproved},
	{"stamped, capturer in sync, reader 30s behind, 5s before the raise", 0, -30 * time.Second, -5 * time.Second, true, 0, nil, types.ApprovalPending},
	{"stamped, capturer in sync, reader 30s ahead, 5s after the raise", 0, 30 * time.Second, 5 * time.Second, true, 0, nil, types.ApprovalApproved},
	{"stamped, capturer 30s ahead, reader 30s behind, 5s before the raise", 30 * time.Second, -30 * time.Second, -5 * time.Second, true, 0, nil, types.ApprovalPending},
	{"stamped, reader's database clock unreadable, 5s after the raise", 0, 0, 5 * time.Second, true, 0, errClockRead, types.ApprovalApproved},
}

// signInClockRaise is the raise every row is timed from, on the database's clock; the gate runs a minute later.
var signInClockRaise = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// stampCapture writes the row's capture times into blob: CapturedAt on the capturing server's clock, and
// CapturedDBAt on the database's when the row is stamped.
func stampCapture(blob *adoEntraBlob, capturer, capture time.Duration, stamped bool) {
	blob.CapturedAt = signInClockRaise.Add(capture + capturer)
	blob.CapturedDBAt = time.Time{}
	if stamped {
		blob.CapturedDBAt = signInClockRaise.Add(capture)
	}
}

// readerClock is the reading server's clock for a row, a minute after the raise, and the database's reading of it:
// advance moves the server's clock on by the row's read gap, as each database clock read does.
func readerClock(reader, readGap time.Duration) (dbAt time.Time, now func() time.Time, advance func()) {
	dbAt = signInClockRaise.Add(time.Minute)
	at := dbAt.Add(reader)
	return dbAt, func() time.Time { return at }, func() { at = at.Add(readGap) }
}

// pendingAgain puts the raised row back to PENDING, raised at at, for the next row.
func pendingAgain(fa *fakeApprovals, id uuid.UUID, at time.Time) types.ApprovalRequest {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	ap := fa.byID[id]
	ap.State, ap.DecidedBy, ap.RequestedAt = types.ApprovalPending, "", at
	fa.byID[id] = ap
	return ap
}

// adoSignInClockStore is the sign-in fixture's store on a database clock of its own; onRead runs on each read.
type adoSignInClockStore struct {
	*adoSignInStore
	at     time.Time
	err    error
	onRead func()
}

func (s adoSignInClockStore) DatabaseNow(context.Context) (time.Time, error) {
	s.onRead()
	return s.at, s.err
}

// azA3ClockStore is the Azure sink fixture's store on a database clock of its own; onRead runs on each read.
type azA3ClockStore struct {
	*azA3Store
	at     time.Time
	err    error
	onRead func()
}

func (s azA3ClockStore) DatabaseNow(context.Context) (time.Time, error) {
	s.onRead()
	return s.at, s.err
}

// An Azure DevOps sign-in answers the request it follows, read on one clock. Catches: the capture (a server's clock)
// compared with requested_at (the database's), so a server behind the database never lets a sign-in made just after
// the raise answer it, and one ahead lets a sign-in made just before it count; a capture's own database stamp ignored,
// so a server reading another's capture moves it by the difference between the two servers' clocks; and the anchor
// read in the wrong order, so a capture inside the read's delay counts.
func TestADOSignIn_CaptureAnswersTheRaiseOnTheDatabaseClock(t *testing.T) {
	f := newADOSignInFixture(t)
	ctx := context.Background()
	blob, _ := f.stored(t, f.subject)
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
	for _, tc := range signInClockRows {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingAgain(f.approvals, id, signInClockRaise)
			f.srv.cfg.Store = f.st // the previous row's clock is not this write's
			stampCapture(&blob, tc.capturer, tc.capture, tc.stamped)
			if err := f.srv.storeADOEntraBlob(ctx, f.subject, f.cfg.RowID, blob); err != nil {
				t.Fatal(err)
			}
			dbAt, now, advance := readerClock(tc.reader, tc.readGap)
			f.srv.cfg.Now = now
			f.srv.cfg.Store = adoSignInClockStore{adoSignInStore: f.st, at: dbAt, err: tc.clockErr, onRead: advance}
			if got, _ := f.srv.reconcileADOReauth(ctx, row); got.State != tc.want {
				t.Errorf("the request is %s, want %s", got.State, tc.want)
			}
		})
	}
}

// The same gate for the Azure sign-in lanes (reconcileAzureReauth), on a model-provider row.
func TestAzureSink_CaptureAnswersTheRaiseOnTheDatabaseClock(t *testing.T) {
	f := newAzA3Fixture(t)
	ctx := context.Background()
	f.signIn(t, azUIDAnthropic)
	plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
	if w := f.resolve(t, plan, "?phase=boot"); w.Code != http.StatusOK {
		t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
	}
	f.fake.SetInvalidGrant(true)
	id := pendingID(t, f.resolve(t, plan, ""), reauthPendingState)
	// A usable sign-in on record again; each row below restamps its capture.
	f.fake.SetInvalidGrant(false)
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	f.signIn(t, azUIDAnthropic)
	row, _ := f.st.approvals.Get(ctx, id)
	_, ec, _, ok := f.srv.azureReauthCapture(ctx, row)
	blob, found, err := f.srv.readEntraBlob(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), f.owner, ec)
	if !ok || !found || err != nil {
		t.Fatalf("the sign-in on record: capture ok=%v found=%v err=%v", ok, found, err)
	}
	for _, tc := range signInClockRows {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingAgain(f.st.approvals, id, signInClockRaise)
			f.srv.cfg.Store = f.st // the previous row's clock is not this write's
			stampCapture(&blob, tc.capturer, tc.capture, tc.stamped)
			if err := f.srv.storeEntraBlob(ctx, f.owner, ec, blob); err != nil {
				t.Fatal(err)
			}
			dbAt, now, advance := readerClock(tc.reader, tc.readGap)
			f.srv.cfg.Now = now
			f.srv.cfg.Store = azA3ClockStore{azA3Store: f.st, at: dbAt, err: tc.clockErr, onRead: advance}
			if got, _ := f.srv.reconcileAzureReauth(ctx, row); got.State != tc.want {
				t.Errorf("the request is %s, want %s", got.State, tc.want)
			}
		})
	}
}
