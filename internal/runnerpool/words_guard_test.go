// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retiredPoolWords are the words the organization and user pool model retired from everything a person
// reads: a runner is self-hosted or remote-hosted, a pool is an organization pool or a user pool.
var retiredPoolWords = regexp.MustCompile(`(?i)executor|remote[ -]provided|self-hosted pool|\bhosting\b`)

// TestPoolWordsAreRetired guards the user-facing text of the pool surfaces: the server's sentences and
// the CLI's help (Go string literals), the SDK's doc comments and strings, and the console's pool types,
// refusal sentences and golden table (the whole file). Go identifiers may keep the old names.
func TestPoolWordsAreRetired(t *testing.T) {
	root := filepath.Join("..", "..")
	glob := func(pattern string) []string {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(m) == 0 {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		var out []string
		for _, f := range m {
			if !strings.HasSuffix(f, "_test.go") {
				out = append(out, f)
			}
		}
		return out
	}
	var literals, withComments, whole []string
	literals = append(literals, glob("internal/runnerpool/reasons.go")...)
	literals = append(literals, glob("internal/types/runner_pool.go")...)
	literals = append(literals, glob("internal/types/remote_runner.go")...)
	literals = append(literals, glob("internal/api/runner_pools*.go")...)
	literals = append(literals, glob("internal/api/remote_runners.go")...)
	literals = append(literals, glob("cmd/wardyn/runner_pool*.go")...)
	literals = append(literals, glob("cmd/wardyn/runner_remote.go")...)
	literals = append(literals, glob("cmd/wardyn/run_placement.go")...)
	withComments = append(withComments, glob("pkg/client/runner_pools.go")...)
	withComments = append(withComments, glob("pkg/client/remote_runners.go")...)
	whole = append(whole, glob("ui/src/app/lib/types/runner-pools.ts")...)
	whole = append(whole, glob("ui/src/app/lib/runner-pool-refusals.ts")...)
	whole = append(whole, glob("ui/src/app/lib/runner-pool-refusals.golden.json")...)

	report := func(file string, pos token.Position, text string) {
		if m := retiredPoolWords.FindString(text); m != "" {
			t.Errorf("%s:%d says %q: %q", file, pos.Line, m, text)
		}
	}
	for _, f := range append(literals, withComments...) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				// A struct tag is a wire name, pinned by the wire-parity tests, not a sentence: dropped
				// before Inspect walks the field's children.
				n.Tag = nil
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					report(f, fset.Position(n.Pos()), n.Value)
				}
			}
			return true
		})
		if !strings.HasPrefix(filepath.ToSlash(f), filepath.ToSlash(filepath.Join(root, "pkg"))) {
			continue
		}
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				report(f, fset.Position(c.Pos()), c.Text)
			}
		}
	}
	for _, f := range whole {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			report(f, token.Position{Line: i + 1}, line)
		}
	}
}
