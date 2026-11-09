// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A unit that names a port beside a bare host keeps it when its parent names it too, and the meet
// reads a bare entry as 80 and 443 only: a change from [h] to [h, h:22] is a widening wherever a
// second person decides one, and an envelope admits it only within its root's walls.

// portRuleEnv is newUnitAdminEnv whose Default ceiling also names pypi.org:22.
func portRuleEnv(t *testing.T) *unitAdminEnv {
	return newUnitAdminEnv(t, false, func(c *Config) {
		c.DefaultPolicy.AllowedDomains = append([]string{"pypi.org:22"}, c.DefaultPolicy.AllowedDomains...)
	})
}

// composedDomains is u's effective allowed_domains, composed down its chain as a write reads it.
func (e *unitAdminEnv) composedDomains(u types.GovernanceUnit) []string {
	e.t.Helper()
	chain, err := unitChainQ(context.Background(), e.pool, u.ID, false)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.srv.composeChain(chain)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Ceiling.AllowedDomains
}

// A path to a unit that adds pypi.org:22 beside pypi.org does not narrow; the way back does.
func TestPathNarrowsReadsAPortBesideABareName(t *testing.T) {
	e := portRuleEnv(t)
	x := treeChild(t, e.govEnv, uaUnitBody(e.fresh("port-x"), store.DefaultUnitID, uaDomains("pypi.org", "pypi.org:22"), nil))
	bare, port := e.envBelow(x, uaDomains("pypi.org")), e.envBelow(x, uaDomains("pypi.org", "pypi.org:22"))
	if got := e.composedDomains(port); !slices.Contains(got, "pypi.org:22") {
		t.Fatalf("composed %v, want pypi.org:22 kept where every level names it", got)
	}
	ctx := context.Background()
	if ok, err := e.srv.pathNarrowsQ(ctx, e.pool, bare.ID, port.ID); err != nil || ok {
		t.Errorf("bare to port narrows = %v (%v), want false: the port unit reaches pypi.org:22", ok, err)
	}
	if ok, err := e.srv.pathNarrowsQ(ctx, e.pool, port.ID, bare.ID); err != nil || !ok {
		t.Errorf("port to bare narrows = %v (%v), want true", ok, err)
	}
}

// A unit admin's edit from [pypi.org] to [pypi.org, pypi.org:22] waits for a second person with no
// envelope. Below an envelope root that names pypi.org:22 it applies and the unit reaches it. Below a
// root that names pypi.org bare it is refused as an overlay the walls do not cover, envelope or not, and
// the unit still does not reach it: never beyond the root's walls.
func TestEnvelopeAdmitsAPortBesideABareNameOnlyWithinTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  []string
		reach bool
	}{
		{"a root naming the port", []string{"pypi.org", "pypi.org:22"}, true},
		{"a root naming the host bare", []string{"pypi.org"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := portRuleEnv(t)
			root := treeChild(t, e.govEnv, uaUnitBody(e.fresh("port-root"), store.DefaultUnitID, uaDomains(tc.root...), nil))
			e.grant(root, types.CapabilitySubjectUser, uaSubAdminA1)
			g := e.envBelow(root, uaDomains("pypi.org"))
			wide := envUnitBody(g, root.ID, uaDomains("pypi.org", "pypi.org:22"))
			if tc.reach {
				e.reject(e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(g), wide))) // no envelope yet: held
			}

			e.seedEnvelope(root)
			before := e.pendingCount()
			w := e.call(e.adminA1, http.MethodPut, envUnitPath(g), wide)
			switch {
			case tc.reach && w.Code != http.StatusOK:
				t.Fatalf("an edit within the root's walls = %d %s, want 200", w.Code, w.Body)
			case !tc.reach && (w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceOverlayInvalid):
				t.Fatalf("an edit beyond the root's walls = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceOverlayInvalid)
			}
			if e.pendingCount() != before {
				t.Errorf("%d pending (was %d), want no new change", e.pendingCount(), before)
			}
			if got := e.composedDomains(g); slices.Contains(got, "pypi.org:22") != tc.reach {
				t.Errorf("composed %v, want pypi.org:22 reached = %v", got, tc.reach)
			}
		})
	}
}
