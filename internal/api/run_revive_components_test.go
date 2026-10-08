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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revive, a restart and an extension re-authorise a run's components from
// its run_components rows: the doors those rows record, never their content.

// componentReviveStore is reviveStore plus the two component reads the owner
// re-check makes. The embedded ComponentStore is nil: any other method of the
// seam being called is a test failure worth a panic.
type componentReviveStore struct {
	*reviveStore
	store.ComponentStore
	snapshot []types.RunComponent
	// org is the organisation components that still exist.
	org     map[uuid.UUID]bool
	listErr error
	getErr  error
}

func (s *componentReviveStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.snapshot, s.listErr
}

func (s *componentReviveStore) GetComponent(_ context.Context, id uuid.UUID, owner string) (types.Component, error) {
	if s.getErr != nil {
		return types.Component{}, s.getErr
	}
	if owner != "" || !s.org[id] {
		return types.Component{}, store.ErrNotFound
	}
	return types.Component{ID: id, Name: "Org Tool"}, nil
}

// withRunComponents gives the fixture's run a component snapshot.
func (f *reviveFixture) withRunComponents(snapshot ...types.RunComponent) *componentReviveStore {
	cs := &componentReviveStore{reviveStore: f.rs, snapshot: snapshot, org: map[uuid.UUID]bool{}}
	f.srv.cfg.Store = cs
	return cs
}

var (
	reviveOrgComponent = uuid.MustParse("7d3f2f0e-5a54-4d6e-9d0f-6c1f6f1c0a11")
	// What a person typed into their own component, which no refusal may say.
	reviveSelfRow = types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "My Tool",
		Definition: types.ComponentDefinition{Hosts: []string{"typed-by-the-person.example"}}}
	// The same row after the person's components were erased.
	reviveErasedRow = types.RunComponent{SelfDefined: true, Erased: true}
	reviveOrgRow    = types.RunComponent{ComponentID: &reviveOrgComponent, Name: "Org Tool", Version: 2,
		Definition: types.ComponentDefinition{Hosts: []string{"org-api.example"}}}
)

func denyCustomComponents() []types.CapabilityGrant {
	return []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capFeature, featureCustomComponent, types.CapabilityDeny)}
}

// TestRevive_RechecksComponentDoorsFromTheSnapshot: a run that carried a
// component of its owner's is revived only while the owner still holds the
// custom_component feature — whether the row still has its content or was
// erased to a tombstone — and one that carried an organisation's component
// only while the owner is still granted it. The owner and an admin get the
// same answer, and it says nothing of what the component was.
func TestRevive_RechecksComponentDoorsFromTheSnapshot(t *testing.T) {
	orgAllow := func(sub string) []types.CapabilityGrant {
		return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, sub, capComponent, reviveOrgComponent.String(), types.CapabilityAllow)}
	}
	featureAllow := func(sub string) []types.CapabilityGrant {
		return []types.CapabilityGrant{grant(types.CapabilitySubjectUser, sub, capFeature, featureCustomComponent, types.CapabilityAllow)}
	}
	restrictOrg := map[string]map[string]bool{capComponent: {reviveOrgComponent.String(): true}}
	for _, tc := range []struct {
		name       string
		rows       []types.RunComponent
		caps       func(sub string) []types.CapabilityGrant
		enforced   map[string]bool
		restricted map[string]map[string]bool
		refused    string // what the refusal names; "" = revived
	}{
		{name: "own component, feature on by default", rows: []types.RunComponent{reviveSelfRow}},
		{name: "own component, feature denied", rows: []types.RunComponent{reviveSelfRow},
			caps: func(string) []types.CapabilityGrant { return denyCustomComponents() }, refused: "the feature capability for custom components"},
		{name: "erased tombstone, feature on by default", rows: []types.RunComponent{reviveErasedRow}},
		{name: "erased tombstone, feature denied", rows: []types.RunComponent{reviveErasedRow},
			caps: func(string) []types.CapabilityGrant { return denyCustomComponents() }, refused: "the feature capability for custom components"},
		{name: "erased tombstone, feature enforced and not granted", rows: []types.RunComponent{reviveErasedRow},
			enforced: map[string]bool{capFeature: true}, refused: "the feature capability for custom components"},
		{name: "erased tombstone, feature enforced and granted", rows: []types.RunComponent{reviveErasedRow},
			enforced: map[string]bool{capFeature: true}, caps: featureAllow},
		{name: "organisation component, still granted", rows: []types.RunComponent{reviveOrgRow},
			restricted: restrictOrg, caps: orgAllow},
		{name: "organisation component, grant withdrawn", rows: []types.RunComponent{reviveOrgRow},
			restricted: restrictOrg, refused: "the component capability for a component your organisation provides"},
		{name: "organisation component beside an erased own one, feature denied", rows: []types.RunComponent{reviveOrgRow, reviveErasedRow},
			restricted: restrictOrg, caps: func(sub string) []types.CapabilityGrant { return append(orgAllow(sub), denyCustomComponents()...) },
			refused: "the feature capability for custom components"},
	} {
		for _, asOwner := range []bool{true, false} {
			t.Run(tc.name+"/owner="+map[bool]string{true: "yes", false: "no"}[asOwner], func(t *testing.T) {
				f, _ := newOwnerFixture(t)
				cs := f.withRunComponents(tc.rows...)
				cs.org[reviveOrgComponent] = true
				f.st.run.UserType = types.UserTypeStandard
				if tc.caps != nil {
					f.st.caps = tc.caps(f.run.CreatedBy)
				}
				f.st.enf, f.st.restricted = tc.enforced, tc.restricted
				code, body := f.reviveAs(t, asOwner)
				if tc.refused == "" {
					if code != http.StatusOK {
						t.Fatalf("revive = %d %s, want 200", code, body)
					}
					return
				}
				if code != http.StatusForbidden || !strings.Contains(body, tc.refused) {
					t.Fatalf("revive = %d %s; want 403 naming %q", code, body, tc.refused)
				}
				for _, secret := range []string{reviveOrgComponent.String(), "Org Tool", "org-api.example", "My Tool", "typed-by-the-person"} {
					if strings.Contains(body, secret) {
						t.Errorf("the refusal discloses %q: %s", secret, body)
					}
				}
				f.assertReviveRefused(t, reasonOwnerCapabilityComponent)
			})
		}
	}
}

// A deleted organisation component never re-opens its door: the run that
// carried it is refused for its owner and for an admin, whatever grant rows
// remain, and an admin who owns the run is not exempt.
func TestRevive_RefusesARunWhoseOrganisationComponentIsGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller func(t *testing.T, f *reviveFixture) (int, string)
	}{
		{"owner", func(t *testing.T, f *reviveFixture) (int, string) { return f.reviveAs(t, true) }},
		{"admin", func(t *testing.T, f *reviveFixture) (int, string) { return f.reviveAs(t, false) }},
		{"owner who is an admin", func(t *testing.T, f *reviveFixture) (int, string) {
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive",
				ssoSession(t, f.run.CreatedBy, ownerEmail, oidc.RoleAdmin), "")
			return w.Code, w.Body.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			cs := f.withRunComponents(reviveOrgRow)
			// Still granted by name, and the kind is open: only the row is gone.
			f.st.caps = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capComponent, reviveOrgComponent.String(), types.CapabilityAllow)}
			if tc.name == "owner who is an admin" {
				f.st.run.GovernanceProfileID = nil // a super admin captures none
			}
			code, body := tc.caller(t, f)
			if code != http.StatusConflict || !strings.Contains(body, reasonOwnerComponentGone) {
				t.Fatalf("revive = %d %s, want 409 %s", code, body, reasonOwnerComponentGone)
			}
			if strings.Contains(body, reviveOrgComponent.String()) || strings.Contains(body, "Org Tool") {
				t.Errorf("the refusal names the component: %s", body)
			}
			f.assertReviveRefused(t, reasonOwnerComponentGone)

			// The component back in place, the same run revives.
			cs.org[reviveOrgComponent] = true
			if code, body := tc.caller(t, f); code != http.StatusOK {
				t.Fatalf("revive with the component in place = %d %s, want 200", code, body)
			}
		})
	}
}

// A run with no snapshot rows is a run launched without components, or before
// they existed: nothing is asked of it. A snapshot or a component that cannot
// be read refuses rather than reading as "none".
func TestRevive_NoComponentRowsIsALegacyRunAndAnUnreadableSnapshotRefuses(t *testing.T) {
	t.Run("no rows, feature denied", func(t *testing.T) {
		f, _ := newOwnerFixture(t)
		f.withRunComponents()
		f.st.caps = denyCustomComponents()
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200: a run without components holds no component door", code, body)
		}
	})
	t.Run("a store that keeps no components", func(t *testing.T) {
		f, _ := newOwnerFixture(t)
		f.st.caps = denyCustomComponents()
		if code, body := f.reviveAs(t, false); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
	})
	for name, breakIt := range map[string]func(*componentReviveStore){
		"snapshot unreadable":  func(cs *componentReviveStore) { cs.listErr = errors.New("connection refused") },
		"component unreadable": func(cs *componentReviveStore) { cs.getErr = errors.New("connection refused") },
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			cs := f.withRunComponents(reviveOrgRow)
			cs.org[reviveOrgComponent] = true
			breakIt(cs)
			code, body := f.reviveAs(t, true)
			if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonReviveOwnerAuthorityUnreadable) {
				t.Fatalf("revive = %d %s, want 503 %s", code, body, reasonReviveOwnerAuthorityUnreadable)
			}
			if len(f.rr.replaced) != 0 {
				t.Error("a revive that could not re-check the run's components replaced the proxy")
			}
		})
	}
}

// The interception entry of a component's header host is part of the stored
// proxy config, and a revive restores it from there: nothing is rebuilt from
// the snapshot (whose content may be gone), and the owner's current denies
// prune it with its injection.
func TestRevive_RestoresComponentInterceptionFromTheStoredConfig(t *testing.T) {
	f, _ := newOwnerFixture(t)
	f.withRunComponents(reviveErasedRow) // the content is gone; the render is the record
	f.rs.profiles[0].Ceiling.DeniedDomains = append(f.rs.profiles[0].Ceiling.DeniedDomains, "walled-comp.example")
	f.editConfig(t, func(c *proxy.Config) {
		c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "person-api.example", "walled-comp.example")
		for _, host := range []string{"person-api.example", "walled-comp.example"} {
			c.Injection = append(c.Injection, proxy.InjectionConfig{GrantID: uuid.New(),
				InjectionRule: egress.InjectionRule{Host: host, Header: "Authorization", Format: "Bearer %s", RequireTLS: true}})
		}
		c.MITMHosts = []string{"person-api.example:443", "walled-comp.example:443"}
	})
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	cfg := f.newConfig(t)
	if !slices.Equal(cfg.MITMHosts, []string{"person-api.example:443"}) {
		t.Errorf("revived MITMHosts = %v, want the stored entry kept and the denied host's pruned", cfg.MITMHosts)
	}
	var hosts []string
	for _, in := range cfg.Injection {
		hosts = append(hosts, in.Host)
	}
	if !slices.Contains(hosts, "person-api.example") || slices.Contains(hosts, "walled-comp.example") {
		t.Errorf("revived injection hosts = %v, want the component's kept and the denied one's dropped", hosts)
	}
	if !slices.Contains(cfg.Policy.DeniedDomains, "walled-comp.example") {
		t.Errorf("revived denies = %v, want the profile's entry", cfg.Policy.DeniedDomains)
	}
	if cfg.MITMCACertPEM != "ca-cert" || cfg.MITMCAKeyPEM != "ca-key" {
		t.Error("the revived config lost the run's CA")
	}
}

// Extending a run's end keeps its sandbox and credentials alive, so it asks
// the same component questions a revive does.
func TestOwnerCapability_ExtensionRechecksComponentDoors(t *testing.T) {
	extend := func(t *testing.T, rows []types.RunComponent, exists bool, caps []types.CapabilityGrant) int {
		t.Helper()
		f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
		f.srv.cfg.Store = &extendComponentStore{leaseStore: f.st, snapshot: rows, exists: exists}
		f.st.caps = caps
		code, _ := f.patch(t, ssoSession(t, endWaitOwner, ownerEmail, oidc.RoleUser), endsAtBody(f.now.Add(7*24*time.Hour)))
		return code
	}
	if code := extend(t, []types.RunComponent{reviveErasedRow}, false, nil); code != http.StatusOK {
		t.Errorf("extend with the feature on = %d, want 200", code)
	}
	if code := extend(t, []types.RunComponent{reviveErasedRow}, false, denyCustomComponents()); code != http.StatusForbidden {
		t.Errorf("extend with the feature denied = %d, want 403", code)
	}
	if code := extend(t, []types.RunComponent{reviveOrgRow}, true, nil); code != http.StatusOK {
		t.Errorf("extend with the organisation's component in place = %d, want 200", code)
	}
	if code := extend(t, []types.RunComponent{reviveOrgRow}, false, nil); code != http.StatusConflict {
		t.Errorf("extend with the organisation's component deleted = %d, want 409", code)
	}
	if code := extend(t, nil, false, denyCustomComponents()); code != http.StatusOK {
		t.Errorf("extend of a run without components = %d, want 200", code)
	}
}

// extendComponentStore is the extension fixture's store plus the component reads.
type extendComponentStore struct {
	*leaseStore
	store.ComponentStore
	snapshot []types.RunComponent
	exists   bool
}

func (s *extendComponentStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.snapshot, nil
}

func (s *extendComponentStore) GetComponent(_ context.Context, id uuid.UUID, _ string) (types.Component, error) {
	if !s.exists {
		return types.Component{}, store.ErrNotFound
	}
	return types.Component{ID: id}, nil
}

// The door list is read off the snapshot rows alone.
func TestOwnerCapability_ComponentDoorsComeFromTheRowNotItsContent(t *testing.T) {
	other := uuid.New()
	doors := persistedLaunchDoors(types.AgentRun{}, nil, []types.RunComponent{
		reviveSelfRow, reviveOrgRow, reviveErasedRow, {ComponentID: &other},
		{SelfDefined: true, ComponentID: &other, Owner: "sub-owner"}, // the owner's saved component: their feature, not a grant
	})
	var got []string
	for _, d := range doors {
		got = append(got, d.kind+"="+d.value)
		if strings.Contains(d.label, d.value) && d.kind == capComponent {
			t.Errorf("an organisation component's label names its id: %q", d.label)
		}
	}
	want := []string{capComponent + "=" + reviveOrgComponent.String(), capComponent + "=" + other.String(), capFeature + "=" + featureCustomComponent}
	if !slices.Equal(got, want) {
		t.Errorf("doors = %v, want %v", got, want)
	}
	if doors := persistedLaunchDoors(types.AgentRun{}, nil, nil); len(doors) != 0 {
		t.Errorf("a run without components yields %v", doors)
	}
}

// The credential re-check reads a shared grant where the sink reads it: the
// operator's namespace alone. The run's owner holding a secret of the same
// name does not stand in for it, and the refusal never says what the
// organisation's secret is called.
func TestRevive_ASharedCredentialIsRecheckedInTheOperatorNamespaceAndNeverNamed(t *testing.T) {
	shared := func(f *reviveFixture) {
		f.st.credGrants[0].Spec.Scope = mustJSON(map[string]any{"host": "artifactory.corp.example", "secret_name": "artifactory-token", "shared": true})
		f.st.site.Integrations = nil
	}
	t.Run("the operator holds it", func(t *testing.T) {
		f, _ := newModelCredFixture(t)
		shared(f)
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
	})
	t.Run("only the owner holds one of that name", func(t *testing.T) {
		f, sec := newModelCredFixture(t)
		shared(f)
		if err := sec.For(f.run.CreatedBy).Put(context.Background(), "artifactory-token", []byte("art-own")); err != nil {
			t.Fatal(err)
		}
		delete(sec.m, "artifactory-token")
		code, body := f.reviveAs(t, true)
		if code != http.StatusConflict || !strings.Contains(body, "your organisation provides") {
			t.Fatalf("revive = %d %s; want 409 saying the organisation's credential is gone", code, body)
		}
		if strings.Contains(body, "artifactory-token") {
			t.Errorf("the refusal names the organisation's secret: %s", body)
		}
		f.assertReviveRefused(t, reasonOwnerModelCredentialErased)
	})
}
