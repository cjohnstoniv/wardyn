// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ciWorkflow is the part of .github/workflows/ci.yml the stop-early guards read.
type ciWorkflow struct {
	Jobs map[string]struct {
		Name        string            `yaml:"name"`
		Needs       yaml.Node         `yaml:"needs"`
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Strategy    struct {
			Matrix map[string]any `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadCIWorkflow(t *testing.T) (ciWorkflow, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("parsed no ci.yml jobs")
	}
	return wf, string(raw)
}

func ciNeeds(t *testing.T, n yaml.Node) []string {
	t.Helper()
	var list []string
	switch n.Kind {
	case 0:
	case yaml.ScalarNode:
		list = []string{n.Value}
	default:
		if err := n.Decode(&list); err != nil {
			t.Fatalf("decode needs: %v", err)
		}
	}
	slices.Sort(list)
	return list
}

// TestCIStopEarlyJobsFireOnlyOnCheapDeterministicGates pins the jobs that cancel
// a red pull request run (docs/CI.md, "Stop a red run early"). Each one needs
// exactly one cheap deterministic gate, so none can fire on a flake-prone job
// (`ui-e2e`, `test-pg-shard`, a test leg) or wait for the slowest gate; it fires
// only on that gate's `failure`, only on a same-repository, non-Dependabot pull
// request (push, merge_group, forks and Dependabot keep a full run), holds no
// permission but `actions: write`, and runs nothing from the pull request.
func TestCIStopEarlyJobsFireOnlyOnCheapDeterministicGates(t *testing.T) {
	wf, raw := loadCIWorkflow(t)
	want := map[string]string{
		"stop-early-go-lint":  "go-lint",
		"stop-early-compose":  "compose",
		"stop-early-gates":    "gates",
		"stop-early-notices":  "notices",
		"stop-early-diagrams": "diagrams",
		"stop-early-dco":      "dco",
	}
	for id := range wf.Jobs {
		if strings.HasPrefix(id, "stop-early-") && want[id] == "" {
			t.Errorf("%s is a stop-early job outside the pinned set; a gate may be added here only after its reds are shown deterministic", id)
		}
	}
	for id, gate := range want {
		job, ok := wf.Jobs[id]
		if !ok {
			t.Errorf("ci.yml has no %s job", id)
			continue
		}
		if got := ciNeeds(t, job.Needs); !slices.Equal(got, []string{gate}) {
			t.Errorf("%s needs %v, want exactly [%s]: one gate per job, so it starts when that gate fails", id, got, gate)
		}
		for _, frag := range []string{
			"!cancelled()",
			"needs." + gate + ".result == 'failure'",
			"github.event_name == 'pull_request'",
			"github.event.pull_request.head.repo.full_name == github.repository",
			"github.actor != 'dependabot[bot]'",
		} {
			if !strings.Contains(job.If, frag) {
				t.Errorf("%s: if: lacks %q, so it could fire on a push, a fork, a skipped gate or a cancelled run: %s", id, frag, job.If)
			}
		}
		if len(job.Permissions) != 1 || job.Permissions["actions"] != "write" {
			t.Errorf("%s: permissions are %v, want only actions: write", id, job.Permissions)
		}
		cancels := false
		for _, st := range job.Steps {
			if st.Uses != "" {
				t.Errorf("%s runs `uses: %s`; this job must check out and run nothing from the pull request", id, st.Uses)
			}
			if strings.Contains(st.Run, `actions/runs/$RUN_ID/cancel`) {
				cancels = true
			}
		}
		if !cancels {
			t.Errorf("%s never calls the run cancel endpoint", id)
		}
		if blk := ciJobBlock(t, raw, id); strings.Contains(blk, "secrets.") || strings.Contains(blk, "PAT") {
			t.Errorf("%s names a stored credential; only github.token is allowed:\n%s", id, blk)
		}
	}
	// `actions: write` stays with the jobs that need it.
	allowed := map[string]bool{"go-cache-prune": true, "train-suites": true}
	for id, job := range wf.Jobs {
		if job.Permissions["actions"] == "write" && !allowed[id] && want[id] == "" {
			t.Errorf("%s holds actions: write; only the stop-early jobs, go-cache-prune and train-suites may", id)
		}
	}
}

// TestCIGoLintRunsGuardsFirstAndGatesBuild pins the lint job: it keeps the
// check name `go (lint)`, runs the whole-tree guard list before `make lint`, and
// `build` (a required context) needs it, so splitting it out of the `go` matrix
// dropped no gate.
func TestCIGoLintRunsGuardsFirstAndGatesBuild(t *testing.T) {
	wf, _ := loadCIWorkflow(t)
	lint, ok := wf.Jobs["go-lint"]
	if !ok {
		t.Fatal("ci.yml has no go-lint job")
	}
	if lint.Name != "go (lint)" {
		t.Errorf("go-lint reports as %q, want %q", lint.Name, "go (lint)")
	}
	guard, mk := -1, -1
	for i, st := range lint.Steps {
		if strings.Contains(st.Run, "candidate-tests.py guards") {
			guard = i
			if !strings.Contains(st.Run, "go test -count=1 -run") || !strings.Contains(st.Run, "exit \"$rc\"") {
				t.Errorf("the guard step no longer runs each package and fails on any red:\n%s", st.Run)
			}
		}
		if st.Run == "make build tidy-check lint" {
			mk = i
		}
	}
	if guard < 0 || mk < 0 || guard > mk {
		t.Errorf("go-lint must run the guard list (step %d) before `make build tidy-check lint` (step %d)", guard, mk)
	}
	for _, leg := range wf.Jobs["go"].Strategy.Matrix["include"].([]any) {
		if leg.(map[string]any)["suite"] == "lint" {
			t.Error("the lint leg is back in the go matrix as well as go-lint")
		}
	}
	if got := ciNeeds(t, wf.Jobs["build"].Needs); !slices.Equal(got, []string{"changes", "go", "go-lint"}) {
		t.Errorf("build needs %v, want [changes go go-lint]", got)
	}
}

// requiredCIContexts is branch protection's required_status_checks.contexts on
// main (gh api repos/cjohnstoniv/wardyn/branches/main/protection). Renaming a job or a matrix cell that produces one would
// leave the pull request waiting on a check that never reports.
var requiredCIContexts = []string{
	"build", "dco", "gates (gitleaks)", "gates (govulncheck)", "gates (license-headers)",
	"gates (licenses)", "gates (staticcheck)", "notices", "ui", "trivy (wardynd)",
	"trivy (wardyn-proxy)", "trivy (agent-base)", "trivy (agent-codex-cli)",
	"trivy (agent-aws-sso)", "test-pg", "conformance", "conformance-k8s", "helm", "compose",
	"helm-install-test", "envbuild-integration", "trivy (agent-vscode)", "trivy (agent-novnc)",
	"diagrams",
}

func TestCIRequiredContextsAreStillProduced(t *testing.T) {
	wf, _ := loadCIWorkflow(t)
	produced := map[string]bool{}
	matrixVals := func(m map[string]any, key string) []string {
		var out []string
		if l, ok := m[key].([]any); ok {
			for _, v := range l {
				out = append(out, fmt.Sprint(v))
			}
		}
		if inc, ok := m["include"].([]any); ok {
			for _, e := range inc {
				if v, ok := e.(map[string]any)[key]; ok {
					out = append(out, fmt.Sprint(v))
				}
			}
		}
		return out
	}
	expr := regexp.MustCompile(`\$\{\{ matrix\.(\w+) \}\}`)
	for id, job := range wf.Jobs {
		name := job.Name
		switch {
		case name == "" && len(job.Strategy.Matrix) > 0:
			for key := range job.Strategy.Matrix {
				if key == "include" {
					continue
				}
				for _, v := range matrixVals(job.Strategy.Matrix, key) {
					produced[id+" ("+v+")"] = true
				}
			}
		case name == "":
			produced[id] = true
		case expr.MatchString(name):
			key := expr.FindStringSubmatch(name)[1]
			for _, v := range matrixVals(job.Strategy.Matrix, key) {
				produced[expr.ReplaceAllString(name, v)] = true
			}
		default:
			produced[name] = true
		}
	}
	for _, ctx := range requiredCIContexts {
		if !produced[ctx] {
			t.Errorf("required context %q is no longer produced by any ci.yml job", ctx)
		}
	}
}
