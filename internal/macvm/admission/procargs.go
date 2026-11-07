// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import "errors"

// argsReader is a Host that can read a process's argv.
type argsReader interface {
	Args(pid int) ([]string, error)
}

// Args is pid's argv from h; errors.ErrUnsupported when h cannot read one.
func Args(h Host, pid int) ([]string, error) {
	return nil, errors.ErrUnsupported
}

// parseProcArgs2 reads a kern.procargs2 buffer.
func parseProcArgs2(b []byte) ([]string, error) {
	return nil, errors.ErrUnsupported
}
