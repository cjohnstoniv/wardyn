// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const routeChannels = `"channels":[
	{"id":"a","type":"webhook","url":"http://h.example.com/a"},
	{"id":"b","type":"webhook","url":"http://h.example.com/b"},
	{"id":"red","type":"webhook","url":"http://h.example.com/r","redact_requester":true}]`

func parseRoutes(t *testing.T, routes string) *Config {
	t.Helper()
	c, err := Parse(`{` + routeChannels + `,"routes":` + routes + `}`)
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	return c
}

type planned struct {
	tier int16
	ch   string
	secs int
}

func planFor(t *testing.T, c *Config, kind types.ApprovalKind, profile *uuid.UUID) []planned {
	t.Helper()
	SetActive(c, nil)
	t.Cleanup(func() { SetActive(nil, nil) })
	var out []planned
	for _, p := range Plan(kind, profile) {
		out = append(out, planned{p.Tier, p.Channel, int(p.After.Seconds())})
	}
	return out
}

func eq(a, b []planned) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlan_RoutesAbsentPlansEveryChannelAtTierZero(t *testing.T) {
	c, err := Parse(`{` + routeChannels + `}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []planned{{0, "a", 0}, {0, "b", 0}, {0, "red", 0}}
	if got := planFor(t, c, types.ApprovalEgressDomain, nil); !eq(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

func TestPlan_RouteMatchingByKindProfileFirstMatchAndNoMatch(t *testing.T) {
	prof := uuid.New()
	other := uuid.New()
	c := parseRoutes(t, `[
		{"kinds":["push_content"],"tiers":[{"after":"0s","channels":["a"]},{"after":"30m","channels":["a","b"]}]},
		{"profiles":["`+strings.ToUpper(prof.String())+`"],"tiers":[{"after":"5s","channels":["b"]}]},
		{"kinds":["egress_domain"],"profiles":["`+prof.String()+`"],"tiers":[{"after":"0s","channels":["red"]}]}]`)

	// by kind (no profile needed)
	if got, want := planFor(t, c, types.ApprovalPushContent, nil), []planned{{0, "a", 0}, {1, "a", 1800}, {1, "b", 1800}}; !eq(got, want) {
		t.Errorf("by kind: %v, want %v", got, want)
	}
	// by profile; a different kind skips route 1
	if got, want := planFor(t, c, types.ApprovalToolCall, &prof), []planned{{0, "b", 5}}; !eq(got, want) {
		t.Errorf("by profile: %v, want %v", got, want)
	}
	// first match wins: route 2 also matches egress_domain+prof, route 3 never fires
	if got, want := planFor(t, c, types.ApprovalEgressDomain, &prof), []planned{{0, "b", 5}}; !eq(got, want) {
		t.Errorf("first match: %v, want %v", got, want)
	}
	// a profile key never matches a run with no profile or another profile; no match plans nothing
	if got := planFor(t, c, types.ApprovalToolCall, nil); len(got) != 0 {
		t.Errorf("no profile: %v, want none", got)
	}
	if got := planFor(t, c, types.ApprovalToolCall, &other); len(got) != 0 {
		t.Errorf("other profile: %v, want none", got)
	}
}

func tierJSON(after, chans, extra string) string {
	return `{"after":"` + after + `","channels":[` + chans + `]` + extra + `}`
}

func TestParse_RouteRefusals(t *testing.T) {
	six := []string{
		tierJSON("0s", `"a"`, ""), tierJSON("1s", `"a"`, ""), tierJSON("2s", `"a"`, ""),
		tierJSON("3s", `"a"`, ""), tierJSON("4s", `"a"`, ""), tierJSON("5s", `"a"`, ""),
	}
	cases := []struct {
		name, routes, want string
	}{
		{"unknown channel", `[{"tiers":[` + tierJSON("0s", `"nope"`, "") + `]}]`, "names no configured channel"},
		{"unknown kind", `[{"kinds":["teleport"],"tiers":[` + tierJSON("0s", `"a"`, "") + `]}]`, "unknown approval kind"},
		{"profile by name", `[{"profiles":["payments"],"tiers":[` + tierJSON("0s", `"a"`, "") + `]}]`, "ids (uuids), not names"},
		{"equal after", `[{"tiers":[` + tierJSON("5m", `"a"`, "") + `,` + tierJSON("5m", `"a"`, "") + `]}]`, "strictly greater"},
		{"descending after", `[{"tiers":[` + tierJSON("10m", `"a"`, "") + `,` + tierJSON("5m", `"a"`, "") + `]}]`, "strictly greater"},
		{"negative after", `[{"tiers":[` + tierJSON("-1s", `"a"`, "") + `]}]`, "zero or more"},
		{"six tiers", `[{"tiers":[` + strings.Join(six, ",") + `]}]`, "1 to 5 tiers"},
		{"no tiers", `[{"tiers":[]}]`, "1 to 5 tiers"},
		{"run_owner on a redacting channel", `[{"tiers":[` + tierJSON("0s", `"a","red"`, `,"notify":["run_owner"]`) + `]}]`, `"red"`},
		{"unknown notify target", `[{"tiers":[` + tierJSON("0s", `"a"`, `,"notify":["boss"]`) + `]}]`, "notify must be"},
		{"duplicate channel in a tier", `[{"tiers":[` + tierJSON("0s", `"a","a"`, "") + `]}]`, "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(`{` + routeChannels + `,"routes":` + c.routes + `}`)
			if err == nil {
				t.Fatal("config accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q lacks %q", err, c.want)
			}
			for _, leak := range []string{"teleport", "payments", "nope", "h.example.com"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks %q", err, leak)
				}
			}
		})
	}
	// profile_contact is allowed through a redacting channel; five tiers are fine
	parseRoutes(t, `[{"tiers":[`+tierJSON("0s", `"red"`, `,"notify":["profile_contact"]`)+`]}]`)
	parseRoutes(t, `[{"tiers":[`+strings.Join(six[:5], ",")+`]}]`)
}

func TestExpiryWarnings(t *testing.T) {
	c := parseRoutes(t, `[{"tiers":[{"after":"0s","channels":["a"]},{"after":"24h","channels":["a"]},{"after":"30h","channels":["a"]}]}]`)
	got := c.ExpiryWarnings(24 * time.Hour)
	if len(got) != 2 || !strings.Contains(got[0], "tier #2") || !strings.Contains(got[1], "tier #3") {
		t.Fatalf("warnings = %v, want tiers 2 and 3", got)
	}
	if w := c.ExpiryWarnings(48 * time.Hour); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
}
