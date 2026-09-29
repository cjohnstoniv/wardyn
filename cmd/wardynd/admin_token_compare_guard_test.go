// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// adminTokenMention reports whether a Go token refers to the admin token's
// principal string: the constant, its exported accessor, the reserved-set
// predicate, or the literal itself. It is mention-shaped on purpose — a guard
// that pattern-matched comparison operators missed `strings.Compare`, an
// unqualified `AdminTokenPrincipal()` and a `case "admin-token":` arm, and
// isReservedPrincipal used as a grant predicate instead of a refusal.
func adminTokenMention(tok token.Token, lit string) bool {
	switch tok {
	case token.IDENT:
		return lit == "adminTokenPrincipal" || lit == "AdminTokenPrincipal" || lit == "isReservedPrincipal"
	case token.STRING:
		v, err := strconv.Unquote(lit)
		return err == nil && v == "admin-token"
	}
	return false
}

// adminTokenCompareAllowed is every mention of the admin token's principal
// string in non-test Go, keyed by file and the trimmed line. Each one REFUSES
// or demotes, is paired with a typed actor, or is the reserved set itself;
// none grants on the string alone, because an IdP sub could once spell it
// (#1162). A grant must read what authenticated the request
// (actorFromRequest's ActorSystem, AgentRun.OperatorOwned) instead.
var adminTokenCompareAllowed = map[string]string{
	"internal/api/reserved_principal.go|return p == adminTokenPrincipal || (op != \"\" && p == op) ||":                                                         "the reserved set itself",
	"internal/api/approvals.go|if actorType == types.ActorSystem && principal == adminTokenPrincipal {":                                                        "break-glass, paired with the typed ActorSystem",
	"internal/api/approvals_decidable.go|if actorType == types.ActorSystem && principal == adminTokenPrincipal {":                                              "#1197: mayDecide's read-only mirror of the same break-glass, paired with the typed ActorSystem",
	"internal/api/harnesscred.go|mechanismCaller := s.cfg.OIDC != nil && runIdentitySubject(r.Context(), principalFromRequest(r)) == adminTokenPrincipal":      "refuses the mechanism a per-user sign-in",
	"internal/api/model_provider_credentials.go|if owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal) {":                                     "refuses the mechanism a credential of its own",
	"internal/api/modelaccess.go|return sc.perUser && sc.owner == adminTokenPrincipal":                                                                         "marks the mechanism as holding no per-user credential",
	"internal/api/provider_access.go|return owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal)":                                              "grades the mechanism as holding no credential",
	"internal/api/provider_bedrock.go|case owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):":                                              "refuses the mechanism a Bedrock credential",
	"internal/api/provider_subscription.go|case owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):":                                         "refuses the mechanism a subscription sign-in",
	"internal/api/runs_dispatch_provider.go|return subject != \"\" && (s.cfg.OIDC == nil || subject != adminTokenPrincipal)":                                   "refuses the mechanism a model credential",
	"internal/api/sshkeys.go|if principal == adminTokenPrincipal && s.cfg.OIDC != nil {":                                                                       "refuses registering a key under the mechanism",
	"cmd/wardynd/boot_flags.go|if lm.operator == api.AdminTokenPrincipal() {":                                                                                  "refuses the local seat named after the mechanism",
	"internal/api/delegation.go|if t.Principal == \"\" || s.isReservedPrincipal(t.Principal) {":                                                                "refuses a delegated token whose principal is reserved",
	"internal/api/delegation_exchange.go|sess, denied := s.cfg.OIDC.VerifySubjectToken(r, subjectToken, d.IdPClientID, s.isReservedPrincipal)":                 "hands the refusal predicate to the portal token exchange",
	"internal/api/modelaccess.go|func AdminTokenPrincipal() string { return adminTokenPrincipal }":                                                             "the accessor itself",
	"internal/api/people.go|case s.isReservedPrincipal(req.Principal):":                                                                                        "refuses recording a person under a reserved subject",
	"internal/api/people.go|if errors.Is(err, store.ErrNotFound) || (err == nil && s.isReservedPrincipal(p.Principal)) {":                                      "answers 404 for a person recorded under a reserved subject",
	"internal/api/reserved_principal.go|func (s *Server) isReservedPrincipal(p string) bool {":                                                                 "the reserved set's own declaration",
	"internal/api/reserved_principal.go|if !s.isReservedPrincipal(p) {":                                                                                        "refuseReservedPrincipal answers 401 for a reserved principal",
	"internal/api/routes.go|r.Get(\"/auth/callback\", s.cfg.OIDC.CallbackHandlerWithDenials(s.isReservedPrincipal, s.auditSignInDenied))":                      "hands the refusal predicate to the sign-in callback",
	"internal/api/runs_policy.go|const adminTokenPrincipal = \"admin-token\"":                                                                                  "the constant's own declaration",
	"internal/api/runs_policy.go|return types.ActorSystem, adminTokenPrincipal":                                                                                "records the admin bearer as the typed ActorSystem, never read back as a grant",
	"internal/api/sshgateway.go|if s.cfg.OIDC != nil && s.isReservedPrincipal(key.Principal) {":                                                                "refuses a key stored under a reserved principal",
	"internal/identity/identitytest/conformance.go|ri, err := p.MintRunIdentity(ctx, uuid.New(), \"admin-token\", \"admin-token\", aud, true)":                 "conformance fixture minting an identity for the mechanism, granting nothing",
	"cmd/wardynd/boot_flags.go|adminToken:              flagEnv(\"admin-token\", \"WARDYN_ADMIN_TOKEN\", \"\", \"admin bearer token gating the public API\"),": "the --admin-token flag's name, not a principal",
}

// TestNoNewAdminTokenPrincipalCompare fails on any mention of the admin
// token's principal string not on the allow-list above, and on an allow-list
// entry that no longer exists or whose line now appears twice.
func TestNoNewAdminTokenPrincipalCompare(t *testing.T) {
	root := repoRoot(t)
	seen := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			lines := strings.Split(string(b), "\n")
			f := token.NewFileSet().AddFile(path, -1, len(b))
			var sc scanner.Scanner
			lastLine := 0         // two mentions on one line are one finding
			sc.Init(f, b, nil, 0) // comments are skipped, so prose may still name the string
			for {
				pos, tok, lit := sc.Scan()
				if tok == token.EOF {
					break
				}
				n := f.Line(pos)
				if !adminTokenMention(tok, lit) || n == lastLine {
					continue
				}
				lastLine = n
				text := strings.TrimSpace(lines[n-1])
				key := filepath.ToSlash(rel) + "|" + text
				seen[key]++
				if _, ok := adminTokenCompareAllowed[key]; ok && seen[key] > 1 {
					// Each entry is one reviewed line; an identical second line is a new
					// mention the review never saw (a grant hiding behind a refusal's text).
					t.Errorf("%s:%d repeats an allow-listed line: %s\n"+
						"the allow-list covers each line once; a second copy needs its own review "+
						"and text that differs from the first", rel, n, text)
				} else if !ok {
					t.Errorf("%s:%d mentions the admin token's principal string: %s\n"+
						"a principal string can be spelled by an identity provider; grant on what authenticated the request "+
						"(ActorSystem, AgentRun.OperatorOwned), or add a refusing site to adminTokenCompareAllowed with its reason",
						rel, n, text)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for key := range adminTokenCompareAllowed {
		if seen[key] == 0 {
			t.Errorf("allow-list entry no longer matches any line: %s", key)
		}
	}
}
