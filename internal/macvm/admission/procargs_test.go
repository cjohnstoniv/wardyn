// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

// procArgs2 builds a kern.procargs2 buffer as XNU lays it out: argc, the executable path, NUL padding,
// argv, then the environment.
func procArgs2(argc int32, exe string, pad int, strs ...string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	b = append(b, make([]byte, 1+pad)...)
	for _, s := range strs {
		b = append(append(b, s...), 0)
	}
	return b
}

func TestParseProcArgs2(t *testing.T) {
	argv := []string{"/usr/local/libexec/wardyn-macvm", "update-tools", "--runner-id", "runner-a", "--ledger-entry", "abc"}
	b := procArgs2(int32(len(argv)), "/usr/local/libexec/wardyn-macvm", 5, append(slices.Clone(argv), "HOME=/Users/a", "PATH=/usr/bin")...)
	got, err := parseProcArgs2(b)
	if err != nil || !slices.Equal(got, argv) {
		t.Fatalf("argv %q, %v; want %q", got, err, argv)
	}
	// An empty argument is an argument, not padding.
	withEmpty := []string{"wardyn-macvm", "", "prepare"}
	if got, err := parseProcArgs2(procArgs2(3, "/x", 0, withEmpty...)); err != nil || !slices.Equal(got, withEmpty) {
		t.Fatalf("argv with an empty argument %q, %v", got, err)
	}
	for name, b := range map[string][]byte{
		"empty":             nil,
		"argc only":         procArgs2(1, "", -1),
		"no path end":       append(binary.NativeEndian.AppendUint32(nil, 1), "/x"...),
		"argc past the end": procArgs2(4, "/x", 2, "a", "b"),
		"argument unended":  append(procArgs2(2, "/x", 0, "a"), 'b'),
		"negative argc":     procArgs2(-1, "/x", 0, "a"),
	} {
		if got, err := parseProcArgs2(b); err == nil {
			t.Errorf("%s: parsed %q, want an error", name, got)
		}
	}
}

// A host that cannot read argv says so: the caller then never acts on the process.
func TestArgsUnsupportedWithoutAReader(t *testing.T) {
	if _, err := Args(&fakeHost{}, 1); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("argv from a host with no reader: %v", err)
	}
	if got, err := Args(argvHost{fakeHost: &fakeHost{}, argv: []string{"a", "b"}}, 7); err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("argv from a reader: %q, %v", got, err)
	}
}

type argvHost struct {
	*fakeHost
	argv []string
}

func (h argvHost) Args(int) ([]string, error) { return h.argv, nil }
