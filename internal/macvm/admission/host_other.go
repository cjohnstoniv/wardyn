// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package admission

import "errors"

// SystemHost is macOS only; elsewhere every read fails, so nothing is admitted.
func SystemHost() Host { return noHost{} }

var errNotMac = errors.New("admission: the macOS VM ledger runs only on macOS")

type noHost struct{}

func (noHost) BootTime() (int64, error)              { return 0, errNotMac }
func (noHost) MemoryBytes() (uint64, error)          { return 0, errNotMac }
func (noHost) AvailableMemoryBytes() (uint64, error) { return 0, errNotMac }
func (noHost) FreeDiskBytes(string) (int64, error)   { return 0, errNotMac }
func (noHost) Processes() ([]Proc, error)            { return nil, errNotMac }
