// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// destructiveRunnerCalls are the runner methods that stop or cut what a run's sandbox runs. A reference to one, a
// call or a method value handed on, is a site of the census.
var destructiveRunnerCalls = map[string]bool{
	"StopSandbox": true, "StopProxy": true, "KillSandbox": true, "EndSandbox": true,
	"FenceProxy": true, "StopProxyAt": true, "EndSandboxAt": true,
}

// destructiveWrappers are the functions that make one of those calls for their caller. A reference to one is a site
// of the census too, so a new caller of a wrapper must be listed with its guard, as a new direct call must.
var destructiveWrappers = map[string]bool{
	"stopSandboxOrAudit": true, "stopLostSandbox": true, "endSandbox": true, "cutAgent": true, "finalizeRunEffects": true,
}

// destructiveSite is one function's references to one destructive call. "teardown" is a call of a teardown verb
// handed in as a func value (teardownOrAudit's, through teardownUnlessLost and boundedTeardown).
type destructiveSite struct{ file, fn, call string }

type destructiveRow struct {
	n     int
	guard string
}

// The guards a site may name. A site with none names the Known issues line that tells users, as "unguarded: "
// and the line's ids.
const (
	guardChecked        = "lockLost/holdPassed checked"    // the lock's loss (and, under an end hold, its deadline) is asked just before the call, by lockLost and holdPassed or by the hold context's own Err
	guardHeldReread     = "under a held lock with re-read" // the run's lock is held and the run read again under it
	guardFenced         = "generation-fenced"              // the runner refuses a call for an older proxy generation
	guardNotReplaceable = "not replaceable"                // the kill path, or a ref no revive can take (a terminal run)
)

// destructiveKnownIssues are the Known issues lines an unguarded site may name, by their id in the release's
// planned changelog, with each line's first words.
var destructiveKnownIssues = map[string]string{
	"K11": "After a lost database lock, a slower attempt can still stop a newer revive's proxy or sandbox",
	"K12": "If a headless run that is not on a laptop runner loses its database lock while a revive or live egress edit of it is failing",
	"K14": "If a run's lost mark cannot be written after its database lock was lost, the server tries to stop its proxy anyway",
	"K16": "A delayed end whose lock was lost can also remove a sandbox that another server was keeping to copy changes back",
}

// destructiveCallCensus is every destructive runner call in internal/api and cmd/wardynd, frozen: a new one fails
// TestRunnerDestructiveCallCensus until it is listed with its guard, and a row moved to a stronger guard needs only
// its edit here.
var destructiveCallCensus = map[destructiveSite]destructiveRow{
	// The orphan sweep of a terminal run, under its lock, through teardownOrAudit.
	{"internal/api/reconcile.go", "Server.reconcileOrphanedRun", "KillSandbox"}: {2, guardNotReplaceable},
	{"internal/api/reconcile.go", "Server.reconcileOrphanedRun", "StopSandbox"}: {1, guardNotReplaceable},
	// The cut of an ended run's agent (refused for a run that is not terminal), and its proxy-stop fallback.
	{"internal/api/run_agent_cut.go", "Server.cutAgent", "FenceProxy"}:     {2, guardNotReplaceable},
	{"internal/api/run_agent_cut.go", "Server.stopProxyOnly", "StopProxy"}: {1, guardNotReplaceable},
	// A RUNNING run's quiesce before its pass back: under the end's held lock, the run read under it.
	{"internal/api/run_computer_folder.go", "Server.quiesceKeptSandbox", "StopProxy"}: {1, guardHeldReread},
	// The containment loop's stop of an ended run: the lock's loss is asked inside the call's own step.
	{"internal/api/run_contain_d1.go", "Server.d1Stop", "StopSandbox"}: {1, guardChecked},
	// The lease sweep's re-stop of a kept run on every pass (leaseRun), and an ended lost run's (stopLostSandbox):
	// the lock taken and the run re-read can be lost after.
	{"internal/api/run_lease.go", "Server.endSandbox", "EndSandbox"}: {1, "unguarded: K11"},
	// The unfenced lost-run stops: a failed revive or edit (endUnconfirmedRun's callers, reloseRun), the fence-0
	// sweeps (sweepLapsedRunTokens, keepRebootedRun, the gone-runner end), the lease sweep's re-stop; and the stop
	// after a lost mark that could not be written (loseWriteFailed).
	{"internal/api/run_lost.go", "Server.stopLostSandbox", "StopProxy"}: {1, "unguarded: K11, K14"},
	// The lost-run stops that name a policy generation: the runner refuses a call for an older proxy generation.
	{"internal/api/run_lost_at_gen.go", "Server.stopLostSandboxAt", "StopProxyAt"}:  {1, guardFenced},
	{"internal/api/run_lost_at_gen.go", "Server.stopLostSandboxAt", "EndSandboxAt"}: {1, guardFenced},
	// The headless proxy stop of a run whose revive or edit failed (stopUnconfirmedProxyAt).
	{"internal/api/run_proxy_readback.go", "Server.stopProxyOrAudit", "StopProxy"}: {1, "unguarded: K12"},
	// The end's stop: the lock's loss and the end hold's deadline are asked before the call, not during it.
	{"internal/api/run_terminal_effects.go", "Server.finalizeRunEffects", "StopSandbox"}: {1, "unguarded: K16"},
	// Handed to teardownOrAudit by the lost-run stops of a failed revive or edit, and by dispatch's failures.
	{"internal/api/runs_lifecycle.go", "Server.stopSandboxOrAudit", "StopSandbox"}: {1, "unguarded: K11, K12"},
	// The kill.
	{"internal/api/runs_lifecycle.go", "Server.killTeardownTail", "KillSandbox"}: {1, guardNotReplaceable},
	// teardownOrAudit's verb, called only while the lock is not lost; boundedTeardown's wrap of it is reached only
	// through that call.
	{"internal/api/run_lock_lost.go", "teardownUnlessLost", "teardown"}: {1, guardChecked},
	{"internal/api/reconcile.go", "boundedTeardown", "teardown"}:        {1, guardChecked},
	// The idle and max-age stop: the lock's loss is asked before the call, not during it.
	{"cmd/wardynd/adapters.go", "lifecycleStopper.stop", "StopSandbox"}: {1, "unguarded: K16"},

	// The wrappers' callers. Each carries its wrapper's guard; a caller with a stronger guard of its own may name
	// that instead.
	{"internal/api/run_kept_stop_forced.go", "Server.retryForcedStop", "stopSandboxOrAudit"}:                         {1, "unguarded: K11, K12"},
	{"internal/api/run_local_folder.go", "Server.failLocalFolderRun", "stopSandboxOrAudit"}:                          {1, "unguarded: K11, K12"},
	{"internal/api/run_proxy_readback.go", "Server.abortUnconfirmedSandbox", "stopSandboxOrAudit"}:                   {1, "unguarded: K11, K12"},
	{"internal/api/run_proxy_readback.go", "Server.stopTerminalSandbox", "stopSandboxOrAudit"}:                       {1, "unguarded: K11, K12"},
	{"internal/api/run_proxy_readback.go", "Server.stopUnconfirmedProxyAt", "stopSandboxOrAudit"}:                    {3, "unguarded: K11, K12"},
	{"internal/api/runs_dispatch.go", "Server.dispatchRun", "stopSandboxOrAudit"}:                                    {1, "unguarded: K11, K12"},
	{"internal/api/runs_dispatch_byoi.go", "Server.byoiExecLessRefused", "stopSandboxOrAudit"}:                       {1, "unguarded: K11, K12"},
	{"internal/api/runs_dispatch_mounts.go", "Server.abortUnrecordedSandbox", "stopSandboxOrAudit"}:                  {1, "unguarded: K11, K12"},
	{"internal/api/runs_dispatch_start.go", "Server.prepareMacOSInteractive", "stopSandboxOrAudit"}:                  {1, "unguarded: K11, K12"},
	{"internal/api/runs_dispatch_start.go", "Server.startAgentsOrIdle", "stopSandboxOrAudit"}:                        {3, "unguarded: K11, K12"},
	{"internal/api/run_lease.go", "Server.leaseRun", "stopLostSandbox"}:                                              {1, "unguarded: K11, K14"},
	{"internal/api/run_lost_at_gen.go", "Server.stopLostSandboxAt", "stopLostSandbox"}:                               {2, "unguarded: K11, K14"},
	{"internal/api/run_lease.go", "Server.endRun", "endSandbox"}:                                                     {1, "unguarded: K11"},
	{"internal/api/run_lease.go", "Server.leaseRun", "endSandbox"}:                                                   {1, "unguarded: K11"},
	{"internal/api/run_lost.go", "Server.stopLostSandbox", "endSandbox"}:                                             {1, "unguarded: K11"},
	{"internal/api/run_agent_cut.go", "Server.copyBackCut", "cutAgent"}:                                              {1, guardNotReplaceable},
	{"internal/api/run_computer_folder.go", "Server.quiesceKeptSandbox", "cutAgent"}:                                 {2, guardNotReplaceable},
	{"internal/api/run_contain.go", "Server.containCutItem", "cutAgent"}:                                             {1, guardNotReplaceable},
	{"internal/api/run_contain_d1.go", "Server.d1LastCut", "cutAgent"}:                                               {1, guardNotReplaceable},
	{"internal/api/run_kept_stop_forced.go", "Server.forceKeptStop", "cutAgent"}:                                     {1, guardNotReplaceable},
	{"internal/api/run_kept_stop_forced.go", "Server.retryForcedStop", "cutAgent"}:                                   {1, guardNotReplaceable},
	{"internal/api/run_kept_stop_forced.go", "Server.settleUnsettledForcedFolder", "cutAgent"}:                       {1, guardNotReplaceable},
	{"internal/api/run_computer_folder_bring_back.go", "Server.handleBringComputerFolderBack", "finalizeRunEffects"}: {1, "unguarded: K16"},
	{"internal/api/run_tail_owner.go", "Server.tailFinalize", "finalizeRunEffects"}:                                  {1, "unguarded: K16"},
	{"internal/api/runner_exits.go", "Server.applyRunnerExit", "finalizeRunEffects"}:                                 {1, "unguarded: K16"},
	{"internal/api/runs_lifecycle.go", "Server.finalizeRunTailOrdered", "finalizeRunEffects"}:                        {1, "unguarded: K16"},
}

// destructiveCallSites counts each function's destructive references in files, keyed by their path.
func destructiveCallSites(files map[string]*ast.File) map[destructiveSite]int {
	out := map[destructiveSite]int{}
	for path, f := range files {
		for _, d := range f.Decls {
			fn := "(package level)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					t := fd.Recv.List[0].Type
					if st, ok := t.(*ast.StarExpr); ok {
						t = st.X
					}
					if id, ok := t.(*ast.Ident); ok {
						fn = id.Name + "." + fn
					}
				}
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if destructiveRunnerCalls[x.Sel.Name] || destructiveWrappers[x.Sel.Name] {
						out[destructiveSite{path, fn, x.Sel.Name}]++
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "teardown" {
						out[destructiveSite{path, fn, "teardown"}]++
					}
				}
				return true
			})
		}
	}
	return out
}

// destructiveCensusProblems is what found and table disagree on, and every row whose guard is not one of the
// guards or names an unknown Known issues line.
func destructiveCensusProblems(found map[destructiveSite]int, table map[destructiveSite]destructiveRow) []string {
	var out []string
	for site, n := range found {
		row, ok := table[site]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s %s: %d unlisted %s call(s); list each with its guard", site.file, site.fn, n, site.call))
		case row.n != n:
			out = append(out, fmt.Sprintf("%s %s: %d %s call(s), the census lists %d", site.file, site.fn, n, site.call, row.n))
		}
	}
	for site, row := range table {
		if _, ok := found[site]; !ok {
			out = append(out, fmt.Sprintf("%s %s: the census lists %d %s call(s) and there are none; drop the row", site.file, site.fn, row.n, site.call))
		}
		if err := destructiveGuardProblem(row.guard); err != "" {
			out = append(out, fmt.Sprintf("%s %s %s: %s", site.file, site.fn, site.call, err))
		}
	}
	sort.Strings(out)
	return out
}

func destructiveGuardProblem(guard string) string {
	switch guard {
	case guardChecked, guardHeldReread, guardFenced, guardNotReplaceable:
		return ""
	}
	ids, ok := strings.CutPrefix(guard, "unguarded: ")
	if !ok || ids == "" {
		return fmt.Sprintf("guard %q is not a guard and names no Known issues line", guard)
	}
	for _, id := range strings.Split(ids, ", ") {
		if _, known := destructiveKnownIssues[id]; !known {
			return fmt.Sprintf("guard %q names %q, which is no Known issues line", guard, id)
		}
	}
	return ""
}

// parseDestructiveTree parses the non-test files of internal/api and cmd/wardynd, and of every folder below them,
// keyed by their repo path.
func parseDestructiveTree(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	for dir, repo := range map[string]string{".": "internal/api", "../../cmd/wardynd": "cmd/wardynd"} {
		parseDestructiveDir(t, dir, repo, files)
	}
	return files
}

// parseDestructiveDir adds the non-test Go files under root, in every folder below it, to files, keyed by repo and
// their path under root. A testdata folder is skipped, as the go tool skips it.
func parseDestructiveDir(t *testing.T, root, repo string, files map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[repo+"/"+filepath.ToSlash(rel)] = f
		n++
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("no Go files under %s (%v)", root, err)
	}
}

// Every destructive runner call in internal/api and cmd/wardynd is in the frozen census with its guard, or with
// the Known issues line that says it has none; every row matches a call. A planted call no row lists is found, and
// a row naming an unknown Known issues line fails.
func TestRunnerDestructiveCallCensus(t *testing.T) {
	files := parseDestructiveTree(t)
	found := destructiveCallSites(files)
	if len(found) < 10 {
		t.Fatalf("found only %d destructive sites: the scan broke, not the tree", len(found))
	}
	for _, p := range destructiveCensusProblems(found, destructiveCallCensus) {
		t.Error(p)
	}

	t.Run("a planted unlisted call is found", func(t *testing.T) {
		const src = `package api

func (s *Server) plantedStop(ref string) {
	_ = s.cfg.Runner.StopSandbox(nil, ref)
	stop := s.cfg.Runner.KillSandbox
	_ = stop
	s.endSandbox(nil, ref)
}
`
		f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		withPlant := map[string]*ast.File{"internal/api/planted.go": f}
		for k, v := range files {
			withPlant[k] = v
		}
		problems := destructiveCensusProblems(destructiveCallSites(withPlant), destructiveCallCensus)
		var stop, kill, wrap bool
		for _, p := range problems {
			stop = stop || strings.Contains(p, "planted.go Server.plantedStop: 1 unlisted StopSandbox")
			kill = kill || strings.Contains(p, "planted.go Server.plantedStop: 1 unlisted KillSandbox")
			wrap = wrap || strings.Contains(p, "planted.go Server.plantedStop: 1 unlisted endSandbox")
		}
		if !stop || !kill || !wrap || len(problems) != 3 {
			t.Errorf("problems %q, want exactly the planted call, the planted method value and the planted wrapper call", problems)
		}
	})

	t.Run("a call in a folder below is found and testdata is skipped", func(t *testing.T) {
		root := t.TempDir()
		const src = `package sub

func (s *Server) nestedStop(ref string) {
	_ = s.cfg.Runner.StopSandbox(nil, ref)
}
`
		for _, name := range []string{"sub/nested.go", "testdata/planted.go"} {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		nested := map[string]*ast.File{}
		parseDestructiveDir(t, root, "tmp", nested)
		found := destructiveCallSites(nested)
		want := destructiveSite{"tmp/sub/nested.go", "Server.nestedStop", "StopSandbox"}
		if len(found) != 1 || found[want] != 1 {
			t.Errorf("found %v, want only %v once", found, want)
		}
	})

	t.Run("an unknown Known issues line fails", func(t *testing.T) {
		for _, guard := range []string{"unguarded: K99", "unguarded: K11, K99", "unguarded: ", "trusted"} {
			table := map[destructiveSite]destructiveRow{}
			for k, v := range destructiveCallCensus {
				table[k] = v
			}
			site := destructiveSite{"internal/api/run_lost.go", "Server.stopLostSandbox", "StopProxy"}
			table[site] = destructiveRow{table[site].n, guard}
			if problems := destructiveCensusProblems(found, table); len(problems) != 1 || !strings.Contains(problems[0], "stopLostSandbox") {
				t.Errorf("guard %q: problems %q, want the one row's", guard, problems)
			}
		}
	})
}
