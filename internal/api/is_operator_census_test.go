// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// isOperatorCensus is the FROZEN list of production functions that ask
// isOperator, with how many times each asks. isOperator is the route tier, the
// credential namespace and the run-reach predicate; the run seams ask
// runUngoverned instead (govern_admin.go), and the two answer differently once
// WARDYN_GOVERN_ADMIN_RUNS is on. A new isOperator call therefore has to be
// classified before it merges: a run seam belongs on runUngoverned, anything
// else belongs here with its reason.
//
// Keyed by (file, enclosing function), never by line number, so a sibling lane
// editing the same file does not red this test. See
// docs/design/0.8/0.8.6-cadm.md (D9, "Seam census").
var isOperatorCensus = map[string]int{
	// Route tier: the mode changes no route's class.
	"http.go:requireOperator": 1,
	// Reach: who may write or read what they do not own.
	"helpers.go:ownsWorkspaceOrAdmin":     1,
	"helpers.go:ownsRunOrSuperAdmin":      1,
	"helpers.go:workspaceReadTierFor":     1,
	"attach_ticket.go:handleAttachTicket": 3, // reach, and the ticket's reach stamp
	"attach.go:refuseAttachEntry":         1,
	"run_entry.go:getRunForEntry":         1,
	"sshkeys.go:handleAddSSHKey":          1,
	// Credential namespace.
	"runs_policy.go:secretOwnerFromRequest":  1,
	"inline_policy.go:boundEnvSecretPosture": 1,
	"secrets.go:handlePutSecret":             1,
	"secrets.go:secretOwnerParam":            1,
	"secrets.go:handleListSecrets":           1,
	// Reading a recording is a privacy surface, not a launch.
	"recording.go:recordingAuthorizer": 1,
	"recording.go:recordingReader":     1,
	// The deployer funnel, admin read projections and display tiers.
	"setup.go:handleSetupStatus":                   2,
	"setup_integrations.go:handleListIntegrations": 1,
	"me.go:handleMe":                               3,
	// Compares stamped tiers: the switch must not change whether a demotion
	// revokes tokens.
	"apitokens.go:roleSnapshotDrops": 2,
	// List and read only; the launch check is presetLaunchOpenTo.
	"presets.go:presetOpenTo": 1,
	// The predicate itself.
	"govern_admin.go:runUngoverned": 1,
}

// TestIsOperatorCensus scans every production file in internal/api for calls to
// isOperator and compares them with isOperatorCensus. It fails on a function not
// in the list, on a count that differs, and on a listed entry nothing calls.
func TestIsOperatorCensus(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	found := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && callsIsOperator(call) {
					found[name+":"+fn.Name.Name]++
				}
				return true
			})
		}
	}

	var problems []string
	for key, n := range found {
		want, listed := isOperatorCensus[key]
		switch {
		case !listed:
			problems = append(problems, fmt.Sprintf("%s calls isOperator %d time(s) and is not in isOperatorCensus: a run seam asks runUngoverned; anything else is classified in the census with its reason", key, n))
		case want != n:
			problems = append(problems, fmt.Sprintf("%s calls isOperator %d time(s), census says %d", key, n, want))
		}
	}
	for key := range isOperatorCensus {
		if _, ok := found[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s is in isOperatorCensus but calls isOperator nowhere: remove the entry", key))
		}
	}
	slices.Sort(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func callsIsOperator(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == "isOperator"
	case *ast.Ident:
		return fun.Name == "isOperator"
	}
	return false
}
