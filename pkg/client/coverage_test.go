// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// coverage_test.go pins the SDK's DOCUMENTED route-family coverage (the package
// doc's "# Coverage" list) to real methods, so the doc can never again overclaim
// what the SDK exposes (the old doc said it "mirrors the REST API surface
// exactly" while covering ~5 of ~15 families). Each family below lists every
// method the package doc claims for it; removing, renaming, or adding one
// fails this test, forcing the doc and the method set to stay in lockstep in
// BOTH directions — doc->method (every listed method must exist) and
// method->doc (every exported method must be listed by exactly one family),
// the second of which is what would have caught the sources family (five
// shipped methods) shipping with no Coverage entry at all.

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
	_ client.WorkspaceStatus
	_ client.WorkspaceAttachment
	_ client.WorkspaceLLMCred
	_ client.WorkspaceBedrockRef
	_ client.BaseImageEntry
	_ client.SiteConfig
	_ map[string]client.ArtifactOverride
	_ []client.EgressRedirect
	_ client.ApprovalScope
	_ client.DecisionOpts
)

// routeFamilies lists EVERY exported *client.Client method under the family
// the package doc's "# Coverage" block claims it under — the full partition,
// not one representative each, so the two checks below can assert real
// parity in both directions.
func routeFamilies() map[string][]string {
	return map[string][]string{
		"runs":        {"CreateRun", "Preflight", "GetRun", "ListGrants", "KillRun", "SynthesizeProfile", "GetRecording"},
		"runs.list":   {"ListRuns"},
		"approvals":   {"ListApprovals", "Approve", "Deny"},
		"policies":    {"CreatePolicy", "GetPolicy", "GetDefaultPolicy", "ListPolicies", "UpdatePolicy", "DeletePolicy"},
		"workspaces":  {"CreateWorkspace", "GetWorkspace", "ListWorkspaces", "UpdateWorkspace", "DeleteWorkspace", "ScanWorkspace", "RecordWorkspaceTask"},
		"sources":     {"ListSources", "CreateSource", "GetSource", "ScanSource", "DeleteSource"},
		"audit":       {"AuditEvents", "AuditEventsPage", "RecentAuditEvents"},
		"secrets":     {"ListSecrets", "SetSecret", "DeleteSecret"},
		"site-config": {"GetSiteConfig", "PutSiteConfig"},
		"setup":       {"SetupStatus", "ConnectManagedSubscription", "DisconnectManagedSubscription"},
		"identity":    {"Me"},
		"health":      {"Healthz"},
		"sessions":    {"RevokeSessions"},
	}
}

// TestClientCoversRouteFamilies is the doc->method direction: every method the
// package doc claims for a family must actually exist on *client.Client.
func TestClientCoversRouteFamilies(t *testing.T) {
	ct := reflect.TypeOf(&client.Client{})
	for family, methods := range routeFamilies() {
		for _, method := range methods {
			if _, ok := ct.MethodByName(method); !ok {
				t.Errorf("route family %q: *client.Client has no method %s — the package "+
					"doc claims this family is covered; add the method or drop the claim so "+
					"the doc stays honest", family, method)
			}
		}
	}
}

// TestRouteFamiliesCoverEveryMethod is the reverse, method->doc direction:
// every EXPORTED *client.Client method must be claimed by exactly one family
// above — an addition (like the sources family) that lands with no Coverage
// entry fails here instead of silently underclaiming the doc.
func TestRouteFamiliesCoverEveryMethod(t *testing.T) {
	ct := reflect.TypeOf(&client.Client{})
	seenIn := map[string]string{} // method -> the family that already claimed it
	for family, methods := range routeFamilies() {
		for _, method := range methods {
			if other, dup := seenIn[method]; dup {
				t.Errorf("method %s is listed under both %q and %q — a family list must partition the methods, not overlap", method, other, family)
				continue
			}
			seenIn[method] = family
		}
	}
	for i := 0; i < ct.NumMethod(); i++ {
		name := ct.Method(i).Name
		if _, ok := seenIn[name]; !ok {
			t.Errorf("*client.Client.%s is exported but claimed by no route family — add it to the "+
				"appropriate family in routeFamilies() and to the package doc's \"# Coverage\" block", name)
		}
	}
}
