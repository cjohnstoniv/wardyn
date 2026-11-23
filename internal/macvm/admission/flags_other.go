// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !darwin

package admission

import (
	"errors"

	"golang.org/x/sys/unix"
)

// statFlags reads none where the system has no BSD file flags.
func statFlags(unix.Stat_t) uint32 { return 0 }

// fchflags is unsupported where the system has no BSD file flags.
func fchflags(int, uint32) error { return errors.ErrUnsupported }
