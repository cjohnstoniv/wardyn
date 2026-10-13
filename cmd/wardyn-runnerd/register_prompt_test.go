// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// asked runs the limit prompts against typed input on testHost, then the checks the flags have.
func asked(t *testing.T, input string, args ...string) (types.RunnerLimits, string, error) {
	t.Helper()
	var f registerLimits
	cmd := &cobra.Command{}
	f.register(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := f.ask(cmd, strings.NewReader(input), &out, testHost); err != nil {
		return types.RunnerLimits{}, out.String(), err
	}
	l, err := f.limits(cmd, testHost)
	return l, out.String(), err
}

// Enter takes each suggestion shown, which is the proposal; a typed value replaces its own, and the
// per-run suggestions follow the totals and Runs at once as answered. The mock's example numbers (Runs at
// once 2, 2 CPUs per run) are what types.SuggestedRunnerMaxRuns does not give for 8 CPUs: it gives 4.
func TestRegisterAsksLimitsInTheMocksWords(t *testing.T) {
	l, out, err := asked(t, strings.Repeat("\n", 7))
	if want := types.ProposeRunnerLimits(testHost, 4); err != nil || l != want {
		t.Fatalf("Enter through = %+v, %v; want the proposal %+v", l, err, want)
	}
	for _, want := range []string{
		"limits for this runner (press Enter to accept the suggestion, or type a lower value)",
		"  total across all its runs: half of what this computer reports",
		"    CPU [4]: ", "    memory GB [8]: ", "    disk GB [250]: ", "  runs at once [4]: ",
		"  per run, the most one run may use: total / runs at once, at least 1 CPU and 1 GB",
		"    CPU [1]: ", "    memory GB [2]: ", "    disk GB [62]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompts lack %q:\n%s", want, out)
		}
	}

	l, out, err = asked(t, "6\n\n\n2\n\n3\n\n")
	if err != nil || l.RunsAtOnce != 2 || l.Total.CPUMillis != 6000 || l.Total.MemoryMiB != 8192 || l.PerRun.CPUMillis != 3000 || l.PerRun.MemoryMiB != 3072 || l.PerRun.DiskMiB != 125*1024 {
		t.Fatalf("typed values = %+v, %v", l, err)
	}
	if !strings.Contains(out, "CPU [3]: ") || !strings.Contains(out, "memory GB [4]: ") {
		t.Errorf("per-run suggestions did not follow the answers:\n%s", out)
	}
}

func TestRegisterAskRefusesWithTheFlagsSentences(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"\n64\n", "--total-memory 64 is more than this computer reports (16 GB)"},
		{"\n\n\n0\n", "--max-runs is a whole number, 1 or more"},
		{"\n\n\n65\n", "runs at once is a whole number from 1 to 64"},
		{"x\n", "--total-cpu is a number more than 0"},
		{"\n8.5\n", "--total-memory is a whole number of GB, 1 or more"},
		{"\n\n\n\n\n20\n\n", "a per-run limit cannot be more than the runner's total"},
		{"", "the limits were not answered"},
	} {
		if _, _, err := asked(t, tc.input); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("input %q = %v, want %q", tc.input, err, tc.want)
		}
	}
}

// A limit given as a flag is not asked, and when every one is given nothing is printed.
func TestRegisterAskSkipsLimitsGivenAsFlags(t *testing.T) {
	l, out, err := asked(t, "", "--max-runs", "2", "--total-cpu", "4", "--total-memory", "8", "--total-disk", "250",
		"--per-run-cpu", "2", "--per-run-memory", "4", "--per-run-disk", "125")
	if err != nil || out != "" || l.RunsAtOnce != 2 || l.PerRun.DiskMiB != 125*1024 {
		t.Fatalf("every flag given = %+v, %v, printed %q", l, err, out)
	}
	_, out, err = asked(t, "\n\n\n\n", "--max-runs", "2", "--total-memory", "12", "--per-run-cpu", "2", "--per-run-disk", "125")
	if err != nil {
		t.Fatal(err)
	}
	for _, not := range []string{"memory GB [8]", "runs at once [", "CPU [2]", "disk GB [125]"} {
		if strings.Contains(out, not) {
			t.Errorf("a given flag was asked (%q):\n%s", not, out)
		}
	}
	if !strings.Contains(out, "CPU [4]: ") || !strings.Contains(out, "memory GB [6]: ") {
		t.Errorf("suggestions ignored the given flags:\n%s", out)
	}
}

// Where stdin is not a terminal nothing is asked: register runs on the proposal and flags alone.
func TestRegisterAsksNothingWithoutATerminal(t *testing.T) {
	sent, _, out, err := registerWith(t)
	if err != nil || sent.Limits == nil || strings.Contains(out, "limits for this runner") {
		t.Fatalf("no terminal: %+v, %v\n%s", sent.Limits, err, out)
	}
}

// register asks only where both its input and its output are a terminal. With the output sent to a file
// the questions would land in the file while the command waited unseen, so nothing is asked then.
func TestRegisterAsksOnlyWhenTheAnswersAndTheQuestionsAreBothAtATerminal(t *testing.T) {
	// A pseudo-terminal's controlling end is a terminal to the same check a real one passes.
	tty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal on this host: %v", err)
	}
	defer tty.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, tc := range []struct {
		name string
		in   io.Reader
		out  io.Writer
		want bool
	}{
		{"both at a terminal", tty, tty, true},
		{"output sent to a file", tty, file, false},
		{"output captured", tty, &strings.Builder{}, false},
		{"input from a file", file, tty, false},
		{"input piped", strings.NewReader(""), tty, false},
		{"no terminal", strings.NewReader(""), &strings.Builder{}, false},
	} {
		if got := asksAt(tc.in, tc.out); got != tc.want {
			t.Errorf("%s: asks = %v, want %v", tc.name, got, tc.want)
		}
	}
}
