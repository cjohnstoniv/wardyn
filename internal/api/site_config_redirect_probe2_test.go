// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runRedirectProbe runs the REAL redirectProbeScript with a hostname-shaped To
// (no --connect-to swap) and returns the exit code the classifier will see.
// Proxy env is cleared so a developer shell's http_proxy can never route the
// loopback dials.
func runRedirectProbe(t *testing.T, toURL, fromURL string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", redirectProbeScript)
	cmd.Env = append(cmd.Environ(),
		"WARDYN_PROBE_TO_URL="+toURL,
		"WARDYN_PROBE_TO_CONNECT=",
		"WARDYN_PROBE_FROM_URL="+fromURL,
		"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=",
		"no_proxy=", "NO_PROXY=",
	)
	_ = cmd.Run()
	return cmd.ProcessState.ExitCode()
}

// bareCurlExit reports what curl's OWN exit code is for a direct dial of url on
// this box's curl build, so the TLS arms below assert the script's mapping of a
// code that actually occurs here rather than a code this repo assumes.
func bareCurlExit(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", "-sS", "-o", "/dev/null",
		"--connect-timeout", "5", "--max-time", "15", "--noproxy", "*", url)
	cmd.Env = append(cmd.Environ(), "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=")
	_ = cmd.Run()
	return cmd.ProcessState.ExitCode()
}

// TestRedirectProbe2_ClassifiesAnsweredVsUnreachable is probe 2's classification
// table. The question it answers is "did the public From host answer a DIRECT
// dial?", NOT "did the fetch succeed" — those differ on every HTTP error status,
// and collapsing them (which -f did) reported a wide-open network as enforced.
//
// Every "answered" row must exit the bypass sentinel; the unreachable row must
// exit 0 (reached/enforced), which is also the pin against over-correcting the
// other way and calling a genuinely blocked host a bypass.
func TestRedirectProbe2_ClassifiesAnsweredVsUnreachable(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	answers := func(status int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusInternalServerError} {
		t.Run("From answers "+http.StatusText(status), func(t *testing.T) {
			if got := runRedirectProbe(t, mirror.URL, answers(status).URL); got != redirectProbeBypassCode {
				t.Fatalf("exit = %d, want %d (bypass): the public host ANSWERED %d on a direct dial, so the confinement class does not block it",
					got, redirectProbeBypassCode, status)
			}
		})
	}

	t.Run("From refuses the connection", func(t *testing.T) {
		// Port 1 on loopback: refused instantly, no network dependency.
		if got := runRedirectProbe(t, mirror.URL, "http://127.0.0.1:1"); got != 0 {
			t.Fatalf("exit = %d, want 0 (reached): a refused direct dial is what enforcement looks like", got)
		}
	})
}

// TestRedirectProbe2_TLSLevelAnswerIsBypass pins the half an HTTP-status-only
// reading would miss: a dial that completed TCP and then failed inside TLS still
// PROVES the confinement class let the connection out to the public host. Both
// shapes here are ones a real network produces — a TLS-intercepting middlebox
// (untrusted cert) and a port that is not speaking TLS at all.
func TestRedirectProbe2_TLSLevelAnswerIsBypass(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	selfSigned := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer selfSigned.Close()
	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plaintext.Close()

	cases := []struct{ name, from string }{
		{"an untrusted certificate (the intercepting-middlebox shape)", selfSigned.URL},
		{"a port that does not speak TLS", "https://" + strings.TrimPrefix(plaintext.URL, "http://")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := bareCurlExit(t, c.from)
			if rc == 0 {
				t.Fatalf("this curl fetched %s cleanly (exit 0) — the row proves nothing about the TLS arm", c.from)
			}
			if !strings.Contains(" 35 52 56 60 ", " "+strconv.Itoa(rc)+" ") {
				t.Skipf("this curl build reports %d for %s, which is not one of the post-connect codes the script classifies", rc, c.from)
			}
			if got := runRedirectProbe(t, mirror.URL, c.from); got != redirectProbeBypassCode {
				t.Fatalf("exit = %d, want %d (bypass): curl %d means the TCP connection to the public host succeeded",
					got, redirectProbeBypassCode, rc)
			}
		})
	}
}

// TestRedirectProbeScript_Probe2DoesNotFailOnHTTPErrors pins the SHAPE, so the
// collapse cannot be reintroduced by a one-flag edit: probe 2's dial must read
// the status code and must NOT carry -f, which turns an answer into a failure.
// Probe 1 keeps -f — there an HTTP error IS the mirror being unreachable.
func TestRedirectProbeScript_Probe2DoesNotFailOnHTTPErrors(t *testing.T) {
	var probe2 string
	for _, line := range strings.Split(redirectProbeScript, "\n") {
		if strings.Contains(line, "--noproxy") {
			probe2 = line
		}
	}
	if probe2 == "" {
		t.Fatal("probe 2's direct dial (--noproxy) is gone from the script")
	}
	if strings.Contains(probe2, " -f ") {
		t.Errorf("probe 2 carries -f: %q — an HTTP error response is an ANSWER (bypass), never a blocked host", probe2)
	}
	if !strings.Contains(probe2, "%{http_code}") {
		t.Errorf("probe 2 must read the status code it was answered with: %q", probe2)
	}
	if !strings.Contains(redirectProbeScript, "to() { curl -sS -f ") {
		t.Error("probe 1 must KEEP -f: there an HTTP error means the mirror did not serve the request")
	}
}

// TestRedirectProbe2_AcceptAndHoldIsBypass drives the SHIPPED script against a
// public "From" that accepts the TCP connection and then says nothing — a
// tarpit, an accept-and-hold load balancer, or simply a host slower than the
// probe's budget. curl reports 28 for that, the SAME code it reports for a dial
// that never left the sandbox, so an exit-code-only reading called a wide-open
// network "correctly blocked when dialed directly (redirect enforced)". The
// connection fact is in curl's own -w (%{num_connects} = 1 here, 0 for a real
// block, as the refused row of TestRedirectProbe2_ClassifiesAnsweredVsUnreachable
// keeps proving), so the verdict must follow it: bypass.
func TestRedirectProbe2_AcceptAndHoldIsBypass(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accepted and held: never read, never written, never closed until
			// the test's own cleanup tears the listener down.
			defer c.Close() //nolint:revive // held open deliberately for the life of the probe
		}
	}()
	t.Cleanup(func() { <-done })

	// Both schemes, because they stall at different layers and only one of them
	// was pinned: https:// stalls inside the TLS handshake (the tarpitting
	// middlebox shape) and http:// stalls waiting for a response line. curl
	// reports 28 with num_connects=1 for both, so both are bypass — the plain
	// http:// row is the one an internal mirror's From actually wears.
	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme+" accepted and held", func(t *testing.T) {
			from := scheme + "://" + ln.Addr().String() + "/"
			if got := runRedirectProbe(t, mirror.URL, from); got != redirectProbeBypassCode {
				t.Fatalf("exit = %d, want %d (bypass): %s ACCEPTED the sandbox's connection and then stalled — "+
					"reporting that as 'redirect enforced' tells the operator a wide-open path is blocked", got, redirectProbeBypassCode, from)
			}
		})
	}
}

// TestRedirectProbe2_NoConnectionFactIsNotEnforcement pins the other half of the
// same law: a curl that never got as far as a dial proves NOTHING, so it must
// not exit 0 either. `example.com/a b` is a From validateSiteConfig accepts (it
// is a host with a path, which redirects legitimately carry), and curl exits 3
// on the URL it builds — with `000 0`, a count the bypass arm correctly skips.
// The base's script then fell off the end to exit 0 and the endpoint reported
// "correctly blocked when dialed directly (redirect enforced)" for a redirect
// nothing had tested. The verdict must be the inconclusive sentinel instead.
func TestRedirectProbe2_NoConnectionFactIsNotEnforcement(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	const from = "example.com/a b"
	// The operator can save this: it is the write-time gate, run for real.
	if err := validateSiteConfig(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: from, To: "mirror.corp.internal"},
	}}); err != nil {
		// Loudly, not a skip: if the write-time gate closes this shape the pin's
		// premise has moved and the arm it guards must be re-derived, not
		// quietly stop running (the wave's own rule for a premise-changed pin).
		t.Fatalf("validateSiteConfig now rejects %q (%v) — the write-time gate closed this shape; re-derive what the probe can still be handed", from, err)
	}
	if got := runRedirectProbe(t, mirror.URL, probeTargetURL(from)); got != redirectProbeInconclusiveCode {
		t.Fatalf("exit = %d, want %d (inconclusive): curl never dialled %s, so nothing was learned — "+
			"exit 0 here reports an UNTESTED redirect as enforced", got, redirectProbeInconclusiveCode, from)
	}
}

// TestClassifyRedirectProbe_InconclusiveIsNeverReached is the verdict half: the
// sentinel must never render as the green "redirect enforced" line, and the
// detail must say the redirect was not tested.
func TestClassifyRedirectProbe_InconclusiveIsNeverReached(t *testing.T) {
	got := classifyRedirectProbe(
		probeRunResult{hasExitCode: true, exitCode: redirectProbeInconclusiveCode},
		"mirror.corp.internal", "example.com/a b", testControlPlaneURL)
	if got.State == "reached" {
		t.Fatalf("state = %q: a probe that produced no connection fact must never read as enforcement; detail=%q", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "NOT tested") {
		t.Errorf("detail = %q, want it to say the redirect was not tested", got.Detail)
	}
	if strings.Contains(got.Detail, "correctly blocked") || strings.Contains(got.Detail, "redirect enforced") {
		t.Errorf("detail = %q must not claim the direct dial was blocked", got.Detail)
	}
}

// TestRedirectProbeScript_NoConnectionFactExitsInconclusive pins the SHAPE, so
// neither arm can be deleted back into an exit 0: an empty count and the
// pre-connect codes 1/3 must both reach the inconclusive sentinel, and the
// bypass arms must still be matched FIRST so a dial that DID connect can never
// be downgraded to "untested".
func TestRedirectProbeScript_NoConnectionFactExitsInconclusive(t *testing.T) {
	if !strings.Contains(redirectProbeScript, `[ -z "$conns" ] && exit 252`) {
		t.Errorf("an empty %%{num_connects} is not a block — the script must exit the inconclusive sentinel:\n%s", redirectProbeScript)
	}
	if !strings.Contains(redirectProbeScript, `case "$rc" in 1|3) exit 252 ;; esac`) {
		t.Errorf("curl 1 (unsupported protocol) and 3 (malformed URL) fail BEFORE any dial — the script must exit the inconclusive sentinel:\n%s", redirectProbeScript)
	}
	bypass := strings.Index(redirectProbeScript, `[ "$conns" != "0" ] && exit 250`)
	inconclusive := strings.Index(redirectProbeScript, `exit 252`)
	if bypass < 0 || inconclusive < 0 || bypass > inconclusive {
		t.Errorf("the bypass arms must be matched BEFORE the inconclusive ones, or a dial that connected could be reported as untested:\n%s", redirectProbeScript)
	}
}

// TestRedirectProbeScript_Probe2ReadsTheConnectFact pins the SHAPE of the read,
// so the ambiguity cannot come back by deleting one arm: probe 2's -w must ask
// for num_connects, and the script must have an arm that exits the bypass
// sentinel on a non-zero count.
func TestRedirectProbeScript_Probe2ReadsTheConnectFact(t *testing.T) {
	var probe2 string
	for _, line := range strings.Split(redirectProbeScript, "\n") {
		if strings.Contains(line, "--noproxy") {
			probe2 = line
		}
	}
	if !strings.Contains(probe2, "%{num_connects}") {
		t.Errorf("probe 2 must read %%{num_connects}: curl exit 28 alone cannot tell a connect that never happened "+
			"from one that did and then stalled; got %q", probe2)
	}
	if !strings.Contains(redirectProbeScript, `[ "$conns" != "0" ] && exit 250`) {
		t.Errorf("the script must exit the bypass sentinel on a non-zero connect count:\n%s", redirectProbeScript)
	}
}

// TestRedirectStateTableDocumentsTheUntestedVerdict (F148, adversarial fix-up)
// pins docs/OPERATIONS.md's test-redirect state table to the verdict this round
// introduced.
//
// The inconclusive sentinel renders as state "blocked" on a probe where probe 1
// SUCCEEDED (the script exits probe 1's own code on failure, so reaching probe 2
// means the mirror WAS reached) and probe 2 produced no connection fact at all.
// That made both of the table's own sentences about `blocked` false: its row
// enumerated causes none of which apply and asserted the mirror was unreachable,
// and the `not_run` row drew the contrast as "`blocked` means the probe DID run
// and observed a real network fact". Docs say what the code does, so the rows
// carry the untested case now — and this guard reads the verdict off
// classifyRedirectProbe rather than a line number, so if the arm ever stops
// rendering as `blocked` the prose is re-derived instead of silently rotting.
func TestRedirectStateTableDocumentsTheUntestedVerdict(t *testing.T) {
	// The premise, from the classifier itself.
	got := classifyRedirectProbe(
		probeRunResult{hasExitCode: true, exitCode: redirectProbeInconclusiveCode},
		"mirror.corp", "example.com", "")
	if got.State != "blocked" {
		t.Fatalf("the inconclusive arm renders as %q, not \"blocked\" — re-derive OPERATIONS.md's state table before trusting this guard", got.State)
	}
	if !strings.Contains(got.Detail, "NOT tested") {
		t.Fatalf("the inconclusive detail no longer says the redirect was NOT tested (%q) — re-derive the doc rows", got.Detail)
	}

	doc := readOperationsDoc(t)
	for _, want := range []string{
		"It also carries the one case where the mirror answered but the direct dial of the public host produced no connection fact at all to read",
		"`detail` then says the redirect was NOT tested, and the setup gate stays held, because an untested redirect must never render as `reached`",
		"`blocked` means the probe DID run — usually observing a real network fact, and otherwise saying in `detail` that nothing was learned",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/OPERATIONS.md's test-redirect state table no longer states: %q", want)
		}
	}
	// The retired absolute contrast, which the new arm falsifies.
	if strings.Contains(doc, "`blocked` means the probe DID run and observed a real network fact") {
		t.Error("docs/OPERATIONS.md is back to the retired claim that `blocked` always observed a real network fact — the inconclusive arm is a `blocked` that observed none")
	}
}

// readOperationsDoc reads the runbook from the package's own directory, the
// same relative path internal/api's other doc guards use.
func readOperationsDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	return string(b)
}
