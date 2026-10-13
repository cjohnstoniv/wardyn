// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_design_lines_test.go walks every `wardyn` command line the approved runners and pools design
// draws in its command-line frames, as drawn (its trailing "# ..." note included), and proves each one
// parses and reaches its handler:
//
//   - a synopsis line (`pool default get|set|clear`, `<pool>`, `[--all]`) names commands and flags that
//     must all exist;
//   - a line drawn with "..." stands for flags given earlier, so its command and the flags it does show
//     must exist;
//   - every other line is run against a stand-in for the server. One the design answers with a sentence
//     of the command's own must be refused with exactly that sentence; any other must send at least one
//     request and fail, if it fails, only with the server's answer, never over its own words or flags.
//
// Two things the design draws are ruled out and are not asserted here: a pool limit wider than its
// runners is accepted (the design shows it refused), and a remote-hosted runner's kind carries no
// substrate word. `wardyn-runnerd` lines belong to that command's own tests.

const designAlice = "d41a96c0-7b2e-4e38-8f15-62c9a0e7b4d1"

var designLines = []struct {
	line string
	// refusal is the sentence the design shows the command itself answering with; "" where it shows the
	// command succeeding or the server answering.
	refusal string
}{
	// Grammar.
	{`wardyn runner pool list [--all] [--json]                          # --all (operator or security admin) adds every person's user pools, read-only`, ""},
	{`wardyn runner pool default get|set|clear                          # your own defaults`, ""},
	{`wardyn runner pool show <pool> [--json]`, ""},
	{`wardyn runner pool create --name <name> [--scope user|organization] <limits> [--json]`, ""},
	{`wardyn runner pool update <pool> [--name <name>] [--switch on|off] <limits> [--json]`, ""},
	{`wardyn runner pool delete <pool> --yes`, ""},
	{`wardyn runner pool add <pool> <runner>...                         # your own self-hosted runners and available remote-hosted runners into your user pool; an admin adds remote-hosted runners (only) to an organization pool`, ""},
	{`wardyn runner pool remove <pool> <runner>...`, ""},
	{`wardyn runner pool use-policy get|set|clear <pool>                # organization pools, security admin or admin`, ""},
	{`wardyn runner pool org-default get|set|clear                      # the organization default pool, admin`, ""},
	{`wardyn runner list [--all] [--json]                               # RESOURCES: in use / total, per run; --all adds every runner`, ""},
	{`wardyn runner set <runner> [--max-runs N] [--total-cpu N --total-memory GB --total-disk GB] [--per-run-cpu N --per-run-memory GB --per-run-disk GB] [--available-to-people on|off]`, ""},
	// create.
	{`wardyn runner pool create --name Laptop --run-type background --run-type interactive --barrier fence --barrier wall`, ""},
	{`wardyn runner pool add Laptop laptop-7 shared-docker`, ""},
	{`wardyn runner pool create --name "Home server" --run-type background --background-memory 2:8 --background-disk 20:50 --max-runs 2`, ""},
	{`wardyn runner pool create --name General --scope organization --run-type background --barrier wall`, ""},
	{`wardyn runner pool add General eu-cluster docker-host-1`, ""},
	// create: refusals.
	{`wardyn runner pool create --name Tiny --run-type background --background-memory 8:4`, "--background-memory: the default (8) can't be more than the maximum (4)"},
	{`wardyn runner pool create --name Tiny --limits-file t.json --barrier wall`, "--limits-file replaces the limit flags; use one or the other"},
	{`wardyn runner pool create --name Laptop ...                       # you already have a pool of that name`, ""},
	{`wardyn runner pool update Laptop --background-memory 2:64        # its runners allow at most 16 GB per run`, ""},
	{`wardyn runner pool create --name General --scope organization ...  # not an admin`, ""},
	{`wardyn runner pool create ...        # a server without pool storage`, ""},
	// show, list.
	{`wardyn runner pool show Laptop`, ""},
	{`wardyn runner pool show "CI jobs"        # as a security admin or admin`, ""},
	{`wardyn runner pool list`, ""},
	{`wardyn runner pool list --all`, ""},
	// update.
	{`wardyn runner pool update Laptop --max-runs 6 --barrier fence --barrier wall --barrier vault`, ""},
	{`wardyn runner pool update "Home server" --name "Home lab"`, ""},
	{`wardyn runner pool update Laptop --max-runs 6        # you saved first from the console`, ""},
	{`wardyn runner pool update Laptop`, "name something to change: --name, --switch, --max-runs or a limit flag"},
	{`wardyn runner pool update d41a96c0-7b2e-4e38-8f15-62c9a0e7b4d1 --max-runs 3     # alice's pool`, ""},
	// delete.
	{`wardyn runner pool delete "Home server"`, "refusing without --yes: this ends every run waiting for the pool, takes every runner out of it and clears it as your default"},
	{`wardyn runner pool delete "Home server" --yes`, ""},
	{`wardyn runner pool delete "CI jobs" --yes                # organization pool, admin`, ""},
	// use-policy.
	{`wardyn runner pool use-policy get "CI jobs"`, ""},
	{`wardyn runner pool use-policy get General`, ""},
	{`wardyn runner pool use-policy set "CI jobs" --group platform-eng --role ci-bot --user lee@example.com`, ""},
	{`wardyn runner pool use-policy clear "CI jobs"`, ""},
	{`wardyn runner pool use-policy set Laptop --user lee@example.com`, "Laptop is a user pool. Only its owner can use it; a use policy narrows organization pools only"},
	// org-default.
	{`wardyn runner pool org-default get`, ""},
	{`wardyn runner pool org-default set --pool "CI jobs"`, ""},
	{`wardyn runner pool org-default clear`, ""},
	{`wardyn runner pool org-default set --pool Staging`, ""},
	{`wardyn runner pool org-default set --pool Laptop`, "there is no organization default for user pools; people choose their own"},
	// add, remove.
	{`wardyn runner pool add Laptop laptop-7 shared-docker`, ""},
	{`wardyn runner pool add Laptop laptop-7`, ""},
	{`wardyn runner pool add "Home server" desk-2 --default`, ""},
	{`wardyn runner pool add Laptop mbp-ben`, "mbp-ben is not claimed yet; claim it first with `wardyn runner claim`"},
	{`wardyn runner pool add Laptop eu-cluster`, ""},
	{`wardyn runner pool add Laptop build-ben                         # ben's self-hosted runner`, ""},
	{`wardyn runner pool add General docker-host-1                    # a person, not an admin`, ""},
	{`wardyn runner pool add General laptop-7                         # an admin adding a self-hosted runner to an organization pool`, ""},
	{`wardyn runner pool remove Laptop shared-docker`, ""},
	// runner list and runner set.
	{`wardyn runner list`, ""},
	{`wardyn runner set desk-2 --max-runs 4 --total-memory 32 --per-run-memory 8`, ""},
	{`wardyn runner set sandbox-cluster --available-to-people on`, ""},
	{`wardyn runner set desk-2 --total-memory 128`, ""},
	{`wardyn runner set desk-2 --per-run-memory 64 --total-memory 32`, "per-run memory (64) can't be more than total memory (32)"},
	// The command's own sentence goes on to name --available-to-people, a flag the design's line omits.
	{`wardyn runner set desk-2`, "name something to change: --max-runs, a --total-* or --per-run-* flag"},
	{`wardyn runner set desk-2 --max-runs 0`, "--max-runs is a whole number, 1 or more"},
	{`wardyn runner set build-ben --max-runs 2                        # ben's self-hosted runner`, ""},
	{`wardyn runner set desk-2 --max-runs 4 --token $WARDYN_ADMIN_TOKEN   # an admin reads a self-hosted runner's limits but cannot change them`, ""},
}

// designLimitFlags are the pool limit flags the grammar frame lists under <limits>.
var designLimitFlags = []string{"run-type", "barrier", "max-runs", "background-cpu", "background-memory", "background-disk", "background-lifetime",
	"interactive-cpu", "interactive-memory", "interactive-disk", "interactive-lifetime", "interactive-idle", "limits-file"}

// designWords splits a drawn line into its words, without the leading "wardyn" and the trailing note,
// keeping a double-quoted name as one word.
func designWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	quoted, open := false, false
	flush := func() {
		if open {
			words = append(words, cur.String())
			cur.Reset()
			open = false
		}
	}
scan:
	for _, r := range line {
		switch {
		case r == '"':
			quoted, open = !quoted, true
		case r == '#' && !quoted && !open:
			break scan
		case r == ' ' && !quoted:
			flush()
		default:
			cur.WriteRune(r)
			open = true
		}
	}
	flush()
	if len(words) == 0 || words[0] != "wardyn" {
		t.Fatalf("not a wardyn line: %q", line)
	}
	return words[1:]
}

var designFlag = regexp.MustCompile(`--[a-z][a-z-]*`)

// designCommands are the commands a synopsis line names: its leading words up to the first argument or
// flag, one command for each alternative of a `get|set|clear` word.
func designCommands(words []string) [][]string {
	paths := [][]string{{}}
	for _, w := range words {
		if strings.HasPrefix(w, "-") || strings.ContainsAny(w, "<[.") {
			break
		}
		var next [][]string
		for _, alt := range strings.Split(w, "|") {
			for _, p := range paths {
				next = append(next, append(append([]string{}, p...), alt))
			}
		}
		paths = next
	}
	return paths
}

// designWorld stands in for the server: the design's pools and runners, every write accepted.
func designWorld(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	ids := map[string]uuid.UUID{"Laptop": uuid.New(), "Home server": uuid.New(), "General": uuid.New(), "CI jobs": uuid.New(), "Staging": uuid.New()}
	scope := func(name string) sdk.RunnerPoolScope {
		if name == "Laptop" || name == "Home server" {
			return sdk.RunnerPoolUser
		}
		return sdk.RunnerPoolOrganization
	}
	pool := func(id string) sdk.RunnerPool {
		for name, pid := range ids {
			if pid.String() == id {
				return sdk.RunnerPool{ID: pid, Name: name, Scope: scope(name), State: types.RunnerPoolActive, Revision: 4}
			}
		}
		return sdk.RunnerPool{ID: uuid.MustParse(designAlice), Name: "Laptop", Scope: sdk.RunnerPoolUser, Owner: "alice@example.com", State: types.RunnerPoolActive, Revision: 3}
	}
	limits := &types.RunnerLimits{RunsAtOnce: 4, Total: types.RunnerAmounts{CPUMillis: 12000, MemoryMiB: 48 << 10, DiskMiB: 800 << 10},
		PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 16 << 10, DiskMiB: 100 << 10}}
	mine := []sdk.RunnerView{}
	for name, state := range map[string]types.RunnerState{"laptop-7": types.RunnerClaimed, "desk-2": types.RunnerClaimed, "mbp-ben": types.RunnerUnclaimed} {
		mine = append(mine, sdk.RunnerView{ID: uuid.New(), Name: name, State: state, Limits: limits})
	}
	remote := []sdk.RemoteRunnerView{}
	for _, name := range []string{"shared-docker", "sandbox-cluster", "eu-cluster", "docker-host-1"} {
		remote = append(remote, sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: name, Name: name, Revision: 2, Limits: limits}})
	}
	policy := sdk.RunnerPoolUsePolicy{Subjects: ciSubjects, Revision: 9, UpdatedAt: time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		seg := strings.Split(strings.Trim(path, "/"), "/")
		if seg[0] == "me" && len(seg) > 1 && seg[1] != "runners" && seg[1] != "remote-runners" {
			seg = seg[1:] // the caller's own pool routes answer as the organization's do
		}
		var body any
		switch get := r.Method == http.MethodGet; {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case path == "/me":
			body = map[string]string{"principal": "dana@example.com"}
		case path == "/me/runners" || path == "/runners":
			body = mine
		case seg[0] == "me" && seg[1] == "runners" || seg[0] == "runners":
			body = mine[0]
		case path == "/me/remote-runners" || path == "/remote-runners":
			body = remote
		case seg[0] == "remote-runners" && len(seg) == 3:
			body = remote[0].RemoteRunner
		case seg[0] == "remote-runners":
			body = remote[0]
		case seg[0] == "runner-pool-defaults":
			body = sdk.RunnerPoolDefaults{}
		case path == "/runner-pools" && get:
			list := sdk.RunnerPoolList{}
			for name, id := range ids {
				list.Pools = append(list.Pools, sdk.RunnerPoolCatalogueEntry{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: id, Name: name, Scope: scope(name), Availability: sdk.RunnerPoolAvailable}})
			}
			body = list
		case len(seg) == 1:
			body = sdk.RunnerPool{ID: ids["Laptop"], Name: "Laptop", Scope: sdk.RunnerPoolUser, Revision: 1}
		case len(seg) == 2:
			body = pool(seg[1])
		case seg[2] == "members" && get:
			body = []sdk.RunnerPoolMemberView{{Kind: sdk.RunnerKindRemoteHosted, ID: "shared-docker", Name: "shared-docker", Online: true, Limits: limits}}
		case seg[2] == "use-policy" && get && pool(seg[1]).Name == "General":
			body = sdk.RunnerPoolUsePolicy{Subjects: []sdk.RunnerPoolSubject{}, Revision: 6}
		case seg[2] == "use-policy":
			body = policy
		default:
			body = sdk.RunnerPoolMember{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEveryDesignCommandLineReachesItsHandler(t *testing.T) {
	var hits atomic.Int64
	srv := designWorld(t, &hits)
	for _, dl := range designLines {
		words := designWords(t, dl.line)
		synopsis := strings.ContainsAny(strings.Join(words, " "), "<[|")
		elided := strings.Contains(strings.Join(words, " "), "...") && !synopsis
		// A synopsis names its commands in words; any other line is whatever command its words reach.
		var cmds []*cobra.Command
		if synopsis {
			for _, p := range designCommands(words) {
				c, rest, err := rootCmd().Find(p)
				if err != nil || len(rest) != 0 || !c.Runnable() {
					t.Errorf("%q: no command %q (%v, left over %v)", dl.line, strings.Join(p, " "), err, rest)
					continue
				}
				cmds = append(cmds, c)
			}
		} else if c, _, err := rootCmd().Find(words); err != nil || !c.Runnable() {
			t.Errorf("%q: its words reach no command that runs (%v)", dl.line, err)
			continue
		} else {
			cmds = append(cmds, c)
		}
		if synopsis || elided {
			// Every flag the line shows exists on every command it names.
			for _, c := range cmds {
				for _, f := range designFlag.FindAllString(strings.Join(words, " "), -1) {
					if c.Flag(strings.TrimPrefix(f, "--")) == nil {
						t.Errorf("%q: %s has no %s", dl.line, c.CommandPath(), f)
					}
				}
				if strings.Contains(dl.line, "<limits>") {
					for _, f := range designLimitFlags {
						if c.Flag(f) == nil {
							t.Errorf("%q: %s has no --%s", dl.line, c.CommandPath(), f)
						}
					}
				}
			}
			continue
		}
		before := hits.Load()
		root := rootCmd()
		root.SetArgs(append(append([]string{}, words...), "--url", srv.URL, "--token", "t"))
		root.SetIn(strings.NewReader(""))
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		ran, err := root.ExecuteC()
		if len(cmds) == 1 && ran != nil && ran.CommandPath() != cmds[0].CommandPath() {
			t.Errorf("%q: ran %q, want %q", dl.line, ran.CommandPath(), cmds[0].CommandPath())
		}
		if dl.refusal != "" {
			if err == nil || !strings.HasPrefix(err.Error(), dl.refusal) {
				t.Errorf("%q: err = %v, want the command's own %q", dl.line, err, dl.refusal)
			}
			continue
		}
		var api *sdk.APIError
		if hits.Load() == before || (err != nil && !errors.As(err, &api)) {
			t.Errorf("%q: sent %d request(s), err = %v; want its handler reached and, at most, the server's answer", dl.line, hits.Load()-before, err)
		}
	}
}
