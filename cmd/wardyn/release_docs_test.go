// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// readRepoDoc reads a repo-root-relative doc file (this package sits two
// levels below the repo root, same convention as version_test.go).
func readRepoDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestReleasingDocumentsVersionBump is the regression proof for W30-S1-1:
// RELEASING.md's release steps must actually instruct bumping the four
// shipped version strings cmd/wardyn/version_test.go's
// TestShippedVersionStringsAgree enforces agree — that test only checks the
// FILES agree with each other; nothing checked that the PROCESS document ever
// told a maintainer to touch them, so a by-the-book release could tag a
// version-string mismatch with the gate never re-run to catch it.
func TestReleasingDocumentsVersionBump(t *testing.T) {
	doc := readRepoDoc(t, "RELEASING.md")
	for _, want := range []string{
		"internal/version/version.go",
		"deploy/helm/wardyn/Chart.yaml",
		"ui/package.json",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("RELEASING.md's Steps never mention bumping %s", want)
		}
	}
}

// TestReleasingAndMakefileNameConformanceK8s is the regression proof for
// W30-S1-3: the pre-tag gate's "cannot run locally" job list (RELEASING.md
// and the matching release-check summary in the Makefile) must name
// conformance-k8s and helm-install-test, not just the pre-k8s-runner three.
func TestReleasingAndMakefileNameConformanceK8s(t *testing.T) {
	releasing := readRepoDoc(t, "RELEASING.md")
	if !strings.Contains(releasing, "conformance-k8s") {
		t.Error("RELEASING.md's CI job list omits conformance-k8s")
	}
	if !strings.Contains(releasing, "helm-install-test") {
		t.Error("RELEASING.md's \"cannot run locally\" list omits helm-install-test")
	}
	makefile := readRepoDoc(t, "Makefile")
	if !strings.Contains(makefile, "release-check PASSED. NOT covered here: conformance, conformance-k8s,") {
		t.Error("Makefile's release-check summary omits conformance-k8s")
	}
}

// TestContributingConformanceGateNotStale is the regression proof for the
// CONTRIBUTING.md half of W30-S1-3: the Kubernetes conformance target shipped
// in v0.5 (test/conformance/conformance_k8s_test.go, the ci.yml
// conformance-k8s job) — CONTRIBUTING.md must not still call it
// "[v0.5+ — planned]" / "has no conformance target yet".
func TestContributingConformanceGateNotStale(t *testing.T) {
	doc := readRepoDoc(t, "CONTRIBUTING.md")
	if strings.Contains(doc, "has no conformance target yet") {
		t.Error("CONTRIBUTING.md still calls the Kubernetes runner conformance-target-less; it shipped in v0.5")
	}
	if !strings.Contains(doc, "conformance-k8s") {
		t.Error("CONTRIBUTING.md's Conformance Gate section never names the shipped conformance-k8s job")
	}
}

// TestReleaseNotesMatchSBOMReality is the inverse of the guard it replaces.
//
// The original asserted that CHANGELOG.md and ROADMAP.md must call the SBOM a
// "workflow artifact", because release.yml deliberately did NOT attach one to the
// Release. That was true and worth pinning at the time. As of 0.6.2 release.yml
// attests a per-digest SBOM with cosign AND uploads it as a release asset, with a
// job step that fails if the asset did not land — so the old assertion now pins
// docs to a claim that is no longer true, which is the opposite of what a
// doc-truth guard is for.
//
// Same job, flipped: the docs must not still describe the SBOM as a
// hand-attached workflow artifact.
func TestReleaseNotesMatchSBOMReality(t *testing.T) {
	wf := readRepoDoc(t, ".github/workflows/release.yml")
	// Guard the guard: if the workflow ever stops attaching, this test is the
	// thing that should be revisited, not silently inverted again.
	if !strings.Contains(wf, "gh release upload") {
		t.Skip("release.yml no longer uploads release assets; revisit this guard rather than the docs")
	}
	for _, path := range []string{"CHANGELOG.md", "ROADMAP.md"} {
		doc := readRepoDoc(t, path)
		// CHANGELOG entries for SHIPPED releases are a historical record: at 0.6.0
		// the SBOM really was a hand-attached workflow artifact, and rewriting that
		// to match today would make the changelog lie about the past. Only the
		// unreleased section describes what is true now.
		if path == "CHANGELOG.md" {
			doc = unreleasedSection(doc)
		}
		if !strings.Contains(doc, "SBOM") {
			continue
		}
		if strings.Contains(doc, "deliberately not auto-attached") ||
			strings.Contains(doc, "attach it by hand") {
			t.Errorf("%s still says the SBOM is a hand-attached workflow artifact; release.yml attaches it and asserts it landed", path)
		}
	}
}

// TestThreatModelSSOSessionNotStale is the regression proof for W31-S1-3:
// SSO-session auth shipped in v0.5 (internal/auth/oidc, ui/.../sign-in.tsx) —
// THREAT-MODEL.md must not still call it "not yet built, unscheduled".
func TestThreatModelSSOSessionNotStale(t *testing.T) {
	doc := readRepoDoc(t, "threatmodel/THREAT-MODEL.md")
	if strings.Contains(doc, "not yet built, unscheduled") {
		t.Error(`THREAT-MODEL.md still calls the SSO session path "not yet built, unscheduled"; it shipped in v0.5`)
	}
	if !strings.Contains(doc, "SSO session (the hardened path, shipped v0.5)") {
		t.Error("THREAT-MODEL.md's SSO session bullet does not read as shipped v0.5")
	}
}

// TestReleaseWorkflowPublishesAgentAWSSSO is the regression proof for
// W5-S1-3: harnesscred.go's launchHarnessLoginRun resolves the AWS SSO
// login sandbox's image through agentImage("aws-sso", ...) — the SAME
// ghcr.io/cjohnstoniv/agent-<key>:latest fallback convention the two coding
// harnesses use — so release.yml must build+push agent-aws-sso exactly like
// agent-claude-code/agent-codex-cli, or a by-the-book Helm install (no
// WARDYN_AGENT_IMAGES override) dead-ends the guided login at
// ImagePullBackOff/"registry: denied".
func TestReleaseWorkflowPublishesAgentAWSSSO(t *testing.T) {
	doc := readRepoDoc(t, ".github/workflows/release.yml")
	if !strings.Contains(doc, "name: agent-aws-sso") {
		t.Fatal("release.yml's build matrix has no agent-aws-sso row")
	}
	if !strings.Contains(doc, "deploy/images/aws-sso/Dockerfile") {
		t.Error("release.yml's agent-aws-sso row does not point at deploy/images/aws-sso/Dockerfile")
	}
	// agentImage()'s ghcr fallback pulls the daemon's version tag (D19), which the
	// per-row semver push covers; the float-latest rows must ALSO push :latest as
	// the no-version-string fallback tag.
	if !strings.Contains(doc, `IMAGE_NAME}:latest`) {
		t.Error("release.yml's tag-compute step never publishes an agent-*:latest tag")
	}
}

// TestRunHostMapsAwsSsoImage is the regression proof for W5-S1-3's host-mode
// half: `make agent-images` builds wardyn/agent-aws-sso:local, but
// run-host.sh's WARDYN_AGENT_IMAGES default never routed the "aws-sso" agent
// id to it, so a host-mode guided AWS SSO login fell back to the unpublished
// ghcr convention ref even locally. The 0.6.5 "base" row is the same shape of
// proof for the setup connectivity probe's image (site_config_probe.go
// dispatches the "base" key). The default lives in a single-quoted shell
// variable now, so the keys are asserted unescaped.
func TestRunHostMapsAwsSsoImage(t *testing.T) {
	doc := readRepoDoc(t, "scripts/run-host.sh")
	for _, want := range []string{
		`"aws-sso":"wardyn/agent-aws-sso:local"`,
		`"base":"wardyn/agent-base:local"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("scripts/run-host.sh's default WARDYN_AGENT_IMAGES omits %s", want)
		}
	}
}

// unreleasedSection returns CHANGELOG.md's "## [Unreleased]" body — everything up
// to the first released heading. Shipped entries are history and are not rewritten
// to match later behaviour.
func unreleasedSection(doc string) string {
	i := strings.Index(doc, "## [Unreleased]")
	if i < 0 {
		return ""
	}
	rest := doc[i+len("## [Unreleased]"):]
	if j := strings.Index(rest, "\n## ["); j >= 0 {
		return rest[:j]
	}
	return rest
}
