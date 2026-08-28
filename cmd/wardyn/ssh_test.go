// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// runSSH --json: the target (host, port, username, fingerprint, command) for
// a script or an external tool that dials the sandbox itself, rather than
// shelling out to the local ssh(1) the way plain `wardyn ssh` does.
// --------------------------------------------------------------------------

func runSSHJSON(t *testing.T, healthzBody, runID string) (sshTarget, error) {
	t.Helper()
	srv := fakeHealthzServer(t, healthzBody)
	defer srv.Close()

	cmd := sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{runID, "--json"})
	if err := cmd.Execute(); err != nil {
		return sshTarget{}, err
	}
	var target sshTarget
	if err := json.Unmarshal(out.Bytes(), &target); err != nil {
		t.Fatalf("--json output not valid JSON: %v (%q)", err, out.String())
	}
	return target, nil
}

func TestRunSSH_JSON(t *testing.T) {
	got, err := runSSHJSON(t,
		`{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com:2222","host_key_fingerprint":"SHA256:abc"}}`,
		"run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := sshTarget{
		Host: "wardyn.example.com", Port: 2222, Username: "run-1",
		HostKeyFingerprint: "SHA256:abc", Command: "ssh run-1@wardyn.example.com -p 2222",
	}
	if got != want {
		t.Errorf("--json output = %+v, want %+v", got, want)
	}
}

// TestRunSSH_JSON_BareHostDefaultsPort22: --print's bare-host form omits the
// -p flag entirely (ssh(1) applies its own default), but --json's Port field
// always carries a number so a caller never has to reimplement that default.
func TestRunSSH_JSON_BareHostDefaultsPort22(t *testing.T) {
	got, err := runSSHJSON(t,
		`{"ssh":{"enabled":true,"advertise_addr":"wardyn.example.com","host_key_fingerprint":"SHA256:abc"}}`,
		"run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 22 {
		t.Errorf("port = %d, want the conventional default 22 when the gateway advertises none", got.Port)
	}
	if got.Host != "wardyn.example.com" || got.Username != "run-1" {
		t.Errorf("got %+v, want host=wardyn.example.com username=run-1", got)
	}
	if got.Command != "ssh run-1@wardyn.example.com" {
		t.Errorf("command = %q, want the bare-host form with no -p flag", got.Command)
	}
}

// TestRunSSH_JSON_CommandMatchesPrintOutput pins the doc comment's own claim:
// --json's "command" field is the exact string --print would emit, not a
// close approximation a caller might reasonably expect to differ.
func TestRunSSH_JSON_CommandMatchesPrintOutput(t *testing.T) {
	body := `{"ssh":{"enabled":true,"advertise_addr":"[2001:db8::1]:2222","host_key_fingerprint":"SHA256:abc"}}`
	printOut, err := runSSHPrint(t, body, "run-1")
	if err != nil {
		t.Fatalf("--print: unexpected error: %v", err)
	}
	got, err := runSSHJSON(t, body, "run-1")
	if err != nil {
		t.Fatalf("--json: unexpected error: %v", err)
	}
	if got.Command != printOut {
		t.Errorf("json command = %q, want it to equal --print's output %q", got.Command, printOut)
	}
	if got.HostKeyFingerprint != "SHA256:abc" {
		t.Errorf("host_key_fingerprint = %q, want it carried through from /healthz", got.HostKeyFingerprint)
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
