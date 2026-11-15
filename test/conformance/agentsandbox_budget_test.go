// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

// agentsandbox_budget_test.go is an INTERNAL test like ephemeral_budget_test.go: it needs no cluster and no tag.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// warmClaimGoneWait mirrors the 15 s wait for a claimed placeholder to be gone in internal/runner/k8s/warm_claim.go,
// a k8s-tagged file this test cannot import: the test reads that line and fails when the two differ.
const warmClaimGoneWait = 15 * time.Second

// agentSandboxMeasuredCases is how many cases in ci.yml's "cases ran and passed" list ran hosted before the keep and
// warm pool cases; their go test time is ASMeasuredSuite. Every other case in the list needs a budget below.
const agentSandboxMeasuredCases = 7

// agentSandboxWorstCase is each new case's worst case: its context limit, the longest of its waits (only one wait can
// run past the limit: the call after it fails the case), plus the cleanups its helpers run after it.
var agentSandboxWorstCase = map[string]time.Duration{
	"TestAgentSandboxKeepProxyThroughService":             ASKeepServiceLimit + ASKillBound,
	"TestAgentSandboxKeepEndpointSliceElsewhereIsDropped": ASKeepSliceLimit + max(ASKeepSliceDecoyWait, ASKeepSliceGoneWait, ASKeepSliceBackWait) + ASKillBound,
	"TestAgentSandboxKeepLiveReplaceProxy":                ASKeepReplaceLimit + ASKeepReplaceWait + ASKeepHopfakeStart + ASKillBound,
	"TestAgentSandboxKeepAPISequence":                     ASKeepSequenceLimit + max(ASKeepSequenceStop, ASKeepSequenceHeld) + ASKeepSequenceSettle + ASKeepHopfakeStart + 2*ASKillBound,
	"TestAgentSandboxKeepAdmissionBoundsServices":         ASKeepAdmissionLimit,
	"TestWarmPoolPlaceholderHoldsNothing":                 ASWarmReadyWait + ASWarmCleanupBound,
	"TestWarmPoolPlaceholderClaimedAndRefilled": ASWarmReadyWait + runner.DefaultSandboxStartTimeout + warmClaimGoneWait +
		ASWarmRefillWait + ASKillBound + ASWarmCleanupBound,
}

// TestAgentSandboxCaseBudgetsFitTheJobTimeout reads ci.yml's agent-sandbox job: the cases its "ran and passed" step
// requires, the CONFORMANCE_K8S_TIMEOUT its suite step passes, and its timeout-minutes. The measured suite time plus
// the new cases' worst cases must fit that go test timeout, which must fit inside the job's. A go test timeout expiry
// is a panic that discards every verdict already produced, so a raised limit or a new case reds here instead.
func TestAgentSandboxCaseBudgetsFitTheJobTimeout(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	src, err := os.ReadFile("../../internal/runner/k8s/warm_claim.go")
	if err != nil {
		t.Fatalf("read warm_claim.go: %v", err)
	}
	if want := fmt.Sprintf("d.waitPodGone(ctx, ns, pod, %d*time.Second)", warmClaimGoneWait/time.Second); !strings.Contains(string(src), want) {
		t.Errorf("internal/runner/k8s/warm_claim.go has no %q: warmClaimGoneWait must equal the driver's wait for a claimed placeholder to go", want)
	}
	job := string(b)
	i := strings.Index(job, "\n  conformance-k8s-agent-sandbox:\n")
	if i < 0 {
		t.Fatal("ci.yml has no conformance-k8s-agent-sandbox job")
	}
	job = job[i+1:]
	if j := regexp.MustCompile(`\n  [a-z][\w-]*:\n`).FindStringIndex(job); j != nil {
		job = job[:j[0]]
	}
	find := func(re string) []string {
		m := regexp.MustCompile(re).FindStringSubmatch(job)
		if m == nil {
			t.Fatalf("the agent-sandbox job has nothing matching %s", re)
		}
		return m
	}
	timeout, err := time.ParseDuration(find(`make test-conformance-k8s CONFORMANCE_K8S_TIMEOUT=(\S+)`)[1])
	if err != nil {
		t.Fatalf("parse CONFORMANCE_K8S_TIMEOUT: %v", err)
	}
	var minutes int
	if _, err := fmt.Sscan(find(`\n    timeout-minutes: (\d+)`)[1], &minutes); err != nil {
		t.Fatalf("parse timeout-minutes: %v", err)
	}
	total, measured, listed := ASMeasuredSuite, 0, map[string]bool{}
	for _, name := range regexp.MustCompile(`Test\w+`).FindAllString(find(`(?s)for t in (.*?); do`)[1], -1) {
		listed[name] = true
		if w, ok := agentSandboxWorstCase[name]; ok {
			total += w
		} else {
			measured++
		}
	}
	for name := range agentSandboxWorstCase {
		if !listed[name] {
			t.Errorf("%s has a budget here but is not in ci.yml's list of cases that must pass", name)
		}
	}
	if measured != agentSandboxMeasuredCases {
		t.Errorf("ci.yml lists %d cases without a budget here, want the %d that ran hosted: give each new case a worst case in agentSandboxWorstCase", measured, agentSandboxMeasuredCases)
	}
	if total > timeout {
		t.Errorf("the measured suite %s plus the new cases' worst cases come to %s, above the CONFORMANCE_K8S_TIMEOUT %s the agent-sandbox suite step passes", ASMeasuredSuite, total, timeout)
	}
	if time.Duration(minutes)*time.Minute <= timeout {
		t.Errorf("the agent-sandbox job's timeout-minutes %d does not exceed CONFORMANCE_K8S_TIMEOUT %s", minutes, timeout)
	}
}
