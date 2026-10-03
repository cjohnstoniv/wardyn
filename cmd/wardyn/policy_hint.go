// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// policyHint is the one remedy line printed after a ceiling refusal, naming the
// policy that bound the caller and how to ask for a change, or "" when err
// carries no policy. The route is Ref.RequestRoute's, never built here. The
// server validated every field, but a terminal is no place to trust that, so the
// line is stripped of control and invisible characters before it is printed.
func policyHint(err error) string {
	var ae *sdk.APIError
	if !errors.As(err, &ae) || ae.Policy == nil {
		return ""
	}
	p := ae.Policy
	by := "this deployment's policy"
	if p.Source == policyref.SourceProfile && p.Name != "" {
		by = `profile "` + p.Name + `"`
	}
	if p.Owner != "" {
		by += " (owner: " + p.Owner + ")"
	}
	line := "governed by " + by
	if ask := strings.TrimSpace(p.RequestText + " " + p.RequestRoute()); ask != "" {
		line += ", to request a change: " + ask
	}
	return strings.Map(func(r rune) rune {
		if policyref.UnsafeRune(r) {
			return -1
		}
		return r
	}, line)
}
