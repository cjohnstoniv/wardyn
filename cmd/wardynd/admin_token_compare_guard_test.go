// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// adminTokenCompare matches a comparison against the admin token's principal
// string, in either order, as a switch case, or folded.
var adminTokenCompare = regexp.MustCompile(`[!=]=\s*(adminTokenPrincipal|api\.AdminTokenPrincipal\(\)|"admin-token")|(adminTokenPrincipal|AdminTokenPrincipal\(\)|"admin-token")\s*[!=]=|case\s+adminTokenPrincipal|EqualFold\([^)]*adminTokenPrincipal`)

// adminTokenCompareAllowed is every comparison against the admin token's
// principal string in non-test Go, keyed by file and the trimmed line. Each
// one REFUSES or demotes, is paired with a typed actor, or is the reserved
// set itself; none grants on the string alone, because an IdP sub could once
// spell it (#1162). A comparison that grants must read what authenticated the
// request (actorFromRequest's ActorSystem, AgentRun.OperatorOwned) instead.
var adminTokenCompareAllowed = map[string]string{
	"internal/api/reserved_principal.go|return p == adminTokenPrincipal || (op != \"\" && p == op) ||":                                                    "the reserved set itself",
	"internal/api/approvals.go|if actorType == types.ActorSystem && principal == adminTokenPrincipal {":                                                   "break-glass, paired with the typed ActorSystem",
	"internal/api/approvals_decidable.go|if actorType == types.ActorSystem && principal == adminTokenPrincipal {":                                        "#1197: mayDecide's read-only mirror of the same break-glass, paired with the typed ActorSystem",
	"internal/api/model_provider_credentials.go|if owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal) {":                                "refuses the mechanism a credential of its own",
	"internal/api/provider_access.go|return owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal)":                                         "grades the mechanism as holding no credential",
	"internal/api/provider_bedrock.go|case owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):":                                         "refuses the mechanism a Bedrock credential",
	"internal/api/provider_subscription.go|case owner == \"\" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):":                                    "refuses the mechanism a subscription sign-in",
	"internal/api/runs_dispatch_provider.go|return subject != \"\" && (s.cfg.OIDC == nil || subject != adminTokenPrincipal)":                              "refuses the mechanism a model credential",
	"internal/api/sshkeys.go|if principal == adminTokenPrincipal && s.cfg.OIDC != nil {":                                                                  "refuses registering a key under the mechanism",
	"cmd/wardynd/boot_flags.go|if lm.operator == api.AdminTokenPrincipal() {":                                                                             "refuses the local seat named after the mechanism",
}

// TestNoNewAdminTokenPrincipalCompare fails on any comparison against the
// admin token's principal string not on the allow-list above, and on an
// allow-list entry that no longer exists.
func TestNoNewAdminTokenPrincipalCompare(t *testing.T) {
	root := repoRoot(t)
	seen := map[string]bool{}
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
			for i, line := range strings.Split(string(b), "\n") {
				if !adminTokenCompare.MatchString(line) {
					continue
				}
				key := filepath.ToSlash(rel) + "|" + strings.TrimSpace(line)
				if _, ok := adminTokenCompareAllowed[key]; !ok {
					t.Errorf("%s:%d compares against the admin token's principal string: %s\n"+
						"a principal string can be spelled by an identity provider; grant on what authenticated the request "+
						"(ActorSystem, AgentRun.OperatorOwned), or add a refusing site to adminTokenCompareAllowed with its reason",
						rel, i+1, strings.TrimSpace(line))
				}
				seen[key] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for key := range adminTokenCompareAllowed {
		if !seen[key] {
			t.Errorf("allow-list entry no longer matches any line: %s", key)
		}
	}
}
