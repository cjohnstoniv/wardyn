// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestAIGatewayDocClaimTable holds the claim table in docs/operations/ai-gateway.md equal to the
// claims the signer sets: a claim added to Claims without a row, or a row naming a claim the signer
// never sets, fails here.
func TestAIGatewayDocClaimTable(t *testing.T) {
	b, err := os.ReadFile("../../docs/operations/ai-gateway.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	_, section, ok := strings.Cut(doc, "\n## The claims a gateway receives\n")
	if !ok {
		t.Fatal("docs/operations/ai-gateway.md has no \"The claims a gateway receives\" section")
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}

	var documented []string
	row := regexp.MustCompile("^\\| `([a-z_]+)` \\|")
	for _, line := range strings.Split(section, "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			documented = append(documented, m[1])
		}
	}

	var signed []string
	ct := reflect.TypeOf(Claims{})
	for i := 0; i < ct.NumField(); i++ {
		name, _, _ := strings.Cut(ct.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("Claims.%s has no json name, so the claim table cannot name it", ct.Field(i).Name)
		}
		signed = append(signed, name)
	}

	slices.Sort(documented)
	slices.Sort(signed)
	if !slices.Equal(documented, signed) {
		t.Errorf("the claim table in docs/operations/ai-gateway.md names %v, but Claims sets %v", documented, signed)
	}
	if len(slices.Compact(slices.Clone(documented))) != len(documented) {
		t.Errorf("the claim table names a claim twice: %v", documented)
	}
	for _, want := range []string{"`" + tokenFormat + "`", "Compatibility"} {
		if !strings.Contains(strings.ToLower(section), strings.ToLower(want)) {
			t.Errorf("the claim section does not say %q", want)
		}
	}
}
