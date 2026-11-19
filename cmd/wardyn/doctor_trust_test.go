// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
)

// TestDoctorTrustsTheStoredOrgCA: a CA added with `wardyn ca add` verifies the organisation in doctor as it does
// for every other command, for that organisation only, and a store the CLI refuses fails doctor too.
func TestDoctorTrustsTheStoredOrgCA(t *testing.T) {
	doctorEnv(t)
	isolateStore(t)
	tc := newTestCA(t, "Corp Inspection Root")
	srv := tc.serve(t, nil)
	orgA := srv.URL // https://127.0.0.1:<port>
	orgB := "https://localhost:" + strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	if out, err := addNonInteractive(t, tc.caFile, orgA, certbundle.Fingerprint(tc.ca)); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, _ := runDoctorCmd(t, "--url", orgA); !strings.Contains(out, "ok    tls") || !strings.Contains(out, "verified by the `wardyn ca add` trust store") {
		t.Fatalf("doctor for the organisation the CA was added for:\n%s", out)
	}
	if out, _ := runDoctorCmd(t, "--url", orgB); !strings.Contains(out, "FAIL  tls") || strings.Contains(out, "`wardyn ca add` trust store") {
		t.Fatalf("doctor for another organisation trusted the stored CA:\n%s", out)
	}

	if runtime.GOOS == "windows" {
		return // the refusal below is of a Unix file mode
	}
	path, _, _ := anchorFile(orgA)
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	out, err := runDoctorCmd(t, "--url", orgA)
	if err == nil || !strings.Contains(out, "FAIL  cli-ca") || !strings.Contains(out, path) || !strings.Contains(out, "wardyn ca remove "+orgA) {
		t.Fatalf("err = %v, want a cli-ca FAIL naming the refused store and its remove line:\n%s", err, out)
	}
}

// TestDoctorNamesAStoredSignIn: with no token set, doctor says none was sent; with a stored `wardyn login` sign-in
// it names that sign-in, and a refused one sends the person back to `wardyn login`, not to a personal API token.
func TestDoctorNamesAStoredSignIn(t *testing.T) {
	doctorEnv(t)
	f := newSignInOrg(t)
	f.version = "0.10.0"
	cliEnv(t, nil)
	if out, err := runDoctorCmd(t, "--url", f.srv.URL); err == nil || !strings.Contains(out, "FAIL  identity     /api/v1/me answered 401: no token was sent") {
		t.Fatalf("err = %v, want an identity FAIL saying no token was sent:\n%s", err, out)
	}
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	if out, err := runDoctorCmd(t, "--url", f.srv.URL); err != nil || !strings.Contains(out, "ok    identity     your stored `wardyn login` sign-in: ") {
		t.Fatalf("err = %v, want identity naming the stored sign-in:\n%s", err, out)
	}
	f.mu.Lock()
	f.tokens = map[string]fakeToken{}
	f.mu.Unlock()
	out, err := runDoctorCmd(t, "--url", f.srv.URL)
	if err == nil || !strings.Contains(out, "FAIL  identity     /api/v1/me answered 401: your stored `wardyn login` sign-in was refused; sign in again with `wardyn login`") ||
		strings.Contains(out, "personal API token") {
		t.Fatalf("err = %v, want a refused stored sign-in sent back to `wardyn login`:\n%s", err, out)
	}
}
