// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// oidcMethodsCalled is every method this package calls on cfg.OIDC, with whether it reads the identity
// provider's state, so a pending authenticator (one whose provider has not answered yet) acts differently in it.
// The other methods read nothing from the provider and answer the same pending as discovered. A method that is
// not here fails the test until it is added and tagged; a method tagged true names the test that pins its
// pending behaviour. internal/auth/oidc's TestAuthenticatorMethodCensus is the package-wide view.
var oidcMethodsCalled = map[string]struct {
	needsProvider bool
	pinnedBy      string
}{
	"AttachLogoutQueueSink":      {},
	"AttachPersonKeying":         {},
	"BackchannelLogoutEnabled":   {},
	"BackchannelLogoutHandler":   {true, "TestBackchannelPending_AuditRows"},
	"CallbackHandlerWithDenials": {true, "TestPending_CallbackRefusedCookiesKept (internal/auth/oidc)"},
	"ChartRoleMap":               {},
	"ClientID":                   {},
	"DefaultRole":                {},
	"DefaultRoleIsAdmin":         {},
	"DropUserView":               {},
	"EnforcesEmailVerified":      {},
	"FrontchannelFrameOrigin":    {},
	"FrontchannelLogoutEnabled":  {},
	"FrontchannelLogoutHandler":  {},
	"HasOperatorEmails":          {},
	"Issuer":                     {},
	"KeysPeopleByObjectID":       {},
	"LoginHandler":               {true, "TestPending_LoginRefusedNoCookies (internal/auth/oidc)"},
	"LogoutQueueStats":           {},
	"MergedMapEmpty":             {},
	"Middleware":                 {},
	"NoteLogoutRateLimited":      {},
	"Pending":                    {},
	"PreviewRole":                {},
	"PreviewRoleAgainst":         {},
	"PurposeMAC":                 {},
	"RecordAttach":               {},
	"SessionBounds":              {},
	"SetUserView":                {},
	"SignOut":                    {},
	"UserViewSuperAdmin":         {},
	"VerifyActorToken":           {true, "TestPending_SubjectAndActorTokensInvalid (internal/auth/oidc)"},
	"VerifySubjectToken":         {true, "TestPending_SubjectAndActorTokensInvalid (internal/auth/oidc)"},
}

// A5: the methods internal/api calls on cfg.OIDC equal the pinned table, and exactly the five that reach the
// provider's state are tagged.
func TestOIDCMethodCensus(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	called := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "OIDC" {
				if cfg, ok := inner.X.(*ast.SelectorExpr); ok && cfg.Sel.Name == "cfg" {
					called[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	var got, want []string
	for m := range called {
		got = append(got, m)
	}
	needs := 0
	for m, tag := range oidcMethodsCalled {
		want = append(want, m)
		if tag.needsProvider {
			needs++
			if tag.pinnedBy == "" {
				t.Errorf("%s reads provider state but names no pending test", m)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the methods internal/api calls on cfg.OIDC changed: tag each new one needsProvider or not, and pin the ones that are\n got  %v\n want %v", got, want)
	}
	if needs != 5 || len(want) != 33 {
		t.Errorf("table has %d methods and %d tagged needsProvider, want 33 and 5", len(want), needs)
	}
}
