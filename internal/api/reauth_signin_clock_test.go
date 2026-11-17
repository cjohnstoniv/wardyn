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

// signInClockRows are the sign-in gate's rows. The database stamps requested_at and this server stamps the capture,
// so skew is this server's clock less the database's, and capture is when the sign-in came on the database's clock,
// from the raise. A capture after the raise answers it whatever the skew; one before never does; and with the
// database's clock unreadable nothing answers.
var signInClockRows = []struct {
	name          string
	skew, capture time.Duration
	clockErr      error
	want          types.ApprovalState
}{
	{"server 30s behind, capture 5s after the raise", -30 * time.Second, 5 * time.Second, nil, types.ApprovalApproved},
	{"server 30s behind, capture 5s before the raise", -30 * time.Second, -5 * time.Second, nil, types.ApprovalPending},
	{"clocks agree, capture 5s after the raise", 0, 5 * time.Second, nil, types.ApprovalApproved},
	{"clocks agree, capture 5s before the raise", 0, -5 * time.Second, nil, types.ApprovalPending},
	{"server 30s ahead, capture 5s after the raise", 30 * time.Second, 5 * time.Second, nil, types.ApprovalApproved},
	{"server 30s ahead, capture 5s before the raise", 30 * time.Second, -5 * time.Second, nil, types.ApprovalPending},
	{"database clock unreadable, capture 5s after the raise", 0, 5 * time.Second, errClockRead, types.ApprovalPending},
}

// signInClockRaise is the raise every row is timed from, on the database's clock; the gate runs a minute later.
var signInClockRaise = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// pendingAgain puts the raised row back to PENDING, raised at at, for the next row.
func pendingAgain(fa *fakeApprovals, id uuid.UUID, at time.Time) types.ApprovalRequest {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	ap := fa.byID[id]
	ap.State, ap.DecidedBy, ap.RequestedAt = types.ApprovalPending, "", at
	fa.byID[id] = ap
	return ap
}

// adoSignInClockStore is the sign-in fixture's store on a database clock of its own.
type adoSignInClockStore struct {
	*adoSignInStore
	at  time.Time
	err error
}

func (s adoSignInClockStore) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// azA3ClockStore is the Azure sink fixture's store on a database clock of its own.
type azA3ClockStore struct {
	*azA3Store
	at  time.Time
	err error
}

func (s azA3ClockStore) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// An Azure DevOps sign-in answers the request it follows, read on one clock. Catches: the capture (this server's
// clock) compared with requested_at (the database's), so a server behind the database never lets a sign-in made
// just after the raise answer it, and one ahead lets a sign-in made just before it count.
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
			blob.CapturedAt = signInClockRaise.Add(tc.capture + tc.skew)
			if err := f.srv.storeADOEntraBlob(ctx, f.subject, f.cfg.RowID, blob); err != nil {
				t.Fatal(err)
			}
			dbAt := signInClockRaise.Add(time.Minute)
			f.at(dbAt.Add(tc.skew))
			f.srv.cfg.Store = adoSignInClockStore{adoSignInStore: f.st, at: dbAt, err: tc.clockErr}
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
			blob.CapturedAt = signInClockRaise.Add(tc.capture + tc.skew)
			if err := f.srv.storeEntraBlob(ctx, f.owner, ec, blob); err != nil {
				t.Fatal(err)
			}
			dbAt := signInClockRaise.Add(time.Minute)
			f.srv.cfg.Now = func() time.Time { return dbAt.Add(tc.skew) }
			f.srv.cfg.Store = azA3ClockStore{azA3Store: f.st, at: dbAt, err: tc.clockErr}
			if got, _ := f.srv.reconcileAzureReauth(ctx, row); got.State != tc.want {
				t.Errorf("the request is %s, want %s", got.State, tc.want)
			}
		})
	}
}
