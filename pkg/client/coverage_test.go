// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// coverage_test.go pins the SDK's DOCUMENTED route-family coverage (the package
// doc's "# Coverage" list) to real methods, so the doc can never again overclaim
// what the SDK exposes (the old doc said it "mirrors the REST API surface
// exactly" while covering ~5 of ~15 families). Each family below names one
// representative method; removing or renaming it fails this test, forcing the
// doc and the method set to stay in lockstep.

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Compile-time surface guard: every type an exported method returns or accepts
// must be nameable through client.*, or the SDK is documented-but-unusable from
// outside the module (Go forbids importing internal/types). This file is
// package client_test, so each name below must resolve from OUTSIDE — dropping
// an alias in types.go fails the BUILD here. Reflect cannot replace this: `=`
// aliases are type-identical, so PkgPath reports internal/types either way.
var (
	_ []client.Workspace
	_ client.WorkspaceKind
	_ client.WorkspaceLLMCred
	_ client.WorkspaceBedrockRef
	_ client.SiteConfig
	_ map[string]client.ArtifactOverride
)

func TestClientCoversRouteFamilies(t *testing.T) {
	// family -> a representative method the package doc claims. Keep in sync with
	// the "# Coverage" block in client.go's package doc.
	families := map[string]string{
		"runs":        "CreateRun",
		"runs.list":   "ListRuns",
		"approvals":   "ListApprovals",
		"policies":    "ListPolicies",
		"workspaces":  "ListWorkspaces",
		"audit":       "AuditEvents",
		"secrets":     "ListSecrets",
		"site-config": "GetSiteConfig",
		"setup":       "SetupStatus",
		"identity":    "Me",
		"health":      "Healthz",
	}
	ct := reflect.TypeOf(&client.Client{})
	for family, method := range families {
		if _, ok := ct.MethodByName(method); !ok {
			t.Errorf("route family %q: *client.Client has no method %s — the package "+
				"doc claims this family is covered; add the method or drop the claim so "+
				"the doc stays honest", family, method)
		}
	}
}
