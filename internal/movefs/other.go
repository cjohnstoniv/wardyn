// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package movefs

import (
	"errors"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// errNotLinux: the helper runs only in a Linux pod.
var errNotLinux = errors.New("movefs: moves read and write files only on Linux")

// Export is Linux only.
func Export(io.Writer, movepack.Set, string) (movepack.Manifest, error) {
	return movepack.Manifest{}, errNotLinux
}

// Extract is Linux only.
func Extract(io.Reader, ExtractOptions) (movepack.Manifest, error) {
	return movepack.Manifest{}, errNotLinux
}
