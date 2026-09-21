// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"io/fs"
	"strings"
	"testing"
)

func TestValidateManagedFiles(t *testing.T) {
	ok := ManagedFile{Path: "/etc/wardyn/agent/settings.json", Content: []byte("{}")}

	cases := []struct {
		name  string
		files []ManagedFile
		want  string // substring of the refusal; "" means accept
	}{
		{name: "accepts a two-deep path", files: []ManagedFile{ok}},
		{name: "accepts nothing at all", files: nil},
		{name: "relative path", files: []ManagedFile{{Path: "etc/wardyn/x"}}, want: "must be absolute"},
		{name: "traversal", files: []ManagedFile{{Path: "/etc/wardyn/../../x"}}, want: "must be clean"},
		{name: "trailing separator", files: []ManagedFile{{Path: "/etc/wardyn/x/"}}, want: "must be clean"},
		{name: "top-level parent", files: []ManagedFile{{Path: "/etc/settings.json"}}, want: "at least two directories deep"},
		{name: "root", files: []ManagedFile{{Path: "/x"}}, want: "at least two directories deep"},
		{name: "empty", files: []ManagedFile{{}}, want: "empty path"},
		{name: "duplicate", files: []ManagedFile{ok, ok}, want: "duplicate path"},
		{name: "group-writable", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Mode: 0o664}}, want: "group- or other-writable"},
		{name: "other-writable", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Mode: 0o646}}, want: "group- or other-writable"},
		{name: "world-writable", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Mode: 0o666}}, want: "group- or other-writable"},
		{name: "non-permission bits", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Mode: fs.ModeSetuid | 0o644}}, want: "outside the permission bits"},
		{name: "read-only is fine", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Mode: 0o444}}},
		{name: "over the ceiling", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Content: make([]byte, ManagedFilesMaxBytes+1)}}, want: "exceeds the"},
		{name: "at the ceiling", files: []ManagedFile{{Path: "/etc/wardyn/a/f", Content: make([]byte, ManagedFilesMaxBytes)}}},
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
		err := ValidateManagedFiles([]ManagedFile{{Path: "/etc/wardyn/a/f", Mode: mode}})
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
