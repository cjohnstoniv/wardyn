// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// wantEnvAliases is UT-5's six renames written out literally
// (user-types-design.md rev 4 §6), so a dropped pair or a misspelled name in
// deprecatedEnvAliases fails here instead of passing a loop over itself.
var wantEnvAliases = [][2]string{
	{"WARDYN_USER_DESKTOP", "WARDYN_MEMBER_MODE"},
	{"WARDYN_USER_WORKSPACE_ROOTS", "WARDYN_MEMBER_WORKSPACE_ROOTS"},
	{"WARDYN_USER_WORKSPACE_ROOTS_MAP", "WARDYN_MEMBER_WORKSPACE_ROOTS_MAP"},
	{"WARDYN_USER_WRITABLE_ROOTS", "WARDYN_MEMBER_WRITABLE_ROOTS"},
	{"WARDYN_USER_WRITABLE_DENY", "WARDYN_MEMBER_WRITABLE_DENY"},
	{"WARDYN_ALLOW_USER_ENV_SECRET", "WARDYN_ALLOW_MEMBER_ENV_SECRET"},
}

func TestDeprecatedEnvAliases_ExactPairs(t *testing.T) {
	if len(deprecatedEnvAliases) != len(wantEnvAliases) {
		t.Fatalf("deprecatedEnvAliases has %d pairs, want %d: %v", len(deprecatedEnvAliases), len(wantEnvAliases), deprecatedEnvAliases)
	}
	for i, want := range wantEnvAliases {
		if deprecatedEnvAliases[i] != want {
			t.Errorf("deprecatedEnvAliases[%d] = %v, want %v", i, deprecatedEnvAliases[i], want)
		}
	}
}

// captureSlog routes the default logger into a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestParseBootFlags_HonoursDeprecatedNames drives the real parseBootFlags with
// only the deprecated spellings set and checks each lands in the field its new
// name feeds — the security-relevant deny list and env-secret opt-in included —
// and that each one WARNs naming both names and 0.9.
func TestParseBootFlags_HonoursDeprecatedNames(t *testing.T) {
	resetFlags(t)
	for _, p := range wantEnvAliases {
		t.Setenv(p[0], "")
	}
	t.Setenv("WARDYN_MEMBER_MODE", "true")
	t.Setenv("WARDYN_MEMBER_WORKSPACE_ROOTS", "/srv/src")
	t.Setenv("WARDYN_MEMBER_WORKSPACE_ROOTS_MAP", `{"a@example.com":["/srv/a"]}`)
	t.Setenv("WARDYN_MEMBER_WRITABLE_ROOTS", "/srv/src/rw")
	t.Setenv("WARDYN_MEMBER_WRITABLE_DENY", "/srv/src/rw/locked")
	t.Setenv("WARDYN_ALLOW_MEMBER_ENV_SECRET", "true")
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	logs := captureSlog(t)

	f := parseBootFlags()

	if !*f.memberMode {
		t.Error("WARDYN_MEMBER_MODE=true did not turn member mode on")
	}
	for name, got := range map[string]string{
		"memberRoots (WARDYN_MEMBER_WORKSPACE_ROOTS)":                         *f.memberRoots,
		"memberRootsMap (WARDYN_MEMBER_WORKSPACE_ROOTS_MAP)":                  *f.memberRootsMap,
		"memberWritableRoots (WARDYN_MEMBER_WRITABLE_ROOTS)":                  *f.memberWritableRoots,
		"memberWritableDeny (WARDYN_MEMBER_WRITABLE_DENY)":                    *f.memberWritableDeny,
		"WARDYN_ALLOW_USER_ENV_SECRET (api's reader, pinned in internal/api)": os.Getenv("WARDYN_ALLOW_USER_ENV_SECRET"),
	} {
		if got == "" {
			t.Errorf("%s is empty: the deprecated name's value never reached it", name)
		}
	}
	if *f.memberWritableDeny != "/srv/src/rw/locked" {
		t.Errorf("memberWritableDeny = %q, want the deprecated WARDYN_MEMBER_WRITABLE_DENY value", *f.memberWritableDeny)
	}
	out := logs.String()
	for _, p := range wantEnvAliases {
		// Suffix match: WARDYN_MEMBER_WORKSPACE_ROOTS is a prefix of its _MAP
		// sibling, so a substring match would accept the sibling's line.
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasSuffix(l, "old_env="+p[1]+" new_env="+p[0]) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no boot WARN for deprecated %s; log:\n%s", p[1], out)
			continue
		}
		for _, want := range []string{"level=WARN", p[1], p[0], "removed in 0.9"} {
			if !strings.Contains(line, want) {
				t.Errorf("boot WARN for %s lacks %q: %s", p[1], want, line)
			}
		}
	}
}

// TestDeprecatedEnvAliases_NewNameWinsAndNamesTheIgnoredOne pins both spellings
// set to different values: the new one is kept, and the WARN names the old one
// as ignored, so a leftover (possibly longer) deny list is never dropped
// silently.
func TestDeprecatedEnvAliases_NewNameWinsAndNamesTheIgnoredOne(t *testing.T) {
	for _, p := range wantEnvAliases {
		t.Setenv(p[0], "")
		t.Setenv(p[1], "")
	}
	t.Setenv("WARDYN_USER_WRITABLE_DENY", "/srv/a")
	t.Setenv("WARDYN_MEMBER_WRITABLE_DENY", "/srv/a,/srv/b")
	logs := captureSlog(t)

	resolveDeprecatedEnvAliases()

	if got := os.Getenv("WARDYN_USER_WRITABLE_DENY"); got != "/srv/a" {
		t.Errorf("WARDYN_USER_WRITABLE_DENY = %q, want the new name's value kept", got)
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "ignoring WARDYN_MEMBER_WRITABLE_DENY", "removed in 0.9"} {
		if !strings.Contains(out, want) {
			t.Errorf("both-set WARN lacks %q; log:\n%s", want, out)
		}
	}
}

// TestDeprecatedEnvAliases_NoWarnWhenOnlyNewNamesSet pins the quiet path: a
// compose file forwards every old name as "", which must not WARN.
func TestDeprecatedEnvAliases_NoWarnWhenOnlyNewNamesSet(t *testing.T) {
	for _, p := range wantEnvAliases {
		t.Setenv(p[0], "x")
		t.Setenv(p[1], "")
	}
	logs := captureSlog(t)
	resolveDeprecatedEnvAliases()
	if out := logs.String(); out != "" {
		t.Errorf("WARNed with only the new names set: %s", out)
	}
}
