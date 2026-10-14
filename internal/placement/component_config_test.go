// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestOrgComponentConfigNeverReachesALaptopCopy(t *testing.T) {
	env := map[string]string{
		"HTTP_PROXY":       "http://proxy:3128",
		"CORP_API_TOKEN":   "s3cr3t-in-an-org-config",
		"CORP_REGION":      "eu",
		"MY_OWN_COMPONENT": "own value",
	}
	org := []types.ComponentDefinition{
		{Config: map[string]string{"CORP_API_TOKEN": "s3cr3t-in-an-org-config", "CORP_REGION": "eu"}},
		{Config: map[string]string{"CORP_REGION": "eu"}},
	}
	keys := OrgComponentConfigKeys(org)
	if !slices.Equal(keys, []string{"CORP_API_TOKEN", "CORP_REGION"}) {
		t.Fatalf("org config keys = %v", keys)
	}
	kept, dropped := StripOrgComponentConfig(env, keys)
	if _, ok := kept["CORP_API_TOKEN"]; ok {
		t.Fatal("an org component's config variable reached the laptop-bound env")
	}
	if kept["HTTP_PROXY"] == "" || kept["MY_OWN_COMPONENT"] != "own value" {
		t.Fatalf("platform and the person's own variables must stay: %v", kept)
	}
	if !slices.Equal(dropped, []string{"CORP_API_TOKEN", "CORP_REGION"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	if env["CORP_API_TOKEN"] == "" {
		t.Fatal("the org-side env was modified")
	}
}

func TestStripOrgComponentConfigWithNothingToStrip(t *testing.T) {
	kept, dropped := StripOrgComponentConfig(nil, []string{"X"})
	if len(kept) != 0 || len(dropped) != 0 {
		t.Fatalf("kept=%v dropped=%v", kept, dropped)
	}
}

// The table says it too: Env is no longer only exempt, an org component's
// config is an operator-class strip. Reverting to a single exempt row fails here.
func TestEnvClassificationCoversOrgComponentConfig(t *testing.T) {
	var stripped bool
	for _, e := range Entries(StructSandboxSpec, "Env") {
		if e.Class == ClassOperator && e.Rule == RuleStrip {
			stripped = true
		}
	}
	if !stripped {
		t.Fatal("SandboxSpec.Env has no operator-class strip row for an org component's config")
	}
}
