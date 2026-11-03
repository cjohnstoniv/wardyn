// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestKeptMachineRoutesRefuseCallersWhoAreNotPeople: only a person's session or own token lists or
// deletes their kept machines; a device, a delegated caller, SCIM, a service identity, the admin token
// and local mode are refused before anything is read.
func TestKeptMachineRoutesRefuseCallersWhoAreNotPeople(t *testing.T) {
	person := personCtx(aliceSub)
	for name, ctx := range map[string]context.Context{
		"admin token":      context.Background(),
		"local mode":       withLocalPrincipal(context.Background(), "local:alice"),
		"device":           context.WithValue(person, deviceCtxKey{}, types.Device{ID: uuid.New()}),
		"delegated":        audit.WithDelegation(person, types.DelegationVia{}),
		"service identity": withServiceIdentity(person, "ci-bot", aliceSub),
		"scim":             withSCIMCaller(person, "primary"),
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := memberDriveServer(newMemberDriveStore(), &memberDriveRunner{}, "docker")
			body := wantReason(t, memberCall(ctx, srv.handleListMyMachines, http.MethodGet, "/api/v1/me/machines", "", nil),
				http.StatusForbidden, reasonKeptMachineNotYourMac)
			if body.Error != keptMachineNotYourMacMsg() {
				t.Errorf("list: %q, want %q", body.Error, keptMachineNotYourMacMsg())
			}
			id := uuid.NewString()
			wantReason(t, memberCall(ctx, srv.handleDeleteMyMachine, http.MethodDelete, "/api/v1/me/machines/"+id, "", map[string]string{"id": id}),
				http.StatusForbidden, reasonKeptMachineNotYourMac)
		})
	}
}

// TestKeptMachineGovernanceDenyComesBeforeTheMac: a profile's deny_kept_machines refuses keeping with
// governance_unit before the caller's Macs are read, whatever the limit says.
func TestKeptMachineGovernanceDenyComesBeforeTheMac(t *testing.T) {
	srv, _ := memberDriveServer(newMemberDriveStore(), &memberDriveRunner{}, "docker")
	ceiling := governanceCeiling{Limits: types.GovernanceLimits{DenyKeptMachines: true}}
	got, err := srv.keptMachineKeepRefusal(personCtx(aliceSub), nil, aliceSub, ceiling, 0, 1)
	if err != nil || got != string(authz.ReasonGovernanceUnit) {
		t.Fatalf("deny_kept_machines: %q, %v; want %s", got, err, authz.ReasonGovernanceUnit)
	}
}

// TestKeptMachineDeleteReasonNamesWhoDeletesIt: the list says expired for an expiry, admin for an admin's
// delete or kill, person and erasure for theirs, and nothing for a delete the person never sees.
func TestKeptMachineDeleteReasonNamesWhoDeletesIt(t *testing.T) {
	for by, want := range map[string]string{
		keptMachineByOwner: "person", keptMachineByExpiry: "expired", keptMachineByAdmin: "admin",
		keptMachineByNonOwnerKill: "admin", keptMachineByErasure: "erasure", keptMachineByRevokedRunner: "",
	} {
		if got := keptMachineDeleteReason(by); got != want {
			t.Errorf("by %s: %q, want %q", by, got, want)
		}
	}
}

// scimKeptStore is the purge fake with kept machines: only the list the purge's audit row reads.
type scimKeptStore struct {
	*scimCovStore
	store.KeptMachineStore
	kept []types.KeptMachine
}

func (s *scimKeptStore) ListKeptMachinesOf(_ context.Context, owner string) ([]types.KeptMachine, error) {
	var out []types.KeptMachine
	for _, m := range s.kept {
		if m.OwnerSubject == owner {
			out = append(out, m)
		}
	}
	return out, nil
}

// TestSCIMPurgeListsKeptMachines: a purge's audit row lists the kept machines the person still has, by
// id, beside their drives, and no one else's; with none it carries no kept_machine_ids at all.
func TestSCIMPurgeListsKeptMachines(t *testing.T) {
	cov := newSCIMCovStore()
	l := scimCovSeedPurgee(cov)
	mine, theirs := types.KeptMachine{ID: uuid.New(), OwnerSubject: "sub-pat"}, types.KeptMachine{ID: uuid.New(), OwnerSubject: "sub-bystander"}
	e := newSCIMCovEnv(t, &scimKeptStore{scimCovStore: cov, kept: []types.KeptMachine{mine, theirs}})
	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	if got := fmt.Sprint(e.auditData(e.purgeRow())["kept_machine_ids"]); got != fmt.Sprint([]string{mine.ID.String()}) {
		t.Errorf("kept_machine_ids = %s, want theirs only: [%s]", got, mine.ID)
	}

	cov = newSCIMCovStore()
	l = scimCovSeedPurgee(cov)
	e = newSCIMCovEnv(t, &scimKeptStore{scimCovStore: cov})
	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	if _, ok := e.auditData(e.purgeRow())["kept_machine_ids"]; ok {
		t.Error("a purge of a person with no kept machines carries kept_machine_ids")
	}
}
