// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
)

// Every test in this package runs with the registry strict: a door that
// refuses with a reason nobody registered panics its own test.
func init() { strictRefusals = true }

// TestRefuseFailsClosedOnAnUnregisteredReason: in production an unregistered
// reason is a 500, never the 403 the door meant — the only way to ship a new
// reason is to register it, which is what holds the audit enum closed — and
// the row is still written, because a refusal that happened is evidence.
func TestRefuseFailsClosedOnAnUnregisteredReason(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, nil))
	d := authz.Deny(authz.Reason("invented_here"), "runs.image", "a sentence the door meant")

	t.Run("production answers 500 and records the row", func(t *testing.T) {
		strictRefusals = false
		t.Cleanup(func() { strictRefusals = true })
		w := httptest.NewRecorder()
		srv.refuse(w, httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), d)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("unregistered reason answered %d %q, want 500 — a 403 here ships an undocumented reason", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "a sentence the door meant") {
			t.Errorf("body %q carries the door's sentence; an unregistered refusal must not look like a normal one", w.Body.String())
		}
		ev := lastAuditEvent(t, h.audit.events, authz.AuditAction)
		if data := auditData(t, ev); data["reason"] != "invented_here" {
			t.Errorf("row data = %v, want the refusal recorded as it happened", data)
		}
	})

	t.Run("tests panic", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("an unregistered reason did not panic under strictRefusals")
			}
		}()
		srv.refuse(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), d)
	})
}

// TestStaleSnapshotRowCarriesTheMemberModeMarker: the two groups_snapshot_stale
// emits hand-rolled their datum, so an admin in member mode refused there wrote
// a row with no member_mode marker — reading as a member incident, the one
// outcome the marker exists to prevent. Every row now comes from authz.Datum.
func TestStaleSnapshotRowCarriesTheMemberModeMarker(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, staleAuditStore{hasGroupTier: true})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	on := memberModeSSOSession(t, "sub-stale-admin", "admin@corp.example", oidc.RoleAdmin, true)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default", on, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member-mode admin with no group snapshot = %d, want 403: %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, authz.AuditAction)
	data := auditData(t, ev)
	if data["reason"] != string(authz.ReasonGroupsSnapshotStale) || ev.Target != "governance.ceiling" {
		t.Fatalf("row = %s %v, want governance.ceiling groups_snapshot_stale", ev.Target, data)
	}
	if data["member_mode"] != true {
		t.Errorf("data = %v, want member_mode:true — an admin walking the member path", data)
	}
}

// TestCapKindReasonsAreRegistered: every capability kind refuses with a
// registered reason, so a new kind cannot ship a refusal the audit enum lacks.
func TestCapKindReasonsAreRegistered(t *testing.T) {
	for name, k := range capKinds {
		if _, ok := authz.Lookup(k.reason); !ok {
			t.Errorf("capKinds[%s].reason = %q, not registered in internal/authz", name, k.reason)
		}
	}
}

// TestEveryRegisteredReasonIsEmitted is G5: a registered reason no door names
// is a documented refusal that cannot happen.
func TestEveryRegisteredReasonIsEmitted(t *testing.T) {
	used := map[string]bool{}
	for _, f := range parseAPISources(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "authz" {
					used[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	for name, value := range reasonConsts(t) {
		if !used[name] {
			t.Errorf("reason %s (authz.%s) is registered but no door in internal/api refuses with it", value, name)
		}
	}
}

// adHocReasonLiterals are the places a registered reason's STRING appears in
// this package's code as some other vocabulary's value. May only shrink.
var adHocReasonLiterals = map[string]string{
	"internal.go:run_not_found":                    "identity.renew's own reason, not an authz.denied row",
	"user_drives_resolve.go:governance_profile":    "a drive's bound_by value on /me",
	"user_drives_resolve.go:groups_snapshot_stale": "a drive's unavailable reason on /me",
	"user_drives_resolve.go:user_type_unknown":     "a drive's unavailable reason on /me",
}

// roleComparisons counts the == / != comparisons against a stamped admin role
// per file: a tier re-derived outside isOperator/isSecurityOperator. The lanes
// with no live request (ticket, SSH, UI gateway, the role preview) are why they
// exist. May only shrink.
var roleComparisons = map[string]int{
	"access.go":            2,
	"attach.go":            1,
	"http.go":              3, // isOperator and isSecurityOperator themselves
	"sshgateway.go":        1,
	"uigateway.go":         1,
	"uigateway_session.go": 1,
}

// TestNoAdHocAuthz is G4: a refusal is registered and emitted through refuse,
// never hand-built. Outside internal/authz no code writes the authz.denied
// action by its literal; in this package no code writes a registered reason as
// a string; and a new comparison against a stamped admin role fails until it
// is either replaced by the tier predicates or listed above.
func TestNoAdHocAuthz(t *testing.T) {
	reasons := map[string]bool{}
	for _, r := range authz.Reasons() {
		reasons[string(r)] = true
	}
	seenLiterals := map[string]bool{}
	gotRoles := map[string]int{}
	for _, f := range parseAPISources(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				v, _ := strconv.Unquote(n.Value)
				if v == authz.AuditAction {
					t.Errorf("%s writes %q by hand; refuse (refusal.go) is the one emitter", f.name, v)
				}
				if reasons[v] {
					key := f.name + ":" + v
					seenLiterals[key] = true
					if _, ok := adHocReasonLiterals[key]; !ok {
						t.Errorf("%s writes the registered reason %q as a string; use authz.Deny with its constant", f.name, v)
					}
				}
			case *ast.BinaryExpr:
				if (n.Op == token.EQL || n.Op == token.NEQ) && (isStampedRole(n.X) || isStampedRole(n.Y)) {
					gotRoles[f.name]++
				}
			}
			return true
		})
	}
	for key := range adHocReasonLiterals {
		if !seenLiterals[key] {
			t.Errorf("adHocReasonLiterals lists %s, which no longer occurs — delete the entry", key)
		}
	}
	names := map[string]bool{}
	for k := range gotRoles {
		names[k] = true
	}
	for k := range roleComparisons {
		names[k] = true
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if gotRoles[name] != roleComparisons[name] {
			t.Errorf("%s compares a stamped admin role %d times, roleComparisons says %d — decide through isOperator / "+
				"isSecurityOperator, or (a lane with no live request) update the list", name, gotRoles[name], roleComparisons[name])
		}
	}

	// The action literal, everywhere else it could be written.
	for _, dir := range []string{"../../cmd", ".."} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.Contains(path, "/authz/") || strings.HasPrefix(path, "../api/") {
				return nil
			}
			b, rerr := os.ReadFile(filepath.Clean(path))
			if rerr == nil && strings.Contains(string(b), `"`+authz.AuditAction+`"`) {
				t.Errorf("%s writes %q by hand; the authz.denied row is internal/api's refuse", path, authz.AuditAction)
			}
			return nil
		})
	}
}

func isStampedRole(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "oidc" && (sel.Sel.Name == "RoleAdmin" || sel.Sel.Name == "RoleSecurityAdmin")
}

// refusalSentence is one door's refusal as written: the reason, the target and
// the sentence, each as its source expression, so a byte of any sentence
// changing shows in the golden's diff.
type refusalSentence struct {
	Reason   string `json:"reason"`
	Target   string `json:"target"`
	Sentence string `json:"sentence"`
}

// TestRefusalSentencesGolden pins every refusal body this package writes
// through authz.Deny and denyMemberCapability to testdata/refusal_sentences.json.
// A sentence the console renders verbatim is a contract; changing one is a
// reviewed golden diff, never a drive-by.
func TestRefusalSentencesGolden(t *testing.T) {
	consts := reasonConsts(t)
	var got []refusalSentence
	for _, f := range parseAPISources(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := types.ExprString(call.Fun); {
			case fn == "authz.Deny" && len(call.Args) == 3:
				reason := types.ExprString(call.Args[0])
				if v, ok := consts[strings.TrimPrefix(reason, "authz.")]; ok {
					reason = v
				}
				sentence := types.ExprString(call.Args[2])
				if sentence == `""` {
					if ref, ok := authz.Lookup(authz.Reason(reason)); ok && ref.Sentence != "" {
						sentence = strconv.Quote(ref.Sentence) + " (registry)"
					} else {
						sentence = "(audit only)"
					}
				}
				got = append(got, refusalSentence{reason, types.ExprString(call.Args[1]), sentence})
			case strings.HasSuffix(fn, ".denyMemberCapability") && len(call.Args) == 6:
				got = append(got, refusalSentence{"capKinds[" + types.ExprString(call.Args[2]) + "].reason",
					types.ExprString(call.Args[4]), types.ExprString(call.Args[5])})
			}
			return true
		})
	}
	slices.SortFunc(got, func(a, b refusalSentence) int {
		return strings.Compare(a.Reason+"\x00"+a.Target+"\x00"+a.Sentence, b.Reason+"\x00"+b.Target+"\x00"+b.Sentence)
	})
	if len(got) < 30 {
		t.Fatalf("found %d refusals — the scanner, not the package, changed", len(got))
	}
	compareOrUpdateGolden(t, filepath.Join("testdata", "refusal_sentences.json"), got)
}

type apiSource struct {
	name string
	file *ast.File
}

// parseAPISources parses this package's non-test sources.
func parseAPISources(t *testing.T) []apiSource {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []apiSource
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, apiSource{name, f})
	}
	return out
}

// reasonConsts maps internal/authz's Reason constant names to their codes.
func reasonConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../authz/registry.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || types.ExprString(vs.Type) != "Reason" {
			return true
		}
		for i, name := range vs.Names {
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
				out[name.Name], _ = strconv.Unquote(lit.Value)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no Reason constants parsed from internal/authz/registry.go")
	}
	return out
}
