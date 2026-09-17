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
	"sync"
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
	// What the single Enter on that prompt must produce: the REPL, where the
	// hosts this test exists to measure are actually fetched.
	bootEgressWantREPL = "Amazon Bedrock"
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
// walks the run the way a human does: the workspace-trust prompt (the one screen
// a human is meant to answer, and NOT the theme picker or "Security notes"),
// then the single Enter that prompt's default invites, then the REPL — asserting
// throughout that no first-use approval was raised and no host denied. A third
// arm runs the AUTONOMOUS `claude -p` shape, which meets no dialog at all.
//
// RED at d8f26511: the boot parks on `downloads.claude.ai` and `github.com` —
// the field report's pair — and the first screen is the theme picker. Those two
// are the CLI auto-installing the OFFICIAL PLUGIN MARKETPLACE on first REPL
// start (GCS fetch, git fallback), NOT the updater: on this image's npm install
// the update check dials registry.npmjs.org, which the default policy already
// allows, so it never parked.
//
// Which is why this test presses Enter and keeps measuring. An earlier revision
// stopped at the trust prompt and reported "zero approvals" while the product
// was one keystroke away from dialling both — a measurement that agreed with a
// wrong explanation because it never reached the code that fetches.
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

	// PHASE 1 — up to the workspace-trust prompt, the one screen a human is meant
	// to answer.
	tail := bootEgressWatch(sess)
	screen := tail.waitFor(bootEgressWantTrust, 45*time.Second)
	t.Logf("first screen, rendered:\n%s", screen)

	claudeImage := strings.Contains(image, "claude")
	if claudeImage {
		if !strings.Contains(screen, bootEgressWantScreen) || !strings.Contains(screen, bootEgressWantTrust) {
			t.Errorf("the first screen is not the workspace-trust prompt (%q + %q)\nscreen:\n%s",
				bootEgressWantScreen, bootEgressWantTrust, screen)
		}
		for _, unwanted := range bootEgressUnwantedScreens {
			if strings.Contains(screen, unwanted) {
				t.Errorf("the first screen still shows %q — the onboarding seed did not land, so an operator meets a product tour before their agent\nscreen:\n%s", unwanted, screen)
			}
		}
	} else {
		t.Logf("%s is not the claude-code image; screen assertions skipped (the host measurement still applies)", image)
	}

	// PHASE 2 — PAST the dialog, which is where this test used to be blind. The
	// hosts that matter are fetched once the REPL is actually up, so a run that
	// stops at a pre-REPL prompt can report "zero approvals" while the product is
	// about to dial two.
	//
	// Press the one key the human presses (the default option is already "Yes, I
	// trust this folder") and wait for the prompt. Then keep pressing, bounded:
	// on an image WITHOUT the onboarding seed the run is parked behind the theme
	// picker and the Security-notes page as well, and those extra Enters are what
	// a human would press to get to the same place. Phase 1 already asserts the
	// fixed image needs none of them; here the count is EVIDENCE, and reaching the
	// REPL at all is what makes the measurement below mean anything.
	enters := 0
	var repl string
	for enters < 4 {
		if _, err := sess.Write([]byte("\r")); err != nil {
			t.Fatalf("send Enter (#%d): %v", enters+1, err)
		}
		enters++
		repl = tail.waitFor(bootEgressWantREPL, 30*time.Second)
		if strings.Contains(repl, bootEgressWantREPL) {
			break
		}
	}
	t.Logf("reached the CLI prompt after %d Enter(s); rendered:\n%s", enters, repl)
	if claudeImage && enters > 1 {
		t.Logf("NOTE: %d Enters were needed — this image parks behind onboarding screens the seed removes", enters)
	}
	if !strings.Contains(repl, bootEgressWantREPL) {
		t.Errorf("the CLI prompt (%q) never appeared after %d Enter(s); everything the REPL fetches is unmeasured\nscreen:\n%s", bootEgressWantREPL, enters, repl)
	}
	// The REPL is up: give its first-start fetches (the plugin-marketplace
	// auto-install among them) room to happen before the proxy log is read.
	time.Sleep(20 * time.Second)

	// Give the sink's async worker a moment after the read window closes.
	time.Sleep(3 * time.Second)
	logs := bootEgressProxyLogs(t, runID)
	bootEgressAssertQuiet(t, image, "interactive attach, past the trust prompt", logs)

	// PHASE 3 — the AUTONOMOUS shape, which meets no dialog at all and is what the
	// live walk's case G launches. Its first-use approvals are the ones a member
	// sees on the run they did not attach to, so they get their own arm rather
	// than an assumption that the interactive measurement covers them.
	if !claudeImage {
		return
	}
	if _, err := sub.Exec(ctx, sb.Ref, []string{
		"agent-run", "say hello in five words",
	}); err != nil {
		t.Fatalf("Exec autonomous agent-run: %v", err)
	}
	time.Sleep(45 * time.Second)
	autoLogs := bootEgressProxyLogs(t, runID)
	bootEgressAssertQuiet(t, image, "autonomous `claude -p` run", autoLogs)
}

// bootEgressAssertQuiet is the whole verdict: what the sandbox dialled, and
// whether any of it parked. Logged pass or fail — the measurement is the
// deliverable, not just the verdict.
func bootEgressAssertQuiet(t *testing.T, image, phase, logs string) {
	t.Helper()
	hosts, pending, denied := bootEgressDecisions(logs)
	t.Logf("hosts dialled by %s (%s): %v", image, phase, hosts)
	t.Logf("proxy decision stream (%s):\n%s", phase, bootEgressDecisionLines(logs))
	if len(pending) > 0 {
		t.Errorf("%s raised %d first-use approval(s): %v\n"+
			"A member's first run must park on nothing before they have asked the agent for anything.", phase, len(pending), pending)
	}
	if len(denied) > 0 {
		t.Errorf("%s was denied %d host(s): %v\n"+
			"Every host a stock boot reaches for is the product's own bootstrap, not the user's work.", phase, len(denied), denied)
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

// bootEgressTail is the ONE reader of the PTY for the whole test. It must be
// one: a second goroutine reading the same session competes for the bytes, and
// the loser's buffer comes back empty — which reads exactly like "the screen
// never appeared" while the CLI is in fact fine.
type bootEgressTail struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func bootEgressWatch(sess runner.Session) *bootEgressTail {
	tl := &bootEgressTail{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := sess.Read(b)
			if n > 0 {
				tl.mu.Lock()
				tl.buf.Write(b[:n])
				tl.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return tl
}

func (tl *bootEgressTail) rendered() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return bootEgressPlain(tl.buf.String())
}

// waitFor polls the RENDERED screen until it contains want or the deadline
// passes, returning what it saw either way. A TUI redraws continuously, so
// watching for a marker rather than burning the clock is what lets the test
// spend its budget on the phase AFTER the dialog instead of staring at it.
func (tl *bootEgressTail) waitFor(want string, d time.Duration) string {
	deadline := time.Now().Add(d)
	for {
		got := tl.rendered()
		if strings.Contains(got, want) {
			time.Sleep(1500 * time.Millisecond) // let the frame settle
			return tl.rendered()
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
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
