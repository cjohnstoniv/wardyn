// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// `wardyn runner remote propose` prints the server's boot proposal for what the runner reports, as the flags
// `wardyn runner set` takes: half of each as the total, Runs at once half its CPUs, per run the total
// divided by Runs at once, in whole GB. --free-disk stands in for the runner's report, and an amount
// nobody measured is refused, naming the flag: nothing is guessed.
func TestRunnerRemotePropose(t *testing.T) {
	amounts := func(cpu, memMiB, diskMiB int64) *sdk.RunnerAmounts {
		return &sdk.RunnerAmounts{CPUMillis: cpu, MemoryMiB: memMiB, DiskMiB: diskMiB}
	}
	for name, tc := range map[string]struct {
		reported *sdk.RunnerAmounts
		why      string
		freeDisk string
		want     string // the flags, or a part of the refusal
		refused  bool
	}{
		"a 32-CPU host whose disk the server cannot see, with the host's free disk given": {
			reported: amounts(32000, 36075, 0), freeDisk: "100",
			want: "--max-runs 16 --total-cpu 16 --total-memory 17 --total-disk 50 --per-run-cpu 1 --per-run-memory 1 --per-run-disk 3"},
		"a small host that reports everything": {
			reported: amounts(3000, 4096, 20480),
			want:     "--max-runs 1 --total-cpu 1.5 --total-memory 2 --total-disk 10 --per-run-cpu 1.5 --per-run-memory 2 --per-run-disk 10"},
		"--free-disk replaces what the runner reports": {
			reported: amounts(8000, 16384, 409600), freeDisk: "40",
			want: "--max-runs 4 --total-cpu 4 --total-memory 8 --total-disk 20 --per-run-cpu 1 --per-run-memory 2 --per-run-disk 5"},
		"a host past 128 CPUs: Runs at once is the remote-hosted ceiling's, not a self-hosted runner's 64": {
			reported: amounts(300000, 1048576, 2097152),
			want:     "--max-runs 150 --total-cpu 150 --total-memory 512 --total-disk 1024 --per-run-cpu 1 --per-run-memory 3 --per-run-disk 6"},
		"no free disk reported and none given":       {reported: amounts(32000, 36075, 0), want: "--free-disk", refused: true},
		"nothing reported at all":                    {want: "no CPU or memory", refused: true},
		"nothing reported, with the server's reason": {why: "no ResourceQuota in runs and k8s.readNodes is off", want: "(no ResourceQuota in runs and k8s.readNodes is off)", refused: true},
		"no memory reported":                         {reported: amounts(4000, 0, 0), freeDisk: "100", want: "no CPU or memory", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			view := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "Docker host", Revision: 1}, Usage: sdk.RunnerUsage{Reported: tc.reported, ReportedWhy: tc.why}}
			srv := newCmdServer(t, http.StatusOK, view)
			args := []string{"runner", "remote", "propose", "docker", "--url", srv.URL, "--token", "t"}
			if tc.freeDisk != "" {
				args = append(args, "--free-disk", tc.freeDisk)
			}
			out, err := execCmdCapture(t, args...)
			if r := srv.last(); r.method != http.MethodGet || r.path != "/api/v1/remote-runners/docker" || len(srv.reqs) != 1 {
				t.Errorf("propose sent %d requests, the last %s %s; want one read and nothing set", len(srv.reqs), r.method, r.path)
			}
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), tc.want) || out != ""):
				t.Errorf("propose = %q, %v; want it refused naming %q and nothing printed", out, err, tc.want)
			case !tc.refused && (err != nil || strings.TrimSpace(out) != tc.want):
				t.Errorf("propose = %q, %v\nwant     %q", strings.TrimSpace(out), err, tc.want)
			}
		})
	}
}

// What propose prints is what `wardyn runner set` takes: the flags are accepted for a runner with no
// limits yet, and send the proposed set.
func TestRunnerSetTakesTheProposedFlags(t *testing.T) {
	flags, err := proposedLimitFlags(&sdk.RunnerAmounts{CPUMillis: 32000, MemoryMiB: 36075}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	remote := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "Docker host", Revision: 1}}
	srv := newLimitsServer(t, sdk.RunnerView{}, remote, sdk.RunnerPool{})
	args := append([]string{"runner", "set", "docker", "--url", srv.URL, "--token", "t"}, strings.Fields(flags)...)
	if err := execCmd(t, args...); err != nil {
		t.Fatalf("runner set %s: %v", flags, err)
	}
	want := `{"revision":1,"limits":{"runs_at_once":16,"total":{"cpu_millis":16000,"memory_mib":17408,"disk_mib":51200},"per_run":{"cpu_millis":1000,"memory_mib":1024,"disk_mib":3072}}}`
	if got := srv.wrote()["PUT /api/v1/remote-runners/docker"]; got != want {
		t.Errorf("runner set sent %s\nwant            %s", got, want)
	}
}

// `wardyn runner remote list --no-limits` keeps only the runners that take no run until limits are set.
func TestRunnerRemoteListNoLimits(t *testing.T) {
	set := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "k8s", Name: "Cluster", Limits: &sdk.RunnerLimits{RunsAtOnce: 4}}}
	unset := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "Docker host"}}
	srv := newCmdServer(t, http.StatusOK, []sdk.RemoteRunnerView{set, unset})
	out, err := execCmdCapture(t, "runner", "remote", "list", "--no-limits", "--url", srv.URL, "--token", "t")
	if err != nil || !strings.Contains(out, "docker") || strings.Contains(out, "k8s") {
		t.Fatalf("list --no-limits = %v\n%s\nwant the Docker host alone", err, out)
	}
}

// `--ids` prints one id per line, whatever the names hold, and `--set-up-here` leaves out the rows that are
// another computer's: a script reads these instead of the table, whose columns a double space in a name shifts.
func TestRunnerRemoteListIDsAndSetUpHere(t *testing.T) {
	here := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "docker", Name: "say  no"}, Configured: true}
	away := sdk.RemoteRunnerView{RemoteRunner: sdk.RemoteRunner{ID: "k8s", Name: "Cluster"}}
	srv := newCmdServer(t, http.StatusOK, []sdk.RemoteRunnerView{here, away})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--ids"}, "docker\nk8s\n"},
		{[]string{"--ids", "--set-up-here"}, "docker\n"},
	} {
		out, err := execCmdCapture(t, append([]string{"runner", "remote", "list"}, append(tc.args, "--url", srv.URL, "--token", "t")...)...)
		if err != nil || out != tc.want {
			t.Errorf("list %v = %q, %v; want %q", tc.args, out, err, tc.want)
		}
	}
}

// --ids prints ids and --json prints JSON: asking for both is refused, not answered with one of them.
func TestRunnerRemoteListIDsWithJSONIsRefused(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []sdk.RemoteRunnerView{{RemoteRunner: sdk.RemoteRunner{ID: "docker"}}})
	if err := execCmd(t, "runner", "remote", "list", "--ids", "--json", "--url", srv.URL, "--token", "t"); err == nil {
		t.Error("--ids with --json was accepted")
	}
}

// `wardyn runner list --no-limits` keeps only the self-hosted runners that take no run until limits are
// set, and skips the remote-hosted section (`wardyn runner remote list --no-limits` is that list).
func TestRunnerListNoLimits(t *testing.T) {
	set := sdk.RunnerView{ID: uuid.New(), Name: "has-limits", State: "claimed", Limits: &sdk.RunnerLimits{RunsAtOnce: 4}}
	unset := sdk.RunnerView{ID: uuid.New(), Name: "no-limits-yet", State: "claimed"}
	srv := newCmdServer(t, http.StatusOK, []sdk.RunnerView{set, unset})
	for _, args := range [][]string{{"runner", "list", "--no-limits"}, {"runner", "list", "--all", "--no-limits"}} {
		out, err := execCmdCapture(t, append(args, "--url", srv.URL, "--token", "t")...)
		if err != nil || !strings.Contains(out, "no-limits-yet") || strings.Contains(out, "has-limits") {
			t.Fatalf("%v = %v\n%s\nwant the runner with no limits alone", args, err, out)
		}
	}
}

// A --free-disk of zero or less is refused before any request, not ignored.
func TestRunnerRemotePropose_FreeDiskMustBePositive(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		srv := newCmdServer(t, http.StatusOK, sdk.RemoteRunnerView{})
		out, err := execCmdCapture(t, "runner", "remote", "propose", "docker", "--free-disk", v, "--url", srv.URL, "--token", "t")
		if err == nil || !strings.Contains(err.Error(), "--free-disk must be a positive") || out != "" || len(srv.reqs) != 0 {
			t.Errorf("--free-disk %s = %q, %v, %d requests; want it refused before any request", v, out, err, len(srv.reqs))
		}
	}
}

// `runner remote people clear` says what it did, like its sibling verbs.
func TestRunnerRemotePeopleClear_SaysWhatItDid(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)
	out, err := execCmdCapture(t, "runner", "remote", "people", "clear", "docker", "--url", srv.URL, "--token", "t")
	if err != nil || !strings.Contains(out, "docker") || !strings.Contains(out, "no longer available to people's pools") {
		t.Errorf("people clear = %q, %v; want a line naming the runner and the change", out, err)
	}
}
