// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every writer of a list that can name a person calls the ruling itself, not only the shared helper: each is
// driven here with a portal person. A refuse list refuses one whose issuer is bound as well as one whose
// issuer is not (ErrDelegatePersonNotAllowed). An allow list refuses one of an unbound issuer
// (ErrDelegatePersonUnknownPortal), and lets the ruling pass one of a bound issuer, so the write goes on to
// fail or succeed on its own account.
func TestPortalSubjectRuling_EveryWriterRules_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	bound := types.PortalPrincipal(newIssuerPortal(t, st, "").Key, "alice")
	unbound := types.PortalPrincipal(types.PortalIssuerKey("https://never-bound.example.com"), "alice")
	user := types.CapabilitySubjectUser
	keys := keydomain.NewService(pool, nil)

	refuse := map[string]func(subject string) error{
		"GrantUnitAdminQ": func(s string) error {
			_, err := store.GrantUnitAdminQ(ctx, pool, types.GovernanceUnitAdmin{UnitID: uuid.New(), SubjectType: user, Subject: s, GrantedBy: "admin@example.com"})
			return err
		},
		"PutTemplateGroupAdmin": func(s string) error {
			_, err := st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "group-" + uuid.NewString(), Person: s, GrantedBy: "admin@example.com"})
			return err
		},
		"UpsertRoleMappingQ": func(s string) error {
			_, err := store.UpsertRoleMappingQ(ctx, pool, types.RoleMapping{Value: s, Role: "user", UserType: "standard", CreatedBy: "admin@example.com"})
			return err
		},
		"SetRemoteRunnerPeopleQ": func(s string) error {
			_, err := store.SetRemoteRunnerPeopleQ(ctx, pool, "runner-"+uuid.NewString(), nil, types.PeoplePoolsListed,
				[]types.RunnerPoolSubject{{SubjectType: user, Subject: s}}, "admin@example.com")
			return err
		},
		"CreateServiceIdentityQ": func(s string) error {
			_, err := store.CreateServiceIdentityQ(ctx, pool, types.ServiceIdentity{Name: "svc-" + uuid.NewString(), Sponsor: s})
			return err
		},
		"UpdateServiceIdentityQ": func(s string) error {
			_, err := store.UpdateServiceIdentityQ(ctx, pool, "svc-"+uuid.NewString(), store.ServiceIdentityChange{Sponsor: &s, By: "admin@example.com"})
			return err
		},
		"keydomain.Service.Set": func(s string) error {
			_, err := keys.Set(ctx, keydomain.Assignment{SubjectType: string(user), Subject: s, Domain: keydomain.Default, SetBy: "admin@example.com"})
			return err
		},
	}
	allow := map[string]func(subject string) error{
		"UpsertCapabilityGrantQ": func(s string) error {
			_, err := store.UpsertCapabilityGrantQ(ctx, pool, capabilityGrantFor(s))
			return err
		},
		"UpsertGovernanceMembershipQ": func(s string) error {
			_, err := store.UpsertGovernanceMembershipQ(ctx, pool, types.GovernanceMembership{SubjectType: user, Subject: s, UnitID: uuid.New(), CreatedBy: "admin@example.com"})
			return err
		},
		"InsertGovernanceMembershipQ": func(s string) error {
			return store.InsertGovernanceMembershipQ(ctx, pool, types.GovernanceMembership{SubjectType: user, Subject: s, UnitID: uuid.New(), CreatedBy: "admin@example.com"})
		},
		"UpsertUserDriveGrant": func(s string) error {
			_, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: user, Subject: s, DriveID: uuid.New()}, false)
			return err
		},
		"ReplaceUserDriveGrant": func(s string) error {
			_, _, err := st.ReplaceUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: user, Subject: s, DriveID: uuid.New()}, false)
			return err
		},
		"AddUserDriveGrant": func(s string) error {
			_, err := st.AddUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: user, Subject: s, DriveID: uuid.New()}, false)
			return err
		},
		"PutRunnerPoolUsePolicyQ": func(s string) error {
			_, err := store.PutRunnerPoolUsePolicyQ(ctx, pool, uuid.New(), nil, []types.RunnerPoolSubject{{SubjectType: user, Subject: s}}, "admin@example.com")
			return err
		},
		"PutRestrictionQ": func(s string) error {
			_, err := store.PutRestrictionQ(ctx, pool, types.Restriction{ID: uuid.New(), Name: "r-" + uuid.NewString(),
				Members: []types.RestrictionMember{{SubjectType: user, Subject: s}}, CreatedBy: "admin@example.com"}, "admin@example.com")
			return err
		},
	}
	for name, write := range refuse {
		for _, who := range []string{bound, unbound} {
			if err := write(who); !errors.Is(err, store.ErrDelegatePersonNotAllowed) {
				t.Errorf("%s with %s = %v, want ErrDelegatePersonNotAllowed", name, who, err)
			}
		}
	}
	for name, write := range allow {
		if err := write(unbound); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
			t.Errorf("%s with an unbound person = %v, want ErrDelegatePersonUnknownPortal", name, err)
		}
		if err := write(bound); errors.Is(err, store.ErrDelegatePersonUnknownPortal) || errors.Is(err, store.ErrDelegatePersonNotAllowed) {
			t.Errorf("%s with a bound person = %v, want the ruling to pass", name, err)
		}
	}
}
