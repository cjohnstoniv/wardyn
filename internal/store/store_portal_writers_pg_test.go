// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every writer of a list that can name a person calls the ruling itself, not only the shared helper: each is
// driven here with a portal person. A refuse list refuses one whose issuer is bound as well as one whose
// issuer is not (ErrDelegatePersonNotAllowed). An allow list refuses one of an unbound issuer
// (ErrDelegatePersonUnknownPortal) and stores one of a bound issuer, which is read back.
func TestPortalSubjectRuling_EveryWriterRules_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	bound := types.PortalPrincipal(p.Key, "alice")
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
	// The rows an allow-list write refers to exist: the Default unit, a drive and an organization pool.
	drive, err := st.UpsertUserDrive(ctx, types.UserDrive{Name: "drive-" + uuid.NewString(), Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	orgPool, err := st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "pool-" + uuid.NewString(), Scope: types.RunnerPoolOrganization}, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// An allow writer, and the statement that counts the stored rows holding the subject it wrote.
	type allowed struct {
		write func(subject string) error
		count string
	}
	allow := map[string]allowed{
		"UpsertCapabilityGrantQ": {func(s string) error {
			_, err := store.UpsertCapabilityGrantQ(ctx, pool, capabilityGrantFor(s))
			return err
		}, `SELECT count(*) FROM capability_grants WHERE subject_type = 'user' AND subject = $1`},
		"UpsertGovernanceMembershipQ": {func(s string) error {
			_, err := store.UpsertGovernanceMembershipQ(ctx, pool, types.GovernanceMembership{ID: uuid.New(), SubjectType: user, Subject: s, UnitID: store.DefaultUnitID, CreatedBy: "admin@example.com"})
			return err
		}, `SELECT count(*) FROM governance_memberships WHERE subject_type = 'user' AND subject = $1`},
		"InsertGovernanceMembershipQ": {func(s string) error {
			return store.InsertGovernanceMembershipQ(ctx, pool, types.GovernanceMembership{ID: uuid.New(), SubjectType: user, Subject: s, UnitID: store.DefaultUnitID, CreatedBy: "admin@example.com"})
		}, `SELECT count(*) FROM governance_memberships WHERE subject_type = 'user' AND subject = $1`},
		"UpsertUserDriveGrant": {func(s string) error {
			_, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{ID: uuid.New(), SubjectType: user, Subject: s, DriveID: drive.ID}, false)
			return err
		}, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'user' AND subject = $1`},
		"ReplaceUserDriveGrant": {func(s string) error {
			_, _, err := st.ReplaceUserDriveGrant(ctx, types.UserDriveGrant{ID: uuid.New(), SubjectType: user, Subject: s, DriveID: drive.ID}, false)
			return err
		}, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'user' AND subject = $1`},
		"AddUserDriveGrant": {func(s string) error {
			_, err := st.AddUserDriveGrant(ctx, types.UserDriveGrant{ID: uuid.New(), SubjectType: user, Subject: s, DriveID: drive.ID}, false)
			return err
		}, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'user' AND subject = $1`},
		"PutRunnerPoolUsePolicyQ": {func(s string) error {
			_, err := store.PutRunnerPoolUsePolicyQ(ctx, pool, orgPool.ID, nil, []types.RunnerPoolSubject{{SubjectType: user, Subject: s}}, "admin@example.com")
			return err
		}, `SELECT count(*) FROM runner_pool_use_policies WHERE subjects @> jsonb_build_array(jsonb_build_object('subject_type', 'user', 'subject', $1::text))`},
		"PutRestrictionQ": {func(s string) error {
			_, err := store.PutRestrictionQ(ctx, pool, types.Restriction{ID: uuid.New(), Name: "r-" + uuid.NewString(),
				Members: []types.RestrictionMember{{SubjectType: user, Subject: s}}, CreatedBy: "admin@example.com"}, "admin@example.com")
			return err
		}, `SELECT count(*) FROM governance_restriction_members WHERE subject_type = 'user' AND subject = $1`},
	}
	for name, write := range refuse {
		for _, who := range []string{bound, unbound} {
			if err := write(who); !errors.Is(err, store.ErrDelegatePersonNotAllowed) {
				t.Errorf("%s with %s = %v, want ErrDelegatePersonNotAllowed", name, who, err)
			}
		}
	}
	n := 0
	for name, w := range allow {
		if err := w.write(unbound); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
			t.Errorf("%s with an unbound person = %v, want ErrDelegatePersonUnknownPortal", name, err)
		}
		if got := rowCount(t, pool, w.count, unbound); got != 0 {
			t.Errorf("%s stored the unbound person (%d rows)", name, got)
		}
		n++
		person := types.PortalPrincipal(p.Key, "person"+strconv.Itoa(n)) // one person per writer, so no write sees another's row
		if err := w.write(person); err != nil {
			t.Errorf("%s with a bound person = %v, want it stored", name, err)
			continue
		}
		if got := rowCount(t, pool, w.count, person); got != 1 {
			t.Errorf("%s stored %d rows for the bound person, want 1", name, got)
		}
	}
}
