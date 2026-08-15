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

// TestReleaseNotesDoNotOverclaimSBOMAttachment is the regression proof for
// W30-S1-4: release.yml deliberately does NOT attach the release SBOM to the
// GitHub Release object (see that workflow's own header comment) — it uploads
// a workflow artifact for hand-attachment. CHANGELOG.md and ROADMAP.md must
// not claim otherwise.
func TestReleaseNotesDoNotOverclaimSBOMAttachment(t *testing.T) {
	for _, path := range []string{"CHANGELOG.md", "ROADMAP.md"} {
		doc := readRepoDoc(t, path)
		if !strings.Contains(doc, "SBOM") {
			continue // this doc's SBOM mention may move; only assert when present
		}
		if strings.Contains(doc, "attaches a CycloneDX SBOM release asset") ||
			strings.Contains(doc, "attaches a CycloneDX SBOM") {
			t.Errorf("%s claims release.yml attaches the SBOM to the Release; it only uploads a workflow artifact", path)
		}
		if !strings.Contains(doc, "workflow") || !strings.Contains(doc, "artifact") {
			t.Errorf("%s's SBOM mention should call the SBOM a workflow artifact, matching what release.yml actually does", path)
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
	// agentImage()'s ghcr fallback is hardcoded to ":latest" (never the release
	// version) — the agent-* rows must publish that tag, not only the semver one.
	if !strings.Contains(doc, `IMAGE_NAME}:latest`) {
		t.Error("release.yml's tag-compute step never publishes an agent-*:latest tag")
	}
}

// TestRunHostMapsAwsSsoImage is the regression proof for W5-S1-3's host-mode
// half: `make agent-images` builds wardyn/agent-aws-sso:local, but
// run-host.sh's WARDYN_AGENT_IMAGES default never routed the "aws-sso" agent
// id to it, so a host-mode guided AWS SSO login fell back to the unpublished
// ghcr convention ref even locally.
func TestRunHostMapsAwsSsoImage(t *testing.T) {
	doc := readRepoDoc(t, "scripts/run-host.sh")
	if !strings.Contains(doc, `\"aws-sso\":\"wardyn/agent-aws-sso:local\"`) {
		t.Error(`scripts/run-host.sh's default WARDYN_AGENT_IMAGES omits "aws-sso":"wardyn/agent-aws-sso:local"`)
	}
}
