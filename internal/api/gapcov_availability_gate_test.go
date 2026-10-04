// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const gapCovAvailImage = "ghcr.io/acme/agent:1.4.2"

var gapCovAvailPath = "/api/v1/permissions/availability/image/" + gapCovAvailImage

// gapCovAvailAllow lists one user type for the image, so turning "Only" on is not the empty-list 400.
func gapCovAvailAllow() types.CapabilityGrant {
	return grant(types.CapabilitySubjectUserType, utDev, capImage, gapCovAvailImage, types.CapabilityAllow)
}

// gapCovAvailStore fails the restriction write on demand.
type gapCovAvailStore struct {
	*permStore
	setErr error
}

func (s *gapCovAvailStore) SetCapabilityRestriction(ctx context.Context, kind, value string, restricted bool, by string) error {
	if s.setErr != nil {
		return s.setErr
	}
	return s.permStore.SetCapabilityRestriction(ctx, kind, value, restricted, by)
}

// With the switch on, the admin token still turns "Only" on directly and the write leaves a
// break-glass row naming the value.
func TestGapCovAvailabilityWriteByTheAdminTokenLeavesABreakGlassRow(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	srv, st, h := gapCovPermServer(t, false)
	st.grants = []types.CapabilityGrant{gapCovAvailAllow()}

	w := do(t, srv, http.MethodPut, gapCovAvailPath, adminToken, `{"restricted":true}`)
	if w.Code != http.StatusOK || !st.restricted[capImage][gapCovAvailImage] {
		t.Fatalf("PUT = %d %s, restricted %v; want 200 with the image restricted", w.Code, w.Body, st.restricted)
	}
	if row := gapCovBypassRow(t, h, govKindAvailability); row.Target != capImage+"/"+gapCovAvailImage {
		t.Errorf("break-glass target = %q, want %q", row.Target, capImage+"/"+gapCovAvailImage)
	}
}

// In local mode a human's availability change is refused 503 with the local-mode reason, and
// nothing is written.
func TestGapCovAvailabilityChangeIsRefusedInLocalModeWithTheSwitchOn(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	srv, st, h := gapCovPermServer(t, true)
	st.grants = []types.CapabilityGrant{gapCovAvailAllow()}

	r := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"restricted":true}`))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("kind", capImage)
	rc.URLParams.Add("*", gapCovAvailImage)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	r = r.WithContext(withHumanIdentity(r.Context(), "sub-a", "a@corp.example", oidc.RoleAdmin, types.UserTypeStandard, nil, false))
	w := httptest.NewRecorder()
	srv.handlePutAvailability(w, r)

	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
		t.Fatalf("PUT = %d %q, want 503 %s", w.Code, errorReason(w), reasonGovernanceSecondHumanLocalMode)
	}
	if len(st.restricted[capImage]) != 0 || len(govCovAudits(h, "capability.availability.write")) != 0 {
		t.Errorf("restricted %v, %d availability audit rows; want nothing written", st.restricted, len(govCovAudits(h, "capability.availability.write")))
	}
}

// A store that refuses the restriction write is a 500 that leaves no audit row.
func TestGapCovAvailabilityStoreFailureIsAServerError(t *testing.T) {
	h := newHarness(t)
	st := &gapCovAvailStore{permStore: &permStore{capStore: &capStore{}}, setErr: errors.New("gapcov: write refused")}
	st.grants = []types.CapabilityGrant{gapCovAvailAllow()}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	w := doSSO(t, srv, http.MethodPut, gapCovAvailPath, permAdmin(t), `{"restricted":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d %s, want 500", w.Code, w.Body)
	}
	if n := len(govCovAudits(h, "capability.availability.write")); n != 0 || st.restricted[capImage][gapCovAvailImage] {
		t.Errorf("%d audit rows, restricted %v; want nothing recorded after a refused write", n, st.restricted)
	}
}

// gapCovQueueAvailStore is the held-change store plus the two reads an availability write makes.
type gapCovQueueAvailStore struct{ *govCovStore }

func (gapCovQueueAvailStore) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return map[string]map[string]bool{}, nil
}

func (gapCovQueueAvailStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return []types.CapabilityGrant{gapCovAvailAllow()}, nil
}

// With the switch on, a human's change to the restriction is held, not written.
func TestGapCovAvailabilityChangeByAHumanIsHeld(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	gs := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID}}
	srv, _ := govCovServer(t, gapCovQueueAvailStore{gs})
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPut, gapCovAvailPath, super, `{"restricted":true}`)
	ch := govCovHoldAnswer(t, w, gs)
	if ch.TargetKind != govKindAvailability || ch.Op != "set" || ch.TargetKey != capImage+"/"+gapCovAvailImage {
		t.Fatalf("stored change = %+v", ch)
	}
	if want := `{"kind":"image","value":"` + gapCovAvailImage + `","restricted":true}`; string(ch.Payload) != want {
		t.Fatalf("payload = %s, want %s", ch.Payload, want)
	}
}
