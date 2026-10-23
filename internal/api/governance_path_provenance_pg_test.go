// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// path_provenance (the policy tree's admin reads): which unit on a context's path changed each
// knob, beside the leaf-only provenance a run gets. pathTree (governance_provenance_golden_test.go)
// is the tree every pin reads.

// checkWalls is POST /governance/check's two provenances for one user subject.
type checkWalls struct {
	Provenance     map[string]policyref.Provenance `json:"provenance"`
	PathProvenance map[string]policyref.Provenance `json:"path_provenance"`
}

func checkWallsFor(t *testing.T, e *govEnv, sub string) checkWalls {
	t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["`+sub+`"]}`)
	var body struct {
		Effective checkWalls `json:"effective"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("check %s = %d %s", sub, w.Code, w.Body)
	}
	return body.Effective
}

// unitPathProvenance is GET /governance's path_provenance per unit id.
func unitPathProvenance(t *testing.T, e *govEnv) map[uuid.UUID]map[string]policyref.Provenance {
	t.Helper()
	w := e.admin(http.MethodGet, "/api/v1/governance", "")
	var body struct {
		Units []struct {
			ID             uuid.UUID                       `json:"id"`
			PathProvenance map[string]policyref.Provenance `json:"path_provenance"`
		} `json:"units"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("GET /governance = %d %s", w.Code, w.Body)
	}
	out := map[uuid.UUID]map[string]policyref.Provenance{}
	for _, u := range body.Units {
		out[u.ID] = u.PathProvenance
	}
	return out
}

func unitRec(name string) policyref.Provenance {
	return policyref.Provenance{Layer: policyref.LayerUnit, Name: name}
}

// pathPin asserts one knob's path_provenance on GET /governance and on check for the unit's home
// member, and that check's provenance names the leaf for it (so the scenario fired).
func pathPin(t *testing.T, knob string, want policyref.Provenance) {
	t.Helper()
	e := newGovEnvOn(t, throwawayPGPool(t))
	_, b, _ := pathTree(t, e)
	if got := unitPathProvenance(t, e)[b.ID][knob]; got != want {
		t.Errorf("GET /governance pp-b path_provenance[%s] = %+v, want %+v", knob, got, want)
	}
	walls := checkWallsFor(t, e, "sub-pp-b")
	if got := walls.PathProvenance[knob]; got != want {
		t.Errorf("check pp-b path_provenance[%s] = %+v, want %+v", knob, got, want)
	}
	if got := walls.Provenance[knob]; got != unitRec("pp-b") {
		t.Errorf("check pp-b provenance[%s] = %+v, want the leaf pp-b: the scenario did not fire", knob, got)
	}
}

// TestPathProvenanceRestatedIsTheParents: pp-b restates pp-a's max_holds, so pp-a changed it.
func TestPathProvenanceRestatedIsTheParents(t *testing.T) {
	pathPin(t, "max_holds", unitRec("pp-a"))
}

// TestPathProvenanceClampedWideningIsTheParents: pp-b widens first_use_hold_seconds and pp-a's meet
// keeps its own value, so pp-b changed nothing.
func TestPathProvenanceClampedWideningIsTheParents(t *testing.T) {
	pathPin(t, "first_use_hold_seconds", unitRec("pp-a"))
}

// TestPathProvenanceNarrowedIsTheChilds: pp-b narrows auto_stop_after_sec.
func TestPathProvenanceNarrowedIsTheChilds(t *testing.T) {
	pathPin(t, "auto_stop_after_sec", unitRec("pp-b"))
}

// TestPathProvenancePassThroughIsNeverCredited: pp-c is a pass-through leaf. check's provenance
// names it (it is the leaf), its path_provenance never does: each knob is its parents'.
func TestPathProvenancePassThroughIsNeverCredited(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	_, _, c := pathTree(t, e)
	walls := checkWallsFor(t, e, "sub-pp-c")
	if walls.Provenance["max_holds"] != unitRec("pp-c") {
		t.Fatalf("check pp-c provenance[max_holds] = %+v, want the leaf pp-c: the scenario did not fire", walls.Provenance["max_holds"])
	}
	for where, path := range map[string]map[string]policyref.Provenance{
		"GET /governance": unitPathProvenance(t, e)[c.ID], "check": walls.PathProvenance,
	} {
		if len(path) == 0 {
			t.Errorf("%s pp-c path_provenance is empty", where)
		}
		for knob, rec := range path {
			if rec.Name == "pp-c" {
				t.Errorf("%s pp-c path_provenance[%s] = %+v: a pass-through unit changes nothing", where, knob, rec)
			}
		}
		if path["max_holds"] != unitRec("pp-a") || path["auto_stop_after_sec"] != unitRec("pp-b") {
			t.Errorf("%s pp-c path_provenance max_holds %+v auto_stop_after_sec %+v, want pp-a and pp-b",
				where, path["max_holds"], path["auto_stop_after_sec"])
		}
	}
}

// pathHome is one unit of pathTree, the member homed in it and the leaf check names ("" for Default).
type pathHome struct {
	id        uuid.UUID
	sub, leaf string
}

// pathHomes seeds pathTree and names each unit's home member (sub-pp-none has no membership: Default).
func pathHomes(t *testing.T, e *govEnv) map[string]pathHome {
	t.Helper()
	a, b, c := pathTree(t, e)
	return map[string]pathHome{
		"Default": {store.DefaultUnitID, "sub-pp-none", ""},
		"pp-a":    {a.ID, "sub-pp-a", "pp-a"},
		"pp-b":    {b.ID, "sub-pp-b", "pp-b"},
		"pp-c":    {c.ID, "sub-pp-c", "pp-c"},
	}
}

// TestPathProvenanceGetEqualsCheck: each unit's GET /governance path_provenance is check's for a
// person homed in it, over every knob, across the three levels and Default.
func TestPathProvenanceGetEqualsCheck(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	homes := pathHomes(t, e)
	units := unitPathProvenance(t, e)
	boundaryCredited := false
	for name, h := range homes {
		got, want := units[h.id], checkWallsFor(t, e, h.sub).PathProvenance
		if len(want) == 0 || !maps.Equal(got, want) {
			t.Errorf("%s: GET /governance path_provenance =\n%+v\ncheck's =\n%+v", name, got, want)
		}
		for _, rec := range want {
			boundaryCredited = boundaryCredited || rec == boundaryProvenance
		}
	}
	if !boundaryCredited {
		t.Errorf("no knob is the boundary's: the scenario does not exercise the boundary step")
	}
}

// TestPathProvenanceDeploymentFillInIsTheDeployments: no unit on pathTree sets resources, so the
// size Default's root fills in from the deployment stays the deployment's on every unit, Default's
// own included, on GET /governance and on check.
func TestPathProvenanceDeploymentFillInIsTheDeployments(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	if reflect.DeepEqual(e.srv.cfg.DefaultPolicy.Resources, e.srv.deploymentAuthority().Ceiling.Resources) {
		t.Fatalf("the deployment default's resources need no fill-in: the scenario does not fire")
	}
	homes := pathHomes(t, e)
	units := unitPathProvenance(t, e)
	for name, h := range homes {
		for where, path := range map[string]map[string]policyref.Provenance{
			"GET /governance": units[h.id], "check": checkWallsFor(t, e, h.sub).PathProvenance,
		} {
			if got := path["resources"]; got != deploymentProvenance {
				t.Errorf("%s %s path_provenance[resources] = %+v, want the deployment's", where, name, got)
			}
		}
	}
}

// TestPathProvenanceStandaloneRootIsTheRoots: a standalone root's own walls are its own: its
// resources (unlike the deployment's) stay the root's under a composed child that leaves them.
func TestPathProvenanceStandaloneRootIsTheRoots(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	eight := 8
	root, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{Name: "pp-sa", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2,
			Resources: &types.ResourceLimits{CPUMillis: 500}}})
	if err != nil {
		t.Fatal(err)
	}
	child := e.seedComposed("pp-sa-child", root, types.CeilingOverlay{MaxHolds: &eight})
	treeMember(t, e, child, types.CapabilitySubjectUser, "sub-pp-sa-child", 1, true)
	if reflect.DeepEqual(root.Ceiling.Resources, e.srv.deploymentAuthority().Ceiling.Resources) {
		t.Fatalf("the root's resources are the deployment's: the scenario does not fire")
	}
	for where, path := range map[string]map[string]policyref.Provenance{
		"GET /governance": unitPathProvenance(t, e)[child.ID], "check": checkWallsFor(t, e, "sub-pp-sa-child").PathProvenance,
	} {
		if got := path["resources"]; got != unitRec("pp-sa") {
			t.Errorf("%s pp-sa-child path_provenance[resources] = %+v, want the standalone root pp-sa", where, got)
		}
	}
}

// TestPathProvenanceAgreesWithProvenance: for every knob of every context, path_provenance names a
// unit exactly when provenance names the leaf, and the two are equal on every deployment and
// boundary record. The one exception is resources' deployment fill-in, which check's provenance
// credits to the leaf.
func TestPathProvenanceAgreesWithProvenance(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	refined := false
	for name, h := range pathHomes(t, e) {
		walls := checkWallsFor(t, e, h.sub)
		if len(walls.Provenance) != len(walls.PathProvenance) {
			t.Errorf("%s: provenance has %d knobs, path_provenance %d", name, len(walls.Provenance), len(walls.PathProvenance))
		}
		for knob, prov := range walls.Provenance {
			path, ok := walls.PathProvenance[knob]
			leaf := h.leaf != "" && prov == unitRec(h.leaf)
			switch {
			case leaf && knob == "resources" && path == deploymentProvenance:
				// check's leaf-only provenance credits the leaf with the deployment's own size fill-in;
				// the path keeps it the deployment's (TestPathProvenanceDeploymentFillInIsTheDeployments).
			case !ok:
				t.Errorf("%s: path_provenance has no %s", name, knob)
			case leaf != (path.Layer == policyref.LayerUnit):
				t.Errorf("%s %s: provenance %+v, path_provenance %+v: a unit is named exactly where the leaf is", name, knob, prov, path)
			case !leaf && path != prov:
				t.Errorf("%s %s: provenance %+v, path_provenance %+v: the deployment and boundary records agree", name, knob, prov, path)
			}
			refined = refined || (leaf && path.Name != h.leaf)
		}
	}
	if !refined {
		t.Errorf("path_provenance never names a unit above the leaf: the scenario proves nothing")
	}
}

// TestContextWallsRedactAtTheCallersTier: the check's walls (contextWalls) and GET /governance's units
// are redacted at the caller's tier: a security admin reads the deployment's mount host path as the
// sentinel, the super admin reads it.
func TestContextWallsRedactAtTheCallersTier(t *testing.T) {
	e := newMountEnv(t, false)
	e.pinRead(http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["sub-pp-none"]}`, pinBaseSrc)
	e.pinRead(http.MethodGet, "/api/v1/governance", "", pinBaseSrc)
}
