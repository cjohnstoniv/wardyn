// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func uiFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRefusalSentencesMatchGolden(t *testing.T) {
	sentences := map[string]func(a []string) string{
		"INVALID":                      func([]string) string { return InvalidMsg() },
		"REQUIRED":                     func(a []string) string { return RequiredMsg(a[0]) },
		"REQUIRED_ANY":                 func([]string) string { return RequiredMsg("") },
		"NOT_FOUND":                    func([]string) string { return NotFoundMsg() },
		"UNAVAILABLE":                  func(a []string) string { return UnavailableMsg(a[0]) },
		"DEFAULT_UNAVAILABLE_PERSONAL": func([]string) string { return DefaultUnavailablePersonalMsg() },
		"DEFAULT_UNAVAILABLE_ORG":      func([]string) string { return DefaultUnavailableOrgMsg() },
		"STALE":                        func(a []string) string { return StaleMsg(a[0]) },
		"NO_ELIGIBLE_MEMBER":           func(a []string) string { return NoEligibleMemberMsg(a[0]) },
		"NO_OWN_RUNNER":                func(a []string) string { return NoOwnRunnerMsg(a[0]) },
		"MEMBER_MISMATCH":              func(a []string) string { return MemberMismatchMsg(a[0], a[1]) },
		"HOSTING_MISMATCH":             func(a []string) string { return HostingMismatchMsg(a[0], a[1]) },
		"UNAVAILABLE_SERVER":           func([]string) string { return UnavailableServerMsg() },
		"RUN_TYPE_NOT_ALLOWED":         func(a []string) string { return RunTypeNotAllowedMsg(a[0], types.RunnerPoolRunType(a[1])) },
		"BARRIER_NOT_ALLOWED": func(a []string) string {
			var allowed []types.ConfinementClass
			for _, c := range a[2:] {
				allowed = append(allowed, types.ConfinementClass(c))
			}
			return BarrierNotAllowedMsg(a[0], types.ConfinementClass(a[1]), allowed)
		},
		"AT_CAPACITY": func(a []string) string { n, _ := strconv.Atoi(a[1]); return AtCapacityMsg(a[0], n) },
	}
	var golden []struct {
		Key  string   `json:"key"`
		Args []string `json:"args"`
		Text string   `json:"text"`
	}
	if err := json.Unmarshal(uiFile(t, "runner-pool-refusals.golden.json"), &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) != len(sentences) {
		t.Fatalf("golden has %d sentences, Go has %d", len(golden), len(sentences))
	}
	for _, g := range golden {
		fn, ok := sentences[g.Key]
		if !ok {
			t.Errorf("golden key %s has no Go sentence", g.Key)
			continue
		}
		if got := fn(g.Args); got != g.Text {
			t.Errorf("%s = %q, golden %q", g.Key, got, g.Text)
		}
	}
}

func TestReasonsAreTheTypeScriptOnes(t *testing.T) {
	src, err := os.ReadFile("reasons.go")
	if err != nil {
		t.Fatal(err)
	}
	var goSet []string
	for _, m := range regexp.MustCompile(`\bReason\w+\s+Reason = "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		goSet = append(goSet, m[1])
	}
	var tsSet []string
	block := regexp.MustCompile(`(?s)RUNNER_POOL_REASONS = \[(.*?)\]`).FindSubmatch(uiFile(t, "runner-pool-refusals.ts"))
	if block == nil {
		t.Fatal("runner-pool-refusals.ts has no RUNNER_POOL_REASONS")
	}
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(block[1], -1) {
		tsSet = append(tsSet, string(m[1]))
	}
	slices.Sort(goSet)
	slices.Sort(tsSet)
	if !slices.Equal(goSet, tsSet) || len(goSet) != len(Reasons()) {
		t.Errorf("Go reasons %v, TypeScript reasons %v, registry %d", goSet, tsSet, len(Reasons()))
	}
	for _, r := range Reasons() {
		if r.Status() == 0 || !strings.HasPrefix(string(r), "runner_pool") {
			t.Errorf("%s has no status or the wrong family", r)
		}
	}
}

func pool(name string, h types.RunnerPoolHosting, st types.RunnerPoolState) types.RunnerPool {
	return types.RunnerPool{ID: uuid.New(), Name: name, HostingType: h, State: st, Revision: 3}
}

func idp(p types.RunnerPool) *uuid.UUID { return &p.ID }

func TestResolvePrecedence(t *testing.T) {
	remoteA := pool("A", types.RunnerPoolRemoteProvided, types.RunnerPoolActive)
	remoteB := pool("B", types.RunnerPoolRemoteProvided, types.RunnerPoolActive)
	selfC := pool("C", types.RunnerPoolSelfHosted, types.RunnerPoolActive)
	selfOff := pool("Off", types.RunnerPoolSelfHosted, types.RunnerPoolDisabled)
	gone := pool("Gone", types.RunnerPoolRemoteProvided, types.RunnerPoolDeleted)
	all := []types.RunnerPool{remoteA, remoteB, selfC, selfOff, gone}
	org := types.RunnerPoolDefaults{PreferredHosting: types.RunnerPoolRemoteProvided, RemoteProvided: idp(remoteA), SelfHosted: idp(selfC)}

	cases := []struct {
		name     string
		choice   Choice
		personal types.RunnerPoolDefaults
		org      types.RunnerPoolDefaults
		pool     *types.RunnerPool
		sel      types.RunnerPoolSelection
		reason   Reason
		source   types.RunnerPoolSelection
		msg      string
	}{
		{name: "organisation default seeds the choice", org: org, pool: &remoteA, sel: types.RunnerPoolSelectedOrg},
		{name: "a person's default beats the organisation's", personal: types.RunnerPoolDefaults{RemoteProvided: idp(remoteB)}, org: org, pool: &remoteB, sel: types.RunnerPoolSelectedPersonal},
		{name: "explicit pool beats both", choice: Choice{PoolID: idp(remoteB)}, personal: types.RunnerPoolDefaults{RemoteProvided: idp(remoteA)}, org: org, pool: &remoteB, sel: types.RunnerPoolSelectedExplicit},
		{name: "personal preferred hosting beats the organisation's", personal: types.RunnerPoolDefaults{PreferredHosting: types.RunnerPoolSelfHosted}, org: org, pool: &selfC, sel: types.RunnerPoolSelectedOrg},
		{name: "explicit hosting beats the personal preference", choice: Choice{Hosting: types.RunnerPoolRemoteProvided}, personal: types.RunnerPoolDefaults{PreferredHosting: types.RunnerPoolSelfHosted}, org: org, pool: &remoteA, sel: types.RunnerPoolSelectedOrg},
		{name: "a refused personal default never falls through to the organisation's", personal: types.RunnerPoolDefaults{RemoteProvided: idp(gone)}, org: org, reason: ReasonDefaultUnavailable, source: types.RunnerPoolSelectedPersonal, msg: DefaultUnavailablePersonalMsg()},
		{name: "a disabled personal default is refused", choice: Choice{Hosting: types.RunnerPoolSelfHosted}, personal: types.RunnerPoolDefaults{SelfHosted: idp(selfOff)}, org: org, reason: ReasonDefaultUnavailable, source: types.RunnerPoolSelectedPersonal, msg: DefaultUnavailablePersonalMsg()},
		{name: "an unknown organisation default is refused", org: types.RunnerPoolDefaults{RemoteProvided: &uuid.UUID{1}, PreferredHosting: types.RunnerPoolRemoteProvided}, reason: ReasonDefaultUnavailable, source: types.RunnerPoolSelectedOrg, msg: DefaultUnavailableOrgMsg()},
		{name: "a wrong-kind default is refused, not flipped to the other type", org: types.RunnerPoolDefaults{PreferredHosting: types.RunnerPoolRemoteProvided, RemoteProvided: idp(selfC)}, reason: ReasonDefaultUnavailable, source: types.RunnerPoolSelectedOrg, msg: DefaultUnavailableOrgMsg()},
		{name: "no default at all asks for a choice", org: types.RunnerPoolDefaults{PreferredHosting: types.RunnerPoolSelfHosted}, reason: ReasonRequired, msg: RequiredMsg("Self-Hosted")},
		{name: "no hosting type asks for a choice", reason: ReasonRequired, msg: RequiredMsg("")},
		{name: "an inaccessible explicit pool is not found", choice: Choice{PoolID: &uuid.UUID{9}}, org: org, reason: ReasonNotFound, msg: NotFoundMsg()},
		{name: "a deleted explicit pool is not found", choice: Choice{PoolID: idp(gone)}, org: org, reason: ReasonNotFound, msg: NotFoundMsg()},
		{name: "a disabled explicit pool is unavailable", choice: Choice{PoolID: idp(selfOff)}, org: org, reason: ReasonUnavailable, msg: UnavailableMsg("Off")},
		{name: "an explicit pool contradicting the hosting choice is a mismatch", choice: Choice{PoolID: idp(selfC), Hosting: types.RunnerPoolRemoteProvided}, org: org, reason: ReasonMemberMismatch, msg: HostingMismatchMsg("C", "Self-Hosted")},
		{name: "a garbage hosting choice is invalid", choice: Choice{Hosting: "cloud"}, org: org, reason: ReasonInvalid, msg: InvalidMsg()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal := Resolve(all, tc.choice, tc.personal, tc.org)
			if tc.reason != "" {
				if refusal == nil || refusal.Reason != tc.reason || refusal.Source != tc.source {
					t.Fatalf("got %+v %+v, want refusal %s (source %q)", got, refusal, tc.reason, tc.source)
				}
				if refusal.Message() != tc.msg || tc.msg == "" {
					t.Errorf("sentence = %q, want %q", refusal.Message(), tc.msg)
				}
				return
			}
			if refusal != nil || got.ID != tc.pool.ID || got.Selection != tc.sel || got.Revision != 3 || got.HostingType != tc.pool.HostingType {
				t.Fatalf("got %+v %+v, want pool %s via %s", got, refusal, tc.pool.Name, tc.sel)
			}
		})
	}
}

func TestResolveNeverPicksTheOnlyOrFirstPool(t *testing.T) {
	only := pool("Only", types.RunnerPoolRemoteProvided, types.RunnerPoolActive)
	if got, refusal := Resolve([]types.RunnerPool{only}, Choice{Hosting: types.RunnerPoolRemoteProvided}, types.RunnerPoolDefaults{}, types.RunnerPoolDefaults{}); refusal == nil || refusal.Reason != ReasonRequired {
		t.Fatalf("a sole pool was chosen for a request with no default: %+v", got)
	}
}

func TestOwnRunnerCandidatesAreOnlyThePersonsOwn(t *testing.T) {
	mine1, mine2, theirs, unclaimed, revoked, notMember := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	runners := []types.Runner{
		{ID: mine1, Owner: "alice", State: types.RunnerClaimed},
		{ID: mine2, Owner: "alice", State: types.RunnerClaimed},
		{ID: theirs, Owner: "bob", State: types.RunnerClaimed},
		{ID: unclaimed, Owner: "alice", State: types.RunnerUnclaimed},
		{ID: revoked, Owner: "alice", State: types.RunnerRevoked},
		{ID: notMember, Owner: "alice", State: types.RunnerClaimed},
	}
	member := func(id uuid.UUID) types.RunnerPoolMember { return types.RunnerPoolMember{RunnerID: &id} }
	members := []types.RunnerPoolMember{member(theirs), member(mine2), member(unclaimed), member(mine1), member(revoked), member(mine1), {ExecutorID: "exec-1"}, member(uuid.New())}

	got := OwnRunnerCandidates("alice", members, runners)
	want := []uuid.UUID{mine1, mine2}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Fatalf("alice's candidates = %v, want %v", got, want)
	}
	slices.Reverse(members)
	if again := OwnRunnerCandidates("alice", members, runners); !slices.Equal(again, want) {
		t.Fatalf("member order changed the candidates: %v", again)
	}
	if got := OwnRunnerCandidates("bob", members, runners); !slices.Equal(got, []uuid.UUID{theirs}) {
		t.Fatalf("bob's candidates = %v, want only his own runner", got)
	}
	for _, who := range []string{"", "carol", "ALICE"} {
		if got := OwnRunnerCandidates(who, members, runners); got != nil {
			t.Fatalf("%q was handed candidates %v", who, got)
		}
	}
}

// TestEveryReasonHasItsOwnSentence: a refusal never falls through to the
// "invalid" sentence, so a D-117 refusal built from any reason says its own words.
func TestEveryReasonHasItsOwnSentence(t *testing.T) {
	variants := map[Reason][]*Refusal{
		ReasonRequired:           {{Reason: ReasonRequired, Hosting: types.RunnerPoolSelfHosted}},
		ReasonNotFound:           {{Reason: ReasonNotFound}},
		ReasonUnavailable:        {{Reason: ReasonUnavailable, Name: "Build farm"}},
		ReasonDefaultUnavailable: {{Reason: ReasonDefaultUnavailable}, {Reason: ReasonDefaultUnavailable, Source: types.RunnerPoolSelectedOrg}},
		ReasonStale:              {{Reason: ReasonStale, Name: "Build farm"}},
		ReasonNoEligibleMember:   {{Reason: ReasonNoEligibleMember, Name: "P"}, {Reason: ReasonNoEligibleMember, Name: "P", Hosting: types.RunnerPoolSelfHosted}},
		ReasonMemberMismatch:     {{Reason: ReasonMemberMismatch, Name: "P", Hosting: types.RunnerPoolSelfHosted}, {Reason: ReasonMemberMismatch, Name: "P", Runner: "desk-1"}},
		ReasonPoolsUnavailable:   {{Reason: ReasonPoolsUnavailable}},
		ReasonRunTypeNotAllowed:  {{Reason: ReasonRunTypeNotAllowed, Name: "P", RunType: types.RunnerPoolRunBackground}},
		ReasonBarrierNotAllowed:  {{Reason: ReasonBarrierNotAllowed, Name: "P", Barrier: types.CC1, Allowed: []types.ConfinementClass{types.CC3}}},
		ReasonAtCapacity:         {{Reason: ReasonAtCapacity, Name: "P", Max: 2}},
	}
	for _, reason := range Reasons() {
		if reason == ReasonInvalid {
			continue
		}
		list, ok := variants[reason]
		if !ok {
			t.Errorf("%s has no case here", reason)
		}
		for _, r := range list {
			if got := r.Message(); got == InvalidMsg() || got == "" {
				t.Errorf("%+v answers %q, the invalid sentence", r, got)
			}
		}
	}
	for _, c := range []struct {
		r    *Refusal
		want string
	}{
		{&Refusal{Reason: ReasonMemberMismatch, Name: "Team laptops", Runner: "desk-1"}, MemberMismatchMsg("desk-1", "Team laptops")},
		{&Refusal{Reason: ReasonMemberMismatch, Name: "Team laptops", Hosting: types.RunnerPoolSelfHosted}, HostingMismatchMsg("Team laptops", "Self-Hosted")},
		{&Refusal{Reason: ReasonNoEligibleMember, Name: "Team laptops", Hosting: types.RunnerPoolSelfHosted}, NoOwnRunnerMsg("Team laptops")},
		{&Refusal{Reason: ReasonNoEligibleMember, Name: "Build farm", Hosting: types.RunnerPoolRemoteProvided}, NoEligibleMemberMsg("Build farm")},
		{&Refusal{Reason: ReasonPoolsUnavailable}, UnavailableServerMsg()},
	} {
		if got := c.r.Message(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.r, got, c.want)
		}
	}
}
