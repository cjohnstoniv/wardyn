// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A purge deletes the invitations that target the person and clears their name from the invitations and
// service identities they created, revoked, suspended or retired. A service identity they still sponsor
// keeps the purge pending, named in the ledger, until it is handed to another sponsor.
func TestSCIMPurgeErasesInvitationsAndWaitsOnSponsorships(t *testing.T) {
	const bystander = "sub-bystander"
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.seedEntra(bystander, "bystander@corp.example", "aaaaaaaa-0000-4000-8000-0000000000b2")
	ctx := context.Background()
	now := time.Now().UTC()
	invite := func(target, by string) uuid.UUID {
		inv, err := e.st.CreateInvitation(ctx, types.Invitation{ID: uuid.New(), Target: target, CreatedBy: by, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, 10, now)
		if err != nil {
			t.Fatal(err)
		}
		return inv.ID
	}
	toLeaver := invite(purgeSub, "admin")
	sent := invite(bystander, purgeSub)
	revoked := invite(bystander, "admin")
	if _, err := e.st.RevokeInvitation(ctx, revoked, purgeSub, now); err != nil {
		t.Fatal(err)
	}
	for _, si := range []types.ServiceIdentity{
		{Name: "ci-bot", Sponsor: purgeSub, UserType: types.UserTypeStandard, CreatedBy: purgeSub},
		{Name: "ops-bot", Sponsor: bystander, UserType: types.UserTypeStandard, CreatedBy: purgeSub},
	} {
		if _, err := e.st.CreateServiceIdentity(ctx, si); err != nil {
			t.Fatal(err)
		}
	}

	if w := e.del(e.a, f.id); w.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE while sponsoring = %d %s, want 500 (the purge stays pending)", w.Code, w.Body.String())
	}
	if _, err := e.st.GetInvitation(ctx, toLeaver, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an invitation to the leaver survived the purge: %v", err)
	}
	for _, id := range []uuid.UUID{sent, revoked} {
		inv, err := e.st.GetInvitation(ctx, id, "")
		if err != nil || inv.CreatedBy == purgeSub || inv.RevokedBy == purgeSub {
			t.Errorf("another person's invitation after the purge = %+v, %v; want it kept without the leaver's name", inv, err)
		}
	}
	if si, err := e.st.GetServiceIdentity(ctx, "ops-bot"); err != nil || si.CreatedBy != "" {
		t.Errorf("ops-bot after the purge = %+v, %v; want its creator cleared", si, err)
	}
	if si, err := e.st.GetServiceIdentity(ctx, "ci-bot"); err != nil || si.Sponsor != purgeSub {
		t.Errorf("ci-bot after the purge = %+v, %v; want its sponsor kept", si, err)
	}
	jobs, err := e.st.ListDeprovisionJobs(ctx, uuid.MustParse(f.id), store.JobKindPurge)
	if err != nil {
		t.Fatal(err)
	}
	if j, ok := jobByKey(jobs, jobStepSponsor, ""); !ok || j.Done || !strings.Contains(j.LastError, "ci-bot") {
		t.Errorf("service identity step = %+v, %v; want pending, naming ci-bot", j, ok)
	}
	if len(e.purgeRows(e.a)) != 0 {
		t.Error("a purge still waiting on a sponsorship wrote its person.deprovision row")
	}

	other := bystander
	if _, err := e.st.UpdateServiceIdentity(ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &other, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent || len(e.purgeRows(e.a)) != 1 {
		t.Fatalf("DELETE after the hand-over = %d %s, purge rows %d; want 204 and one", w.Code, w.Body.String(), len(e.purgeRows(e.a)))
	}
}
