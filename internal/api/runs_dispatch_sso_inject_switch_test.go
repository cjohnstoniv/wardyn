// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// THE KILL SWITCH IS ONE CONSTANT, and these are the tests that make that
// sentence true rather than merely written down (general B3).
//
// It was false as shipped: cmd/wardynd hard-coded the flag default string
// "on", so with nothing set in the environment the resolver was handed "on" and
// never consulted awsSSOProxyInjectDefaultOn. Flipping the constant to false —
// the documented O-10 rollback, promised by the REPORT, the code comment, the
// changelog canon and the measurement canon — would have shipped the lane ON
// with every document saying off.

// The DEFAULT the boot flag advertises is derived, not typed. Run at both
// positions of the constant through the seam, because the point is that the
// constant decides and nothing else does.
func TestAWSSSOProxyInjectFlagDefault_DerivesFromTheOneConstant(t *testing.T) {
	want := "off"
	if awsSSOProxyInjectDefaultOn {
		want = "on"
	}
	if got := AWSSSOProxyInjectFlagDefault(); got != want {
		t.Errorf("AWSSSOProxyInjectFlagDefault() = %q, want %q — the flag default must follow the constant", got, want)
	}
}

// NOTHING SET => the constant, whichever way it points. This is the case the
// hard-coded flag default hid.
func TestResolveAWSSSOProxyInject_UnsetTakesTheConstant(t *testing.T) {
	for _, def := range []bool{true, false} {
		// The empty string is what a deployment that never heard of the knob
		// hands the resolver once the flag default is derived.
		if got := resolveAWSSSOProxyInject("", def); got != def {
			t.Errorf("with the constant %v and nothing set, the lane resolves %v — the constant does not decide", def, got)
		}
		// …and the flag default routes to the same answer.
		raw := "off"
		if def {
			raw = "on"
		}
		if got := resolveAWSSSOProxyInject(raw, def); got != def {
			t.Errorf("with the constant %v the derived flag default %q resolves %v", def, raw, got)
		}
	}
}

// Every spelling, at BOTH positions of the constant: an explicit value always
// wins, and only an unrecognised one falls back.
func TestResolveAWSSSOProxyInject_Table(t *testing.T) {
	for _, tc := range []struct {
		raw                string
		wantOnDefaultTrue  bool
		wantOnDefaultFalse bool
	}{
		{"", true, false},
		{"garbage", true, false},
		{"   ", true, false},
		{"on", true, true},
		{"ON", true, true},
		{" on ", true, true},
		{"true", true, true},
		{"1", true, true},
		{"off", false, false},
		{"OFF", false, false},
		{"false", false, false},
		{"0", false, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := resolveAWSSSOProxyInject(tc.raw, true); got != tc.wantOnDefaultTrue {
				t.Errorf("resolve(%q, default=true) = %v, want %v", tc.raw, got, tc.wantOnDefaultTrue)
			}
			if got := resolveAWSSSOProxyInject(tc.raw, false); got != tc.wantOnDefaultFalse {
				t.Errorf("resolve(%q, default=false) = %v, want %v", tc.raw, got, tc.wantOnDefaultFalse)
			}
		})
	}
	// The exported wrapper is the constant's own position.
	if got := ResolveAWSSSOProxyInject(""); got != awsSSOProxyInjectDefaultOn {
		t.Errorf("ResolveAWSSSOProxyInject(\"\") = %v, want the constant %v", got, awsSSOProxyInjectDefaultOn)
	}
}
