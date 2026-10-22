// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// The UI-sandbox gateway may share a port number with -internal-listen when the internal listener is not bound
// (a loopback http control-plane URL); it is refused only when both really bind.
func TestInternalListenBinds(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://wardynd:8443", true},
		{" HTTPS://wardynd:8443 ", true},
		{"http://localhost:8080", false},
		{"http://127.0.0.1:8080", false},
		{"", false},
		{"%zz", false},
	} {
		if got := internalListenBinds(tc.url); got != tc.want {
			t.Errorf("internalListenBinds(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestUISandboxMayShareTheUnboundInternalAddress(t *testing.T) {
	posture := tlsPosture{tlsEnabled: true, secureCookies: true}
	check := func(controlURL string) error {
		return validateUISandboxConfig(":8443", ":8080", boundInternalListen(":8443", controlURL), "", "", "", posture, false)
	}
	if err := check("http://localhost:8080"); err != nil {
		t.Errorf("unbound internal listener refused the shared port: %v", err)
	}
	if err := check("https://wardynd:8443"); err == nil {
		t.Error("a bound internal listener on the same port was accepted")
	}
}
