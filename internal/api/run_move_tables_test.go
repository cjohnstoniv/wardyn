// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// moveRunnerBoundTables is every table that holds both a run reference and a runner reference, with what a move
// does to it. A later migration that adds another fails TestMoveRunnerBoundTablesCensus until it is classified here,
// with its pg subtest in TestMoveExec_RunnerBoundTables.
var moveRunnerBoundTables = map[string]string{
	"kept_machines":                 "keep: a run that keeps a machine never moves",
	"machine_snapshots":             "keep: a snapshot is of a Linux VM run, and a VM run never moves",
	"run_local_containment":         "keep: a run held contained on its runner is refused at admission and before commit (movePortLocallyContained)",
	"run_containment_pending":       "keep: commit requires none for the run",
	"run_node_records":              "keep",
	"run_teardown_holds":            "keep: commit requires none open",
	"runner_execution_exits":        "keep: commit requires every tail complete",
	"runner_executions":             "keep: the destination's revive writes its own",
	"runner_inventories":            "keep: the intake ignores a moved run's row",
	"runner_inventory_candidates":   "keep: the intake ignores a moved run's row",
	"runner_inventory_observations": "keep: the intake ignores a moved run's row",
	"runner_pending_actions":        "keep: commit requires none unsettled",
	"runner_resident_runs":          "keep: a run with a resident value never moves",
	"runner_retirements":            "keep: written at the run's current runner",
	"runner_spool_cursors":          "close at commit: commit deletes the run's row",
	"sources":                       "keep: a folder bound to a runner refuses the move",
	"workloads":                     "keep: the workload names the run, not where it runs",
	"run_moves":                     "the move's own record",
	"run_placements":                "the move's own record",
}

var (
	sqlComment    = regexp.MustCompile(`--[^\n]*`)
	sqlCreate     = regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\((.*?)\n\s*\)\s*;`)
	sqlAlter      = regexp.MustCompile(`(?is)ALTER TABLE (?:IF EXISTS )?(?:ONLY )?(\w+)\s+(.*?);`)
	sqlAddColumn  = regexp.MustCompile(`(?i)ADD COLUMN (?:IF NOT EXISTS )?(\w+)\s+\w+([^,]*)`)
	sqlDrop       = regexp.MustCompile(`(?i)DROP TABLE (?:IF EXISTS )?(\w+)`)
	sqlColumnLine = regexp.MustCompile(`^\s*(\w+)\s+\w+(.*)`)
)

// runnerBoundTables applies the census rule to the migrations in dir: comments stripped, CREATE TABLE and ALTER
// TABLE ... ADD COLUMN read, dropped tables skipped; a table counts when it holds a run reference (run_id, *_run_id,
// or a column referencing agent_runs) and a runner reference (runner_id, *_runner_id, or one referencing runners).
func runnerBoundTables(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("read migrations: %v (%d files)", err, len(paths))
	}
	slices.Sort(paths)
	run, rn := map[string]bool{}, map[string]bool{}
	note := func(table, col, rest string) {
		col, rest = strings.ToLower(col), strings.ToLower(rest)
		if col == "run_id" || strings.HasSuffix(col, "_run_id") || strings.Contains(rest, "references agent_runs") {
			run[table] = true
		}
		if col == "runner_id" || strings.HasSuffix(col, "_runner_id") || strings.Contains(rest, "references runners") {
			rn[table] = true
		}
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		src := sqlComment.ReplaceAllString(string(b), "")
		for _, m := range sqlCreate.FindAllStringSubmatch(src, -1) {
			for _, line := range strings.Split(m[2], "\n") {
				if c := sqlColumnLine.FindStringSubmatch(line); c != nil {
					note(strings.ToLower(m[1]), c[1], c[2])
				}
			}
		}
		for _, m := range sqlAlter.FindAllStringSubmatch(src, -1) {
			for _, c := range sqlAddColumn.FindAllStringSubmatch(m[2], -1) {
				note(strings.ToLower(m[1]), c[1], c[2])
			}
		}
		for _, m := range sqlDrop.FindAllStringSubmatch(src, -1) {
			delete(run, strings.ToLower(m[1]))
			delete(rn, strings.ToLower(m[1]))
		}
	}
	var out []string
	for table := range run {
		if rn[table] {
			out = append(out, table)
		}
	}
	slices.Sort(out)
	return out
}

// TestMoveRunnerBoundTablesCensus: every table with a run and a runner reference is classified, and every classified
// table still has both.
func TestMoveRunnerBoundTablesCensus(t *testing.T) {
	got := runnerBoundTables(t, filepath.Join("..", "db", "migrations"))
	for _, table := range got {
		if _, ok := moveRunnerBoundTables[table]; !ok {
			t.Errorf("table %s holds a run and a runner reference and says nothing of what a move does to it", table)
		}
	}
	for table := range moveRunnerBoundTables {
		if !slices.Contains(got, table) {
			t.Errorf("moveRunnerBoundTables names %s, which no longer holds both references", table)
		}
	}
}
