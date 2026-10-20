// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// WARDYN_AWS_SSO_PROXY_INJECT=off wrote the AWS sign-in token into the sandbox; 0.10.0 retires it, so
// boot refuses it by name, while the default and unrecognised values still boot.
func TestValidateBootPosture_RefusesRetiredAWSSSOProxyInjectOff(t *testing.T) {
	for _, raw := range []string{"off", "false", "0", " OFF "} {
		err := validateBootPosture(&bootFlags{awsSSOProxyInject: &raw}, tlsPosture{})
		if err == nil || !strings.Contains(err.Error(), "WARDYN_AWS_SSO_PROXY_INJECT") || !strings.Contains(err.Error(), "removed in 0.10.0") {
			t.Errorf("WARDYN_AWS_SSO_PROXY_INJECT=%q: err = %v, want a boot refusal naming the removed value", raw, err)
		}
	}
	for _, raw := range []string{"", "on", "true", "1", "typo"} {
		if err := refuseAWSSSOProxyInjectOff(&raw); err != nil {
			t.Errorf("WARDYN_AWS_SSO_PROXY_INJECT=%q: refused (%v), want it to boot", raw, err)
		}
	}
	if err := refuseAWSSSOProxyInjectOff(nil); err != nil {
		t.Errorf("undeclared flag: refused (%v), want nil", err)
	}
}
