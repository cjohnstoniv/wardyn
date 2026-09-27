// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// eraseFixture seeds the operator namespace and two people, and wires the
// identity directory so bob can be named by his email.
func eraseFixture(t *testing.T, sec secretstore.Store) (*harness, *Server) {
	t.Helper()
	ctx := context.Background()
	seed := map[string][]string{"": {"npm-token"}, "bob": {"anthropic-api-key", "wardyn-harness-aws-oauth"}, "alice": {"anthropic-api-key"}}
	for owner, names := range seed {
		for _, n := range names {
			if err := sec.For(owner).Put(ctx, n, []byte("seeded-credential-value-0000")); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Secrets = sec
	h.srv.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{{ID: uuid.New(), Principal: "bob", Email: "bob@corp.example"}}}
	h.srv.router = h.srv.routes()
	return h, h.srv
}

func namesOf(t *testing.T, sec secretstore.Store, owner string) []string {
	t.Helper()
	names, err := sec.For(owner).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

// An erase removes every credential the named person holds — their stored
// sign-ins included — and nothing of anyone else's or the operator's. The
// security tier may do it, and it is audited with a count, never a value.
func TestErasePersonCredentials_RemovesOnlyThatPerson(t *testing.T) {
	for _, form := range []string{"bob", "bob@corp.example"} {
		t.Run(form, func(t *testing.T) {
			sec := &memSecrets{m: map[string][]byte{}}
			h, srv := eraseFixture(t, sec)
			sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
			w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+form+"/credentials", sa, "")
			if w.Code != http.StatusOK {
				t.Fatalf("erase %s = %d %s, want 200", form, w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["count"] != float64(2) {
				t.Fatalf("erase body = %s (%v), want count 2", w.Body.String(), err)
			}
			if got := namesOf(t, sec, "bob"); len(got) != 0 {
				t.Fatalf("bob still holds %v after the erase", got)
			}
			if got := namesOf(t, sec, "alice"); !slices.Equal(got, []string{"anthropic-api-key"}) {
				t.Fatalf("alice holds %v, want her credential untouched", got)
			}
			if got := namesOf(t, sec, ""); !slices.Equal(got, []string{"npm-token"}) {
				t.Fatalf("the operator namespace holds %v, want it untouched", got)
			}
			ev := lastAuditEvent(t, h.audit.events, "credential.erase")
			if owner, _ := auditDataField(t, ev, "secret_owner"); ev.Outcome != "success" || ev.Target != "bob" || owner != "bob" {
				t.Fatalf("credential.erase row = %+v, want success for bob", ev)
			}
		})
	}
}

// failingDeleteSecrets is memSecrets whose Delete refuses one name.
type failingDeleteSecrets struct {
	*memSecrets
	refuse string
}

func (f failingDeleteSecrets) For(owner string) secretstore.Store {
	return failingDeleteSecrets{f.memSecrets.For(owner).(*memSecrets), f.refuse}
}

func (f failingDeleteSecrets) Delete(ctx context.Context, name string) error {
	if name == f.refuse {
		return errors.New("the store refused the delete")
	}
	return f.memSecrets.Delete(ctx, name)
}

// Fail closed: an erase that could not delete every credential never answers
// or audits success, and says how many it did delete.
func TestErasePersonCredentials_PartialFailureIsNotSuccess(t *testing.T) {
	sec := failingDeleteSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}, refuse: "wardyn-harness-aws-oauth"}
	h, srv := eraseFixture(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("erase with a refused delete = %d %s, want 500", w.Code, w.Body.String())
	}
	if got := namesOf(t, sec, "bob"); !slices.Equal(got, []string{"wardyn-harness-aws-oauth"}) {
		t.Fatalf("bob holds %v, want only the credential the store refused to delete", got)
	}
	for _, ev := range h.audit.events {
		if ev.Action == "credential.erase" && ev.Outcome == "success" {
			t.Fatalf("a partial erase audited success: %s", ev.Data)
		}
	}
	ev := lastAuditEvent(t, h.audit.events, "credential.erase")
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if ev.Outcome != "failure" || data["count"] != float64(1) {
		t.Fatalf("credential.erase row = %s %s, want failure with count 1", ev.Outcome, ev.Data)
	}
}

// In Key Vault store mode the erase says what the vault kept, so the console
// can tell the person their credentials stay recoverable there (§3
// ERASE.BODY_AZURE).
func TestErasePersonCredentials_SaysWhatTheStoreKept(t *testing.T) {
	sec := reportingSecrets{memSecrets: &memSecrets{m: map[string][]byte{}},
		rep: secretstore.DeleteReport{Store: "azurekv", RecoverableDays: 90}}
	_, srv := eraseFixture(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, "")
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", w.Code, w.Body.String())
	}
	if body["store"] != "azurekv" || body["purged"] != false || body["recoverable_days"] != float64(90) {
		t.Fatalf("erase body = %s, want store azurekv, purged false, recoverable_days 90", w.Body.String())
	}
}

// sweepSecrets is memSecrets with the pg store's DeleteExpired.
type sweepSecrets struct {
	*memSecrets
	gone []secretstore.Expired
	err  error
}

func (s *sweepSecrets) DeleteExpired(context.Context) ([]secretstore.Expired, error) { return s.gone, s.err }

// wardynd serves with the store wrapped in secretstore.Audited; the sweep must
// still reach the wrapped store's DeleteExpired through it.
func TestSweepExpiredCredentials_ReachesThroughTheAuditedWrapper(t *testing.T) {
	h := newHarness(t)
	inner := &sweepSecrets{memSecrets: &memSecrets{m: map[string][]byte{}},
		gone: []secretstore.Expired{{Owner: "bob", Name: "wardyn-harness-aws-oauth", ExpiresAt: time.Now().Add(-time.Hour)}}}
	h.srv.cfg.Secrets = secretstore.Audited(inner, nil)
	if n := h.srv.SweepExpiredCredentials(context.Background()); n != 1 {
		t.Fatalf("swept %d through the audited wrapper, want 1", n)
	}
}

// A wrapped store that cannot sweep is least retention switched off: the
// sweep says so at Error, once per process, and reports nothing deleted.
func TestSweepExpiredCredentials_WrappedStoreWithoutSweepLogsOnce(t *testing.T) {
	noSweepOnce = sync.Once{}
	t.Cleanup(func() { noSweepOnce = sync.Once{} })
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t)
	h.srv.cfg.Secrets = secretstore.Audited(&memSecrets{m: map[string][]byte{}}, nil)
	for range 2 {
		if n := h.srv.SweepExpiredCredentials(context.Background()); n != 0 {
			t.Fatalf("swept %d with a store that cannot sweep, want 0", n)
		}
	}
	const msg = "the secret store has no expiry sweep"
	if got := strings.Count(logged.String(), msg); got != 1 || !strings.Contains(logged.String(), "level=ERROR") {
		t.Fatalf("logged %q %d times (want once, at ERROR): %s", msg, got, logged.String())
	}
}

// The daily sweep audits every credential it deleted, even when it also left
// some behind, and a store without the sweep is a no-op.
func TestSweepExpiredCredentials_AuditsEachDeletion(t *testing.T) {
	h := newHarness(t)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	h.srv.cfg.Secrets = &sweepSecrets{memSecrets: &memSecrets{m: map[string][]byte{}},
		gone: []secretstore.Expired{{Owner: "bob", Name: "wardyn-harness-aws-oauth", ExpiresAt: at}},
		err:  errors.New("one row was kept")}
	if n := h.srv.SweepExpiredCredentials(context.Background()); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	ev := lastAuditEvent(t, h.audit.events, "credential.expired.delete")
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if ev.Target != "wardyn-harness-aws-oauth" || data["secret_owner"] != "bob" || data["reason"] != "expired" ||
		data["expires_at"] != at.Format(time.RFC3339) || ev.ActorType != types.ActorSystem {
		t.Fatalf("credential.expired.delete row = %+v %s", ev, ev.Data)
	}

	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	if n := h.srv.SweepExpiredCredentials(context.Background()); n != 0 {
		t.Fatalf("a store without a sweep swept %d", n)
	}
}
