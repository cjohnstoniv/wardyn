// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"strings"
	"testing"
)

func TestSyncPattern_GitSyntax(t *testing.T) {
	cases := []struct {
		line, rel string
		dir, want bool
	}{
		{"node_modules", "node_modules", true, true},
		{"node_modules", "web/node_modules", true, true},
		{"node_modules/", "node_modules", false, false}, // a trailing slash is for directories only
		{"/build", "build", true, true},
		{"/build", "src/build", true, false}, // a leading slash anchors to the file's directory
		{"doc/*.txt", "doc/a.txt", false, true},
		{"doc/*.txt", "x/doc/a.txt", false, false}, // a middle slash anchors too
		{"**/cache", "a/b/cache", true, true},
		{"a/**/z", "a/z", false, true},
		{"a/**/z", "a/b/c/z", false, true},
		{"*.pem", "keys/Server.PEM", false, true}, // case-insensitively, as the denylist
		{`\#hash`, "#hash", false, true},
		{"# a comment", "# a comment", false, false},
		{"", "", false, false},
	}
	for _, c := range cases {
		p, ok := parseSyncPattern(c.line)
		if got := ok && p.match(c.rel, c.dir); got != c.want {
			t.Errorf("%q matching %q (dir %v) = %v, want %v", c.line, c.rel, c.dir, got, c.want)
		}
	}
}

func TestSyncIgnore_DeeperAndLaterRulesWin(t *testing.T) {
	ig := newSyncIgnore(nil)
	ig.byDir[""] = parseSyncPatterns(strings.NewReader("*.log\n"))
	ig.byDir["keep"] = parseSyncPatterns(strings.NewReader("!*.log\n"))
	if !ig.ignored("a.log", false) || ig.ignored("keep/a.log", false) || !ig.ignored("other/a.log", false) {
		t.Error("a deeper file's negation must win over the root's rule only under that directory")
	}
}
