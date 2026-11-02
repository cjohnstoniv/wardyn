// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Sandbox carries the guest's keychain answer across the runner wire: trusted, not trusted with a
// fixed reason, or absent. Absent (an old runner's answer) is not attempted, never failed; a code the
// host does not know reads as unavailable.
func TestSandboxKeychainTrustOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *KeychainTrust
		want *KeychainTrust
	}{
		{"ok", &KeychainTrust{Status: KeychainTrusted}, &KeychainTrust{Status: KeychainTrusted}},
		{"failed", &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainRefused}, &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainRefused}},
		{"absent", nil, nil},
		{"unknown reason", &KeychainTrust{Status: KeychainNotTrusted, Reason: "security: SecTrustSettingsSetTrustSettings: denied"}, &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainUnavailable}},
		{"unknown status", &KeychainTrust{Status: "maybe"}, &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainUnavailable}},
		{"trusted with a reason", &KeychainTrust{Status: KeychainTrusted, Reason: KeychainFailed}, &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(Sandbox{Ref: "r", KeychainTrust: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			if tc.in == nil && strings.Contains(string(b), "KeychainTrust") {
				t.Errorf("an absent answer is on the wire: %s", b)
			}
			var got Sandbox
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			k := got.KeychainTrust.Known()
			if (k == nil) != (tc.want == nil) || (k != nil && *k != *tc.want) {
				t.Errorf("Known() = %+v, want %+v", k, tc.want)
			}
		})
	}
	// An old runner's Sandbox has no field at all.
	var old Sandbox
	if err := json.Unmarshal([]byte(`{"Ref":"r","Driver":"vm"}`), &old); err != nil || old.KeychainTrust.Known() != nil {
		t.Errorf("an old runner's sandbox reads as attempted: %+v %v", old.KeychainTrust, err)
	}
}
