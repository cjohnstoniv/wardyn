// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE STATE-MACHINE REPLAY. One request's whole lifecycle, driven as a SEQUENCE
// of events rather than a set of independent cases — because the properties
// that matter are about ORDER: an older sign-in must not answer a newer
// request, a cancelled one must not come back, and a resolve after a kill must
// not resume.
//
// Each step names the event, and the assertion is the invariant, not the code.

type fsmEvent struct {
	name string
	do   func(t *testing.T, f *reauthFixture)
	want func(t *testing.T, f *reauthFixture)
}

func evResolve(wantCode int) fsmEvent {
	return fsmEvent{
		name: "resolve",
		do:   func(*testing.T, *reauthFixture) {},
		want: func(t *testing.T, f *reauthFixture) {
			if w := f.resolve(t); w.Code != wantCode {
				t.Fatalf("resolve: code = %d, want %d; body=%s", w.Code, wantCode, w.Body.String())
			}
		},
	}
}

func evCredential(blob awsSSOBlob) fsmEvent {
	return fsmEvent{
		name: "credential becomes " + blobWord(blob),
		do:   func(t *testing.T, f *reauthFixture) { f.putBlob(t, "alice@example.com", blob) },
		want: func(*testing.T, *reauthFixture) {},
	}
}

func blobWord(b awsSSOBlob) string {
	if b.expired(time.Now()) {
		return "dead"
	}
	return "live"
}

// evCapture is a sign-in landing, with the login run's OWN creation time — the
// generation check (I6) is the whole reason this is a parameter.
func evCapture(offset time.Duration, owner string) fsmEvent {
	return fsmEvent{
		name: "capture by " + owner,
		do: func(t *testing.T, f *reauthFixture) {
			run := types.AgentRun{ID: uuid.New(), CreatedBy: owner, CreatedAt: time.Now().Add(offset)}
			f.st.loginRun = run
			fresh := liveSSOBlob()
			fresh.AccessToken = "token-after-" + owner
			fresh.SourceRunID = run.ID.String()
			f.putBlob(t, owner, fresh)
			f.srv.resolvePendingReauth(context.Background(),
				awsSSOScope{perUser: true, owner: owner}, owner, run)
		},
		want: func(*testing.T, *reauthFixture) {},
	}
}

func evKillRun() fsmEvent {
	return fsmEvent{
		name: "the held run ends",
		do: func(t *testing.T, f *reauthFixture) {
			if _, err := f.srv.cfg.Approvals.CancelForRun(context.Background(), f.runID, "run_killed"); err != nil {
				t.Fatal(err)
			}
		},
		want: func(*testing.T, *reauthFixture) {},
	}
}

func evRowState(want types.ApprovalState) fsmEvent {
	return fsmEvent{
		name: "the request reads " + string(want),
		do:   func(*testing.T, *reauthFixture) {},
		want: func(t *testing.T, f *reauthFixture) {
			rows, err := f.srv.cfg.Approvals.List(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, ap := range rows {
				if ap.Kind == types.ApprovalCredentialReauth {
					if ap.State != want {
						t.Fatalf("the request is %s, want %s", ap.State, want)
					}
					return
				}
			}
			t.Fatalf("no credential_reauth row at all, want one in %s", want)
		},
	}
}

func evRowCount(want int) fsmEvent {
	return fsmEvent{
		name: "exactly " + string(rune('0'+want)) + " request(s) exist",
		do:   func(*testing.T, *reauthFixture) {},
		want: func(t *testing.T, f *reauthFixture) {
			rows, _ := f.srv.cfg.Approvals.List(context.Background(), "")
			n := 0
			for _, ap := range rows {
				if ap.Kind == types.ApprovalCredentialReauth {
					n++
				}
			}
			if n != want {
				t.Fatalf("credential_reauth rows = %d, want %d", n, want)
			}
		},
	}
}

func TestCredentialReauth_StateMachineReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []fsmEvent
	}{
		{
			// The straight line the lane exists for.
			name: "live, lapse, ask, sign in, resume",
			events: []fsmEvent{
				evCredential(liveSSOBlob()), evResolve(http.StatusOK),
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked), evRowCount(1), evRowState(types.ApprovalPending),
				evCapture(time.Second, "alice@example.com"), evRowState(types.ApprovalApproved),
				evResolve(http.StatusOK),
			},
		},
		{
			// Two resolves while one request is open share it. N concurrent
			// GetRoleCredentials must be ONE human question.
			name: "two lapses in flight are one request",
			events: []fsmEvent{
				evCredential(deadSSOBlob()),
				evResolve(http.StatusLocked), evResolve(http.StatusLocked), evResolve(http.StatusLocked),
				evRowCount(1),
			},
		},
		{
			// I6 — an OLDER sign-in cannot answer a NEWER request, and the
			// newer one is still open afterwards.
			name: "a stale sign-in resolves nothing",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				// A login sandbox alive from BEFORE the lapse uploads. It stored
				// a usable credential, and the row still does not move: an older
				// sign-in cannot be a response to a newer question.
				evCapture(-time.Hour, "alice@example.com"), evRowState(types.ApprovalPending),
				// The accepted cost, stated rather than hidden: the hold waits on
				// the ROW, so it runs to its budget even though the credential is
				// live again — and then the SDK's own retry re-resolves and gets
				// it (the resolve below is that retry). The credential is never
				// wrong; the wait is merely longer than it had to be.
				evResolve(http.StatusOK),
				// Put the lapse back: the request is STILL the one that was
				// raised, not a second one.
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked), evRowCount(1),
			},
		},
		{
			// I2 — another member's sign-in cannot answer it either.
			name: "another member's sign-in resolves nothing",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				evCapture(time.Second, "bob@corp.example"), evRowState(types.ApprovalPending),
			},
		},
		{
			// A cancelled request is TERMINAL: the resolve answers 403 and no
			// second row is raised, so a sidecar never holds on a dead one and a
			// killed run never resumes.
			name: "a killed run's request is terminal, and stays terminal",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				evKillRun(), evRowState(types.ApprovalCancelled),
				evResolve(http.StatusForbidden), evResolve(http.StatusForbidden), evRowCount(1),
			},
		},
		{
			// …and a LIVE credential after the kill still resolves: the row's
			// state gates the DEAD path only. A run whose approvals were
			// cancelled but which is still resolving is a torn-down sidecar's
			// last call, and it gets the credential it was authorized for.
			name: "a cancelled request does not poison a live credential",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked), evKillRun(),
				evCredential(liveSSOBlob()), evResolve(http.StatusOK),
			},
		},
		{
			// NIT-7 — "kill, capture": a capture landing on a CANCELLED row.
			// The row must NOT come back, and the credential must still be
			// usable: the sign-in was real, it simply has no request to answer.
			name: "a capture after the run was killed resolves nothing and poisons nothing",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				evKillRun(), evRowState(types.ApprovalCancelled),
				evCapture(time.Second, "alice@example.com"),
				evRowState(types.ApprovalCancelled), evRowCount(1),
				// The captured credential is live, so a resolve serves it.
				evResolve(http.StatusOK),
			},
		},
		{
			// NIT-7 — "timeout, capture": the hold ran out in the sidecar, so
			// the row is STILL PENDING (the sign-in is still wanted) and the
			// capture resolves it normally. That is the whole reason the row
			// deliberately outlives the hold.
			name: "a capture after the hold timed out still resolves the request",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				// The sidecar's budget ended; nothing in the control plane moved.
				evRowState(types.ApprovalPending),
				evCapture(time.Second, "alice@example.com"), evRowState(types.ApprovalApproved),
				evResolve(http.StatusOK), evRowCount(1),
			},
		},
		{
			// A SECOND lapse after a successful sign-in is a NEW workflow, not a
			// resurrection of the resolved one.
			name: "a second lapse raises a second request",
			events: []fsmEvent{
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked),
				evCapture(time.Second, "alice@example.com"), evResolve(http.StatusOK),
				evCredential(deadSSOBlob()), evResolve(http.StatusLocked), evRowCount(2),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReauthFixture(t, nil)
			for i, ev := range tc.events {
				ev.do(t, f)
				ev.want(t, f)
				_ = i
			}
		})
	}
}

// I8 — SECRET NON-OBSERVABILITY, across every sink this package writes to: the
// audit rows, and every HTTP body the sandbox-facing side can read. A refusal
// names a reason; it never echoes a credential.
func TestCredentialReauth_TokenIsInNoSink(t *testing.T) {
	f := newReauthFixture(t, nil)
	live := liveSSOBlob()
	f.putBlob(t, "alice@example.com", live)

	bodies := []string{}
	// The success path: the 200 body legitimately CARRIES the token (that is
	// the point of the endpoint), so it is excluded — everything else is not.
	if w := f.resolve(t); w.Code != http.StatusOK {
		t.Fatalf("resolve: %d", w.Code)
	}

	// Every refusal shape, in one run.
	f.brk.setHost("evil.example.com")
	bodies = append(bodies, f.resolve(t).Body.String())
	f.brk.setHost(reauthPortal)
	f.st.site = reauthRosterRow(false)
	bodies = append(bodies, f.resolve(t).Body.String())
	f.st.site = reauthRosterRow(true)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	bodies = append(bodies, f.resolve(t).Body.String())

	for i, b := range bodies {
		for _, secret := range []string{live.AccessToken, live.RefreshToken, live.ClientSecret} {
			if secret != "" && strings.Contains(b, secret) {
				t.Errorf("body %d carries credential material: %s", i, b)
			}
		}
	}
	for _, ev := range f.audit.events() {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), live.AccessToken) {
			t.Errorf("an audit row carries the session token: %s", raw)
		}
	}
}
