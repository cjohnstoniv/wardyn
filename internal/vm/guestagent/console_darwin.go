// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ConsoleOwner is the ConsoleUser of a guest: /dev/console belongs to the user logged in at the
// screen, and to root at the login window.
type ConsoleOwner struct{ Run CmdRunner }

func (c ConsoleOwner) Console(ctx context.Context) (uint32, string, error) {
	st, err := os.Stat("/dev/console")
	if err != nil {
		return 0, "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, "", fmt.Errorf("stat /dev/console: unexpected %T", st.Sys())
	}
	if sys.Uid == 0 {
		return 0, "", ErrNoConsoleUser
	}
	out, err := c.Run.Run(ctx, "/usr/bin/id", "-un", strconv.FormatUint(uint64(sys.Uid), 10))
	if err != nil {
		return 0, "", err
	}
	return sys.Uid, strings.TrimSpace(string(out)), nil
}
