// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"
)

// -recovery-rehearse runs behind the boot transport like the other modes that reach a key service:
// the daemon proxy is on the shared transport by the time the mode runs, even when the mode then fails.
func TestRecoveryMode_RehearseRunsBehindTheBootTransport(t *testing.T) {
	defer swapDefaultTransport()()
	f := recoveryFlagsFor("docker", "", "", "fs")
	*f.recoveryInventory, *f.recoveryRehearse = false, true
	boot := testBootFlags("http://proxy.corp.example:3128", "", "", "")
	f.daemonProxyURL, f.daemonProxySecretFile, f.daemonNoProxy = boot.daemonProxyURL, boot.daemonProxySecretFile, boot.daemonNoProxy
	f.awsSSOEndpointOverride, f.oidcInternalIss, f.trustedCAFile = boot.awsSSOEndpointOverride, boot.oidcInternalIss, strp("")
	ran, err := recoveryMode(f)
	if !ran || err == nil {
		t.Fatalf("recoveryMode = ran %v, err %v; want the mode to run and fail on the missing set", ran, err)
	}
	p, perr := http.DefaultTransport.(*http.Transport).Proxy(&http.Request{URL: mustParseURL(t, "https://kms.us-east-1.amazonaws.com/")})
	if perr != nil || p == nil {
		t.Errorf("the rehearse mode ran without the daemon proxy on the shared transport (%v, %v)", p, perr)
	}
}
