// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Widening is one policy field that keeps Leq from holding: the field's JSON name and, for a list
// field, the entries that are not within the other side.
type Widening struct {
	Field   string   `json:"field"`
	Entries []string `json:"entries,omitempty"`
}

// LeqWidenings names the fields of policy a that Leq cannot prove at most as permissive as policy
// b's. It explains Leq and never decides: each field is judged by Leq itself, on two policies that
// hold that field alone, so no second rule here can drift from Leq's. A caller takes the verdict
// from Leq and reads this list only to say why the verdict was false. The list is empty when no
// single field accounts for it.
func LeqWidenings(a, b types.RunPolicySpec) []Widening {
	var out []Widening
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		var x, y types.RunPolicySpec
		reflect.ValueOf(&x).Elem().Field(i).Set(va.Field(i))
		reflect.ValueOf(&y).Elem().Field(i).Set(vb.Field(i))
		if Leq(x, y, types.GovernanceLimits{}, types.GovernanceLimits{}) {
			continue
		}
		name, _, _ := strings.Cut(va.Type().Field(i).Tag.Get("json"), ",")
		out = append(out, Widening{Field: name, Entries: wideningEntries(name, x, y)})
	}
	return out
}

// wideningEntries lists the entries that are not within the other side, for the list fields a
// reader fixes entry by entry. Every other field is named without entries.
func wideningEntries(field string, a, b types.RunPolicySpec) []string {
	var out []string
	switch field {
	case "allowed_domains":
		for _, s := range a.AllowedDomains {
			if !leqAllowedDomains([]string{s}, b.AllowedDomains) {
				out = append(out, s)
			}
		}
	case "denied_domains":
		for _, s := range b.DeniedDomains {
			if !leqDeniedDomains(a.DeniedDomains, []string{s}) {
				out = append(out, s)
			}
		}
	case "allowed_methods":
		base := normMethods(b.AllowedMethods)
		for _, m := range normMethods(a.AllowedMethods) {
			if !slices.Contains(base, m) {
				out = append(out, m)
			}
		}
	case "eligible_grants":
		for _, g := range a.EligibleGrants {
			if kind := string(g.Kind); GrantWithin(g, b.EligibleGrants) != nil && !slices.Contains(out, kind) {
				out = append(out, kind)
			}
		}
	}
	return out
}
