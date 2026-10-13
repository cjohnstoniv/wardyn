// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An event stream an API token opened is checked as the token is: the person signing out of the browser
// (a browser-only cut) leaves it open, and a reconnect after the sign-out opens; revoking the person's
// sessions and tokens, or a deactivation of their identity, ends it at the next keepalive. The run stays
// live, so nothing but the owner check can end the stream.
func TestPG_TokenEventStreamOutlivesABrowserSignOutAndEndsOnARevoke(t *testing.T) {
	const sub, email = "sub-leaver", "leaver@corp.example"
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, e *scimEnv)
	}{
		{"revoke", func(t *testing.T, e *scimEnv) {
			admin := accessSession(t, "sec", "sec@corp.example", "security_admin", []string{})
			if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"`+sub+`"}`); w.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
			}
		}},
		{"identity deactivation", func(t *testing.T, e *scimEnv) {
			if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, sub); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSCIMEnv(t)
			row := e.seedEntra(sub, email, oidLeaver)
			cookie := e.orderedCookieAt(sub, email, row.AuthorityEpoch, time.Now().UTC())
			w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/me/tokens", cookie, `{"name":"cli"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("mint = %d %s", w.Code, w.Body.String())
			}
			token := decodeToken(t, w).Token
			run := e.seedRun(sub, types.RunRunning)
			e.a.srv.runEvents.hold, e.a.srv.runEvents.beat = 0, 10*time.Millisecond
			ts := httptest.NewServer(panicFails(t, e.a.srv.Handler()))
			t.Cleanup(ts.Close)

			// watch opens a stream with the token, waits for three keepalives, and returns a channel closed when
			// the server ends the stream. The body is read continuously, so a close is seen when it happens.
			watch := func(ctx context.Context) <-chan struct{} {
				t.Helper()
				resp := openEventsAs(t, ctx, ts.URL, run, token)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("open the stream = %d", resp.StatusCode)
				}
				held, ended := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(ended)
					sc := bufio.NewScanner(resp.Body)
					for seen := 0; sc.Scan(); {
						if sc.Text() == ": keepalive" {
							if seen++; seen == 3 {
								close(held)
							}
						}
					}
				}()
				select {
				case <-held:
				case <-ended:
					t.Fatal("the stream closed before its third keepalive")
				}
				return ended
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ended := watch(ctx)

			// The person signs out of the browser: the cookie is cut, the token is not.
			if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/auth/logout", cookie, ""); w.Code != http.StatusFound {
				t.Fatalf("sign-out = %d %s", w.Code, w.Body.String())
			}
			if e.cookieWorks(e.a, cookie) {
				t.Fatal("premise: the browser session still works after the sign-out")
			}
			select {
			case <-ended:
				t.Fatal("a browser sign-out ended the event stream the API token opened")
			case <-time.After(300 * time.Millisecond): // thirty keepalives, each one a re-check
			}
			again, stop := context.WithCancel(ctx)
			watch(again) // a reconnect after the sign-out
			stop()

			tc.end(t, e)
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
				t.Fatalf("the token's event stream outlived the %s", tc.name)
			}
		})
	}
}

// A service identity's token opens the stream, and a revoke of its sponsor ends it though the revoke's
// time reads before the moment the stream opened (the wall clock stepped back between the two): the
// stream is ordered against the sponsor's cuts by the token's revocation sequence.
func TestPG_ServiceTokenEventStreamEndsOnItsSponsorsRevokeUnderABackwardClockStep(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	if seq := tokenRevSeq(t, e.pool, tok.ID); seq <= 0 {
		t.Fatalf("service token rev_seq = %d, want the sequence at mint", seq)
	}
	id := uuid.New()
	run, err := e.st.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedBy: types.ServicePrincipalPrefix + svcName, Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.RunRunning {
		if ok, err := e.st.UpdateRunStateIf(context.Background(), id, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("move the run to RUNNING: %v %v", ok, err)
		}
	}
	e.h.srv.runEvents.hold, e.h.srv.runEvents.beat = 0, 10*time.Millisecond
	ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp := openEventsAs(t, ctx, ts.URL, id, tok.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open the stream = %d", resp.StatusCode)
	}
	held, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(resp.Body)
		for seen := 0; sc.Scan(); {
			if sc.Text() == ": keepalive" {
				if seen++; seen == 3 {
					close(held)
				}
			}
		}
	}()
	select {
	case <-held: // three keepalives, each one a re-check that found the sponsor live
	case <-ended:
		t.Fatal("the stream closed before its third keepalive")
	}
	backdatedRevoke(t, e.pool, svcSponsor)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the service token's event stream outlived its sponsor's revoke")
	}
}
