// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"encoding/json"
	"testing"
)

// TestOrgURLSHA256 pins what counts as the same organisation URL (the client
// drops surrounding space and trailing slashes, so the hash must too) and that
// a credential stored before the field existed decodes with an empty hash.
func TestOrgURLSHA256(t *testing.T) {
	for _, same := range []string{
		" https://org.example.com// ", "HTTPS://Org.Example.COM", "https://org.example.com:443", "https://org.example.com:443/",
	} {
		if OrgURLSHA256(same) != OrgURLSHA256("https://org.example.com") {
			t.Errorf("%q hashes differently from https://org.example.com", same)
		}
	}
	if OrgURLSHA256("http://org.example.com:80") != OrgURLSHA256("http://org.example.com") {
		t.Error("http default port changed the hash")
	}
	for _, other := range []string{"https://org.example.com:8443", "http://org.example.com", "https://org.example.com/base", "https://[::1]:443x"} {
		if OrgURLSHA256(other) == OrgURLSHA256("https://org.example.com") {
			t.Errorf("%q hashes like https://org.example.com", other)
		}
	}
	if OrgURLSHA256("https://[::1]:443") != OrgURLSHA256("https://[::1]") {
		t.Error("ipv6 default port changed the hash")
	}
	if OrgURLSHA256("https://org.example.com") == OrgURLSHA256("https://other.example.com") {
		t.Error("different hosts hash alike")
	}
	var old Credential
	if err := json.Unmarshal([]byte(`{"device_id":"6f1c2c1e-0d0c-4b52-9a4e-5b1f0c8f2a11","token":"wdd_x","enrolment_token_sha256":"ab"}`), &old); err != nil || old.OrgURLSHA256 != "" {
		t.Errorf("legacy credential: err=%v hash=%q, want empty", err, old.OrgURLSHA256)
	}
}
