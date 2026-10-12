// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"slices"
	"strings"
	"testing"
)

func TestPresetDocumentConvertsTheRequestField(t *testing.T) {
	policy := RunPolicySpec{AllowedDomains: []string{}}
	doc, err := PresetDocument(Preset{
		Name: "ci-egress", Description: "egress only", Version: 3,
		Request: CreateRunRequest{
			Agent: "claude-code", ModeContract: RunModeContractV09, Experience: ExperienceBackground,
			NoRepositoriesOrDrives: true, InlinePolicy: &policy,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if doc.APIVersion != TemplateDocumentVersion || doc.Kind != TemplateDocumentKind || doc.Name != "ci-egress" ||
		doc.Description != "egress only" || doc.Coverage != TemplateCoveragePartial || len(doc.NeedsSetup) != 0 {
		t.Errorf("document header = %+v", doc)
	}
	want := []string{"agent", "experience", "inline_policy", "no_repositories_or_drives"}
	if got := doc.Intent.Names(); !slices.Equal(got, want) {
		t.Errorf("intent fields = %v, want %v (mode_contract is the sender's, repo is empty)", got, want)
	}
	if got := string(doc.Intent.Raw("inline_policy")); got != `{"allowed_domains":[]}` {
		t.Errorf("inline_policy = %s, want only the explicit empty allowlist (first_use_approval and min_confinement_class are written empty by the type, and a template reads a present key as a choice)", got)
	}
	if _, err := doc.Intent.Request(); err != nil {
		t.Errorf("the intent does not read back as a run request: %v", err)
	}
}

func TestPresetDocumentRefusesUserTypesUnlessEveryone(t *testing.T) {
	p := Preset{Name: "analysts-only", UserTypes: []string{"analyst", "ops"}, Request: CreateRunRequest{Agent: "claude-code"}}
	_, err := PresetDocument(p, false)
	if err == nil || !strings.Contains(err.Error(), "analyst, ops") || !strings.Contains(err.Error(), "everyone") {
		t.Fatalf("a limited preset converted without everyone: %v", err)
	}
	doc, err := PresetDocument(p, true)
	if err != nil || doc.Name != "analysts-only" {
		t.Errorf("with everyone: %+v %v", doc, err)
	}
}

func TestPresetDocumentDropsAnInlinePolicyThatSaysNothing(t *testing.T) {
	doc, err := PresetDocument(Preset{Name: "bare", Request: CreateRunRequest{Agent: "claude-code", InlinePolicy: &RunPolicySpec{}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Intent.Has("inline_policy") || !slices.Equal(doc.Intent.Names(), []string{"agent"}) {
		t.Errorf("intent = %v: an overlay of nothing but zero values is not a choice", doc.Intent.Names())
	}
}
