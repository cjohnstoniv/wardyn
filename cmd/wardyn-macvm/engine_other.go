// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !(darwin && arm64 && cgo)

package main

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

const vzLinked = false

func platformEngine() (macvm.Engine, error) {
	return nil, errors.New("macOS VMs need macOS on Apple silicon, and a helper built with cgo")
}
