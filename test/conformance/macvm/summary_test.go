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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

const (
	notRunMarker   = "NOT_RUN: "
	untestedMarker = "UNTESTED: "
	blockMarker    = "BLOCK_ITEM"
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

// detail is one test or subtest: PASS, FAIL, SKIP, NOT_RUN or UNTESTED (listed untested: a
// sub-property the window cannot observe, which never decides its row).
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
				if i := strings.Index(l, untestedMarker); i >= 0 {
					d.Status, d.Reason = "UNTESTED", clean(l[i+len(untestedMarker):])
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
	if len(ds) == 0 {
		// A binary that died before its first test is a failure, never a row that did not run.
		r.Outcome, r.Reason = "NOT_RUN", "no events for the row (exit 0)"
		if code != 0 {
			r.Outcome, r.Reason = "FAIL", fmt.Sprintf("exit %d with no failing test (no events for the row)", code)
		}
		r.BlockNotRun = r.Outcome == "NOT_RUN" && hasBlock(row)
		return r, nil
	}
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
		var ev []string
		for _, d := range ds {
			if d.Status == "PASS" && d.Reason != "" {
				ev = append(ev, d.Reason)
			}
		}
		r.Reason = clean(strings.Join(ev, " "))
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
	var untested []string
	for _, row := range pRows {
		r, ds := summarizeRow(dir, row)
		for _, d := range ds {
			if d.Status == "UNTESTED" {
				untested = append(untested, fmt.Sprintf("UNTESTED %s: %s", d.Test, d.Reason))
			}
		}
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
	if len(untested) > 0 {
		fmt.Fprintf(w, "Listed untested (never a pass, not an outcome):\n%s\n", strings.Join(untested, "\n"))
	}
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

// declaredRows is the TestP functions the package declares.
func declaredRows(t *testing.T) []string {
	t.Helper()
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
	return got
}

// TestSummaryRowsMatchTests pins pRows to the package's TestP functions: 20 rows, no more, no less.
func TestSummaryRowsMatchTests(t *testing.T) {
	got := declaredRows(t)
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

// TestSummaryMapping feeds fake test2json events and pins each mapping: every row's outcome and
// that its reason comes from its own events only, one summary row per declared TestP function,
// and the listed-untested section.
func TestSummaryMapping(t *testing.T) {
	dir := t.TempDir()
	o := func(test, s string) event { return event{Action: "output", Test: test, Output: s + "\n"} }
	a := func(test, act string) event { return event{Action: act, Test: test} }
	p4, p5, p6, p7, p8 := "TestP04_WarmStartAndIdentity", "TestP05_AgentSession", "TestP06_NetworkPositive", "TestP07_NetworkNegative", "TestP08_Throughput"
	p9, p10, p12, p13, p17 := "TestP09_Desktop", "TestP10_CleanupBoundary", "TestP12_CapSlotsAndLogins", "TestP13_RunnerRestart", "TestP17_DiskBound"
	p18, p19, p21, p22 := "TestP18_ThermalAndPower", "TestP19_TeardownHygiene", "TestP21_GuestClock", "TestP22_DistinctIdentities"
	p11, p14 := "TestP11_CAAndTools", "TestP14_PkgUpgrade"
	failUntested := p14 + "/protocol_mismatch_and_tools_update"
	nic, stop := p7+"/one_filehandle_nic", p12+"/phase3_stop_loops/stop_from_guest"
	// p08's failure is also written into p04's and p10's files: a row reads its own events only.
	p8fail := []event{a(p8, "run"), o(p8, "    x.go:1: LIMIT: tok-p08"), a(p8, "fail")}
	writeEvents(t, dir, p4, 0, append([]event{a(p4, "run"), o(p4, "    x.go:1: EVIDENCE: tok-p04"), a(p4, "pass")}, p8fail...)...)
	writeEvents(t, dir, p5, 1, a(p5, "run"), a(p5+"/exec", "run"), o(p5+"/exec", "    x.go:1: tok-p05"), a(p5+"/exec", "fail"), a(p5, "fail"))
	writeEvents(t, dir, p6, 0, a(p6, "run"), a(p6+"/git_clone", "run"), o(p6+"/git_clone", "    x.go:1: "+notRunMarker+"tok-p06"), a(p6+"/git_clone", "skip"), a(p6, "pass"))
	writeEvents(t, dir, p7, 0, a(p7, "run"), a(p7+"/lan_tcp", "run"), a(p7+"/lan_tcp", "pass"),
		a(nic, "run"), o(nic, "    x.go:1: "+untestedMarker+"tok-p07u"), a(nic, "skip"), a(p7, "pass"))
	writeEvents(t, dir, p8, 1, p8fail...)
	writeEvents(t, dir, p9, 0)             // an empty file: no events
	writeEvents(t, dir, p10, 0, p8fail...) // another row's events only
	writeEvents(t, dir, p11, 2)            // the binary died before its first test
	writeEvents(t, dir, p14, 1, a(p14, "run"), a(failUntested, "run"), o(failUntested, "    x.go:1: "+untestedMarker+"tok-p14u"),
		o(failUntested, "    x.go:2: tok-p14"), a(failUntested, "fail"), a(p14, "fail")) // a FAIL with an UNTESTED line is FAIL
	writeEvents(t, dir, p12, 0, a(p12, "run"), a(p12+"/phase1_cap", "run"), o(p12+"/phase1_cap", "    x.go:1: "+notRunMarker+"tok-p12a"), a(p12+"/phase1_cap", "skip"),
		a(stop, "run"), o(stop, "    x.go:1: "+untestedMarker+"tok-p12u"), a(stop, "skip"), a(p12, "pass"))
	writeEvents(t, dir, p13, 1, a(p13, "run"), a(p13, "pass"))
	writeEvents(t, dir, p17, 1, a(p17, "run"), a(p17+"/a_floor", "run"), a(p17+"/a_floor", "pass"),
		a(p17+"/c_newest_ends", "run"), o(p17+"/c_newest_ends", "    x.go:1: LIMIT tok-p17"), a(p17+"/c_newest_ends", "fail"), a(p17, "fail"))
	writeEvents(t, dir, p18, 0, a(p18, "run"), o(p18, "    x.go:1: sampling")) // no result, exit 0: not run
	writeEvents(t, dir, p19, 0, a(p19, "run"), o(p19, "    x.go:1: tok-p19"), a(p19, "skip"))
	writeEvents(t, dir, p21, 1, a(p21, "run"), o(p21, "    x.go:1: sampling")) // no result, exit 1: a crash
	writeEvents(t, dir, p22, 1, a(p22, "run"), a(p22+"/fresh", "run"), a(p22+"/fresh", "pass"),
		a(p22+"/two_concurrent", "run"), o(p22+"/two_concurrent", "    x.go:1: tok-p22"), a(p22+"/two_concurrent", "fail"), a(p22, "fail"))

	var out strings.Builder
	if code := runSummary(dir, &out); code != 2 {
		t.Fatalf("status %d, want 2 (a BLOCK row)\n%s", code, out.String())
	}
	declared := declaredRows(t)
	rows := readTSV(t, filepath.Join(dir, "summary.tsv"))
	if len(rows) != len(declared) {
		t.Fatalf("%d summary rows, want one per declared TestP function (%d)", len(rows), len(declared))
	}
	want := map[string]string{p4: "PASS", p5: "BLOCK", p6: "NOT_RUN", p7: "PASS", p8: "FAIL", p9: "NOT_RUN", p10: "NOT_RUN",
		p11: "FAIL", p12: "NOT_RUN", p13: "FAIL", p14: "FAIL", p17: "FAIL", p18: "NOT_RUN", p19: "NOT_RUN", p21: "FAIL", p22: "BLOCK"}
	reason := map[string]string{p4: "tok-p04", p5: "tok-p05", p6: "tok-p06", p8: "tok-p08", p9: "no events", p10: "no events",
		p11: "exit 2 with no failing test", p12: "tok-p12a", p13: "exit 1", p14: "tok-p14", p17: "tok-p17", p18: "no result", p19: "tok-p19", p21: "exit 1", p22: "tok-p22"}
	toks := regexp.MustCompile(`tok-p\d+[a-z]?`)
	n := map[string]int{}
	for _, row := range declared {
		got, why := rows[row][0], rows[row][1]
		n[got]++
		if w, ok := want[row]; ok && got != w {
			t.Errorf("%s: %s, want %s (%s)", row, got, w, why)
		} else if !ok && (got != "NOT_RUN" || why != "no output: not run") {
			t.Errorf("%s with no file: %s %q, want NOT_RUN for no output", row, got, why)
		}
		if r, ok := reason[row]; ok && !strings.Contains(why, r) {
			t.Errorf("%s: reason %q lacks %q, from its own events", row, why, r)
		}
		for _, tok := range toks.FindAllString(why, -1) {
			if tok != reason[row] && !(row == p14 && tok == "tok-p14u") {
				t.Errorf("%s: reason %q carries %s, which is not from its own events", row, why, tok)
			}
		}
	}
	if n["PASS"] != 2 || n["FAIL"] != 6 || n["BLOCK"] != 2 || n["NOT_RUN"] != len(declared)-10 {
		t.Errorf("counts %v, want PASS 2 FAIL 6 BLOCK 2 NOT_RUN %d", n, len(declared)-10)
	}
	if !strings.Contains(out.String(), "BLOCK NOT RUN") {
		t.Errorf("the BLOCK NOT RUN line is missing:\n%s", out.String())
	}
	// The listed-untested section: after the rows, exactly the two items, never in summary.tsv.
	sec := "Listed untested (never a pass, not an outcome):\nUNTESTED " + nic + ": tok-p07u\nUNTESTED " + stop + ": tok-p12u\nPASS "
	if i, j := strings.Index(out.String(), sec), strings.Index(out.String(), p22+"\t"); i < 0 || i < j {
		t.Errorf("the listed-untested section is missing, wrong or before the rows:\n%s", out.String())
	}
	det := readTSV(t, filepath.Join(dir, "detail.tsv"))
	for test, st := range map[string]string{p6 + "/git_clone": "NOT_RUN", p19: "SKIP", nic: "UNTESTED", stop: "UNTESTED", failUntested: "FAIL"} {
		if d := det[test]; len(d) < 1 || d[0] != st {
			t.Errorf("detail of %s = %v, want %s", test, d, st)
		}
	}
	// A binary that dies before its first test changes the status on its own.
	died := t.TempDir()
	writeEvents(t, died, p4, 2)
	var o2 strings.Builder
	if code := runSummary(died, &o2); code != 1 {
		t.Errorf("a row whose binary died before its first test: status %d, want 1 (FAIL)\n%s", code, o2.String())
	}
}

// TestRunSh runs run.sh against a fake suite binary whose summary step is this test binary: the
// guards, P19 last, each test's own exit status, and the summary and exit status it ends with.
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
    -test.list) [ "$2" = "^TestP" ] && printf 'TestP19_TeardownHygiene\nTestP04_WarmStartAndIdentity\nTestP08_Throughput\nTestP13_RunnerRestart\n'; exit 0 ;;
    -test.run) name=$(printf %s "$2" | tr -d '^$'); shift ;;
  esac
  shift
done
echo "$name" >> "$ORDER"
echo "=== RUN   $name"
case "$name" in
  TestP08_Throughput) echo "    x.go:1: LIMIT: slow"; echo "--- FAIL: $name (0.00s)"; echo FAIL; exit 1 ;;
  TestP13_RunnerRestart) echo "--- PASS: $name (0.00s)"; echo PASS; exit 3 ;;
esac
echo "--- PASS: $name (0.00s)"; echo PASS
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runSh := func(live string, args ...string) (int, string) {
		c := exec.Command("bash", append([]string{"run.sh", fake}, args...)...)
		// An inherited WARDYN_MACVM_SUMMARIZE must not turn the test runs into summaries.
		c.Env = append(os.Environ(), "REAL="+real, "ORDER="+filepath.Join(dir, "order"), "WARDYN_MACVM_LIVE="+live,
			"WARDYN_MACVM_SUMMARIZE="+filepath.Join(dir, "inherited"))
		b, _ := c.CombinedOutput()
		return c.ProcessState.ExitCode(), string(b)
	}
	if code, out := runSh("", filepath.Join(dir, "a")); code != 2 || !strings.Contains(out, "WARDYN_MACVM_LIVE is not 1") {
		t.Errorf("run.sh without WARDYN_MACVM_LIVE: exit %d, want 2 with the reason:\n%s", code, out)
	}
	if code, out := runSh("1", filepath.Join(dir, "b"), "^NoSuch"); code != 2 || !strings.Contains(out, "no test matches") {
		t.Errorf("run.sh with no tests listed: exit %d, want 2 with the reason:\n%s", code, out)
	}
	used := filepath.Join(dir, "used")
	if err := os.MkdirAll(used, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(used, "TestP04_WarmStartAndIdentity.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runSh("1", used); code != 2 || !strings.Contains(out, "is not empty") {
		t.Errorf("run.sh into a used out dir: exit %d, want 2 with the reason:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "order")); err == nil {
		t.Fatal("a guarded run.sh still ran tests")
	}
	c := filepath.Join(dir, "c")
	code, out := runSh("1", c)
	if code != 1 {
		t.Fatalf("run.sh exited %d, want 1 (FAILs, no BLOCK):\n%s", code, out)
	}
	order, _ := os.ReadFile(filepath.Join(dir, "order"))
	if lines := strings.Fields(string(order)); len(lines) != 4 || lines[3] != "TestP19_TeardownHygiene" {
		t.Errorf("run order %v, want the 4 listed with P19 last", lines)
	}
	if b, _ := os.ReadFile(filepath.Join(c, "TestP13_RunnerRestart.exit")); strings.TrimSpace(string(b)) != "3" {
		t.Errorf("TestP13's .exit is %q, want its own status 3", b)
	}
	rows := readTSV(t, filepath.Join(c, "summary.tsv"))
	if len(rows) != 20 || rows["TestP04_WarmStartAndIdentity"][0] != "PASS" || rows["TestP08_Throughput"][0] != "FAIL" ||
		rows["TestP13_RunnerRestart"][0] != "FAIL" || !strings.Contains(rows["TestP13_RunnerRestart"][1], "exit 3") ||
		rows["TestP19_TeardownHygiene"][0] != "PASS" || rows["TestP05_AgentSession"][0] != "NOT_RUN" {
		t.Errorf("summary rows %v", rows)
	}
	if !strings.Contains(out, "PASS 2 FAIL 2 BLOCK 0 NOT_RUN 16 of 20 P-rows") {
		t.Errorf("run.sh output lacks the counts:\n%s", out)
	}
}

// TestStatusAllowlist: only the named keys of runnerd's status are kept, and no failure echoes
// any of the body.
func TestStatusAllowlist(t *testing.T) {
	const secret = "SECRET-7f3a"
	m, err := allowStatus([]byte(`{"state":"ready","max":2,"one_time_code":"` + secret + `","note":"` + secret + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["state"] != "ready" || m["max"] != float64(2) || strings.Contains(fmt.Sprint(m), secret) {
		t.Errorf("kept %v, want only state and max", m)
	}
	for _, body := range []string{secret + " not json", `{"state": ` + secret + `}`, `["` + secret + `"]`} {
		if _, err := allowStatus([]byte(body)); err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "7f3a") {
			t.Errorf("allowStatus(%q) = %v, want an error that names no part of the body", body, err)
		}
	}
}

// signalWriter reports each prompt written.
type signalWriter chan struct{}

func (s signalWriter) Write(b []byte) (int, error) { s <- struct{}{}; return len(b), nil }

// TestAskDropsLateAnswers: one reader serves every prompt, and a line typed before a prompt is
// never taken as its answer.
func TestAskDropsLateAnswers(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := signalWriter(make(chan struct{}, 4))
	p := &prompts{w: w, lines: lineReader(pr)}
	fmt.Fprintln(pw, "early")
	for deadline := time.Now().Add(5 * time.Second); len(p.lines) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the early line never reached the reader")
		}
	}
	go func() { <-w; fmt.Fprintln(pw, "fresh") }()
	if got := p.ask(t, "q1", 5*time.Second); got != "fresh" {
		t.Errorf("the prompt took %q, want the answer typed after it (fresh)", got)
	}
}

// TestAskAfterTimeout: prompt 1 times out and its answer arrives once prompt 2 has rendered (or
// would have); prompt 2 never takes it, and is NOT_RUN.
func TestAskAfterTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := signalWriter(make(chan struct{}, 4))
	p := &prompts{w: w, lines: lineReader(pr)}
	var first, second *testing.T
	t.Run("q1", func(t *testing.T) { first = t; p.ask(t, "q1", 20*time.Millisecond) })
	<-w // q1 rendered
	go func() {
		select {
		case <-w: // q2 rendered
		case <-time.After(200 * time.Millisecond):
		}
		fmt.Fprintln(pw, "late answer to q1")
	}()
	var got string
	t.Run("q2", func(t *testing.T) { second = t; got = p.ask(t, "q2", 2*time.Second) })
	if !first.Skipped() || !second.Skipped() || got != "" {
		t.Errorf("q1 NOT_RUN %v, q2 NOT_RUN %v, q2 took %q: a late answer must never reach the next prompt", first.Skipped(), second.Skipped(), got)
	}
}

// TestIsolate: a witness stands only when the named limit alone refuses, sized from the ledger.
func TestIsolate(t *testing.T) {
	ok := fits{Licence: 1, RunnerRuns: 1, RunnerCPU: 1, RunnerMemory: 1, RunnerDisk: 1, HostMemory: 1, LiveMemory: 1, Disk: 0, Source: "ledger"}
	for name, c := range map[string]struct {
		f    fits
		skip bool
	}{
		"only disk":          {ok, false},
		"host memory too":    {func() fits { f := ok; f.HostMemory = 0; return f }(), true},
		"live memory too":    {func() fits { f := ok; f.LiveMemory = 0; return f }(), true},
		"runner runs too":    {func() fits { f := ok; f.RunnerRuns = 0; return f }(), true},
		"licence too":        {func() fits { f := ok; f.Licence = 0; return f }(), true},
		"disk still fits":    {func() fits { f := ok; f.Disk = 1; return f }(), true},
		"sized from setting": {func() fits { f := ok; f.Source = "settings"; return f }(), true},
	} {
		var sub *testing.T
		t.Run(name, func(t *testing.T) { sub = t; c.f.isolate(t, "disk") })
		if sub.Skipped() != c.skip {
			t.Errorf("%s: NOT_RUN %v, want %v", name, sub.Skipped(), c.skip)
		}
	}
}

// TestMessagesMatchProduct reads the sign-in refusal from the API source (no build of it) and pins
// that the suite matches it there and never as a placement refusal.
func TestMessagesMatchProduct(t *testing.T) {
	b, err := os.ReadFile("../../../internal/api/run_desktop_vm_signin.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`desktopSignInsOffMsg\s*=\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no desktopSignInsOffMsg in the API source")
	}
	if !msg["signInsOff"].Match(m[1]) || msg["placementAny"].Match(m[1]) {
		t.Errorf("the suite's sign-in refusal does not match the product's %q, or placementAny takes it", m[1])
	}
}

// TestTombstonable pins the ledger's tombstone condition as the harness applies it, and that an
// entry stays counted under a tombstone the ledger would not honour.
func TestTombstonable(t *testing.T) {
	const uid, boot = 502, int64(1000)
	e := admission.Entry{Kind: admission.KindRun, BootTime: boot, OwnerPID: 10, OwnerStart: 1, HelperPID: 20, HelperStart: 2}
	vz := admission.Proc{PID: 30, Start: 3, UID: uid, Name: admission.VirtualizationProcess[:16]}
	for name, c := range map[string]struct {
		e    admission.Entry
		ps   []admission.Proc
		want bool
	}{
		"helper gone, nothing left":     {e, nil, true},
		"helper alive":                  {e, []admission.Proc{{PID: 20, Start: 2, UID: uid, Name: admission.HelperProcess}}, false},
		"pid reused by another process": {e, []admission.Proc{{PID: 20, Start: 9, UID: 0, Name: "launchd"}}, true},
		"a VM process of the uid":       {e, []admission.Proc{vz}, false},
		"a helper process of the uid":   {e, []admission.Proc{{PID: 40, Start: 4, UID: uid, Name: admission.HelperProcess}}, false},
		"a VM process of another uid":   {e, []admission.Proc{{PID: 30, Start: 3, UID: 501, Name: vz.Name}}, true},
		"an earlier boot":               {func() admission.Entry { x := e; x.BootTime = 1; return x }(), nil, false},
		"leaked":                        {func() admission.Entry { x := e; x.Kind = admission.KindLeaked; return x }(), nil, false},
		"no helper yet, owner alive": {func() admission.Entry { x := e; x.HelperPID, x.HelperStart = 0, 0; return x }(),
			[]admission.Proc{{PID: 10, Start: 1, UID: 0, Name: "wardyn-runnerd"}}, false},
	} {
		if got := tombstonable(c.e, uid, boot, c.ps); got != c.want {
			t.Errorf("%s: tombstonable %v, want %v", name, got, c.want)
		}
	}
	fs := []ledgerFile{{Name: "e-a.json"}, {Name: "e-a.json.tombstone", Tomb: true}, {Name: "e-b.json"}, {Name: "e-b.json.tombstone", Tomb: true, Honoured: true}}
	if got := countedFiles(fs); len(got) != 1 || got[0].Name != "e-a.json" {
		t.Errorf("counted %v, want only e-a.json (its tombstone is not honoured)", got)
	}
}
