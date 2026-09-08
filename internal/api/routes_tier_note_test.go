// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// routesNoteFor returns the contiguous //-comment block that immediately
// precedes reg in routes.go — the tier note a reader of that registration
// line is told to believe.
func routesNoteFor(t *testing.T, reg string) string {
	t.Helper()
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	at := -1
	for i, ln := range lines {
		if strings.Contains(ln, reg) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("routes.go no longer registers %s — this pin is keyed to that line", reg)
	}
	var note []string
	for i := at - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "//") {
			break
		}
		note = append([]string{strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))}, note...)
	}
	return strings.Join(note, " ")
}

// TestIntegrationsRouteNoteMatchesItsRealTier is F247's second half.
//
// The finding is two claims in one title: GET /integrations served credential
// refs and internal egress hosts to any member (closed by
// memberSafeIntegration), AND routes.go's own justification for the wide tier
// — "Read-only, same RBAC posture as site-config's GET: Credentials only ever
// holds secret NAMES" — went false when site-config's GET moved to
// operatorOnly. Both halves are load-bearing: the sentence is what the next
// reader consults before widening or narrowing this route, and it asserts a
// parity that the router does not implement and a payload fact ("only secret
// NAMES") that is precisely the reason the sibling narrowed.
//
// The invariant pinned here is self-maintaining rather than a spelling check:
// WHILE the two routes' routeMatrix classes differ, the note may not claim
// they share a posture — and it must instead name the projection that makes
// the wider tier honest. If a later change moves GET /integrations to
// site-config's class, the parity clause becomes true and this pin stops
// demanding its absence.
func TestIntegrationsRouteNoteMatchesItsRealTier(t *testing.T) {
	const reg = `r.Get("/integrations", s.handleListIntegrations)`
	integ, ok := routeMatrix["GET /api/v1/integrations"]
	if !ok {
		t.Fatal("routeMatrix has no GET /api/v1/integrations row")
	}
	site, ok := routeMatrix["GET /api/v1/site-config"]
	if !ok {
		t.Fatal("routeMatrix has no GET /api/v1/site-config row")
	}
	note := routesNoteFor(t, reg)

	if integ.class == site.class {
		t.Skip("the two routes now share a class; the parity clause would be true")
	}

	// The claim, in the shape it is written: "same RBAC posture as\n// site-config's GET".
	if strings.Contains(strings.ToLower(note), "same rbac posture as") {
		t.Errorf("routes.go tells the next reader GET /integrations has the %q posture, but the router gives it %q "+
			"while GET /api/v1/site-config is %q. The sibling narrowed BECAUSE the document carries "+
			"integrations[].secrets[].secret_name, and this route serves those same rows.\nnote=%s",
			"same RBAC", integ.class, site.class, note)
	}
	// A wider tier is honest only because of the projection; the note has to
	// say so, or the next reader re-derives the deleted justification.
	if !strings.Contains(note, "memberSafeIntegration") {
		t.Errorf("routes.go's note for GET /integrations never names memberSafeIntegration — the projection is the "+
			"whole reason a member-class route may serve rows whose document is operator-only.\nnote=%s", note)
	}

	// The behavioural anchor for the class comparison above: the same session,
	// the same rows, two different answers.
	h := newHarness(t)
	cfg := baseTestConfig(h, r3IntegStore{})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	member := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/integrations", member, ""); w.Code != http.StatusOK {
		t.Fatalf("member GET /integrations = %d, want 200 (the projected read)", w.Code)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/site-config", member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET /site-config = %d, want 403 — the asymmetry the note denies", w.Code)
	}
}
