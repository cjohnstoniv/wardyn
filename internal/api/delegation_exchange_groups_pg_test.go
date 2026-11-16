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

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// groupsWriteFails is the Postgres store with a record of people's last sign-ins that cannot be written.
type groupsWriteFails struct{ store.PG }

func (groupsWriteFails) PutPrincipalGroups(context.Context, string, string, string, []string, bool) error {
	return errors.New("db down")
}

// A portal's token exchange leaves the person's email and groups, as the exchange carried them, on the
// record under the portal's id, and a later exchange replaces them; the record of a sign-in to Wardyn
// itself stays as it was, and an exchange the portal's scope refuses writes none.
func TestDelegation_ExchangeRecordsThePortalsGroups(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	id, cred := e.registerPortal(t, delegGroup)
	if err := e.st.PutPrincipalGroups(ctx, delegPerson, "", delegPersonEmail, []string{"eng"}, false); err != nil {
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
