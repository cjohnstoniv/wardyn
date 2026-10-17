// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package dockerutil

import (
	"errors"
	"strings"
)

// ErrCapsDiscarded marks a container the daemon created without a resource
// limit that was requested for it.
var ErrCapsDiscarded = errors.New("daemon discarded a requested resource limit")

// DiscardedLimits returns the ContainerCreate warnings that report a requested
// limit was discarded (for example "Your kernel does not support memory limit
// capabilities ... Limitation discarded."), trimmed. The match is a
// case-insensitive "discard" substring: a daemon that words the loss
// differently is not detected.
func DiscardedLimits(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), "discard") {
			out = append(out, strings.TrimSpace(w))
		}
	}
	return out
}
