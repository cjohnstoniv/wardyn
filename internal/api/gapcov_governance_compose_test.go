// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gapCovResolveStore answers the precedence read with one scripted row.
type gapCovResolveStore struct {
	store.Store
	row  *types.GovernanceProfile
	tier types.CapabilitySubjectType
	err  error

	asked struct {
		users, groups []string
		userType      string
	}
}

func (s *gapCovResolveStore) ResolveGovernanceProfile(_ context.Context, users, groups []string, userType string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	s.asked.users, s.asked.groups, s.asked.userType = users, groups, userType
	return s.row, s.tier, s.err
}

func TestGapCovAssignedProfileIdentity(t *testing.T) {
	pid := uuid.MustParse("00000000-0000-0000-0000-00000000b001")
	boom := errors.New("gapcov: store down")
	for _, tc := range []struct {
		name     string
		st       *gapCovResolveStore
		wantID   uuid.UUID
		wantName string
		wantTier types.CapabilitySubjectType
		wantErr  error
	}{
		{"a bound profile", &gapCovResolveStore{row: &types.GovernanceProfile{ID: pid, Name: "contractors"}, tier: types.CapabilitySubjectGroup}, pid, "contractors", types.CapabilitySubjectGroup, nil},
		{"no row is not found", &gapCovResolveStore{}, uuid.Nil, "", "", store.ErrNotFound},
		{"a store failure passes through", &gapCovResolveStore{err: boom}, uuid.Nil, "", "", boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: tc.st}}
			id, name, tier, err := s.assignedProfileIdentity(t.Context(), []string{"a@corp.example"}, []string{"eng"}, "dev")
			if id != tc.wantID || name != tc.wantName || tier != tc.wantTier || !errors.Is(err, tc.wantErr) {
				t.Fatalf("= %s %q %q %v; want %s %q %q %v", id, name, tier, err, tc.wantID, tc.wantName, tc.wantTier, tc.wantErr)
			}
			if len(tc.st.asked.users) != 1 || tc.st.asked.userType != "dev" {
				t.Errorf("store was asked %+v", tc.st.asked)
			}
		})
	}
}

// The preview names the profile and the tier that bound the claims.
func TestGapCovPreviewNamesTheBoundProfile(t *testing.T) {
	pid := uuid.MustParse("00000000-0000-0000-0000-00000000b002")
	st := &gapCovResolveStore{row: &types.GovernanceProfile{ID: pid, Name: "contractors"}, tier: types.CapabilitySubjectUserType}
	h := newHarness(t)
	srv := New(baseTestConfig(h, st))

	w := do(t, srv, http.MethodPost, "/api/v1/governance/preview", adminToken, `{"user_type":"dev"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", w.Code, w.Body)
	}
	var got governancePreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ProfileID != pid || got.ProfileName != "contractors" || got.MatchedTier != types.CapabilitySubjectUserType {
		t.Fatalf("preview = %+v, want the bound profile and its tier", got)
	}
	if st.asked.userType != "dev" {
		t.Errorf("the resolver was asked for user type %q, want dev", st.asked.userType)
	}
}

// A composition nothing satisfies is a 403 naming the profile, never a 500 and never its detail.
func TestGapCovWriteCeilingErrorPrefixedAnswersAnUnsatisfiableOverlay(t *testing.T) {
	w := httptest.NewRecorder()
	err := &overlayUnsatisfiableError{Leaf: "contractors", Detail: "internal base detail"}
	writeCeilingErrorPrefixed(w, httptest.NewRequest(http.MethodGet, "/x", nil), "resolve ceiling: ", err)
	if w.Code != http.StatusForbidden || errorReason(w) != reasonGovernanceOverlayUnsatisfiable {
		t.Fatalf("= %d %q, want 403 %s", w.Code, errorReason(w), reasonGovernanceOverlayUnsatisfiable)
	}
	var body map[string]string
	if jerr := json.Unmarshal(w.Body.Bytes(), &body); jerr != nil || body["error"] != err.Error() {
		t.Errorf("body %s (%v), want the error text %q", w.Body, jerr, err.Error())
	}
	if strings.Contains(w.Body.String(), "internal base detail") {
		t.Errorf("body %s leaks the composition detail", w.Body)
	}
}

// A composed row whose overlay sets limits no run can satisfy is an unsatisfiable composition that
// names the leaf, not a silently-applied limit.
func TestGapCovApplyStepRefusesAnOverlayWithInvalidLimits(t *testing.T) {
	negative := -1
	s := &Server{cfg: Config{}}
	row := types.GovernanceProfile{Name: "contractors", OverlayLimits: &types.LimitsOverlay{MaxCPUMillis: &negative}}
	out := &ResolvedProfile{Name: "contractors"}

	_, err := s.applyStep(out, composer.Authority{}, row, true)
	u, ok := isOverlayUnsatisfiable(err)
	if !ok {
		t.Fatalf("applyStep error = %v, want an unsatisfiable overlay", err)
	}
	if u.Leaf != "contractors" || !strings.Contains(u.Detail, "max_cpu_millis") {
		t.Errorf("unsatisfiable = %+v, want the leaf and the limit named", u)
	}
}
