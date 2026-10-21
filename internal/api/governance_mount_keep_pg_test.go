// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Mount host paths on governance profiles, against a real Postgres: a security admin reads every
// workspace_mounts[].source as "<redacted>", sends it back that way to keep the mount, and is refused a
// literal source; a super admin reads and sets them. "Stored" is always read from the store, never
// from an API read, since the API read is what redacts.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	pinMountSrc  = "/srv/pin-mount-src"   // the seeded host path every leak check greps for
	pinMountSrcB = "/srv/pin-mount-src-b" // a second, so a by-index match would swap them
	pinMountNew  = "/srv/pin-mount-new"   // what a super admin changes one to
	pinBaseSrc   = "/srv/pin-mount-base"  // the deployment default's mount
	pinTgtA      = "/home/agent/pin-a"
	pinTgtB      = "/home/agent/pin-b"
)

func pinMounts() []types.WorkspaceMount {
	return []types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}, {Source: pinMountSrcB, Target: pinTgtB}}
}

// newMountEnv is a govEnv whose deployment default carries a mount, with four-eyes on only when asked.
func newMountEnv(t *testing.T, fourEyes bool) *govEnv {
	t.Helper()
	if fourEyes {
		t.Setenv(envGovernanceSecondHuman, "true")
	}
	e := newGovEnvOn(t, throwawayPGPool(t), func(c *Config) {
		c.DefaultPolicy.WorkspaceMounts = []types.WorkspaceMount{{Source: pinBaseSrc, Target: pinTgtA}}
	})
	// A stored policy and a launch may mount only an onboarded directory: onboard every seeded one,
	// operator-owned.
	var srcs []types.WorkspaceSource
	for _, dir := range []string{pinMountSrc, pinMountSrcB, pinMountNew, pinBaseSrc} {
		srcs = append(srcs, types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: dir})
	}
	now := time.Now().UTC()
	if _, err := e.pg.CreateWorkspace(context.Background(), types.Workspace{ID: uuid.New(), Name: "ws-pin-mounts",
		Status: types.WorkspaceScanned, Sources: srcs, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("onboard the seeded mount sources: %v", err)
	}
	return e
}

func (e *govEnv) seedMountProfile(name string, mounts []types.WorkspaceMount) types.GovernanceUnit {
	e.t.Helper()
	p, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{
		Name: name, CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2, WorkspaceMounts: mounts},
	})
	if err != nil {
		e.t.Fatalf("seed profile %q: %v", name, err)
	}
	return p
}

// seedComposed stores a profile composed on base with overlay.
func (e *govEnv) seedComposed(name string, base types.GovernanceUnit, overlay types.CeilingOverlay) types.GovernanceUnit {
	e.t.Helper()
	p, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{
		Name: name, CreatedBy: "seed", ParentID: &base.ID, Overlay: &overlay,
	})
	if err != nil {
		e.t.Fatalf("seed composed profile %q: %v", name, err)
	}
	return p
}

// view is GET /governance's entry for id as c reads it, with the whole body.
func (e *govEnv) view(c *http.Cookie, id uuid.UUID) (governanceUnitView, string) {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET /governance = %d %s", w.Code, w.Body)
	}
	var body struct {
		Units []governanceUnitView `json:"units"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		e.t.Fatal(err)
	}
	for _, p := range body.Units {
		if p.ID == id {
			return p, w.Body.String()
		}
	}
	e.t.Fatalf("GET /governance has no profile %s: %s", id, w.Body)
	return governanceUnitView{}, ""
}

func (e *govEnv) putProfile(c *http.Cookie, id uuid.UUID, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.call(c, http.MethodPut, "/api/v1/governance/units/"+id.String(), string(raw))
}

// standaloneBody is the PUT a reader of v makes back with ceiling mounts set to mounts.
func standaloneBody(v governanceUnitView, mounts []types.WorkspaceMount) map[string]any {
	c := v.Ceiling
	c.WorkspaceMounts = mounts
	return map[string]any{"name": v.Name, "ceiling": c}
}

func storedMount(t *testing.T, ms []types.WorkspaceMount, target string) types.WorkspaceMount {
	t.Helper()
	i := slices.IndexFunc(ms, func(m types.WorkspaceMount) bool { return m.Target == target })
	if i < 0 {
		t.Fatalf("the stored mounts %+v have no %s", ms, target)
	}
	return ms[i]
}

func withheld(target string, ro *bool) types.WorkspaceMount {
	return types.WorkspaceMount{Source: mountSourceWithheld, Target: target, ReadOnly: ro}
}

func rowJSON(t *testing.T, p types.GovernanceUnit) string {
	t.Helper()
	p.UpdatedAt, p.CreatedAt = time.Time{}, time.Time{}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hasSentinel reports whether s carries the sentinel, escaped (an API body) or not (a jsonb read).
func hasSentinel(s string) bool {
	return strings.Contains(s, sentinelJSON) || strings.Contains(s, mountSourceWithheld)
}

func wantStatusReason(t *testing.T, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status || wireReason(t, w) != reason {
		t.Fatalf("status = %d reason = %q (%s), want %d %s", w.Code, wireReason(t, w), w.Body, status, reason)
	}
}

// G1: the security admin's GET -> PUT round trip keeps every stored host path, and nothing the
// security admin reads or is answered names one.
func TestPolMount_G1_SecurityRoundTripKeepsStoredSource(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g1", pinMounts())
	v, body := e.view(e.alice, p.ID)
	if strings.Contains(body, pinMountSrc) || v.Ceiling.WorkspaceMounts[0].Source != mountSourceWithheld {
		t.Fatalf("the security admin read a host path: %s", body)
	}
	w := e.putProfile(e.alice, p.ID, map[string]any{"name": v.Name, "ceiling": v.Ceiling})
	if w.Code != http.StatusOK {
		t.Fatalf("the security admin's verbatim PUT = %d %s, want 200", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), pinMountSrc) {
		t.Errorf("the write answer names a host path: %s", w.Body)
	}
	got := e.profile(p.ID).Ceiling.WorkspaceMounts
	if !slices.Equal(got, pinMounts()) {
		t.Errorf("stored mounts after the round trip = %+v, want %+v", got, pinMounts())
	}
}

// G2: leaving a mount out drops it, and the sentinel cannot bring it back.
func TestPolMount_G2_DropIsFinal(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g2", pinMounts())
	v, _ := e.view(e.alice, p.ID)
	if w := e.putProfile(e.alice, p.ID, standaloneBody(v, []types.WorkspaceMount{withheld(pinTgtA, nil)})); w.Code != http.StatusOK {
		t.Fatalf("drop B = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(p.ID).Ceiling.WorkspaceMounts; !slices.Equal(got, pinMounts()[:1]) {
		t.Fatalf("stored after the drop = %+v, want only A", got)
	}
	before := rowJSON(t, e.profile(p.ID))
	w := e.putProfile(e.alice, p.ID, standaloneBody(v, []types.WorkspaceMount{withheld(pinTgtA, nil), withheld(pinTgtB, nil)}))
	wantStatusReason(t, w, http.StatusUnprocessableEntity, reasonGovernanceMountKeepUnknown)
	if after := rowJSON(t, e.profile(p.ID)); after != before {
		t.Errorf("the refused write changed the row:\n%s\n%s", before, after)
	}
}

// G3: adding a mount is the super admin's. The security admin's 403 is an audited admin_surface.
func TestPolMount_G3_AddIsSuperOnly(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g3", pinMounts())
	added := types.WorkspaceMount{Source: pinMountNew, Target: "/home/agent/pin-c"}
	v, _ := e.view(e.alice, p.ID)
	before := rowJSON(t, e.profile(p.ID))
	denied := len(e.audits(authz.AuditAction))
	w := e.putProfile(e.alice, p.ID, standaloneBody(v, append(slices.Clone(v.Ceiling.WorkspaceMounts), added)))
	wantStatusReason(t, w, http.StatusForbidden, string(authz.ReasonAdminSurface))
	if !strings.Contains(w.Body.String(), "only a super admin may set a mount's host path") {
		t.Errorf("403 body = %s, want the super-admin sentence", w.Body)
	}
	rows := e.audits(authz.AuditAction)
	if len(rows) != denied+1 || rows[len(rows)-1].Target != "governance.unit.ceiling.workspace_mounts[2].source" {
		t.Errorf("authz.denied rows = %+v, want one for ceiling.workspace_mounts[2].source", rows[denied:])
	}
	if after := rowJSON(t, e.profile(p.ID)); after != before {
		t.Errorf("the refused add changed the row")
	}
	sv, _ := e.view(e.super, p.ID)
	if w := e.putProfile(e.super, p.ID, standaloneBody(sv, append(slices.Clone(sv.Ceiling.WorkspaceMounts), added))); w.Code != http.StatusOK {
		t.Fatalf("the super admin's add = %d %s, want 200", w.Code, w.Body)
	}
	if got := storedMount(t, e.profile(p.ID).Ceiling.WorkspaceMounts, added.Target); got.Source != pinMountNew {
		t.Errorf("stored added mount = %+v", got)
	}
}

// G4: changing a host path is the super admin's, and a literal equal to the stored one is refused too.
func TestPolMount_G4_ChangeIsSuperOnly(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g4", pinMounts())
	v, _ := e.view(e.alice, p.ID)
	before := rowJSON(t, e.profile(p.ID))
	for _, src := range []string{pinMountNew, pinMountSrc} {
		ms := slices.Clone(v.Ceiling.WorkspaceMounts)
		ms[0].Source = src
		w := e.putProfile(e.alice, p.ID, standaloneBody(v, ms))
		wantStatusReason(t, w, http.StatusForbidden, string(authz.ReasonAdminSurface))
		if strings.Contains(w.Body.String(), pinMountSrcB) {
			t.Errorf("the refusal names a stored host path: %s", w.Body)
		}
	}
	if after := rowJSON(t, e.profile(p.ID)); after != before {
		t.Errorf("a refused change changed the row")
	}
	sv, _ := e.view(e.super, p.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	if w := e.putProfile(e.super, p.ID, standaloneBody(sv, ms)); w.Code != http.StatusOK {
		t.Fatalf("the super admin's change = %d %s, want 200", w.Code, w.Body)
	}
	if got := storedMount(t, e.profile(p.ID).Ceiling.WorkspaceMounts, pinTgtA); got.Source != pinMountNew {
		t.Errorf("stored = %+v, want %s", got, pinMountNew)
	}
}

// G5: a sentinel on a target nothing stores is 422 for both tiers; G6: a duplicate target keeps its
// existing 400. Neither stores anything.
func TestPolMount_G5_G6_UnknownAndDuplicateTargets(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g5", pinMounts())
	before := rowJSON(t, e.profile(p.ID))
	for _, c := range []struct {
		name string
		who  *http.Cookie
	}{{"security", e.alice}, {"super", e.super}} {
		v, _ := e.view(c.who, p.ID)
		t.Run("G5 unknown target, "+c.name, func(t *testing.T) {
			ms := append(slices.Clone(v.Ceiling.WorkspaceMounts), withheld("/home/agent/never", nil))
			wantStatusReason(t, e.putProfile(c.who, p.ID, standaloneBody(v, ms)), http.StatusUnprocessableEntity, reasonGovernanceMountKeepUnknown)
		})
		t.Run("G6 duplicate target, "+c.name, func(t *testing.T) {
			ms := append(slices.Clone(v.Ceiling.WorkspaceMounts), v.Ceiling.WorkspaceMounts[0])
			w := e.putProfile(c.who, p.ID, standaloneBody(v, ms))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "duplicates another") {
				t.Fatalf("duplicate target = %d %s, want the existing 400", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), pinMountSrc) && c.who == e.alice {
				t.Errorf("the 400 names a stored host path to the security admin: %s", w.Body)
			}
		})
	}
	if after := rowJSON(t, e.profile(p.ID)); after != before {
		t.Errorf("a refused write changed the row:\n%s\n%s", before, after)
	}
}

// G7: matching is by target. [A,B] sent back as [B,A] keeps each host path with its own target.
func TestPolMount_G7_ReorderMatchesByTarget(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g7", pinMounts())
	v, _ := e.view(e.alice, p.ID)
	w := e.putProfile(e.alice, p.ID, standaloneBody(v, []types.WorkspaceMount{withheld(pinTgtB, nil), withheld(pinTgtA, nil)}))
	if w.Code != http.StatusOK {
		t.Fatalf("reordered keep = %d %s, want 200", w.Code, w.Body)
	}
	got := e.profile(p.ID).Ceiling.WorkspaceMounts
	if a := storedMount(t, got, pinTgtA); a.Source != pinMountSrc {
		t.Errorf("BY TARGET: %s now mounts %s, want its own %s (stored %+v)", pinTgtA, a.Source, pinMountSrc, got)
	}
	if b := storedMount(t, got, pinTgtB); b.Source != pinMountSrcB {
		t.Errorf("BY TARGET: %s now mounts %s, want its own %s (stored %+v)", pinTgtB, b.Source, pinMountSrcB, got)
	}
}

// G8: making a kept read-only mount writable is the super admin's.
func TestPolMount_G8_WritableIsSuperOnly(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-g8", pinMounts())
	rw := false
	v, _ := e.view(e.alice, p.ID)
	ms := slices.Clone(v.Ceiling.WorkspaceMounts)
	ms[0].ReadOnly = &rw
	w := e.putProfile(e.alice, p.ID, standaloneBody(v, ms))
	wantStatusReason(t, w, http.StatusForbidden, string(authz.ReasonAdminSurface))
	if !strings.Contains(w.Body.String(), "only a super admin may make a mount writable") {
		t.Errorf("403 body = %s", w.Body)
	}
	if got := storedMount(t, e.profile(p.ID).Ceiling.WorkspaceMounts, pinTgtA); !got.ReadOnlyOrDefault() {
		t.Fatalf("the refused write made %s writable", pinTgtA)
	}
	// The super admin may, both as a sentinel keep and with the literal path.
	sv, _ := e.view(e.super, p.ID)
	for _, src := range []string{mountSourceWithheld, pinMountSrc} {
		ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
		ms[0].Source, ms[0].ReadOnly = src, &rw
		if w := e.putProfile(e.super, p.ID, standaloneBody(sv, ms)); w.Code != http.StatusOK {
			t.Fatalf("the super admin's writable flip (%s) = %d %s, want 200", src, w.Code, w.Body)
		}
		if got := storedMount(t, e.profile(p.ID).Ceiling.WorkspaceMounts, pinTgtA); got.ReadOnlyOrDefault() || got.Source != pinMountSrc {
			t.Errorf("stored = %+v, want %s writable", got, pinMountSrc)
		}
		e.putProfile(e.super, p.ID, standaloneBody(sv, sv.Ceiling.WorkspaceMounts)) // back to read-only
	}
}

// overlayBody is the PUT a reader of a composed view makes back with its overlay's mounts set.
func overlayBody(v governanceUnitView, mounts *[]types.WorkspaceMount) map[string]any {
	ov := types.CeilingOverlay{}
	if v.Overlay != nil {
		ov = *v.Overlay
	}
	ov.WorkspaceMounts = mounts
	return map[string]any{"name": v.Name, "parent_id": v.ParentID, "overlay": ov}
}

// G9: a stored overlay mount keeps its own host path after a super admin re-points the base's mount
// at that target; it is never silently re-paired with the base's new one.
func TestPolMount_G9_OverlayKeepsItsStoredSourceFirst(t *testing.T) {
	e := newMountEnv(t, false)
	base := e.seedMountProfile("pm-g9-base", pinMounts()[:1])
	child := e.seedComposed("pm-g9-child", base, types.CeilingOverlay{WorkspaceMounts: &[]types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}}})
	sv, _ := e.view(e.super, base.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	if w := e.putProfile(e.super, base.ID, standaloneBody(sv, ms)); w.Code != http.StatusOK {
		t.Fatalf("the super admin's base change = %d %s, want 200", w.Code, w.Body)
	}
	v, _ := e.view(e.alice, child.ID)
	w := e.putProfile(e.alice, child.ID, overlayBody(v, v.Overlay.WorkspaceMounts))
	stored := e.profile(child.ID).Overlay.WorkspaceMounts
	if got := storedMount(t, *stored, pinTgtA); got.Source != pinMountSrc {
		t.Fatalf("STORED FIRST: the child's overlay mount was re-paired with %s (status %d %s), want its own %s",
			got.Source, w.Code, w.Body, pinMountSrc)
	}
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), pinMountSrc) {
		t.Errorf("the round trip = %d %s, want the overlay's 400 naming the target only", w.Code, w.Body)
	}
}

// G10: an inherit-all overlay that starts naming a base mount narrows to the base's host path.
func TestPolMount_G10_OverlayNarrowsToTheBaseSource(t *testing.T) {
	e := newMountEnv(t, false)
	base := e.seedMountProfile("pm-g10-base", pinMounts())
	child := e.seedComposed("pm-g10-child", base, types.CeilingOverlay{})
	v, _ := e.view(e.alice, child.ID)
	w := e.putProfile(e.alice, child.ID, overlayBody(v, &[]types.WorkspaceMount{withheld(pinTgtA, nil)}))
	if w.Code != http.StatusOK {
		t.Fatalf("narrow to A = %d %s, want 200", w.Code, w.Body)
	}
	stored := e.profile(child.ID).Overlay.WorkspaceMounts
	if stored == nil || len(*stored) != 1 || (*stored)[0].Source != pinMountSrc {
		t.Errorf("stored overlay mounts = %+v, want only A at the base's %s", stored, pinMountSrc)
	}
}

// G11: folding a composed profile into a standalone ceiling keeps the effective host paths.
func TestPolMount_G11_FoldKeepsTheEffectiveSource(t *testing.T) {
	e := newMountEnv(t, false)
	base := e.seedMountProfile("pm-g11-base", pinMounts())
	child := e.seedComposed("pm-g11-child", base, types.CeilingOverlay{})
	v, _ := e.view(e.alice, child.ID)
	if v.Effective.Ceiling.WorkspaceMounts[0].Source != mountSourceWithheld {
		t.Fatalf("the effective ceiling showed a host path: %+v", v.Effective.Ceiling.WorkspaceMounts)
	}
	w := e.putProfile(e.alice, child.ID, map[string]any{"name": v.Name, "ceiling": v.Effective.Ceiling, "overlay": nil})
	if w.Code != http.StatusOK {
		t.Fatalf("fold = %d %s, want 200", w.Code, w.Body)
	}
	row := e.profile(child.ID)
	if row.Composed() || storedMount(t, row.Ceiling.WorkspaceMounts, pinTgtA).Source != pinMountSrc ||
		storedMount(t, row.Ceiling.WorkspaceMounts, pinTgtB).Source != pinMountSrcB {
		t.Errorf("folded row = %+v, want standalone with the base's host paths", row.Ceiling.WorkspaceMounts)
	}
}

// G12: under four-eyes, the security admin's unchanged round trip is metadata-equal and applies directly.
func TestPolMount_G12_UnchangedRoundTripIsExempt(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-g12", pinMounts())
	v, _ := e.view(e.alice, p.ID)
	w := e.putProfile(e.alice, p.ID, map[string]any{"name": v.Name, "ceiling": v.Ceiling})
	if w.Code != http.StatusOK || e.pendingCount() != 0 {
		t.Fatalf("the unchanged round trip = %d %s (pending %d), want 200 applied directly", w.Code, w.Body, e.pendingCount())
	}
}

// storedChange reads a held change's stored diff (a store read).
func (e *govEnv) storedChange(id uuid.UUID) types.GovernanceChange {
	e.t.Helper()
	ch, err := e.pg.GetGovernanceChange(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ch
}

// A1 + A4 (sets) + R8 (404): a super admin's host-path change is shown to and decided by super admins
// only, its stored diff is whole, and a second super admin's approval stores it.
func TestPolMount_A1_HostPathChangeIsSuperOnly(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-a1", pinMounts())
	sv, _ := e.view(e.super, p.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	ch := e.pending(e.putProfile(e.super, p.ID, standaloneBody(sv, ms)))
	if d := string(e.storedChange(ch.ID).Diff); !strings.Contains(d, pinMountNew) || !strings.Contains(d, pinMountSrc) {
		t.Errorf("A4: the host-path change's stored diff is not whole: %s", d)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusNotFound {
		t.Errorf("the security admin's read of the host-path change = %d, want 404", w.Code)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonAdminSurface) {
		t.Errorf("the security admin's approval = %d %s, want 403 admin_surface", w.Code, w.Body)
	}
	super2 := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	if w := e.call(super2, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a second super admin's approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := storedMount(t, e.profile(p.ID).Ceiling.WorkspaceMounts, pinTgtA); got.Source != pinMountNew {
		t.Errorf("stored after approval = %+v, want %s", got, pinMountNew)
	}
}

// R8: the belt on the 404. A security admin asking for a host-path change by id gets 404, from the list
// and the read alike.
func TestPolMount_R8_SecurityReaderGets404OnHostPathChange(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-r8a", pinMounts())
	sv, _ := e.view(e.super, p.ID)
	ms := append(slices.Clone(sv.Ceiling.WorkspaceMounts), types.WorkspaceMount{Source: pinMountNew, Target: "/home/agent/pin-c"})
	ch := e.pending(e.putProfile(e.super, p.ID, standaloneBody(sv, ms)))
	if w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusNotFound {
		t.Errorf("GET the host-path change as a security admin = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes", ""); strings.Contains(w.Body.String(), ch.ID.String()) {
		t.Errorf("the security admin's list shows the host-path change: %s", w.Body)
	}
}

// R8: the reader-tier belt. A change a security admin may read is served with every mount source
// withheld, even one whose stored diff was rendered whole (a change proposed by an older binary).
func TestPolMount_R8_SecurityReaderSeesSentinelOnKeepOnlyChange(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-r8b", pinMounts())
	before := newUnitDiffView(p, nil, specReadFull) // as a binary before this one rendered every diff
	held, _, err := e.pg.ProposeGovernanceChange(context.Background(), types.GovernanceChange{
		TargetKind: govKindUnit, Op: "delete", TargetKey: p.ID.String(),
		Payload: mustJSON(map[string]any{"id": p.ID}), Diff: renderGovernanceDiff(before, nil, changedPaths(before.unitDiffStored, nil), nil),
		BaseHash: "x", DeploymentHash: "x", ProposedBy: "sub-old", ProposedByEmail: "old@corp.example",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.storedChange(held.ID).Diff), pinMountSrc) {
		t.Fatalf("the seeded change does not carry the host path; the pin proves nothing")
	}
	for _, path := range []string{"/api/v1/governance/changes/" + held.ID.String(), "/api/v1/governance/changes"} {
		w := e.call(e.alice, http.MethodGet, path, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), held.ID.String()) {
			t.Fatalf("GET %s as a security admin = %d %s, want 200 with the change", path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), pinMountSrc) || !strings.Contains(w.Body.String(), `"source":"`+sentinelJSON+`"`) {
			t.Errorf("GET %s: the security admin read a host path, or no sentinel: %s", path, w.Body)
		}
	}
	if w := e.call(e.super, http.MethodGet, "/api/v1/governance/changes/"+held.ID.String(), ""); !strings.Contains(w.Body.String(), pinMountSrc) {
		t.Errorf("the super admin's read lost the host path: %s", w.Body)
	}
}

// A2: a super admin's edit that keeps every mount is held with the mounts as sentinels, so a security
// admin may decide it, and the approval keeps the stored host paths.
func TestPolMount_A2_SuperEditKeepingMountsIsSecurityDecidable(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-a2", pinMounts())
	sv, _ := e.view(e.super, p.ID)
	body := standaloneBody(sv, sv.Ceiling.WorkspaceMounts)
	c := sv.Ceiling
	c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "pypi.org")
	body["ceiling"] = c
	ch := e.pending(e.putProfile(e.super, p.ID, body))
	if strings.Contains(string(e.storedChange(ch.ID).Payload), pinMountSrc) {
		t.Errorf("the held payload names a kept host path: %s", e.storedChange(ch.ID).Payload)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the security admin's approval = %d %s, want 200", w.Code, w.Body)
	}
	row := e.profile(p.ID)
	if !slices.Equal(row.Ceiling.WorkspaceMounts, pinMounts()) || !slices.Contains(row.Ceiling.AllowedDomains, "pypi.org") {
		t.Errorf("stored after approval = %+v", row.Ceiling)
	}
}

// A3 + A4 (keep-only): a security admin's keep-and-drop proposal, approved by another security admin,
// keeps the host path it kept and drops the one it left out. Its stored diff names neither.
func TestPolMount_A3_SecurityKeepDropProposalApplies(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-a3", pinMounts())
	v, _ := e.view(e.alice, p.ID)
	body := standaloneBody(v, []types.WorkspaceMount{withheld(pinTgtA, nil)})
	c := body["ceiling"].(types.RunPolicySpec)
	c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "pypi.org")
	body["ceiling"] = c
	ch := e.pending(e.putProfile(e.alice, p.ID, body))
	stored := e.storedChange(ch.ID)
	if d := string(stored.Diff); strings.Contains(d, pinMountSrc) || strings.Contains(d, pinMountSrcB) || !hasSentinel(d) {
		t.Errorf("A4: the keep-only change's stored diff = %s, want sentinels and no host path", d)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusOK {
		t.Errorf("another security admin's read = %d, want 200", w.Code)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("another security admin's approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(p.ID).Ceiling.WorkspaceMounts; !slices.Equal(got, pinMounts()[:1]) {
		t.Errorf("stored after approval = %+v, want only A at %s", got, pinMountSrc)
	}
}

// D1: after the security admin's keep, dispatch still mounts the real host path for a member the
// profile binds: effectiveCeiling and buildRunMounts both carry it.
func TestPolMount_D1_DispatchGetsTheRealSourceAfterAKeep(t *testing.T) {
	e := newMountEnv(t, false)
	p := e.seedMountProfile("pm-d1", pinMounts())
	e.seedAssignment(p, "sub-user")
	v, _ := e.view(e.alice, p.ID)
	if w := e.putProfile(e.alice, p.ID, map[string]any{"name": v.Name, "ceiling": v.Ceiling}); w.Code != http.StatusOK {
		t.Fatalf("the security admin's keep = %d %s, want 200", w.Code, w.Body)
	}
	ceiling, err := e.srv.effectiveCeiling(operatorCtx("sub-user", "user@corp.example", oidc.RoleUser))
	if err != nil {
		t.Fatal(err)
	}
	if ceiling.Unit == nil || ceiling.Unit.ID != p.ID {
		t.Fatalf("the member is not bound by %s: %+v", p.Name, ceiling.Unit)
	}
	if got := storedMount(t, ceiling.Spec.WorkspaceMounts, pinTgtA).Source; got != pinMountSrc {
		t.Errorf("DISPATCH: effectiveCeiling mounts %s at %s, want the real %s", got, pinTgtA, pinMountSrc)
	}
	mounts := buildRunMounts(ceiling.Spec, userMountPosture{})
	i := slices.IndexFunc(mounts, func(m runner.Mount) bool { return m.Target == pinTgtA })
	if i < 0 || mounts[i].Source != pinMountSrc {
		t.Errorf("DISPATCH: buildRunMounts = %+v, want %s at %s", mounts, pinMountSrc, pinTgtA)
	}
}

// M1: after a super admin re-points a base mount, the security admin's GET /governance warning about
// the child names the target, never the old host path.
func TestPolMount_M1_MeetWarningNamesTheTargetOnly(t *testing.T) {
	e := newMountEnv(t, false)
	base := e.seedMountProfile("pm-m1-base", pinMounts()[:1])
	child := e.seedComposed("pm-m1-child", base, types.CeilingOverlay{WorkspaceMounts: &[]types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}}})
	sv, _ := e.view(e.super, base.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	if w := e.putProfile(e.super, base.ID, standaloneBody(sv, ms)); w.Code != http.StatusOK {
		t.Fatalf("the super admin's base change = %d %s", w.Code, w.Body)
	}
	v, body := e.view(e.alice, child.ID)
	if !slices.ContainsFunc(v.Effective.Warnings, func(w string) bool { return strings.Contains(w, pinTgtA) }) {
		t.Fatalf("no warning names %s: %+v", pinTgtA, v.Effective.Warnings)
	}
	if strings.Contains(body, pinMountSrc) {
		t.Errorf("MESSAGE: the security admin's GET /governance names the old host path: %+v", v.Effective.Warnings)
	}
}

// R7: GET /governance withholds every mount host path from the security tier (stored ceiling,
// overlay, effective ceiling) and serves them to a super admin.
func TestPolMount_R7_GovernanceReadWithholdsMountSources(t *testing.T) {
	e := newMountEnv(t, false)
	base := e.seedMountProfile("pm-r7-base", pinMounts())
	e.seedComposed("pm-r7-child", base, types.CeilingOverlay{WorkspaceMounts: &[]types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}}})
	sec := e.call(e.alice, http.MethodGet, "/api/v1/governance", "").Body.String()
	if strings.Contains(sec, pinMountSrc) || strings.Contains(sec, pinMountSrcB) || !strings.Contains(sec, sentinelJSON) {
		t.Errorf("the security admin's GET /governance = %s, want sentinels and no host path", sec)
	}
	if sup := e.call(e.super, http.MethodGet, "/api/v1/governance", "").Body.String(); !strings.Contains(sup, pinMountSrc) {
		t.Errorf("the super admin's GET /governance lost the host path")
	}
}

// R9: an assignment change is decided by the security tier, so its stored diff withholds host paths.
func TestPolMount_R9_AssignmentDiffWithholdsMountSources(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("pm-r9", pinMounts())
	body, _ := json.Marshal(map[string]any{"subject_type": "user", "subject": "sub-user", "unit_id": p.ID, "priority": 1})
	ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/memberships", string(body)))
	if d := string(e.storedChange(ch.ID).Diff); strings.Contains(d, pinMountSrc) || !hasSentinel(d) {
		t.Errorf("the assignment change's stored diff = %s, want sentinels and no host path", d)
	}
}

// A4: a held profile change's stored diff is rendered for the lowest tier that may decide it: a
// keep-only change withholds every host path, a change that sets one is whole (and super-only, A1).
func TestPolMount_A4_DiffRenderedForTheLowestDecidingTier(t *testing.T) {
	e := newMountEnv(t, true)
	keep := e.seedMountProfile("pm-a4-keep", pinMounts())
	v, _ := e.view(e.alice, keep.ID)
	body := standaloneBody(v, v.Ceiling.WorkspaceMounts)
	c := body["ceiling"].(types.RunPolicySpec)
	c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "pypi.org")
	body["ceiling"] = c
	kept := e.pending(e.putProfile(e.alice, keep.ID, body))
	if d := string(e.storedChange(kept.ID).Diff); strings.Contains(d, pinMountSrc) || strings.Contains(d, pinMountSrcB) || !hasSentinel(d) {
		t.Errorf("KEEP-ONLY: the stored diff = %s, want sentinels and no host path", d)
	}
	sets := e.seedMountProfile("pm-a4-sets", pinMounts())
	sv, _ := e.view(e.super, sets.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	ch := e.pending(e.putProfile(e.super, sets.ID, standaloneBody(sv, ms)))
	if d := string(e.storedChange(ch.ID).Diff); !strings.Contains(d, pinMountSrc) || !strings.Contains(d, pinMountNew) {
		t.Errorf("SETS: the stored diff = %s, want the host paths before and after", d)
	}
}
