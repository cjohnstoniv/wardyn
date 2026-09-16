// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
)

// ─── B6-F6: the anonymous /healthz stops publishing fleet volumes ────────────

// TestHealthzEbpfPublishesNoCounters pins B6-F6. /metrics is operator-gated
// precisely because "a member … would learn operational volumes", and the
// anonymous /healthz was publishing the eBPF sensor's cumulative
// observed_total / dropped_total / observed_by_kind to anyone who could reach
// the port. The VERDICT is what a health probe and the sign-in screen need; the
// counters belong on the gated scrape.
func TestHealthzEbpfPublishesNoCounters(t *testing.T) {
	h := newHarness(t)
	hb := groundtruth.HeartbeatEventWithDropped(99_000, 12, 7, map[string]uint64{
		groundtruth.ActionProcessExec: 40_000,
	})
	hb.Time = time.Now()
	h.srv.cfg.Store = stubHeartbeatStore{ev: hb}

	gt := healthzEbpf(t, h)
	allowed := []string{"state", "last_heartbeat", "reason", "missing_kinds"}
	for k := range gt {
		if !slices.Contains(allowed, k) {
			t.Errorf("anonymous /healthz publishes ebpf_groundtruth.%v = %v — the counters are "+
				"fleet-volume data and belong on the operator-gated /metrics", k, gt[k])
		}
	}

	// NEGATIVE CONTROL: the verdict and its reason still reach the anonymous
	// probe — deploy/compose/README.md tells operators to read the idle reason
	// there, and ebpfGroundtruthCaveat reads only state + missing_kinds.
	blind := newHarness(t)
	idle := groundtruth.HeartbeatEventWithDropped(0, 0, 4812, nil)
	idle.Time = time.Now()
	blind.srv.cfg.Store = stubHeartbeatStore{ev: idle}
	got := healthzEbpf(t, blind)
	if got["state"] != "idle" {
		t.Errorf("state = %v, want idle", got["state"])
	}
	reason, _ := got["reason"].(string)
	if !strings.Contains(reason, "none correlated") {
		t.Errorf("reason = %q, want the broken-correlation sentence the compose README points at", reason)
	}
}

// ─── B6-F8: authenticated API responses are not cacheable ────────────────────

// TestAPIResponsesAreNoStore pins B6-F8. The OIDC lane authenticates by COOKIE,
// not by Authorization, so a 200 `GET /runs` carried no validator and no
// Cache-Control at all — heuristically cacheable by any interposed shared
// cache, which is one member's run list served to another.
func TestAPIResponsesAreNoStore(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/api/v1/runs"} {
		w := do(t, h.srv, http.MethodGet, path, "", "")
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, "no-store")
		}
	}
}

// TestHashedAssetsStayImmutable is B6-F8's negative control: content-addressed
// bundles under /assets/ are safe to cache forever and MUST keep doing so, or
// every console load re-downloads the whole bundle. The SPA shell keeps
// no-cache so a redeploy's new hashed bundle is picked up.
func TestHashedAssetsStayImmutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-deadbeef.js"), []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.UIDir = dir
	srv := New(h.srv.cfg)

	for path, want := range map[string]string{
		"/assets/app-deadbeef.js": "public, max-age=31536000, immutable",
		"/":                       "no-cache",
	} {
		w := do(t, srv, http.MethodGet, path, "", "")
		if got := w.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, want)
		}
	}
}

// ─── B6-F7: a handler's doc comment may not misstate its tier ────────────────

// handlerDocRE finds one handler's doc comment block plus its func line.
var handlerDocRE = regexp.MustCompile(`(?s)((?://[^\n]*\n)+)func \(s \*Server\) (handle\w+)\(`)

// handlerDocTiers pairs a handler with the route it serves. routeMatrix is the
// authoritative classification (the chi.Walk test enforces it against the live
// router), so a route that MOVES between router groups reds this test until the
// handler's own doc comment moves with it.
var handlerDocTiers = []struct{ fn, route string }{
	{"handleVerifyAuditChain", "GET /api/v1/audit/chain/verify"},
	{"handleQueryAudit", "GET /api/v1/audit"},
	{"handleMetrics", "GET /metrics"},
}

// TestHandlerDocsMatchTheRegisteredTier is B6-F7. audit.go's two comments were
// both wrong: handleVerifyAuditChain claimed operator-only when it is
// securityOps, and handleQueryAudit said the audit log "is never gated" when it
// is authenticated AND row-scoped per principal. A doc comment is where the
// next person reads the tier from, so a wrong one is how a surface gets widened
// by accident.
func TestHandlerDocsMatchTheRegisteredTier(t *testing.T) {
	docs := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(e.Name())
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		for _, m := range handlerDocRE.FindAllStringSubmatch(string(b), -1) {
			// FIRST PARAGRAPH ONLY. A handler's tier claim belongs in its opening
			// sentences; later paragraphs legitimately mention OTHER routes' tiers
			// ("the same disclosure that keeps the scrape surface gated"), and a
			// whole-comment scan reads those as claims about this handler.
			docs[m[2]], _, _ = strings.Cut(m[1], "//\n")
		}
	}
	if len(docs) < 50 {
		t.Fatalf("found %d handler doc comments; this package has well over 100 — the walk stopped", len(docs))
	}

	// The token a doc comment must carry for the group its route is on, and the
	// tokens it must NOT carry for any other.
	want := map[routeClass][]string{
		classAdmin:    {"operatorOnly", "operator-only", "Operator-only", "admin-only", "Admin-only"},
		classSecurity: {"securityOps", "security_admin"},
	}
	for _, tc := range handlerDocTiers {
		doc, ok := docs[tc.fn]
		if !ok {
			t.Errorf("no doc comment found for %s — the handler was renamed or lost its comment", tc.fn)
			continue
		}
		rc, ok := routeMatrix[tc.route]
		if !ok {
			t.Errorf("%s: route %q is not in routeMatrix", tc.fn, tc.route)
			continue
		}
		if strings.Contains(doc, "never gated") {
			t.Errorf("%s's doc says the surface is \"never gated\"; routeMatrix classifies %s as %s",
				tc.fn, tc.route, rc.class)
		}
		tokens, gated := want[rc.class]
		if !gated {
			continue
		}
		if !slices.ContainsFunc(tokens, func(tok string) bool { return strings.Contains(doc, tok) }) {
			t.Errorf("%s serves %s (class %s) but its doc comment names none of %v — "+
				"the comment is where the next person reads the tier from", tc.fn, tc.route, rc.class, tokens)
		}
		for otherClass, otherTokens := range want {
			if otherClass == rc.class {
				continue
			}
			for _, tok := range otherTokens {
				if strings.Contains(doc, tok) && !slices.Contains(tokens, tok) {
					t.Errorf("%s serves %s (class %s) but its doc comment claims %q",
						tc.fn, tc.route, rc.class, tok)
				}
			}
		}
	}
}
