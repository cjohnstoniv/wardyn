// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// squeeze turns every run of two or more spaces into "|", so a table is compared by its cells.
func squeeze(out string) string {
	return regexp.MustCompile(` {2,}`).ReplaceAllString(strings.TrimSpace(out), "|")
}

// create, update and delete say what is now true, in the mock's words; --json prints the pool as sent.
func TestRunnerPoolSuccessText(t *testing.T) {
	id := uuid.New()
	user := sdk.RunnerPool{ID: id, Name: "Home server", Scope: sdk.RunnerPoolUser, Revision: 1}
	srv := newPoolRoutes(t, map[string]any{
		"POST /api/v1/me/runner-pools":                  user,
		"POST /api/v1/runner-pools":                     sdk.RunnerPool{ID: id, Name: "General", Scope: sdk.RunnerPoolOrganization, Revision: 1},
		"GET /api/v1/runner-pools/" + id.String():       sdk.RunnerPool{ID: id, Name: "Laptop", Scope: sdk.RunnerPoolUser, Revision: 4},
		"PUT /api/v1/me/runner-pools/" + id.String():    sdk.RunnerPool{ID: id, Name: "Laptop", Scope: sdk.RunnerPoolUser, Revision: 5},
		"DELETE /api/v1/me/runner-pools/" + id.String(): nil,
	})
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(append([]string{"runner", "pool"}, args...), "--url", srv.URL, "--token", "t")...)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "--name", "Home server"}, "created user pool Home server (" + id.String() + "), yours, revision 1\n" +
			"  limits: it follows its runners; it has none yet. Put a runner in it: wardyn runner pool add " + id.String() + " <runner>\n"},
		{[]string{"create", "--name", "General", "--scope", "organization"}, "created organization pool General (" + id.String() + "), revision 1\n"},
		{[]string{"update", id.String(), "--max-runs", "6"}, "updated pool Laptop, revision 5\n"},
		{[]string{"delete", id.String(), "--yes"}, "pool Laptop (" + id.String() + ") deleted\n" +
			"  runs that were waiting for it ended: \"The pool this run was waiting for was removed.\"\n" +
			"  runs already running were not stopped; its runners were taken out of it, not touched\n"},
	} {
		if out, err := run(tc.args...); err != nil || out != tc.want {
			t.Errorf("%v = %v\n%q\nwant\n%q", tc.args, err, out, tc.want)
		}
	}
	if out, err := run("create", "--name", "Home server", "--json"); err != nil || strings.Contains(out, "created user pool") || !strings.Contains(out, `"revision": 1`) {
		t.Errorf("create --json = %v\n%s", err, out)
	}
	if _, err := run("delete", id.String()); err == nil || err.Error() != "refusing without --yes: this ends every run waiting for the pool, takes every runner out of it and clears it as your default" {
		t.Errorf("delete without --yes = %v", err)
	}
}

// The list is ID NAME TYPE AVAILABILITY; a pool the caller may only manage is left out, and --all brings
// every row back with its owner.
func TestRunnerPoolListColumns(t *testing.T) {
	row := func(name string, scope sdk.RunnerPoolScope, owner, availability string, reason sdk.RunnerPoolReason) sdk.RunnerPoolCatalogueEntry {
		return sdk.RunnerPoolCatalogueEntry{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: uuid.MustParse("7c1e9b34-5d2a-4f08-9a61-0b3e2d4c8a10"), Name: name, Scope: scope, Availability: availability, Reason: reason}, Owner: owner}
	}
	srv := newCmdServer(t, http.StatusOK, sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{
		row("General", sdk.RunnerPoolOrganization, "", sdk.RunnerPoolAvailable, ""),
		row("Laptop", sdk.RunnerPoolUser, "dana@example.com", sdk.RunnerPoolAvailable, ""),
		row("Theirs", sdk.RunnerPoolUser, "alice@example.com", sdk.RunnerPoolUnavailable, "runner_pool_not_found"),
	}})
	id := "7c1e9b34-5d2a-4f08-9a61-0b3e2d4c8a10"
	out, err := execCmdCapture(t, "runner", "pool", "list", "--url", srv.URL, "--token", "t")
	if want := "ID|NAME|TYPE|AVAILABILITY\n" + id + "|General|organization|available\n" + id + "|Laptop|user|available"; err != nil || squeeze(out) != want {
		t.Fatalf("list = %v\n%s\nwant\n%s", err, squeeze(out), want)
	}
	out, err = execCmdCapture(t, "runner", "pool", "list", "--all", "--url", srv.URL, "--token", "t")
	if err != nil || !strings.HasPrefix(squeeze(out), "ID|NAME|TYPE|OWNER|AVAILABILITY\n") || !strings.Contains(squeeze(out), "General|organization|available") ||
		!strings.Contains(squeeze(out), "Theirs|user|alice@example.com|unavailable (runner_pool_not_found)") {
		t.Fatalf("list --all = %v\n%s", err, squeeze(out))
	}
}

// show adds up the pool's members: a member with no limits adds nothing, the pool's own smaller cap
// lowers the biggest run, and the owner is "(you)" only when the server says the caller is the owner.
func TestRunnerPoolShowAddsUpTheMembers(t *testing.T) {
	id := uuid.New()
	laptop := uuid.New()
	amounts := func(c, m, d int64) types.RunnerAmounts {
		return types.RunnerAmounts{CPUMillis: c * 1000, MemoryMiB: m * 1024, DiskMiB: d * 1024}
	}
	tally := func(runs int, c, m, d int64) types.RunnerTally {
		return types.RunnerTally{Runs: runs, CPUMillis: c * 1000, MemoryMiB: m * 1024, DiskMiB: d * 1024}
	}
	cap8 := 40
	pool := sdk.RunnerPool{ID: id, Name: "Laptop", Scope: sdk.RunnerPoolUser, Owner: "dana@example.com", State: types.RunnerPoolActive, Revision: 2,
		Limits: &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{MemoryMiB: &sdk.RunnerPoolAmount{Default: 2048, Cap: 8 * 1024}},
			Barriers: []types.ConfinementClass{types.CC1, types.CC2}, MaxConcurrentRuns: &cap8}}
	members := []sdk.RunnerPoolMemberView{
		{Kind: sdk.RunnerKindSelfHosted, ID: laptop.String(), Name: "laptop-7", Online: true,
			Limits: &types.RunnerLimits{RunsAtOnce: 3, Total: amounts(6, 12, 200), PerRun: amounts(2, 4, 50)},
			Usage:  types.RunnerUsage{InUse: tally(2, 4, 8, 100), Available: tally(1, 2, 4, 100)}},
		{Kind: sdk.RunnerKindRemoteHosted, ID: "docker", Name: "shared-docker", Online: true,
			Limits: &types.RunnerLimits{RunsAtOnce: 6, Total: amounts(24, 96, 1000), PerRun: amounts(4, 16, 100)},
			Usage:  types.RunnerUsage{InUse: tally(1, 4, 16, 100), Available: tally(5, 20, 80, 900)}},
		{Kind: sdk.RunnerKindSelfHosted, ID: uuid.NewString(), Name: "fresh"},
	}
	routes := map[string]any{
		"GET /api/v1/runner-pools/" + id.String():              pool,
		"GET /api/v1/runner-pools/" + id.String() + "/members": members,
		"GET /api/v1/me": map[string]string{"principal": "dana@example.com"},
	}
	srv := newPoolRoutes(t, routes)
	out, err := execCmdCapture(t, "runner", "pool", "show", id.String(), "--url", srv.URL, "--token", "t")
	want := strings.Join([]string{
		"ID|" + id.String(), "NAME|Laptop", "TYPE|user pool", "OWNER|dana@example.com (you)", "STATE|active",
		"RUN TYPES|background", "BARRIERS|Fence, Wall",
		"RUNNERS|laptop-7 (self-hosted, online), shared-docker (remote-hosted, online), fresh (self-hosted, offline)",
		"TOTAL|CPU 30 · memory 108 GB · disk 1200 GB · runs 9|(sum across its runners)",
		"IN USE|CPU 8 · memory 24 GB · disk 200 GB · runs 3",
		"AVAILABLE|CPU 22 · memory 84 GB · disk 1000 GB · runs 6",
		"BIGGEST RUN|CPU 4 · memory 8 GB · disk 100 GB|(the biggest run this pool can take)",
		"POOL LIMITS|memory per run narrowed by this pool; 40 runs maximum in this pool",
		"WHO MAY USE|only you",
	}, "\n")
	if err != nil || squeeze(out) != want {
		t.Fatalf("show = %v\n%s\nwant\n%s", err, squeeze(out), want)
	}

	// Not the caller's pool, or the caller unknown: the owner is named without "(you)"; a pool that is not
	// active offers nothing; no runners and no limits of its own.
	bare := sdk.RunnerPool{ID: id, Name: "Laptop", Scope: sdk.RunnerPoolUser, Owner: "alice@example.com", State: types.RunnerPoolDisabled}
	var sb strings.Builder
	if err := printPool(&sb, bare, nil, "dana@example.com", ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OWNER|alice@example.com", "RUNNERS|none yet", "TOTAL|CPU 0 · memory 0 GB · disk 0 GB · runs 0", "POOL LIMITS|none of its own: it follows its runners", "WHO MAY USE|only alice@example.com"} {
		if !strings.Contains(squeeze(sb.String()), want) {
			t.Errorf("bare pool lacks %q:\n%s", want, squeeze(sb.String()))
		}
	}
	if strings.Contains(sb.String(), "(you)") {
		t.Error("another person's pool was marked (you)")
	}
	if out, err := execCmdCapture(t, "runner", "pool", "show", id.String(), "--json", "--url", srv.URL, "--token", "t"); err != nil || !strings.Contains(out, `"revision": 2`) || strings.Contains(out, "TOTAL") {
		t.Errorf("show --json = %v\n%s", err, out)
	}
}

// add reads the pool first: a runner already in it is said so and not added again; an unclaimed one is
// refused before any write; --default then makes the pool the caller's default and keeps their barrier.
func TestRunnerPoolAddKnowsWhatIsAlreadyThere(t *testing.T) {
	id, mine, fresh := uuid.New(), uuid.New(), uuid.New()
	pool := sdk.RunnerPool{ID: id, Name: "Home server", Scope: sdk.RunnerPoolUser, Revision: 1}
	srv := newPoolRoutes(t, map[string]any{
		"GET /api/v1/runner-pools/" + id.String():              pool,
		"GET /api/v1/runner-pools/" + id.String() + "/members": []sdk.RunnerPoolMemberView{{Kind: sdk.RunnerKindSelfHosted, ID: mine.String(), Name: "laptop-7"}},
		"GET /api/v1/me/runners":                               []sdk.RunnerView{{ID: mine, Name: "laptop-7", State: types.RunnerClaimed}, {ID: fresh, Name: "mbp-ben", State: types.RunnerUnclaimed}},
		"GET /api/v1/me/remote-runners":                        []sdk.RemoteRunnerView{},
		"GET /api/v1/me/runner-pool-defaults":                  sdk.RunnerPoolDefaults{Barrier: types.CC2},
		"PUT /api/v1/me/runner-pool-defaults":                  sdk.RunnerPoolDefaults{},
		"PUT *":                                                sdk.RunnerPoolMember{PoolID: id},
	})
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(append([]string{"runner", "pool", "add", id.String()}, args...), "--url", srv.URL, "--token", "t")...)
	}
	if out, err := run("laptop-7"); err != nil || out != "laptop-7 is already in Home server\n" || srv.sent(http.MethodPut, "/api/v1/me/runner-pools/"+id.String()+"/runners/"+mine.String()) {
		t.Fatalf("already in = %v\n%q\n%+v", err, out, srv.reqs)
	}
	before := len(srv.reqs)
	_, err := run("mbp-ben")
	if err == nil || err.Error() != "mbp-ben is not claimed yet; claim it first with `wardyn runner claim`" {
		t.Fatalf("unclaimed = %v", err)
	}
	// Named by its id it is the same runner, refused the same way.
	if _, err := run(fresh.String()); err == nil || err.Error() != fresh.String()+" is not claimed yet; claim it first with `wardyn runner claim`" {
		t.Fatalf("unclaimed by id = %v", err)
	}
	for _, r := range srv.reqs[before:] {
		if r.method == http.MethodPut {
			t.Fatalf("an unclaimed runner was written: %s %s", r.method, r.path)
		}
	}
	out, err := run("laptop-7", "--default")
	if err != nil || out != "laptop-7 is already in Home server\ndefault user pool set to Home server\n" {
		t.Fatalf("--default = %v\n%q", err, out)
	}
	var put string
	for _, r := range srv.reqs {
		if r.method == http.MethodPut && strings.HasSuffix(r.path, "/runner-pool-defaults") {
			put = string(r.body)
		}
	}
	if !strings.Contains(put, id.String()) || !strings.Contains(put, "CC2") {
		t.Errorf("default sent %q, want the pool and the kept barrier", put)
	}
}

// A pool's own limits are said as its own whether they narrow its runners or not: a maximum equal to or
// wider than theirs still carries a default every untouched run gets. Only a pool that sets none "follows
// its runners", in show and in what create prints.
func TestPoolLimitsTextSaysWhatThePoolSets(t *testing.T) {
	runners := types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 16 << 10, DiskMiB: 100 << 10}
	two := 2
	amount := func(def, cap int) *sdk.RunnerPoolAmount { return &sdk.RunnerPoolAmount{Default: def, Cap: cap} }
	for _, tc := range []struct {
		name   string
		limits *sdk.RunnerPoolLimits
		want   string
	}{
		{"none", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{}}, "none of its own: it follows its runners"},
		{"no limits at all", nil, "none of its own: it follows its runners"},
		{"lower", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{MemoryMiB: amount(2<<10, 8<<10)}}, "memory per run narrowed by this pool"},
		{"equal", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{MemoryMiB: amount(2<<10, 16<<10)}}, "memory per run set by this pool"},
		{"wider", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{CPUMillis: amount(1000, 64000)}}, "cpu per run set by this pool"},
		{"all three and a runs maximum", &sdk.RunnerPoolLimits{MaxConcurrentRuns: &two, Background: &types.RunnerPoolBackgroundLimits{
			CPUMillis: amount(1000, 64000), MemoryMiB: amount(2<<10, 8<<10), DiskMiB: amount(20<<10, 100<<10)}},
			"memory per run narrowed by this pool; cpu, disk per run set by this pool; 2 runs maximum in this pool"},
	} {
		if got := poolLimitsText(sdk.RunnerPool{Limits: tc.limits}, runners); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	id := uuid.New()
	var sb strings.Builder
	own := sdk.RunnerPool{ID: id, Name: "Home server", Scope: sdk.RunnerPoolUser, Revision: 1,
		Limits: &sdk.RunnerPoolLimits{MaxConcurrentRuns: &two, Background: &types.RunnerPoolBackgroundLimits{MemoryMiB: amount(2<<10, 8<<10)}}}
	if err := printPoolCreated(&sb, own, false, false); err != nil {
		t.Fatal(err)
	}
	want := "created user pool Home server (" + id.String() + "), yours, revision 1\n" +
		"  limits: memory per run set by this pool; 2 runs maximum in this pool; it has no runners yet. Put a runner in it: wardyn runner pool add " + id.String() + " <runner>\n"
	if sb.String() != want {
		t.Errorf("create with its own limits:\n%q\nwant\n%q", sb.String(), want)
	}
}

// The biggest run is what one runner can take: CPU from one runner and memory from another is a run
// no runner starts.
func TestBiggestRunComesFromOneRunner(t *testing.T) {
	amounts := func(c, m, d int64) types.RunnerAmounts {
		return types.RunnerAmounts{CPUMillis: c * 1000, MemoryMiB: m << 10, DiskMiB: d << 10}
	}
	member := func(name string, perRun types.RunnerAmounts) sdk.RunnerPoolMemberView {
		return sdk.RunnerPoolMemberView{Kind: sdk.RunnerKindSelfHosted, ID: name, Name: name, Online: true,
			Limits: &types.RunnerLimits{RunsAtOnce: 2, Total: amounts(16, 64, 1000), PerRun: perRun}}
	}
	wide, deep := member("wide", amounts(8, 4, 50)), member("deep", amounts(2, 16, 100))
	for _, tc := range []struct {
		name    string
		members []sdk.RunnerPoolMemberView
		caps    types.RunnerAmounts
		want    types.RunnerAmounts
	}{
		{"neither allows the most of everything: the one with the most CPU", []sdk.RunnerPoolMemberView{deep, wide}, types.RunnerAmounts{}, amounts(8, 4, 50)},
		{"the pool's caps first, then the one with the most CPU", []sdk.RunnerPoolMemberView{wide, deep}, types.RunnerAmounts{CPUMillis: 2000}, amounts(2, 16, 100)},
		{"one runner allows the most of everything", []sdk.RunnerPoolMemberView{deep, member("big", amounts(8, 32, 200))}, types.RunnerAmounts{}, amounts(8, 32, 200)},
		{"no runner with limits", []sdk.RunnerPoolMemberView{{Name: "fresh"}}, types.RunnerAmounts{}, types.RunnerAmounts{}},
	} {
		if got := biggestRun(tc.members, tc.caps); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A lifetime or idle stop is the pool's own limit: its runners set none. A pool that sets only those
// does not "follow its runners".
func TestPoolLimitsTextSaysTheLifetimesThePoolSets(t *testing.T) {
	dur := func(def, maxSec int, open bool) *types.RunnerPoolDuration {
		return &types.RunnerPoolDuration{DefaultSec: def, MaxSec: maxSec, Unlimited: open}
	}
	for _, tc := range []struct {
		name   string
		limits *sdk.RunnerPoolLimits
		want   string
	}{
		{"a background lifetime", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{Lifetime: dur(3600, 86400, false)}},
			"background lifetime default 1 hour, up to 1 day"},
		{"no end", &sdk.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{Lifetime: dur(0, 0, true)}}, "background lifetime no maximum"},
		{"interactive", &sdk.RunnerPoolLimits{Interactive: &types.RunnerPoolInteractiveLimits{Lifetime: dur(8*3600, 86400, false), Idle: dur(1800, 0, true)}},
			"interactive lifetime default 8 hours, up to 1 day; interactive idle stop default 30 minutes, never"},
	} {
		if got := poolLimitsText(sdk.RunnerPool{Limits: tc.limits}, types.RunnerAmounts{}); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The next step create prints always names the pool by its id: a name with a dollar sign or a quote
// pasted into a shell would run or break, and a plain one such as "--help" would be read as a flag.
func TestPoolCreatedNamesThePoolByItsIDInTheCommand(t *testing.T) {
	for _, name := range []string{`$(touch x) "dana's"`, "--help", "Laptop"} {
		id := uuid.New()
		var sb strings.Builder
		if err := printPoolCreated(&sb, sdk.RunnerPool{ID: id, Name: name, Scope: sdk.RunnerPoolUser, Revision: 1}, false, false); err != nil {
			t.Fatal(err)
		}
		_, cmd, _ := strings.Cut(sb.String(), "Put a runner in it: ")
		if want := "wardyn runner pool add " + id.String() + " <runner>\n"; cmd != want {
			t.Errorf("pool %q: next step = %q, want %q", name, cmd, want)
		}
	}
}
