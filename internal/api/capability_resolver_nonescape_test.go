// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolverTableStore is a capability store that records the ORDER of its reads,
// so the table below can pin laziness (F7 rule 2) as well as answers. Its
// subject filter mirrors the SQL, as capStore's does.
type resolverTableStore struct {
	store.Store
	grants            []types.CapabilityGrant
	enf               map[string]bool
	grantsErr, enfErr error
	reads             []string
}

func (s *resolverTableStore) ListCapabilityGrantsFor(_ context.Context, users, groups []string, _ string) ([]types.CapabilityGrant, error) {
	s.reads = append(s.reads, "grants")
	if s.grantsErr != nil {
		return nil, s.grantsErr
	}
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		if g.SubjectType == types.CapabilitySubjectAll ||
			(g.SubjectType == types.CapabilitySubjectUser && slices.Contains(users, g.Subject)) ||
			(g.SubjectType == types.CapabilitySubjectGroup && slices.Contains(groups, g.Subject)) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *resolverTableStore) ListGroupDenyGrants(_ context.Context, kind string) ([]types.CapabilityGrant, error) {
	s.reads = append(s.reads, "groupDeny")
	if s.grantsErr != nil {
		return nil, s.grantsErr
	}
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		if g.SubjectType == types.CapabilitySubjectGroup && g.Effect == types.CapabilityDeny && g.Capability == kind {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *resolverTableStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	s.reads = append(s.reads, "enforcement")
	if s.enfErr != nil {
		return nil, s.enfErr
	}
	return s.enf, nil
}

func (s *resolverTableStore) count(read string) int {
	n := 0
	for _, r := range s.reads {
		if r == read {
			n++
		}
	}
	return n
}

// resolverDoor is one way into the resolver. widening forces the direction the
// door asks (capGranted); otherwise the kind's own row decides.
type resolverDoor struct {
	name     string
	widening bool
	// strictNoStore: the door errors on a nil Store rather than answering.
	strictNoStore bool
	ask           func(s *Server, ctx context.Context, kind, value string) (bool, error)
}

var resolverDoors = []resolverDoor{
	{name: "capAllowed", strictNoStore: true, ask: (*Server).capAllowed},
	{name: "capSeamAllowed", ask: (*Server).capSeamAllowed},
	{name: "capGranted", widening: true, ask: (*Server).capGranted},
	{name: "capBatch.allowed", ask: func(s *Server, ctx context.Context, kind, value string) (bool, error) {
		return s.newCapBatch(ctx).allowed(ctx, kind, value)
	}},
	// Through an installed memo, asked twice: the second answer comes from the
	// shared snapshot and must equal the first.
	{name: "capSeamAllowed via memo", ask: func(s *Server, ctx context.Context, kind, value string) (bool, error) {
		ctx = withCapBatch(ctx)
		first, err := s.capSeamAllowed(ctx, kind, value)
		if err != nil {
			return false, err
		}
		again, err := s.capSeamAllowed(ctx, kind, value)
		if err != nil || again != first {
			return false, fmt.Errorf("memoized answer %v/%v differs from the first %v", again, err, first)
		}
		return first, nil
	}},
}

// resolverCase is one generated point: tier x snapshot x grant state x switch x store.
type resolverCase struct {
	tier     string // oidc role
	stale    bool   // nil group snapshot: group rows reachable only through ListGroupDenyGrants
	allow    string // "", "user", "all*", "other"
	deny     string // "", "user", "group"
	enforced bool
	noStore  bool
}

func (c resolverCase) String() string {
	return fmt.Sprintf("tier=%s stale=%v allow=%q deny=%q enforced=%v noStore=%v", c.tier, c.stale, c.allow, c.deny, c.enforced, c.noStore)
}

func resolverValue(kind string) string {
	if kind == capEgressHost {
		return "api.example.com"
	}
	return "v1"
}

func (c resolverCase) grants(kind string) []types.CapabilityGrant {
	v := resolverValue(kind)
	var out []types.CapabilityGrant
	switch c.allow {
	case "user":
		out = append(out, grant(types.CapabilitySubjectUser, capSub, kind, v, types.CapabilityAllow))
	case "all*":
		out = append(out, grant(types.CapabilitySubjectAll, "", kind, capWildcard, types.CapabilityAllow))
	case "other":
		out = append(out, grant(types.CapabilitySubjectUser, capSub, kind, "other-"+v, types.CapabilityAllow))
	}
	switch c.deny {
	case "user":
		out = append(out, grant(types.CapabilitySubjectUser, capSub, kind, v, types.CapabilityDeny))
	case "group":
		out = append(out, grant(types.CapabilitySubjectGroup, "eng", kind, v, types.CapabilityDeny))
	}
	return out
}

// want is the seven-step rule stated as plainly as it can be, with no laziness:
// the oracle the resolver's lazy, memoized shape must agree with everywhere.
func (c resolverCase) want(door resolverDoor, kind string) (allowed, wantErr bool) {
	widening := door.widening || capKinds[kind].direction == capWidening
	if c.tier == oidc.RoleAdmin { // 1. operator exempt
		return true, false
	}
	if c.noStore {
		if door.strictNoStore {
			return false, true
		}
		return !widening, false // nil Store: widening => Deny, narrowing => Allow
	}
	// A group deny bites on either snapshot: seen directly when answerable, via
	// the unresolvable-group-deny read when stale.
	if c.deny != "" { // 2. an overlapping deny
		return false, false
	}
	if widening && !c.enforced { // 4.
		return false, false
	}
	if c.allow == "user" || c.allow == "all*" { // 5.
		return true, false
	}
	if !widening && !c.enforced { // 6.
		return true, false
	}
	return false, false // 7.
}

func resolverCases() []resolverCase {
	var out []resolverCase
	for _, tier := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleUser} {
		for _, stale := range []bool{false, true} {
			for _, allow := range []string{"", "user", "all*", "other"} {
				for _, deny := range []string{"", "user", "group"} {
					for _, enforced := range []bool{false, true} {
						for _, noStore := range []bool{false, true} {
							out = append(out, resolverCase{tier, stale, allow, deny, enforced, noStore})
						}
					}
				}
			}
		}
	}
	return out
}

func resolverCtx(tier string, stale bool) context.Context {
	var groups []string
	if !stale {
		groups = []string{"eng"}
	}
	return withOIDCGroups(operatorCtx(capSub, capEmail, tier), groups)
}

// TestCapResolverNonescapeTable is the generated nonescape table for the one
// grant resolver: kind x door x tier x grant state x switch x store, every
// point against the seven-step oracle above. Every door reaches capBatch.decide,
// so a door that disagrees with the oracle anywhere is a second rule order.
func TestCapResolverNonescapeTable(t *testing.T) {
	cases := resolverCases()
	for _, door := range resolverDoors {
		t.Run(door.name, func(t *testing.T) {
			for _, kind := range capabilityKinds {
				for _, c := range cases {
					srv := &Server{}
					if !c.noStore {
						srv = capServer(&resolverTableStore{grants: c.grants(kind), enf: map[string]bool{kind: c.enforced}})
					}
					got, err := door.ask(srv, resolverCtx(c.tier, c.stale), kind, resolverValue(kind))
					want, wantErr := c.want(door, kind)
					if (err != nil) != wantErr || got != want {
						t.Errorf("%s(%s) %v = %v, %v; want %v (error %v)", door.name, kind, c, got, err, want, wantErr)
					}
				}
			}
		})
	}
}

// TestCapResolverReadsStayLazy pins F7 rule 2: reads happen in the order the
// per-value wrappers always made them, so no store failure can turn a refusal
// the rule order already decided into a 500.
func TestCapResolverReadsStayLazy(t *testing.T) {
	member := resolverCtx(oidc.RoleUser, false)
	boom := errors.New("connection refused")

	t.Run("an unenforced widening kind never reads grants", func(t *testing.T) {
		st := &resolverTableStore{grantsErr: boom}
		for _, door := range resolverDoors {
			kind := capImage
			if door.widening {
				kind = capEgressHost // capGranted asks the widening question of any kind
			}
			st.reads = nil
			ok, err := door.ask(capServer(st), member, kind, "x")
			if ok || err != nil {
				t.Errorf("%s(%s) unenforced = %v, %v; want a refusal, not a 500 from the grants read", door.name, kind, ok, err)
			}
			if st.count("grants") != 0 || st.count("enforcement") != 1 {
				t.Errorf("%s(%s) reads = %v; want the switch alone", door.name, kind, st.reads)
			}
		}
	})

	t.Run("a narrowing kind reads grants first and the switch only when unsettled", func(t *testing.T) {
		st := &resolverTableStore{
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", types.CapabilityAllow)},
			enfErr: boom,
		}
		if ok, err := capServer(st).capSeamAllowed(member, capEgressHost, "pypi.org"); !ok || err != nil {
			t.Fatalf("allowed by a grant = %v, %v; the switch read must not be reached", ok, err)
		}
		if !slices.Equal(st.reads, []string{"grants"}) {
			t.Errorf("reads = %v, want [grants]", st.reads)
		}
		st.reads = nil
		if ok, err := capServer(st).capSeamAllowed(member, capEgressHost, "other.org"); ok || err == nil {
			t.Fatalf("unsettled value with a failing switch read = %v, %v; want the error", ok, err)
		}
		if !slices.Equal(st.reads, []string{"grants", "enforcement"}) {
			t.Errorf("reads = %v, want [grants enforcement]", st.reads)
		}
	})

	t.Run("an enforced widening kind reads the switch before grants", func(t *testing.T) {
		st := &resolverTableStore{enf: map[string]bool{capImage: true}}
		if _, err := capServer(st).capGranted(member, capImage, "x"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(st.reads, []string{"enforcement", "grants"}) {
			t.Errorf("reads = %v, want [enforcement grants]", st.reads)
		}
	})

	t.Run("one memo, one snapshot across doors", func(t *testing.T) {
		st := &resolverTableStore{enf: map[string]bool{capImage: true}}
		srv := capServer(st)
		ctx := withCapBatch(resolverCtx(oidc.RoleUser, true))
		if withCapBatch(ctx).Value(capBatchKey{}) != ctx.Value(capBatchKey{}) {
			t.Fatal("withCapBatch(withCapBatch(ctx)) installed a second memo; a nested resolution must share the outer one")
		}
		for _, ask := range []func() (bool, error){
			func() (bool, error) { return srv.capGranted(ctx, capImage, "a") },
			func() (bool, error) { return srv.capSeamAllowed(ctx, capWorkspace, "w") },
			func() (bool, error) { return srv.capAllowed(ctx, capAgent, "claude-code") },
			func() (bool, error) { return srv.capBatchFor(ctx).allowed(ctx, capEgressHost, "h") },
		} {
			if _, err := ask(); err != nil {
				t.Fatal(err)
			}
		}
		for read, want := range map[string]int{"grants": 1, "enforcement": 1} {
			if got := st.count(read); got != want {
				t.Errorf("%s reads = %d under one memo, want %d (all reads: %v)", read, got, want, st.reads)
			}
		}
	})
}

// TestCapKindTableIsTheClosedSet: the kind table is keyed by exactly the closed
// set, image is the one widening kind, and no kind gates an admin pin yet.
func TestCapKindTableIsTheClosedSet(t *testing.T) {
	if len(capKinds) != len(capabilityKinds) {
		t.Fatalf("capKinds has %d rows, capabilityKinds %d", len(capKinds), len(capabilityKinds))
	}
	for _, kind := range capabilityKinds {
		k, ok := capKinds[kind]
		if !ok {
			t.Fatalf("kind %q has no capKinds row", kind)
		}
		if (k.direction == capWidening) != (kind == capImage) {
			t.Errorf("kind %q direction = %v; image is the only widening kind", kind, k.direction)
		}
		if k.hostSet != (kind == capEgressHost) {
			t.Errorf("kind %q hostSet = %v; egress_host is the only host-set kind", kind, k.hostSet)
		}
		if k.restrictable != (kind != capEgressHost && kind != capSecret) {
			t.Errorf("kind %q restrictable = %v; every offered resource is, egress_host and secret are not", kind, k.restrictable)
		}
		if k.gatesAdminPins {
			t.Errorf("kind %q gates admin pins; no shipped kind does", kind)
		}
	}
}

// denyReadCountStore counts the two per-resolution reads over capStore, which
// answers every other read denyMemberRequest makes (the governance ceiling).
type denyReadCountStore struct {
	*capStore
	grantsReads, enfReads int
}

func (s *denyReadCountStore) ListCapabilityGrantsFor(ctx context.Context, users, groups []string, userType string) ([]types.CapabilityGrant, error) {
	s.grantsReads++
	return s.capStore.ListCapabilityGrantsFor(ctx, users, groups, userType)
}

func (s *denyReadCountStore) GetCapabilityEnforcement(ctx context.Context) (map[string]bool, error) {
	s.enfReads++
	return s.capStore.GetCapabilityEnforcement(ctx)
}

// TestDenyMemberRequest_OneSnapshotForEveryField: a member request naming an
// image, a workspace, an agent and an integration is decided on ONE capability
// snapshot — denyMemberRequest installs the ctx memo. Without it each field
// re-reads grants (4) and the switch (3).
func TestDenyMemberRequest_OneSnapshotForEveryField(t *testing.T) {
	const ref = "ghcr.io/acme/agent:1.4.2"
	ws := uuid.New()
	st := &denyReadCountStore{capStore: &capStore{
		grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, capSub, capImage, ref, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capWorkspace, capWildcard, types.CapabilityAllow),
		},
		enf: map[string]bool{capImage: true},
	}}
	h := newHarness(t)
	h.srv.cfg.Store = st
	req := createRunRequest{Image: ref, WorkspaceID: &ws, Agent: "claude-code", IntegrationID: "anthropic"}
	if denied, code := denyRequest(t, h.srv, req); denied {
		t.Fatalf("denied (status %d); every field is granted or unenforced", code)
	}
	if st.grantsReads != 1 || st.enfReads != 1 {
		t.Errorf("grants reads = %d, enforcement reads = %d; want 1 and 1 (one snapshot per resolution)", st.grantsReads, st.enfReads)
	}
}
