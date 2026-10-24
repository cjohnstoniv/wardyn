// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	notRunMarker = "NOT_RUN: "
	blockMarker  = "BLOCK_ITEM"
)

// pRows are the proof rows the window reports, one top-level test each. The summary has exactly one
// row for each; TestSummaryRowsMatchTests pins this list to the TestP functions in the package.
var pRows = []string{
	"TestP04_WarmStartAndIdentity", "TestP05_AgentSession", "TestP06_NetworkPositive", "TestP07_NetworkNegative",
	"TestP08_Throughput", "TestP09_Desktop", "TestP09b_LocalWindow", "TestP10_CleanupBoundary", "TestP11_CAAndTools",
	"TestP12_CapSlotsAndLogins", "TestP13_RunnerRestart", "TestP14_PkgUpgrade", "TestP15_HostSleep",
	"TestP16_WedgedGuest", "TestP17_DiskBound", "TestP18_ThermalAndPower", "TestP19_TeardownHygiene",
	"TestP20_ManagedGate", "TestP21_GuestClock", "TestP22_DistinctIdentities",
}

// blockItems are the tests whose failure is BLOCK: macOS VMs do not ship until it passes. A listed
// test covers its subtests. Rows not listed fail as FAIL (a limit or a decision), never BLOCK.
var blockItems = map[string]bool{
	"TestP05_AgentSession":                      true,
	"TestP06_NetworkPositive":                   true,
	"TestP07_NetworkNegative":                   true,
	"TestP09_Desktop":                           true,
	"TestP09b_LocalWindow/controls":             true,
	"TestP09b_LocalWindow/bound_and_rechecks":   true,
	"TestP10_CleanupBoundary/boundary":          true,
	"TestP12_CapSlotsAndLogins":                 true,
	"TestP16_WedgedGuest":                       true,
	"TestP17_DiskBound/a_floor":                 true,
	"TestP17_DiskBound/b_second_refused":        true,
	"TestP17_DiskBound/d_snapshot_versions":     true,
	"TestP19_TeardownHygiene":                   true,
	"TestP20_ManagedGate":                       true,
	"TestP22_DistinctIdentities/two_concurrent": true,
}

func isBlock(name string) bool {
	for n := name; ; {
		if blockItems[n] {
			return true
		}
		i := strings.LastIndexByte(n, '/')
		if i < 0 {
			return false
		}
		n = n[:i]
	}
}

func hasBlock(row string) bool {
	for k := range blockItems {
		if k == row || strings.HasPrefix(k, row+"/") {
			return true
		}
	}
	return false
}

// TestMain runs the summary instead of the tests when WARDYN_MACVM_SUMMARIZE names a run.sh output
// dir, so the window needs nothing beyond the suite binary and Go's test2json.
func TestMain(m *testing.M) {
	if dir := os.Getenv("WARDYN_MACVM_SUMMARIZE"); dir != "" {
		os.Exit(runSummary(dir, os.Stdout))
	}
	os.Exit(m.Run())
}

type event struct {
	Action, Test, Output string
}

// detail is one test or subtest: PASS, FAIL, SKIP or NOT_RUN.
type detail struct {
	Test, Status string
	Block        bool
	Reason       string
}

// rowResult is one P-row: PASS, FAIL (a non-BLOCK failure), BLOCK (a BLOCK item failed) or NOT_RUN
// (anything skipped or not run inside it, or no output at all).
type rowResult struct {
	Row, Outcome, Reason string
	BlockNotRun          bool
}

func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// summarizeRow reads <dir>/<row>.json (test2json) and <dir>/<row>.exit.
func summarizeRow(dir, row string) (rowResult, []detail) {
	f, err := os.Open(filepath.Join(dir, row+".json"))
	if err != nil {
		return rowResult{Row: row, Outcome: "NOT_RUN", Reason: "no output: not run", BlockNotRun: hasBlock(row)}, nil
	}
	defer f.Close()
	final := map[string]string{}
	out := map[string][]string{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Test != row && !strings.HasPrefix(e.Test, row+"/") {
			continue
		}
		if _, ok := out[e.Test]; !ok {
			order = append(order, e.Test)
			out[e.Test] = nil
		}
		switch e.Action {
		case "output":
			if l := strings.TrimSpace(e.Output); l != "" && !strings.HasPrefix(l, "=== ") && !strings.HasPrefix(l, "--- ") {
				out[e.Test] = append(out[e.Test], l)
			}
		case "pass", "fail", "skip":
			final[e.Test] = e.Action
		}
	}
	var ds []detail
	for _, name := range order {
		d := detail{Test: name, Block: isBlock(name)}
		lines := out[name]
		switch final[name] {
		case "pass":
			d.Status = "PASS"
			for _, l := range lines {
				if strings.Contains(l, "EVIDENCE:") {
					d.Reason = clean(d.Reason + " " + l)
				}
			}
		case "fail":
			d.Status = "FAIL"
			d.Reason = clean(strings.Join(lines, " | "))
		case "skip":
			d.Status = "SKIP"
			for _, l := range lines {
				if i := strings.Index(l, notRunMarker); i >= 0 {
					d.Status, d.Reason = "NOT_RUN", clean(l[i+len(notRunMarker):])
				}
			}
			if d.Status == "SKIP" && len(lines) > 0 {
				d.Reason = clean(lines[len(lines)-1])
			}
		default:
			d.Status, d.Reason = "NOT_RUN", "no result: the test did not finish"
		}
		ds = append(ds, d)
	}
	code := 0
	if b, err := os.ReadFile(filepath.Join(dir, row+".exit")); err == nil {
		code, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	r := rowResult{Row: row}
	var block, fail, skipped []string
	for _, d := range ds {
		switch d.Status {
		case "FAIL":
			child := false
			for _, o := range ds {
				child = child || o.Status == "FAIL" && strings.HasPrefix(o.Test, d.Test+"/")
			}
			if child {
				continue // a parent's failure is its failed children's
			}
			if d.Block {
				block = append(block, d.Test+": "+d.Reason)
			} else {
				fail = append(fail, d.Test+": "+d.Reason)
			}
		case "SKIP", "NOT_RUN":
			skipped = append(skipped, d.Test+": "+d.Reason)
		}
	}
	_, topDone := final[row]
	switch {
	case len(block) > 0:
		r.Outcome, r.Reason = "BLOCK", clean(strings.Join(block, " || "))
	case len(fail) > 0:
		r.Outcome, r.Reason = "FAIL", clean(strings.Join(fail, " || "))
	case code != 0:
		r.Outcome, r.Reason = "FAIL", fmt.Sprintf("exit %d with no failing test", code)
	case !topDone:
		r.Outcome, r.Reason = "NOT_RUN", "no result for the row"
	case len(skipped) > 0:
		r.Outcome, r.Reason = "NOT_RUN", clean(strings.Join(skipped, " || "))
	default:
		r.Outcome = "PASS"
	}
	r.BlockNotRun = r.Outcome == "NOT_RUN" && hasBlock(row)
	return r, ds
}

// runSummary writes summary.tsv (one row per P-row) and detail.tsv (every test and subtest) into
// dir and prints the counts. Its status: 2 when any row is BLOCK, 1 when any is FAIL, else 0.
func runSummary(dir string, w io.Writer) int {
	var sum, det strings.Builder
	sum.WriteString("row\toutcome\treason\n")
	det.WriteString("test\tstatus\tblock\treason\n")
	n := map[string]int{}
	blockNotRun := 0
	for _, row := range pRows {
		r, ds := summarizeRow(dir, row)
		n[r.Outcome]++
		if r.BlockNotRun {
			blockNotRun++
		}
		fmt.Fprintf(&sum, "%s\t%s\t%s\n", r.Row, r.Outcome, r.Reason)
		for _, d := range ds {
			fmt.Fprintf(&det, "%s\t%s\t%v\t%s\n", d.Test, d.Status, d.Block, d.Reason)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.tsv"), []byte(sum.String()), 0o600); err != nil {
		fmt.Fprintln(w, "summary:", err)
		return 3
	}
	if err := os.WriteFile(filepath.Join(dir, "detail.tsv"), []byte(det.String()), 0o600); err != nil {
		fmt.Fprintln(w, "summary:", err)
		return 3
	}
	fmt.Fprint(w, sum.String())
	fmt.Fprintf(w, "PASS %d FAIL %d BLOCK %d NOT_RUN %d of %d P-rows\n", n["PASS"], n["FAIL"], n["BLOCK"], n["NOT_RUN"], len(pRows))
	if n["PASS"]+n["FAIL"]+n["BLOCK"]+n["NOT_RUN"] != len(pRows) {
		fmt.Fprintln(w, "summary: the counts do not add up to the P-rows")
		return 3
	}
	if blockNotRun > 0 {
		fmt.Fprintf(w, "%d BLOCK NOT RUN\n", blockNotRun)
	}
	switch {
	case n["BLOCK"] > 0:
		return 2
	case n["FAIL"] > 0:
		return 1
	}
	return 0
}

// ---- self-tests: these run everywhere, with no Mac ----

// TestSummaryRowsMatchTests pins pRows to the package's TestP functions: 20 rows, no more, no less.
func TestSummaryRowsMatchTests(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	re := regexp.MustCompile(`(?m)^func (TestP\w+)\(t \*testing\.T\)`)
	var got []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			got = append(got, m[1])
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(pRows))
	if !slices.Equal(got, want) || len(pRows) != 20 {
		t.Fatalf("TestP functions %v\npRows %v (want the same 20)", got, want)
	}
	for k := range blockItems {
		row, _, _ := strings.Cut(k, "/")
		if !slices.Contains(pRows, row) {
			t.Errorf("blockItems names %s, which is in no P-row", k)
		}
	}
}

func writeEvents(t *testing.T, dir, row string, exit int, evs ...event) {
	t.Helper()
	var b strings.Builder
	for _, e := range evs {
		j, _ := json.Marshal(e)
		b.Write(j)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, row+".json"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, row+".exit"), []byte(strconv.Itoa(exit)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTSV(t *testing.T, path string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n")[1:] {
		f := strings.Split(l, "\t")
		rows[f[0]] = f[1:]
	}
	return rows
}

// TestSummaryMapping feeds fake test2json events and pins each mapping, and that PASS + FAIL +
// BLOCK + NOT_RUN is the 20 P-rows.
func TestSummaryMapping(t *testing.T) {
	dir := t.TempDir()
	o := func(test, s string) event { return event{Action: "output", Test: test, Output: s + "\n"} }
	a := func(test, act string) event { return event{Action: act, Test: test} }
	p4, p5, p6, p8 := "TestP04_WarmStartAndIdentity", "TestP05_AgentSession", "TestP06_NetworkPositive", "TestP08_Throughput"
	p13, p17, p18, p19 := "TestP13_RunnerRestart", "TestP17_DiskBound", "TestP18_ThermalAndPower", "TestP19_TeardownHygiene"
	p21, p22 := "TestP21_GuestClock", "TestP22_DistinctIdentities"
	writeEvents(t, dir, p4, 0, a(p4, "run"), a(p4, "pass"))
	writeEvents(t, dir, p5, 1, a(p5, "run"), a(p5+"/exec", "run"), o(p5+"/exec", "    x.go:1: exec status 1"), a(p5+"/exec", "fail"), a(p5, "fail"))
	writeEvents(t, dir, p6, 0, a(p6, "run"), a(p6+"/git_clone", "run"), o(p6+"/git_clone", "    x.go:1: "+notRunMarker+"no git URL"), a(p6+"/git_clone", "skip"), a(p6, "pass"))
	writeEvents(t, dir, p8, 1, a(p8, "run"), o(p8, "    x.go:1: LIMIT: slow"), a(p8, "fail"))
	writeEvents(t, dir, p13, 1, a(p13, "run"), a(p13, "pass"))
	writeEvents(t, dir, p17, 1, a(p17, "run"), a(p17+"/a_floor", "run"), a(p17+"/a_floor", "pass"),
		a(p17+"/c_newest_ends", "run"), o(p17+"/c_newest_ends", "    x.go:1: LIMIT (c)"), a(p17+"/c_newest_ends", "fail"), a(p17, "fail"))
	writeEvents(t, dir, p19, 0, a(p19, "run"), o(p19, "    x.go:1: WARDYN_MACVM_LIVE is not 1"), a(p19, "skip"))
	writeEvents(t, dir, p18, 0, a(p18, "run"), o(p18, "    x.go:1: sampling")) // no result, exit 0: not run
	writeEvents(t, dir, p21, 1, a(p21, "run"), o(p21, "    x.go:1: sampling")) // no result, exit 1: a crash
	writeEvents(t, dir, p22, 1, a(p22, "run"), a(p22+"/fresh", "run"), a(p22+"/fresh", "pass"),
		a(p22+"/two_concurrent", "run"), o(p22+"/two_concurrent", "    x.go:1: neither qualifies"), a(p22+"/two_concurrent", "fail"), a(p22, "fail"))

	var out strings.Builder
	if code := runSummary(dir, &out); code != 2 {
		t.Fatalf("status %d, want 2 (a BLOCK row)\n%s", code, out.String())
	}
	rows := readTSV(t, filepath.Join(dir, "summary.tsv"))
	if len(rows) != len(pRows) {
		t.Fatalf("%d summary rows, want %d", len(rows), len(pRows))
	}
	want := map[string]string{p4: "PASS", p5: "BLOCK", p6: "NOT_RUN", p8: "FAIL", p13: "FAIL", p17: "FAIL", p18: "NOT_RUN", p19: "NOT_RUN", p21: "FAIL", p22: "BLOCK"}
	n := map[string]int{}
	for _, row := range pRows {
		got := rows[row][0]
		n[got]++
		if w, ok := want[row]; ok && got != w {
			t.Errorf("%s: %s, want %s (%s)", row, got, w, rows[row][1])
		} else if !ok && got != "NOT_RUN" {
			t.Errorf("%s with no output: %s, want NOT_RUN", row, got)
		}
	}
	if n["PASS"] != 1 || n["FAIL"] != 4 || n["BLOCK"] != 2 || n["NOT_RUN"] != 13 || n["PASS"]+n["FAIL"]+n["BLOCK"]+n["NOT_RUN"] != 20 {
		t.Errorf("counts %v, want PASS 1 FAIL 4 BLOCK 2 NOT_RUN 13 = 20", n)
	}
	if !strings.Contains(rows[p6][1], "no git URL") || !strings.Contains(out.String(), "BLOCK NOT RUN") {
		t.Errorf("the NOT_RUN reason or the BLOCK NOT RUN line is missing:\n%s", out.String())
	}
	det := readTSV(t, filepath.Join(dir, "detail.tsv"))
	if d := det[p6+"/git_clone"]; len(d) < 1 || d[0] != "NOT_RUN" {
		t.Errorf("detail of %s/git_clone = %v, want NOT_RUN", p6, d)
	}
	if d := det[p19]; len(d) < 1 || d[0] != "SKIP" {
		t.Errorf("detail of %s = %v, want SKIP", p19, d)
	}
}

// TestRunSh runs run.sh against a fake suite binary whose summary step is this test binary: the
// guards, P19 last, and the summary and exit status it ends with.
func TestRunSh(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH for test2json")
	}
	real, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake.test")
	script := `#!/bin/sh
if [ -n "$WARDYN_MACVM_SUMMARIZE" ]; then exec "$REAL" -test.run '^$'; fi
while [ $# -gt 0 ]; do
  case "$1" in
    -test.list) [ "$2" = "^TestP" ] && printf 'TestP19_TeardownHygiene\nTestP04_WarmStartAndIdentity\nTestP08_Throughput\n'; exit 0 ;;
    -test.run) name=$(printf %s "$2" | tr -d '^$'); shift ;;
  esac
  shift
done
echo "$name" >> "$ORDER"
echo "=== RUN   $name"
if [ "$name" = TestP08_Throughput ]; then
  echo "    x.go:1: LIMIT: slow"; echo "--- FAIL: $name (0.00s)"; echo FAIL; exit 1
fi
echo "--- PASS: $name (0.00s)"; echo PASS
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runSh := func(live string, args ...string) (int, string) {
		c := exec.Command("bash", append([]string{"run.sh", fake}, args...)...)
		c.Env = append(os.Environ(), "REAL="+real, "ORDER="+filepath.Join(dir, "order"), "WARDYN_MACVM_LIVE="+live, "WARDYN_MACVM_SUMMARIZE=")
		b, _ := c.CombinedOutput()
		return c.ProcessState.ExitCode(), string(b)
	}
	if code, out := runSh("", filepath.Join(dir, "a")); code != 2 || !strings.Contains(out, "WARDYN_MACVM_LIVE is not 1") {
		t.Errorf("run.sh without WARDYN_MACVM_LIVE: exit %d, want 2 with the reason:\n%s", code, out)
	}
	if code, out := runSh("1", filepath.Join(dir, "b"), "^NoSuch"); code != 2 || !strings.Contains(out, "no test matches") {
		t.Errorf("run.sh with no tests listed: exit %d, want 2 with the reason:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "order")); err == nil {
		t.Fatal("a guarded run.sh still ran tests")
	}
	code, out := runSh("1", filepath.Join(dir, "c"))
	if code != 1 {
		t.Fatalf("run.sh exited %d, want 1 (one FAIL, no BLOCK):\n%s", code, out)
	}
	order, _ := os.ReadFile(filepath.Join(dir, "order"))
	if lines := strings.Fields(string(order)); len(lines) != 3 || lines[2] != "TestP19_TeardownHygiene" {
		t.Errorf("run order %v, want P19 last", lines)
	}
	rows := readTSV(t, filepath.Join(dir, "c", "summary.tsv"))
	if len(rows) != 20 || rows["TestP04_WarmStartAndIdentity"][0] != "PASS" || rows["TestP08_Throughput"][0] != "FAIL" ||
		rows["TestP19_TeardownHygiene"][0] != "PASS" || rows["TestP05_AgentSession"][0] != "NOT_RUN" {
		t.Errorf("summary rows %v", rows)
	}
	if !strings.Contains(out, "PASS 2 FAIL 1 BLOCK 0 NOT_RUN 17 of 20 P-rows") {
		t.Errorf("run.sh output lacks the counts:\n%s", out)
	}
}
