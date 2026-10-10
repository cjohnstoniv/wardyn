// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import "testing"

func TestCheckOrgURL(t *testing.T) {
	for _, raw := range []string{"https://org.example.com", "https://org.example.com/base", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if err := CheckOrgURL(raw); err != nil {
			t.Errorf("refused %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "/relative", "http://org.example.com", "ftp://localhost", "https://user:secret@org.example.com", "https://org.example.com?", "https://org.example.com?q=x", "https://org.example.com#", "https://org.example.com#x"} {
		if err := CheckOrgURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
