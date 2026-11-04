// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// driveOpenQuarantine is the extended attribute macOS reads to say a file came from outside: Gatekeeper
// and Quick Look treat what has it as downloaded.
const driveOpenQuarantine = "com.apple.quarantine"

// driveOpenMark flags f, a file copied out of a drive, as downloaded. A file that cannot be flagged
// ends the copy: the closing line says every file is.
func driveOpenMark(f *os.File) error {
	value := []byte(fmt.Sprintf("0081;%x;Wardyn;", time.Now().Unix()))
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := conn.Control(func(fd uintptr) { setErr = unix.Fsetxattr(int(fd), driveOpenQuarantine, value, 0) }); err != nil {
		return err
	}
	return setErr
}
