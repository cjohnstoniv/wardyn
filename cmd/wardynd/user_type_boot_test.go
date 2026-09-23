// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type bootUserTypes struct {
	list []types.UserType
	err  error
}

func (b bootUserTypes) ListUserTypes(context.Context) ([]types.UserType, error) { return b.list, b.err }

// TestWarnUnknownUserTypes: a role-map value or default role naming a type the
// store lacks is one boot WARN per type, naming every entry that names it;
// a type that exists, the built-in one and the tiers are silent.
func TestWarnUnknownUserTypes(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	roleMap := map[string]string{
		"wardyn.admin": oidc.RoleAdmin, "eng": oidc.RoleUser, "std": types.UserTypeStandard,
		"pm-group": "portfolio-manager", "ghost-a": "contractor", "ghost-b": "contractor",
	}
	src := bootUserTypes{list: []types.UserType{{ID: types.UserTypeStandard, BuiltIn: true}, {ID: "portfolio-manager"}}}
	warnUnknownUserTypes(context.Background(), src, roleMap, "contractor")
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, `the user type \"contractor\" doesn't exist yet`) {
		t.Fatalf("want one WARN for contractor, got:\n%s", out)
	}
	for _, ref := range []string{`ghost-a=contractor`, `ghost-b=contractor`, "WARDYN_OIDC_DEFAULT_ROLE", "admin sign-ins get the standard type"} {
		if !strings.Contains(out, ref) {
			t.Errorf("the WARN does not name %s:\n%s", ref, out)
		}
	}

	buf.Reset()
	warnUnknownUserTypes(context.Background(), src, map[string]string{"pm-group": "portfolio-manager"}, oidc.RoleUser)
	if buf.Len() != 0 {
		t.Errorf("an existing type warned:\n%s", buf.String())
	}

	buf.Reset()
	warnUnknownUserTypes(context.Background(), bootUserTypes{err: errors.New("pg down")}, roleMap, "")
	if !strings.Contains(buf.String(), "sign-ins that reach a missing one are refused") {
		t.Errorf("a failed read did not say so:\n%s", buf.String())
	}
}
