// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
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
