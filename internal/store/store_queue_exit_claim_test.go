// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// queueExitStatements is every function whose statement moves runs out of QUEUED or ends them with a
// "WITH moved AS (UPDATE agent_runs" statement. A value of "" means the statement ends runs and must carry the
// claim on the drive the run kept (keptDriveHoldSQL, in a held CTE over moved); any other value is the reason the
// statement takes no claim. A statement added elsewhere fails until it is listed here.
var queueExitStatements = map[string]string{
	"LeaveRunQueueIn":    "",
	"EndUnstartedRun":    "",
	"cancelPoolQueue":    "",
	"promoteQueuedRun":   "promotion: the run is admitted, nothing ended",
	"RequeuePromotedRun": "puts a promoted run back in the queue, nothing ended",
}

// TestEveryRunEndingStatementClaimsTheKeptDrive: the claim on a kept drive is taken in the statement that ends the
// run, so an exit cannot end a run and leave its drive offered between the end and the cleanup.
func TestEveryRunEndingStatementClaimsTheKeptDrive(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if !strings.Contains(body, "WITH moved AS (") || !strings.Contains(body, "UPDATE agent_runs") {
				continue
			}
			reason, listed := queueExitStatements[fn.Name.Name]
			if !listed {
				t.Errorf("%s: %s has a WITH moved AS (UPDATE agent_runs ...) statement that is not in queueExitStatements; "+
					"claim the kept drive in it (keptDriveHoldSQL) and list it, or list it with the reason it takes no claim", name, fn.Name.Name)
				continue
			}
			seen[fn.Name.Name] = true
			if reason == "" && !strings.Contains(body, "held AS (`+keptDriveHoldSQL+") {
				t.Errorf("%s: %s ends runs and does not carry the kept-drive claim (held AS (`+keptDriveHoldSQL+`...))", name, fn.Name.Name)
			}
		}
	}
	for name := range queueExitStatements {
		if !seen[name] {
			t.Errorf("queueExitStatements lists %s, which has no such statement; remove the entry", name)
		}
	}
}
