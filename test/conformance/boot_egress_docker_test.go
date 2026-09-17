// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

// boot_egress_docker_test.go measures what a claude-code agent DIALS before a
// human has asked it to do anything, against the REAL wardyn-proxy sidecar and
// a real allowlist policy.
//
// The sibling file conformance_docker_test.go runs the runner-contract suite
// with ProxyImage: "busybox:latest" — a sidecar that exists on the network and
// relays nothing, which is all those assertions need. That is exactly why this
// lives in its own file: the question here is what the proxy DECIDES, so the
// proxy has to be the real one.
//
// Observation channel: wardyn-proxy mirrors every egress.DecisionLog to its own
// stdout as a JSON line (decisionSink.mirror, internal/egress/proxy), so the
// sidecar's container log IS the list of hosts the sandbox reached for and what
// each one was decided as. No control plane is needed to see it, which matters
// because the control plane is also what a first-use approval would be raised
// TO: with none reachable, `deny_with_review` still records the attempt as a
// `pending` decision. Zero pending rows is therefore the strongest available
// statement of "this boot raised no first-use approval".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The model host this run is allowed to reach, and nothing else — the shape a
// bedrock_sso estate actually ships (PrivateLink VPC endpoint, one region).
// Anything ELSE the boot dials is the finding.
const bootEgressModelHost = "bedrock-runtime.us-east-1.amazonaws.com"

// The first screen the W0 spike pre-declared for the no-seed interactive shape —
// the console's default, and the owner's literal path. FIXED, never "whatever
// stops prompting": the value of this assertion is that it names the ONE screen
// a human is meant to answer.
const (
	bootEgressWantScreen = "Accessing workspace:"
	bootEgressWantTrust  = "trust this folder"
)

// The screens the onboarding seed removes. Seeing either means the seed did not
// land, and an operator is looking at a product tour instead of their agent.
var bootEgressUnwantedScreens = []string{"Security notes", "Choose the text style"}

// The sidecar under test. Unlike the sibling suite's busybox stand-in this has
// to be the real binary, because the thing being measured is its decisions.
// Built by `make agent-images` / compose, same as every other :local tag.
const bootEgressProxyImage = "wardyn/wardyn-proxy:local"

// TestBootEgress_NoFirstUseApproval — finding 5, measured rather than argued.
//
// Boots the claude-code image as a real INTERACTIVE run behind a real
// wardyn-proxy carrying a model-host-only allowlist with
// first_use_approval=deny_with_review, attaches the way the console does, and
// asserts three things over the next 60s: the sandbox raised no first-use
// approval, no host was denied, and the first screen is the workspace-trust
// prompt (not the theme picker, not "Security notes").
//
// RED at d8f26511, measured rather than assumed: the boot raised ONE first-use
// approval — `raw.githubusercontent.com`, the CLI's changelog fetch, which
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 removes — and the first screen was
// the theme picker. The reported `downloads.claude.ai` park did NOT reproduce
// inside this window, which is itself worth knowing: the unseeded CLI stops on
// its onboarding screens, so some of its startup fetches never happen until a
// human has clicked through them. Seeding onboarding therefore has to be
// measured TOGETHER with disabling the updater, not after it — which is what
// this test does.
//
// The host list and the raw decision rows are logged on every run, pass or fail:
// the measurement is the point, not just the verdict.
func TestBootEgress_NoFirstUseApproval(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping boot-egress measurement")
	}
	image := "wardyn/agent-claude-code:local"
	if v := strings.TrimSpace(os.Getenv("WARDYN_TEST_AGENT_IMAGE")); v != "" {
		image = v
	}

	sub, err := docker.New(docker.Config{ProxyImage: bootEgressProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureConformanceNetwork(t, "wardyn-internal")

	runID := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	proxyURL := fmt.Sprintf("http://wardyn-proxy:%d", runner.ProxyListenPort)
	spec := runner.SandboxSpec{
		RunID:            runID,
		Image:            image,
		ConfinementClass: types.CC1,
		// Interactive: the driver launches `agent-run --idle` as the container's
		// whole main process, which is the boot this test is about.
		Interactive: true,
		Env: map[string]string{
			"HTTP_PROXY":       proxyURL,
			"HTTPS_PROXY":      proxyURL,
			"WARDYN_PROXY_URL": proxyURL,
			"NO_PROXY":         "wardyn-proxy,localhost,127.0.0.1,::1",
			// The bedrock_sso lane, minus the credentials: the first model call
			// fails for want of them, which is AFTER the prompt and bounds
			// nothing here. CLAUDE_CONFIG_DIR is deliberately absent — dispatch
			// does not set it on this lane, so the seed's second file is its
			// ${CLAUDE_CONFIG_DIR:-$HOME/.claude} fallback.
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION":              "us-east-1",
			// The console's DEFAULT interactive shape: land in the agent, no
			// seed. No WARDYN_INTERACTIVE_SEED, so no boot session — the agent
			// starts in the attach shell, which is the path a human walks.
			"WARDYN_INTERACTIVE_START": "agent",
		},
		ProxyConfig: runner.ProxyConfig{
			RunToken: "boot-egress-test-token",
			// Deliberately unreachable: a first-use approval has nowhere to go,
			// so it lands as a `pending` decision row instead of a queue entry.
			// That is what makes "zero approvals" observable without a control
			// plane. Discard port, so the dial refuses instantly.
			ControlPlaneURL: "http://127.0.0.1:9",
			Policy: types.RunPolicySpec{
				AllowedDomains:      []string{bootEgressModelHost},
				FirstUseApproval:    types.FirstUseDenyWithReview,
				MinConfinementClass: types.CC1,
			},
		},
	}

	sb, err := sub.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox(%s): %v", image, err)
	}
	t.Cleanup(func() {
		if err := sub.KillSandbox(context.Background(), sb.Ref); err != nil {
			t.Logf("KillSandbox: %v (leaked containers for run %s)", err, runID)
		}
	})

	// Attach exactly as the console does — the driver's own chain, which is
	// `tmux new-session -A -s wardyn bash` under a PTY. With
	// WARDYN_INTERACTIVE_START=agent the image's ~/.bashrc starts `claude` in
	// that shell: this IS the owner's path, not a stand-in for it.
	sess, err := sub.Attach(ctx, sb.Ref, runner.AttachOptions{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sess.Close() //nolint:errcheck // detach only; the sandbox is torn down above

	raw := bootEgressReadFor(sess, 60*time.Second)
	screen := bootEgressPlain(raw)
	t.Logf("first screen (%d raw bytes), rendered:\n%s", len(raw), screen)

	// Give the sink's async worker a moment after the read window closes.
	time.Sleep(2 * time.Second)
	logs := bootEgressProxyLogs(t, runID)
	hosts, pending, denied := bootEgressDecisions(logs)
	// The MEASUREMENT is the point, not just the verdict: log the host set and
	// the decision stream it came from, pass or fail, so a run of this test is
	// self-contained evidence of what the image dialled.
	t.Logf("hosts dialled by %s at boot: %v", image, hosts)
	t.Logf("proxy decision stream:\n%s", bootEgressDecisionLines(logs))

	if len(pending) > 0 {
		t.Errorf("the boot raised %d first-use approval(s): %v\n"+
			"A member's first run must park on nothing before they have asked the agent for anything.", len(pending), pending)
	}
	if len(denied) > 0 {
		t.Errorf("the boot was denied %d host(s): %v\n"+
			"Every host a stock boot reaches for is the product's own bootstrap, not the user's work.", len(denied), denied)
	}

	// The screen assertions are Claude Code's screens. The host measurement above
	// is not — point WARDYN_TEST_AGENT_IMAGE at another agent image and this test
	// measures ITS boot egress, which is how the codex-cli image was checked for
	// a boot host of its own without forking a second copy of all of the above.
	if !strings.Contains(image, "claude") {
		t.Logf("%s is not the claude-code image; screen assertions skipped (the host measurement above still applies)", image)
		return
	}
	if !strings.Contains(screen, bootEgressWantScreen) || !strings.Contains(screen, bootEgressWantTrust) {
		t.Errorf("the first screen is not the workspace-trust prompt (%q + %q)\nscreen:\n%s",
			bootEgressWantScreen, bootEgressWantTrust, screen)
	}
	for _, unwanted := range bootEgressUnwantedScreens {
		if strings.Contains(screen, unwanted) {
			t.Errorf("the first screen still shows %q — the onboarding seed did not land, so an operator meets a product tour before their agent\nscreen:\n%s", unwanted, screen)
		}
	}
}

// A TUI does not put spaces between words: Claude Code advances the cursor with
// CSI-C instead, so the raw PTY bytes read `Accessing\x1b[Cworkspace:` and a
// plain substring match on them fails on a screen that is perfectly correct.
// Cursor-forward becomes a space, every other escape sequence is dropped, and
// all whitespace collapses — so an assertion is about what a HUMAN sees,
// independent of line wrapping and redraws.
var (
	bootEgressCursorFwd = regexp.MustCompile(`\x1b\[[0-9;?]*C`)
	bootEgressEscape    = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)
	bootEgressSpace     = regexp.MustCompile(`\s+`)
)

func bootEgressPlain(raw string) string {
	s := bootEgressCursorFwd.ReplaceAllString(raw, " ")
	s = bootEgressEscape.ReplaceAllString(s, "")
	return strings.TrimSpace(bootEgressSpace.ReplaceAllString(s, " "))
}

// bootEgressReadFor drains the PTY for d, returning everything it saw. A TUI
// keeps redrawing, so this reads to the deadline rather than to a marker.
func bootEgressReadFor(sess runner.Session, d time.Duration) string {
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, sess)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
	return buf.String()
}

// bootEgressProxyLogs returns the wardyn-proxy sidecar's whole container log.
// The sidecar runs without a TTY, so the stream is Docker's multiplexed frame
// protocol and has to be demultiplexed before it can be parsed as JSON lines.
func bootEgressProxyLogs(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatalf("bootEgressProxyLogs: create client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rc, err := cli.ContainerLogs(ctx, "wardyn-proxy-"+runID.String(), dockerclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		t.Fatalf("bootEgressProxyLogs: container logs: %v", err)
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("bootEgressProxyLogs: read: %v", err)
	}
	return bootEgressDemux(b)
}

// bootEgressDemux strips Docker's 8-byte stream frame headers. It tolerates a
// log that is NOT framed (some daemons/TTY configurations), so a header-less
// stream passes through unchanged rather than being silently mangled into
// nothing — which would make every assertion below vacuously pass.
func bootEgressDemux(b []byte) string {
	var out bytes.Buffer
	for len(b) >= 8 {
		if b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			break // not a frame header: treat the remainder as raw bytes
		}
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		if n < 0 || n > len(b)-8 {
			break
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	out.Write(b)
	return out.String()
}

// bootEgressDecisions parses the proxy's mirrored decision stream into the
// sorted set of hosts it saw, plus the hosts whose decision was `pending` (a
// first-use approval) or `deny`.
func bootEgressDecisions(logs string) (hosts, pending, denied []string) {
	seen, pend, den := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"decision"`) {
			continue
		}
		var dl egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &dl); err != nil || dl.Request.Host == "" {
			continue
		}
		seen[dl.Request.Host] = true
		switch dl.Decision {
		case egress.Pending:
			pend[dl.Request.Host] = true
		case egress.Deny:
			den[dl.Request.Host] = true
		case egress.Allow:
		}
	}
	return bootEgressKeys(seen), bootEgressKeys(pend), bootEgressKeys(den)
}

func bootEgressKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bootEgressDecisionLines keeps only the proxy's mirrored decision rows, so the
// logged stream is the evidence and not the sidecar's whole startup chatter.
func bootEgressDecisionLines(logs string) string {
	var keep []string
	for _, line := range strings.Split(logs, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "{") && strings.Contains(line, `"decision"`) {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		return "(no decision rows — the sandbox dialled nothing through the proxy)"
	}
	return strings.Join(keep, "\n")
}
