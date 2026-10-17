// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package domainmatch

import "testing"

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		in         string
		exact, wld string
		port       int
	}{
		{" Example.COM. ", "example.com", "", 0},
		{"example.com:443", "example.com", "", 443},
		{"*.Example.com", "", ".example.com", 0},
		{"*.example.com:8443", "", ".example.com", 8443},
		{"example.com:0", "example.com:0", "", 0},
		{"[::ffff:93.184.216.34]:443", "93.184.216.34", "", 443},
		{"::1", "::1", "", 0},
		{"", "", "", 0},
	} {
		if e, w, p := Classify(tc.in); e != tc.exact || w != tc.wld || p != tc.port {
			t.Errorf("Classify(%q) = (%q, %q, %d), want (%q, %q, %d)", tc.in, e, w, p, tc.exact, tc.wld, tc.port)
		}
	}
}

func TestMatchWild(t *testing.T) {
	if !MatchWild("a.example.com", []string{".example.com"}) || MatchWild("example.com", []string{".example.com"}) ||
		MatchWild("notexample.com", []string{".example.com"}) {
		t.Error("a wildcard suffix matches subdomains on the label boundary only")
	}
	w := []WildPort{{Suffix: ".example.com", Port: 443}}
	if !MatchWildPort("a.example.com", 443, w) || MatchWildPort("a.example.com", 80, w) {
		t.Error("a port-qualified wildcard matches only its port")
	}
}

// Every entry shape that already compiles to a matchable key keeps exactly that
// key: the FQDN-root dot handling only touches the host half of a ":port" entry.
func TestClassifyMatchableShapesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		in         string
		exact, wld string
		port       int
	}{
		{"example.com", "example.com", "", 0},
		{"example.com.", "example.com", "", 0},
		{"example.com...", "example.com", "", 0},
		{"*.example.com", "", ".example.com", 0},
		{"*.example.com..", "", ".example.com", 0},
		{"example.com:443", "example.com", "", 443},
		{"example.com.:443", "example.com", "", 443},
		{"*.example.com:443", "", ".example.com", 443},
		{"EXAMPLE.com.:8443", "example.com", "", 8443},
		{"1.2.3.4", "1.2.3.4", "", 0},
		{"1.2.3.4:443", "1.2.3.4", "", 443},
		{"1.2.3.4.:443", "1.2.3.4", "", 443},
		{"[::1]:443", "::1", "", 443},
		{"[2001:db8::1]", "[2001:db8::1]", "", 0},
		{"example.com:0", "example.com:0", "", 0},
		{"example.com.:99999", "example.com.:99999", "", 0},
		{"*.example.com.:http", "", ".example.com.:http", 0},
	} {
		if e, w, p := Classify(tc.in); e != tc.exact || w != tc.wld || p != tc.port {
			t.Errorf("Classify(%q) = (%q, %q, %d), want (%q, %q, %d)", tc.in, e, w, p, tc.exact, tc.wld, tc.port)
		}
	}
}

// A port-qualified entry whose host half ends in FQDN-root dots compiles to the
// same key as its dotless spelling, as the port-less form already does: the
// request side trims every trailing dot, so any other key is never matched.
func TestClassifyTrimsRootDotsBeforeThePort(t *testing.T) {
	for _, tc := range []struct {
		in         string
		exact, wld string
		port       int
	}{
		{"*.example.com.:443", "", ".example.com", 443},
		{"*.example.com..:443", "", ".example.com", 443},
		{"example.com..:443", "example.com", "", 443},
		{"example.com...:8443", "example.com", "", 8443},
		{"1.2.3.4..:443", "1.2.3.4", "", 443},
	} {
		if e, w, p := Classify(tc.in); e != tc.exact || w != tc.wld || p != tc.port {
			t.Errorf("Classify(%q) = (%q, %q, %d), want (%q, %q, %d)", tc.in, e, w, p, tc.exact, tc.wld, tc.port)
		}
	}
}
