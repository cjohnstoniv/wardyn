// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Mid-run AWS SSO renewal, from the renewer's side: a live run's session is
// renewed while its token is still valid, so the run never reaches the sign-in
// hold a silent renewal could have spared it; a session AWS will not renew
// still ends in that hold.

// liveGrantStore is the reauth fixture's store plus the live-grant list the
// renewer asks for: the fixture run's grants. Which runs are live is the SQL's
// question (store_awssso_live_grants_test.go); this store answers it as all.
type liveGrantStore struct{ *reauthStore }

func (s liveGrantStore) ListLiveAWSSSOGrants(context.Context, string) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

const renewedToken = "renewed-by-the-renewer-0001"

// renewFixture is a running provider run whose owner's session expires in five
// minutes, inside the skew, with a refresh token AWS still honours.
func renewFixture(t *testing.T) (*reauthFixture, *testClock, awsSSOBlob) {
	t.Helper()
	f := newReauthFixture(t, nil)
	f.st.run.State = types.RunRunning
	f.srv.cfg.Store = liveGrantStore{f.st}
	clock := &testClock{now: time.Now().UTC()}
	f.srv.cfg.Now = clock.Now
	b := liveSSOBlob()
	b.ExpiresAt = clock.Now().Add(5 * time.Minute)
	b.RefreshToken = "refresh-token-the-renewer-redeems"
	b.ClientID, b.ClientSecret = "cid", "csec"
	b.RegistrationExpiresAt = clock.Now().Add(30 * 24 * time.Hour)
	f.putBlob(t, "alice@example.com", b)
	return f, clock, b
}

// renewOnceOIDC answers the first CreateToken with a fresh pair and every later
// one with a 500, so a second redemption anywhere shows as a failure.
func renewOnceOIDC(t *testing.T) *atomic.Int32 {
	t.Helper()
	return fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		if call > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": renewedToken, "expiresIn": 3600, "refreshToken": "refresh-token-rotated-by-the-renewer",
		})
	})
}

func pendingReauths(t *testing.T, f *reauthFixture) int {
	t.Helper()
	rows, err := f.srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ap := range rows {
		if ap.Kind == types.ApprovalCredentialReauth {
			n++
		}
	}
	return n
}

func refreshRows(f *reauthFixture, outcome string) []map[string]any {
	var out []map[string]any
	for _, ev := range f.audit.events() {
		var d map[string]any
		if ev.Action == "harness.credential.refresh" && ev.Outcome == outcome && json.Unmarshal(ev.Data, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// A run whose session nears expiry is renewed by the renewer while the token is
// still valid. Once the old token has expired, the proxy's resolve serves the
// renewed one with no second AWS call and no sign-in hold, though AWS is down.
func TestAWSSSORenewer_NearExpiryIsRenewedWithoutAHold(t *testing.T) {
	f, clock, old := renewFixture(t)
	calls := renewOnceOIDC(t)

	n, err := f.srv.RenewAWSSSOCredentials(context.Background())
	if err != nil || n != 1 || calls.Load() != 1 {
		t.Fatalf("renewer pass = %d renewed, err %v, %d CreateToken calls; want 1, nil, 1", n, err, calls.Load())
	}
	stored, found, err := f.srv.readAWSSSOBlob(context.Background(), reauthScope("alice@example.com"))
	if err != nil || !found || stored.AccessToken != renewedToken || !stored.ExpiresAt.After(old.ExpiresAt) {
		t.Fatalf("stored session after the pass = %q expiring %s (found %v, err %v); want the renewed token", stored.AccessToken, stored.ExpiresAt, found, err)
	}

	clock.advance(6 * time.Minute) // past the old token's expiry
	w := f.resolve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve after the old token expired: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Value != renewedToken {
		t.Fatalf("resolve injected %q (err %v), want the token the renewer stored", resp.Value, err)
	}
	if got := pendingReauths(t, f); got != 0 {
		t.Fatalf("pending credential_reauth rows = %d, want none", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("CreateToken calls = %d, want 1: the resolve renewed again", calls.Load())
	}
}

// The renewal's harness.credential.refresh row is the one every other renewal
// writes, plus trigger: renewer.
func TestAWSSSORenewer_RenewalIsAuditedAsTheRenewers(t *testing.T) {
	f, _, _ := renewFixture(t)
	renewOnceOIDC(t)
	if n, err := f.srv.RenewAWSSSOCredentials(context.Background()); n != 1 || err != nil {
		t.Fatalf("renewer pass = %d, %v", n, err)
	}
	rows := refreshRows(f, "success")
	if len(rows) != 1 {
		t.Fatalf("harness.credential.refresh success rows = %d, want 1", len(rows))
	}
	d := rows[0]
	if d["trigger"] != "renewer" || d["owner"] != "alice@example.com" || d["credential_source"] != string(types.CredentialSourcePerUser) ||
		d["provider"] != awsSSOProvider || d["expires_at"] == nil || d["rotated"] != true {
		t.Fatalf("refresh row = %v, want trigger renewer, the owner, per_user, the provider, expires_at and rotated", d)
	}
	for _, ev := range f.audit.events() {
		if ev.Action == "harness.credential.refresh" && (ev.ActorType != types.ActorSystem || ev.RunID != nil) {
			t.Fatalf("refresh row actor type %q, run %v; want the run-less system row", ev.ActorType, ev.RunID)
		}
	}
}

// A refresh token AWS will not honour ends where it did before the renewer: one
// redemption, a failure row, and the run's next resolve holds for a sign-in
// with no second redemption of the token AWS retired.
func TestAWSSSORenewer_RetiredRefreshTokenStillHolds(t *testing.T) {
	f, clock, _ := renewFixture(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"retired"}`))
	})

	n, err := f.srv.RenewAWSSSOCredentials(context.Background())
	if err != nil || n != 0 || calls.Load() != 1 {
		t.Fatalf("renewer pass = %d renewed, err %v, %d CreateToken calls; want 0, nil, 1", n, err, calls.Load())
	}
	rows := refreshRows(f, "failure")
	if len(rows) != 1 || rows[0]["trigger"] != "renewer" || rows[0]["spent"] != true {
		t.Fatalf("failure rows = %v, want one spent row with trigger renewer", rows)
	}

	clock.advance(6 * time.Minute)
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve: code = %d, want 423; body=%s", w.Code, w.Body.String())
	}
	if got := pendingReauths(t, f); got != 1 {
		t.Fatalf("pending credential_reauth rows = %d, want 1", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("CreateToken calls = %d, want 1: the resolve redeemed a refresh token AWS had retired", calls.Load())
	}
}

// The proxy's own resolve renews a session inside the skew and injects the
// renewed token, for the run the renewer has not reached yet.
func TestResolveAWSSSOInjection_NearExpiryRenewsOnDemand(t *testing.T) {
	f, _, _ := renewFixture(t)
	calls := renewOnceOIDC(t)
	w := f.resolve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Value != renewedToken || calls.Load() != 1 {
		t.Fatalf("resolve injected %q (err %v) after %d CreateToken calls, want the renewed token after 1", resp.Value, err, calls.Load())
	}
	if rows := refreshRows(f, "success"); len(rows) != 1 || rows[0]["trigger"] != nil {
		t.Fatalf("refresh rows = %v, want one with no renewer trigger", rows)
	}
}

// The renewer renews only for what the resolve would serve: an ended run, or a
// run whose provider pin moved since dispatch, gets no renewal.
func TestAWSSSORenewer_SkipsEndedAndDriftedRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*reauthFixture)
	}{
		{"ended run", func(f *reauthFixture) { f.st.run.State = types.RunCompleted }},
		{"provider pin moved", func(f *reauthFixture) {
			p := reauthProvider()
			p.Bedrock.SSORoleName = "SomeOtherRole"
			f.st.site = reauthProviderSite(p)
		}},
		{"another person's run", func(f *reauthFixture) { f.st.run.CreatedBy = "mallory@example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := renewFixture(t)
			calls := renewOnceOIDC(t)
			tc.shape(f)
			if n, err := f.srv.RenewAWSSSOCredentials(context.Background()); n != 0 || err != nil || calls.Load() != 0 {
				t.Fatalf("renewer pass = %d renewed, err %v, %d CreateToken calls; want none", n, err, calls.Load())
			}
		})
	}
}

// Every read of a stored session writes a secret.read row, so the renewer reads
// a credential when its renewal falls due, not on every pass.
func TestAWSSSORenewer_DoesNotReReadAnUnchangedCredential(t *testing.T) {
	f, _, _ := renewFixture(t)
	renewOnceOIDC(t)
	f.srv.cfg.Secrets = secretstore.Audited(f.secrets, f.audit)
	reads := func() int {
		n := 0
		for _, ev := range f.audit.events() {
			if ev.Action == "secret.read" {
				n++
			}
		}
		return n
	}
	if n, err := f.srv.RenewAWSSSOCredentials(context.Background()); n != 1 || err != nil {
		t.Fatalf("first pass = %d, %v", n, err)
	}
	first := reads()
	if first == 0 {
		t.Fatal("the first pass read nothing through the audited store")
	}
	for range 3 {
		if _, err := f.srv.RenewAWSSSOCredentials(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := reads(); got != first {
		t.Fatalf("secret.read rows = %d after three more passes, want %d: an unchanged credential was read again", got, first)
	}
}
