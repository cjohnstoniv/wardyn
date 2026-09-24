// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// publish-image.yml pushes wardynd:latest, which desktop installs pull. It used
// to run on every push to main, in parallel with CI, so red commits were
// published as :latest. These two tests pin the fix: the workflow publishes
// only after scripts/ci-green-for-sha.sh says CI passed on the commit, and that
// script says "not green" for every shape of a red, pending or foreign run.

type ghWorkflow struct {
	On   map[string]any     `yaml:"on"`
	Jobs map[string]ghWfJob `yaml:"jobs"`
}

type ghWfJob struct {
	If    string        `yaml:"if"`
	Needs any           `yaml:"needs"`
	Steps []ghWfJobStep `yaml:"steps"`
}

type ghWfJobStep struct {
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Run  string            `yaml:"run"`
}

func TestPublishImagePublishesOnlyAfterGreenCI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "publish-image.yml"))
	if err != nil {
		t.Fatalf("read publish-image.yml: %v", err)
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("parse publish-image.yml: %v", err)
	}

	for _, trig := range []string{"push", "pull_request", "pull_request_target"} {
		if _, ok := wf.On[trig]; ok {
			t.Errorf("publish-image.yml triggers on %q: it would publish without waiting for CI", trig)
		}
	}
	wr, _ := wf.On["workflow_run"].(map[string]any)
	if wr == nil || !slices.Contains(toStrings(wr["workflows"]), "CI") || !slices.Equal(toStrings(wr["types"]), []string{"completed"}) {
		t.Errorf("publish-image.yml must trigger on workflow_run {workflows: [CI], types: [completed]}, got %v", wf.On["workflow_run"])
	}

	publishers := 0
	for name, job := range wf.Jobs {
		if !slices.ContainsFunc(job.Steps, func(s ghWfJobStep) bool {
			return strings.HasPrefix(s.Uses, "docker/build-push-action@") || strings.Contains(s.Run, "cosign sign")
		}) {
			continue
		}
		publishers++
		needs := toStrings(job.Needs)
		if len(needs) != 1 {
			t.Errorf("job %s pushes an image but needs %v; it must need exactly the CI gate job", name, needs)
			continue
		}
		gate := needs[0]
		if want := "needs." + gate + ".outputs.green == 'true'"; strings.TrimSpace(job.If) != want {
			t.Errorf("job %s pushes an image with if %q; want exactly %q", name, job.If, want)
		}
		if ref := job.Steps[0].With["ref"]; job.Steps[0].Uses == "" || !strings.HasPrefix(job.Steps[0].Uses, "actions/checkout@") || ref != "${{ needs."+gate+".outputs.sha }}" {
			t.Errorf("job %s must first check out the commit the gate cleared (ref ${{ needs.%s.outputs.sha }}), got %q with ref %q", name, gate, job.Steps[0].Uses, ref)
		}
		gj, ok := wf.Jobs[gate]
		if !ok {
			t.Errorf("job %s needs %s, which does not exist", name, gate)
			continue
		}
		gateRuns := false
		for _, s := range gj.Steps {
			if strings.Contains(s.Run, "scripts/ci-green-for-sha.sh") {
				gateRuns = true
				if strings.Count(s.Run, "green=true") != 1 || !strings.Contains(s.Run, "0) green=true") {
					t.Errorf("gate job %s must set green=true only on the script's exit 0, got:\n%s", gate, s.Run)
				}
			}
		}
		if !gateRuns {
			t.Errorf("gate job %s never runs scripts/ci-green-for-sha.sh", gate)
		}
	}
	if publishers == 0 {
		t.Fatal("found no job in publish-image.yml that pushes or signs an image; this guard would pass vacuously")
	}
}

func TestCIGreenForSHA_RedSHASkips(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("jq not found; the publish gate needs it")
		}
		t.Skip("jq not found")
	}
	script := filepath.Join(repoRoot(t), "scripts", "ci-green-for-sha.sh")
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "owner/wardyn"
	run := func(id, event, status, conclusion, headRepo string) string {
		return `{"id":` + id + `,"head_sha":"` + sha + `","head_branch":"main","event":"` + event + `","status":"` + status +
			`","conclusion":` + conclusion + `,"head_repository":{"full_name":"` + headRepo + `"}}`
	}
	onRelease := func(r string) string {
		return strings.Replace(r, `"head_branch":"main"`, `"head_branch":"release/0.7"`, 1)
	}
	runs := func(rs ...string) string { return `{"workflow_runs":[` + strings.Join(rs, ",") + `]}` }
	green := run("1", "push", "completed", `"success"`, repo)
	// A release fast-forwards the same commit onto main and release/0.7.
	releasePending := onRelease(run("2", "push", "in_progress", `null`, repo))

	cases := []struct {
		name   string
		sha    string
		branch string
		out    string
		ghFail bool
		want   int
	}{
		{"green push run", sha, "main", runs(green), false, 0},
		{"red push run", sha, "main", runs(run("1", "push", "completed", `"failure"`, repo)), false, 1},
		{"green run beside a red run", sha, "main", runs(green, run("2", "push", "completed", `"failure"`, repo)), false, 1},
		{"cancelled run", sha, "main", runs(run("1", "push", "completed", `"cancelled"`, repo)), false, 1},
		{"run still in progress", sha, "main", runs(run("1", "push", "in_progress", `null`, repo)), false, 1},
		{"no run at all", sha, "main", runs(), false, 1},
		{"only a pull request run", sha, "main", runs(run("1", "pull_request", "completed", `"success"`, repo)), false, 1},
		{"only a fork's run", sha, "main", runs(run("1", "push", "completed", `"success"`, "fork/wardyn")), false, 1},
		{"run for another commit", sha, "main", strings.Replace(runs(green), sha, strings.Repeat("f", 40), 1), false, 1},
		{"green on main beside a pending release branch run", sha, "main", runs(green, releasePending), false, 0},
		{"green only on the release branch", sha, "main", runs(onRelease(green)), false, 1},
		{"no branch counts every branch's runs", sha, "", runs(green, releasePending), false, 1},
		{"gh fails", sha, "main", "", true, 2},
		{"unreadable response", sha, "main", `{"message":"Bad credentials"}`, false, 2},
		{"not a sha", "main", "main", runs(green), false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\ncat \"$FAKE_GH_OUT\"\n"
			if tc.ghFail {
				fake = "#!/bin/sh\necho 'HTTP 502' >&2\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			outFile := filepath.Join(bin, "out.json")
			if err := os.WriteFile(outFile, []byte(tc.out), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script, tc.sha, tc.branch)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY="+repo,
				"FAKE_GH_OUT="+outFile)
			out, err := cmd.CombinedOutput()
			got := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("run script: %v", err)
			}
			if got != tc.want {
				t.Errorf("exit %d, want %d; output:\n%s", got, tc.want, out)
			}
		})
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
