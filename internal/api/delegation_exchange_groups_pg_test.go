// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// groupsWriteFails is the Postgres store with a record of people's last sign-ins that cannot be written.
type groupsWriteFails struct{ store.PG }

func (groupsWriteFails) PutPrincipalGroups(context.Context, string, string, string, []string, bool, time.Time) error {
	return errors.New("db down")
}

// A portal's token exchange leaves the person's email and groups, as the exchange carried them, on the
// record under the portal's id, and a later exchange replaces them; the record of a sign-in to Wardyn
// itself stays as it was, and an exchange the portal's scope refuses writes none.
func TestDelegation_ExchangeRecordsThePortalsGroups(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	id, cred := e.registerPortal(t, delegGroup)
	if err := e.st.PutPrincipalGroups(ctx, delegPerson, "", delegPersonEmail, []string{"eng"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.delegate(t, id, cred, delegPerson, delegPersonEmail, delegGroup, "ops")
	got, err := e.st.GetPrincipalGroups(ctx, delegPerson, id.String())
	if err != nil || got.Email != delegPersonEmail || len(got.Groups) != 2 || !slices.Contains(got.Groups, "ops") ||
		!slices.Contains(got.Groups, delegGroup) || got.Truncated {
		t.Fatalf("the portal's record = %+v, %v; want the exchange's email and both groups", got, err)
	}
	e.delegate(t, id, cred, delegPerson, delegPersonEmail, delegGroup)
	if got, err := e.st.GetPrincipalGroups(ctx, delegPerson, id.String()); err != nil || !slices.Equal(got.Groups, []string{delegGroup}) {
		t.Errorf("after a narrower exchange: %+v, %v; want its groups alone", got, err)
	}
	if got, err := e.st.GetPrincipalGroups(ctx, delegPerson, ""); err != nil || !slices.Equal(got.Groups, []string{"eng"}) {
		t.Errorf("the sign-in's own record = %+v, %v; want it untouched by the exchanges", got, err)
	}
	if w := e.exchange(t, id, cred, e.subjectToken(t, "out-sub", "out@corp.example", []string{"other"}, nil)); w.Code != http.StatusForbidden {
		t.Fatalf("outside the portal's scope: %d %s, want 403", w.Code, w.Body)
	}
	if _, err := e.st.GetPrincipalGroups(ctx, "out-sub", id.String()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused exchange wrote a record: %v", err)
	}
}

// A subject token issued before the one on the portal's record is still exchanged, but leaves the record
// as the newer token wrote it: an older token cannot put back a group the newer one no longer carried.
func TestDelegation_AnOlderSubjectTokenKeepsTheNewerRecord(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	id, cred := e.registerPortal(t, delegGroup)
	e.delegate(t, id, cred, delegPerson, delegPersonEmail, delegGroup)
	older := e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup, "eng"}, func(c map[string]any) {
		c["iat"] = time.Now().Add(-time.Minute).Unix()
	})
	if w := e.exchange(t, id, cred, older); w.Code != http.StatusOK {
		t.Fatalf("older subject token: %d %s, want 200", w.Code, w.Body)
	}
	if got, err := e.st.GetPrincipalGroups(ctx, delegPerson, id.String()); err != nil || !slices.Equal(got.Groups, []string{delegGroup}) {
		t.Fatalf("the portal's record = %+v, %v; want the newer token's groups alone", got, err)
	}
}

// An exchange the portal's scope refuses still narrows the portal's record to the groups its verified
// token carries and adds none; the sign-in's own record is left as it was. (A person with no record
// through the portal gets none: TestDelegation_ExchangeRecordsThePortalsGroups.)
func TestDelegation_ARefusedExchangeNarrowsThePortalsRecord(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	id, cred := e.registerPortal(t, delegGroup)
	if err := e.st.PutPrincipalGroups(ctx, delegPerson, "", delegPersonEmail, []string{"eng"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	admitted := e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup, "eng", "ops"}, func(c map[string]any) {
		c["iat"] = time.Now().Add(-time.Minute).Unix()
	})
	if w := e.exchange(t, id, cred, admitted); w.Code != http.StatusOK {
		t.Fatalf("admitted exchange: %d %s, want 200", w.Code, w.Body)
	}
	refused := e.subjectToken(t, delegPerson, delegPersonEmail, []string{"ops", "sales"}, nil)
	if w := e.exchange(t, id, cred, refused); w.Code != http.StatusForbidden {
		t.Fatalf("outside the portal's scope: %d %s, want 403", w.Code, w.Body)
	}
	got, err := e.st.GetPrincipalGroups(ctx, delegPerson, id.String())
	if err != nil || !slices.Equal(got.Groups, []string{"ops"}) {
		t.Fatalf("the portal's record after a refused exchange = %+v, %v; want ops alone", got, err)
	}
	if got, err := e.st.GetPrincipalGroups(ctx, delegPerson, ""); err != nil || !slices.Equal(got.Groups, []string{"eng"}) {
		t.Errorf("the sign-in's own record = %+v, %v; want it untouched", got, err)
	}
}

// An exchange whose record cannot be written issues no token: 503 temporarily_unavailable.
func TestDelegation_ExchangeRefusedWhenTheRecordFails(t *testing.T) {
	e := newDelegationPG(t)
	id, cred := e.registerPortal(t, delegGroup)
	e.h.srv.cfg.Store = groupsWriteFails{PG: e.st}
	w := e.exchange(t, id, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "temporarily_unavailable") {
		t.Fatalf("exchange = %d %s, want 503 temporarily_unavailable", w.Code, w.Body)
	}
	if n := e.delegatedTokenCount(t); n != 0 {
		t.Errorf("%d delegated token(s) issued without a record", n)
	}
}

// narrowFails is the Postgres store whose record of people's last exchanges cannot be narrowed.
type narrowFails struct{ store.PG }

func (narrowFails) NarrowPrincipalGroups(context.Context, string, string, []string, bool, time.Time) error {
	return errors.New("db down")
}

// An exchange the portal's scope refuses whose record cannot be narrowed is answered 503
// temporarily_unavailable, not the scope's 403, so the person is not told they are outside the portal
// while the record still holds the groups their token no longer carries.
func TestDelegation_ARefusedExchangeWhoseNarrowFailsIsUnavailable(t *testing.T) {
	e := newDelegationPG(t)
	id, cred := e.registerPortal(t, delegGroup)
	e.delegate(t, id, cred, delegPerson, delegPersonEmail, delegGroup)
	e.h.srv.cfg.Store = narrowFails{PG: e.st}
	w := e.exchange(t, id, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{"ops"}, nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "temporarily_unavailable") {
		t.Fatalf("exchange = %d %s, want 503 temporarily_unavailable", w.Code, w.Body)
	}
}
