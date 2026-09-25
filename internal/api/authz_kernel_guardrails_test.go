// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"go/ast"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── G1: every classOwner route reaches the ownership gate its entity names ──
//
// classOwner is the tight, already-declared slice of routeMatrix G1 can check
// today: each entry already carries a `routeEntity` (F155) naming which seeded
// fixture — run, workspace, approval — its {id} is checked against, which is
// exactly "the kernel Action(s) it must call" for that route, stated by data
// rather than read off a comment. classMember has no such column yet (most of
// it is scoped by the caller's own identity in a store query, not by a named
// ownership predicate — that "Owns" naming is K2-K5's job); extending this
// guard to classMember belongs there, not here.
//
// entityGateFuncs maps each routeEntity to the (*Server) methods that decide
// ownership for it today (design 4.4 row I11). The set may only grow.
var entityGateFuncs = map[routeEntity][]string{
	entityRun:       {"ownsRunOrAdmin", "ownsRunOrSuperAdmin", "getRunAuthorized", "getRunAuthorizedBy"},
	entityWorkspace: {"ownsWorkspaceOrAdmin", "ownsWorkspaceOrSecurityAdmin", "getWorkspaceAuthorized", "denyForeignWorkspace"},
	entityApproval:  {"authorizeMemberDecision"},
}

// authzGuardExceptions names a classOwner route whose handler reaches no
// function entityGateFuncs lists for its entity, with why. An entry here is a
// deliberate reading of the handler, not a default; TestNoAuthzGuardExceptionRot
// fails if one goes stale.
var authzGuardExceptions = map[string]string{
	"GET /api/v1/runs/{id}/recording/{runID}": "mounted via recording.Handler(s.cfg.RecordingStore, " +
		"s.recordingAuthorizer) (routes.go); its leaf handler is that sub-package's own closure, which chi.Walk " +
		"cannot resolve to a Go FuncDecl this guard can parse. The ownership check IS there — recording.go:31 " +
		"recordingAuthorizer, owner-or-operator — just not visible to this AST scan.",
}

// authzGuardExceptionsMax caps authzGuardExceptions, which may only shrink
// (K5). Lower it when an entry goes; raising it needs a reviewed reason.
const authzGuardExceptionsMax = 1

// handlerFuncName recovers h's bare method name ("handleGetRun") from the
// *http.HandlerFunc* chi.Walk hands back for a leaf route — chi's ChainHandler
// unwraps to .Endpoint before calling WalkFunc (tree.go), so h is the raw
// method value the route was registered with, never a middleware closure.
func handlerFuncName(h http.Handler) string {
	hf, ok := h.(http.HandlerFunc)
	if !ok {
		return ""
	}
	full := runtime.FuncForPC(reflect.ValueOf(hf).Pointer()).Name()
	// full looks like ".../internal/api.(*Server).handleGetRun-fm"
	full = strings.TrimSuffix(full, "-fm")
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	return full
}

// leafHandlers maps "METHOD /route" to the (*Server) method name chi.Walk
// finds for it, across both the API router and the UI-enabled one.
func leafHandlers(t *testing.T) map[string]string {
	t.Helper()
	srv, _, _, _ := newAuthzMatrixServer(t)
	out := map[string]string{}
	for _, router := range []chi.Router{srv.router, newAuthzMatrixServerWithUI(t).router} {
		if err := chi.Walk(router, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
			if name := handlerFuncName(h); name != "" {
				out[method+" "+route] = name
			}
			return nil
		}); err != nil {
			t.Fatalf("chi.Walk: %v", err)
		}
	}
	return out
}

// serverCallSets maps every (*Server) method's name to the names of every
// OTHER (*Server) method it references — called directly, or passed by value
// (getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin) passes one as an
// argument) — anywhere in its body. Scoped to receiver "s", the same
// convention preflight_launch_gate_parity_test.go's serverMethodName already
// uses: it is what makes the scan collision-free (capBatch's OWN decide/allowed
// methods use receiver "b" and are never picked up here).
func serverCallSets(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, f := range parseAPISources(t) {
		for _, d := range f.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !isServerReceiver(fn) {
				continue
			}
			refs := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
					refs[sel.Sel.Name] = true
				}
				return true
			})
			out[fn.Name.Name] = refs
		}
	}
	return out
}

// isServerReceiver reports whether fn is a (*Server) method — as opposed to,
// say, a (*capBatch) method also named "decide" or "allowed": without this,
// the two methods' identically-named entries in the flat map below would
// silently overwrite one another.
func isServerReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Server"
}

// reaches reports whether starting from name and following serverCallSets
// transitively (BFS, cycle-safe), any function in want is referenced.
func reaches(calls map[string]map[string]bool, name string, want []string) bool {
	seen := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for ref := range calls[cur] {
			if slices.Contains(want, ref) {
				return true
			}
			if !seen[ref] {
				seen[ref] = true
				queue = append(queue, ref)
			}
		}
	}
	return false
}

// TestAuthzMatrixHandlersReachAuthz is G1: every classOwner route's handler
// statically references the ownership check its declared entity names,
// somewhere in its transitive (*Server) call graph. That is a necessary
// condition, not a per-path proof: a handler that gates one branch and reads
// the store ungated on another still passes. It catches the check being
// dropped outright, which TestAuthzMatrix's runtime probes over one fixture
// cannot rule out. Tier strictness is not checked here either — entityRun
// accepts ownsRunOrAdmin as well as ownsRunOrSuperAdmin — and stays with
// TestAuthzMatrix's ownerTier probes.
func TestAuthzMatrixHandlersReachAuthz(t *testing.T) {
	handlerOf := leafHandlers(t)
	calls := serverCallSets(t)

	for key, rc := range routeMatrix {
		if rc.class != classOwner {
			continue
		}
		if _, exempt := authzGuardExceptions[key]; exempt {
			continue
		}
		want := entityGateFuncs[rc.entity]
		if len(want) == 0 {
			t.Errorf("%s: entity %q has no row in entityGateFuncs", key, rc.entity)
			continue
		}
		name, ok := handlerOf[key]
		if !ok {
			t.Errorf("%s: chi.Walk found no leaf handler (router/table drift, or the handler is a closure this guard cannot resolve — see authzGuardExceptions)", key)
			continue
		}
		if !reaches(calls, name, want) {
			t.Errorf("%s -> %s: never reaches any of %v for entity %q — add the ownership check, or list the route in authzGuardExceptions with why",
				key, name, want, rc.entity)
		}
	}
}

// TestNoAuthzGuardExceptionRot: an exception whose handler now DOES reach its
// entity's gate is a stale licence — narrow the map instead of carrying it
// forward. The map may also only shrink.
func TestNoAuthzGuardExceptionRot(t *testing.T) {
	if len(authzGuardExceptions) > authzGuardExceptionsMax {
		t.Errorf("authzGuardExceptions has %d entries, the cap is %d — gate the new route instead of exempting it",
			len(authzGuardExceptions), authzGuardExceptionsMax)
	}
	handlerOf := leafHandlers(t)
	calls := serverCallSets(t)

	for key := range authzGuardExceptions {
		rc, ok := routeMatrix[key]
		if !ok {
			t.Errorf("authzGuardExceptions names %q, which routeMatrix no longer has — drop the entry", key)
			continue
		}
		name, ok := handlerOf[key]
		if !ok {
			continue // still unresolved by chi.Walk: the exception still applies
		}
		if reaches(calls, name, entityGateFuncs[rc.entity]) {
			t.Errorf("authzGuardExceptions[%s] is stale: %s now reaches its entity's gate — drop the entry", key, name)
		}
	}
}

// ─── G6: the resolver is deterministic and monotone in grants ────────────────
//
// TestCapabilityResolutionIsMonotone: adding an ALLOW grant can only move a
// value's answer DENY -> ALLOW, never the reverse; adding a DENY grant can
// only move ALLOW -> DENY, never the reverse. A resolver that reads its
// snapshot in a different order for different inputs, or that lets a wider
// grant narrow an answer, breaks this without needing to know which rule.
// Randomized rather than exhaustive: TestCapResolverNonescapeTable (K0) already
// exhaustively covers kind x tier x grant-state x store; this is the property
// that table's fixed points cannot state. Each iteration draws the kind from
// capabilityKinds and the switch per kind, so the widening path (image, with
// step 2's switch-off skip and step 4) runs as well as the narrowing one.
//
// The added grant's own subject type and value are drawn at random, the same
// way randomGrants draws base's rows — never pinned to the queried value and
// to capSub. A grant pinned that way can only ever land in scan's exact-match
// branch, so the DENY half is checked only by "an exact-value user DENY
// denies", and a wildcard, different-value or group grant (the paths that
// actually need deny-wins/widen-narrow ordering to hold) is never added. Group
// rows are drawn from grantGroups, which includes "ops" — a group the caller's
// snapshot does not hold. Some iterations mark that snapshot stale (via
// withOIDCGroupsTruncated), so an "ops" DENY reaches scan's ListGroupDenyGrants
// path and is the only thing that decides the answer there: a stale branch that
// grants instead of refusing turns an added DENY into an ALLOW here.
func TestCapabilityResolutionIsMonotone(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	groups := []string{"eng"}
	grantGroups := []string{"eng", "ops"}
	values := []string{"a.example", "b.example", "*.example", "*"}

	decide := func(grants []types.CapabilityGrant, enf map[string]bool, kind, value string, stale bool) bool {
		srv := capServer(&monotoneStore{grants: grants, enf: enf})
		ctx := withOIDCGroups(operatorCtx(capSub, capEmail, oidc.RoleUser), groups)
		ctx = withOIDCGroupsTruncated(ctx, stale)
		allowed, err := srv.newCapBatch(ctx).decide(ctx, kind, capKinds[kind].direction, value)
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		return allowed
	}

	randSubject := func() (types.CapabilitySubjectType, string) {
		if rng.Intn(2) == 0 {
			return types.CapabilitySubjectGroup, grantGroups[rng.Intn(len(grantGroups))]
		}
		return types.CapabilitySubjectUser, capSub
	}

	for i := 0; i < 700; i++ {
		kind := capabilityKinds[rng.Intn(len(capabilityKinds))]
		value := values[rng.Intn(len(values))]
		stale := rng.Intn(4) == 0
		base := randomGrants(rng, kind, grantGroups, values)
		enf := map[string]bool{}
		for _, k := range capabilityKinds {
			enf[k] = rng.Intn(2) == 1
		}

		before := decide(base, enf, kind, value, stale)

		allowSubjType, allowSubject := randSubject()
		allowGrant := grant(allowSubjType, allowSubject, kind, values[rng.Intn(len(values))], types.CapabilityAllow)
		afterAllow := decide(append(slices.Clone(base), allowGrant), enf, kind, value, stale)
		if before && !afterAllow {
			t.Fatalf("%s: adding an ALLOW grant (%s %s=%s) turned an ALLOW into a DENY for %q (enforced=%v, stale=%v, base=%v)",
				kind, allowSubjType, allowSubject, allowGrant.Value, value, enf[kind], stale, base)
		}

		denySubjType, denySubject := randSubject()
		denyGrant := grant(denySubjType, denySubject, kind, values[rng.Intn(len(values))], types.CapabilityDeny)
		afterDeny := decide(append(slices.Clone(base), denyGrant), enf, kind, value, stale)
		if !before && afterDeny {
			t.Fatalf("%s: adding a DENY grant (%s %s=%s) turned a DENY into an ALLOW for %q (enforced=%v, stale=%v, base=%v)",
				kind, denySubjType, denySubject, denyGrant.Value, value, enf[kind], stale, base)
		}
	}
}

func randomGrants(rng *rand.Rand, kind string, groups, values []string) []types.CapabilityGrant {
	n := rng.Intn(4)
	out := make([]types.CapabilityGrant, 0, n)
	for i := 0; i < n; i++ {
		effect := types.CapabilityAllow
		if rng.Intn(2) == 0 {
			effect = types.CapabilityDeny
		}
		subjType, subject := types.CapabilitySubjectUser, capSub
		if rng.Intn(2) == 0 {
			subjType, subject = types.CapabilitySubjectGroup, groups[rng.Intn(len(groups))]
		}
		out = append(out, grant(subjType, subject, kind, values[rng.Intn(len(values))], effect))
	}
	return out
}

// monotoneStore is a minimal capability store for TestCapabilityResolutionIsMonotone.
type monotoneStore struct {
	store.Store
	grants []types.CapabilityGrant
	enf    map[string]bool
}

func (s *monotoneStore) ListCapabilityGrantsFor(_ context.Context, users, groups []string) ([]types.CapabilityGrant, error) {
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		if (g.SubjectType == types.CapabilitySubjectUser && slices.Contains(users, g.Subject)) ||
			(g.SubjectType == types.CapabilitySubjectGroup && slices.Contains(groups, g.Subject)) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *monotoneStore) ListGroupDenyGrants(_ context.Context, kind string) ([]types.CapabilityGrant, error) {
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		if g.SubjectType == types.CapabilitySubjectGroup && g.Effect == types.CapabilityDeny && g.Capability == kind {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *monotoneStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return s.enf, nil
}

// ─── G7: the console's kind mirror cannot drift from the Go table (D10) ──────
//
// TestConsoleCapabilityKindsMatchGoTable reads permissions-copy.ts's
// CAPABILITY_KINDS and KIND[...].direction and compares them to capabilityKinds
// and capKinds: nothing today catches this drift (design D10). The
// "restricted value hidden from the picker, a pinned reference disables
// Launch" half of G7 (UT-7b's Playwright acceptance) needs the "Available to"
// feature this compares against (UT-10) wired into the console first — it is
// not yet, so that half is not built here; see follow-up note in the PR.
func TestConsoleCapabilityKindsMatchGoTable(t *testing.T) {
	src := readTSFile(t, "permissions-copy.ts")

	kindsLit := mustMatch(t, src, `CAPABILITY_KINDS\s*=\s*\[([^\]]*)\]`)
	var tsKinds []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(kindsLit, -1) {
		tsKinds = append(tsKinds, m[1])
	}
	slices.Sort(tsKinds)

	goKinds := slices.Clone(capabilityKinds)
	slices.Sort(goKinds)

	if !slices.Equal(tsKinds, goKinds) {
		t.Errorf("permissions-copy.ts CAPABILITY_KINDS = %v, internal/api's capabilityKinds = %v — the console mirror has drifted (D10)", tsKinds, goKinds)
	}

	// direction: every kind entry's `direction: "narrows" | "widens"` must
	// agree with capKinds[kind].direction.
	dirRe := regexp.MustCompile(`(?s)([a-z_]+):\s*\{.*?direction:\s*"(narrows|widens)"`)
	tsDir := map[string]string{}
	for _, m := range dirRe.FindAllStringSubmatch(src, -1) {
		if slices.Contains(tsKinds, m[1]) {
			tsDir[m[1]] = m[2]
		}
	}
	for kind, k := range capKinds {
		want := "narrows"
		if k.direction == capWidening {
			want = "widens"
		}
		got, ok := tsDir[kind]
		if !ok {
			t.Errorf("permissions-copy.ts KIND has no direction parsed for %q", kind)
			continue
		}
		if got != want {
			t.Errorf("permissions-copy.ts KIND[%q].direction = %q, internal/api's capKinds says %q", kind, got, want)
		}
	}
}

func readTSFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../ui/src/app/lib/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func mustMatch(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("pattern %q did not match", pattern)
	}
	return m[1]
}
