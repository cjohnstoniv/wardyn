// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// directCutRE is a direct call of an agent cut, which would leave the run's MCP proxies running.
var directCutRE = regexp.MustCompile(`\.(StopProxy|StopProxyAt|FenceProxy|EndSandbox|EndSandboxAt|StopLostProxyAt)\(`)

// TestEveryEgressCutGoesThroughTheHelpers reads the control plane's non-test Go by path (so it compiles none of
// it) and finds no direct cut: every one goes through a cut helper, which also stops the run's MCP proxies.
func TestEveryEgressCutGoesThroughTheHelpers(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "api", "*.go"))
	if err != nil || len(files) < 100 {
		t.Fatalf("read %d files of internal/api (%v); the census is not looking", len(files), err)
	}
	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(raw), "\n") {
			if directCutRE.MatchString(line) {
				t.Errorf("%s:%d: %s: cut egress through runner.CutEgress, CutEgressAt, FenceEgress, EndSandboxCut or EndSandboxCutAt", path, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no non-test file scanned")
	}
	if !directCutRE.MatchString("x.StopProxy(ctx, ref)") || directCutRE.MatchString("runner.CutEgress(ctx, x, ref)") {
		t.Fatal("the pattern does not tell a direct cut from a helper")
	}
}
