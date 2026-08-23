// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// --------------------------------------------------------------------------
// splitHostPort: mirrors ui/.../run-detail-ssh.tsx's splitHostPort exactly.
// --------------------------------------------------------------------------

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantHost string
		wantPort string
	}{
		{name: "host with port", addr: "wardyn.example.com:2222", wantHost: "wardyn.example.com", wantPort: "2222"},
		{name: "bare host, no port", addr: "wardyn.example.com", wantHost: "wardyn.example.com", wantPort: ""},
		{name: "bracketed IPv6 with port", addr: "[::1]:2222", wantHost: "::1", wantPort: "2222"},
		{name: "bracketed IPv6 without port", addr: "[2001:db8::1]", wantHost: "2001:db8::1", wantPort: ""},
		{name: "bare IPv6, no port (never mangled by a naive last-colon split)", addr: "2001:db8::1", wantHost: "2001:db8::1", wantPort: ""},
		{name: "empty", addr: "", wantHost: "", wantPort: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, port := splitHostPort(tc.addr)
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("splitHostPort(%q) = (%q, %q), want (%q, %q)", tc.addr, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// --------------------------------------------------------------------------
// runSSH --print: exercised against a fake /healthz, covering the gateway
// enabled/disabled and address-shape matrix the plan calls for.
// --------------------------------------------------------------------------

func fakeHealthzServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func runSSHPrint(t *testing.T, healthzBody, runID string) (string, error) {
	t.Helper()
	srv := fakeHealthzServer(t, healthzBody)
	defer srv.Close()

	cmd := sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{runID, "--print"})
	err := cmd.Execute()
	return strings.TrimSpace(out.String()), err
}

func TestRunSSH_Disabled(t *testing.T) {
	_, err := runSSHPrint(t, `{"ssh":null}`, "run-1")
	if err == nil {
		t.Fatal("expected an error when the gateway is off")
	}
	if !strings.Contains(err.Error(), "WARDYN_SSH_LISTEN") {
		t.Errorf("error = %q, want it to name WARDYN_SSH_LISTEN", err.Error())
	}
}

func TestRunSSH_EnabledHostWithPort(t *testing.T) {
	out, err := runSSHPrint(t,
		`{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com:2222","host_key_fingerprint":"SHA256:abc"}}`,
		"run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "ssh run-1@wardyn.example.com -p 2222"
	if out != want {
		t.Errorf("--print output = %q, want %q", out, want)
	}
}

func TestRunSSH_EnabledBareHost(t *testing.T) {
	out, err := runSSHPrint(t,
		`{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com","host_key_fingerprint":"SHA256:abc"}}`,
		"run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "ssh run-1@wardyn.example.com"
	if out != want {
		t.Errorf("--print output = %q, want %q (no -p flag on a bare host)", out, want)
	}
}

func TestRunSSH_EnabledBracketedIPv6(t *testing.T) {
	out, err := runSSHPrint(t,
		`{"ssh":{"enabled":true,"advertise_addr":"[::1]:2222","host_key_fingerprint":"SHA256:abc"}}`,
		"run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "ssh run-1@::1 -p 2222"
	if out != want {
		t.Errorf("--print output = %q, want %q", out, want)
	}
}

// --------------------------------------------------------------------------
// runSSH --config: byte-identical to the console card's Host block
// (run-detail-ssh.tsx's sshConfig).
// --------------------------------------------------------------------------

func TestRunSSH_Config(t *testing.T) {
	srv := fakeHealthzServer(t, `{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com:2222","host_key_fingerprint":"SHA256:abc"}}`)
	defer srv.Close()

	cmd := sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"run_abcdefgh12345", "--config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "Host wardyn-abcdefgh\n  HostName wardyn.example.com\n  Port 2222\n  User run_abcdefgh12345\n"
	if out.String() != want {
		t.Errorf("--config output =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestRunSSH_ConfigDefaultPort(t *testing.T) {
	srv := fakeHealthzServer(t, `{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com","host_key_fingerprint":"SHA256:abc"}}`)
	defer srv.Close()

	cmd := sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"run-1", "--config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "  Port 22\n") {
		t.Errorf("--config output = %q, want a conventional default Port 22 when the gateway advertises no port", out.String())
	}
}

// --------------------------------------------------------------------------
// shortRunID: mirrors the console's Host label (run.id.replace(/^run_/, "").slice(0,8)).
// --------------------------------------------------------------------------

func TestShortRunID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"run_abcdefgh12345", "abcdefgh"},
		{"abcdefgh12345", "abcdefgh"},
		{"short", "short"},
		{"run_ab", "ab"},
	}
	for _, tc := range tests {
		if got := shortRunID(tc.in); got != tc.want {
			t.Errorf("shortRunID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunSSH_EnabledButNoAdvertiseAddr: the gateway is on and WARDYN_SSH_ADVERTISE
// is unset, so /healthz publishes an empty advertise_addr. Before this the CLI
// built `ssh <run>@ -p` and let ssh(1) fail on the empty hostname — the operator
// got a resolver error for what is a wardynd misconfiguration.
func TestRunSSH_EnabledButNoAdvertiseAddr(t *testing.T) {
	for _, body := range []string{
		`{"ssh":{"enabled":true,"advertise_addr":"","host_key_fingerprint":"SHA256:abc"}}`,
		`{"ssh":{"enabled":true,"advertise_addr":":2222","host_key_fingerprint":"SHA256:abc"}}`,
	} {
		out, err := runSSHPrint(t, body, "run-1")
		if err == nil {
			t.Fatalf("expected a refusal, got output %q", out)
		}
		if !strings.Contains(err.Error(), "WARDYN_SSH_ADVERTISE") {
			t.Errorf("error = %q, want it to name WARDYN_SSH_ADVERTISE", err.Error())
		}
	}
}
