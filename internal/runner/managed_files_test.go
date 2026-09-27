// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"io/fs"
	"strings"
	"testing"
)

func TestValidateManagedFiles(t *testing.T) {
	ok := ManagedFile{Path: "/etc/claude-code/managed-settings.json", Content: []byte("{}")}
	const outside = "must sit directly in /etc/claude-code"

	cases := []struct {
		name  string
		files []ManagedFile
		want  string // substring of the refusal; "" means accept
	}{
		{name: "accepts the consumer's path", files: []ManagedFile{ok}},
		{name: "accepts nothing at all", files: nil},
		{name: "relative path", files: []ManagedFile{{Path: "etc/claude-code/x"}}, want: "must be absolute"},
		{name: "traversal", files: []ManagedFile{{Path: "/etc/claude-code/../../x"}}, want: "must be clean"},
		{name: "trailing separator", files: []ManagedFile{{Path: "/etc/claude-code/x/"}}, want: "must be clean"},
		{name: "the directory itself", files: []ManagedFile{{Path: "/etc/claude-code"}}, want: outside},
		{name: "a subdirectory", files: []ManagedFile{{Path: "/etc/claude-code/sub/x"}}, want: outside},
		{name: "a sibling sharing the prefix", files: []ManagedFile{{Path: "/etc/claude-codex/x"}}, want: outside},
		{name: "elsewhere under /etc", files: []ManagedFile{{Path: "/etc/wardyn/agent/settings.json"}}, want: outside},
		{name: "root", files: []ManagedFile{{Path: "/x"}}, want: outside},
		// Each of these was accepted once and shown on a real daemon to be
		// replaceable by the agent, hidden from it, or host-mutating.
		{name: "agent-owned top level", files: []ManagedFile{{Path: "/work/.claude/settings.json"}}, want: outside},
		{name: "tmpfs mounted at start", files: []ManagedFile{{Path: "/tmp/wardyn/policy.json"}}, want: outside},
		{name: "workspace bind mount", files: []ManagedFile{{Path: "/home/agent/work/.claude/settings.json"}}, want: outside},
		{name: "recording cast dir", files: []ManagedFile{{Path: "/var/log/wardyn/p.json"}}, want: outside},
		{name: "recording mount", files: []ManagedFile{{Path: "/wardyn/recordings/p.json"}}, want: outside},
		{name: "user drive", files: []ManagedFile{{Path: "/home/agent/drive/x/p.json"}}, want: outside},
		{name: "empty", files: []ManagedFile{{}}, want: "empty path"},
		{name: "duplicate", files: []ManagedFile{ok, ok}, want: "duplicate path"},
		{name: "group-writable", files: []ManagedFile{{Path: "/etc/claude-code/f", Mode: 0o664}}, want: "group- or other-writable"},
		{name: "other-writable", files: []ManagedFile{{Path: "/etc/claude-code/f", Mode: 0o646}}, want: "group- or other-writable"},
		{name: "world-writable", files: []ManagedFile{{Path: "/etc/claude-code/f", Mode: 0o666}}, want: "group- or other-writable"},
		{name: "non-permission bits", files: []ManagedFile{{Path: "/etc/claude-code/f", Mode: fs.ModeSetuid | 0o644}}, want: "outside the permission bits"},
		{name: "read-only is fine", files: []ManagedFile{{Path: "/etc/claude-code/f", Mode: 0o444}}},
		{name: "over the ceiling", files: []ManagedFile{{Path: "/etc/claude-code/f", Content: make([]byte, ManagedFilesMaxBytes+1)}}, want: "exceeds the"},
		{name: "at the ceiling", files: []ManagedFile{{Path: "/etc/claude-code/f", Content: make([]byte, ManagedFilesMaxBytes)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateManagedFiles(tc.files)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ValidateManagedFiles = %v, want accepted", err)
			case tc.want != "" && err == nil:
				t.Fatalf("ValidateManagedFiles accepted %+v, want a refusal containing %q", tc.files, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("ValidateManagedFiles = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// A managed file the agent's own group can write is not a ceiling, and the
// mode is the ONLY thing standing between the two on the docker substrate
// (where the file is a real file, not a read-only mount). This is the one
// validation rule whose absence would pass every other test in the tree.
func TestValidateManagedFilesRefusesEveryAgentWritableMode(t *testing.T) {
	for mode := fs.FileMode(0); mode <= 0o777; mode++ {
		err := ValidateManagedFiles([]ManagedFile{{Path: "/etc/claude-code/f", Mode: mode}})
		writable := mode&0o022 != 0
		if writable && err == nil {
			t.Fatalf("mode %04o is group- or other-writable and was accepted", mode)
		}
		if !writable && err != nil {
			t.Fatalf("mode %04o is not agent-writable but was refused: %v", mode, err)
		}
	}
}

func TestManagedFileModeDefaults(t *testing.T) {
	if got := (ManagedFile{}).FileMode(); got != DefaultManagedFileMode {
		t.Errorf("FileMode() on an unset Mode = %04o, want %04o", got, DefaultManagedFileMode)
	}
	if got := (ManagedFile{Mode: 0o400}).FileMode(); got != 0o400 {
		t.Errorf("FileMode() = %04o, want 0400", got)
	}
}

func TestManagedFileDirs(t *testing.T) {
	got := ManagedFileDirs([]ManagedFile{
		{Path: "/opt/wardyn/z/b"},
		{Path: "/etc/wardyn/a/one"},
		{Path: "/etc/wardyn/a/two"},
	})
	want := []string{"/etc/wardyn/a", "/opt/wardyn/z"}
	if len(got) != len(want) {
		t.Fatalf("ManagedFileDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ManagedFileDirs = %v, want %v (sorted, deduplicated)", got, want)
		}
	}
}

// No mount may land on ManagedFileDir, above it, or inside it: a bind there is
// a host directory the delivery would write into, and one above it lets the
// agent rename the directory aside.
func TestManagedFileDirIsNoMountTarget(t *testing.T) {
	for _, tgt := range []string{"/", "/etc", ManagedFileDir, ManagedFileDir + "/x"} {
		if ValidateTarget(tgt) == nil {
			t.Errorf("ValidateTarget(%q) accepted a mount target that covers %s", tgt, ManagedFileDir)
		}
	}
}
