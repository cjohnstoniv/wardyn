// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// argsReader is a Host that can read a process's argv.
type argsReader interface {
	Args(pid int) ([]string, error)
}

// Args is pid's argv from h; errors.ErrUnsupported when h cannot read one (every host but macOS's).
func Args(h Host, pid int) ([]string, error) {
	a, ok := h.(argsReader)
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return a.Args(pid)
}

var errProcArgs = errors.New("admission: a malformed kern.procargs2 buffer")

// parseProcArgs2 reads a kern.procargs2 buffer as XNU writes it: argc (a native-endian int32), the
// executable path and its NUL, NUL padding, then argc NUL-terminated arguments. The environment follows
// and is not read.
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errProcArgs
	}
	argc := int32(binary.NativeEndian.Uint32(b))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if argc < 0 || i < 0 {
		return nil, errProcArgs
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	if int(argc) > len(rest) {
		return nil, errProcArgs
	}
	argv := make([]string, 0, argc)
	for range argc {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil, errProcArgs
		}
		argv = append(argv, string(rest[:j]))
		rest = rest[j+1:]
	}
	return argv, nil
}
