// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package main

import (
	"runtime"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/vzengine"
)

const vzLinked = true

// init keeps main on the main thread, which AppKit needs for the guest's window (macvm.ServeScreens).
func init() { runtime.LockOSThread() }

func platformEngine() (macvm.Engine, error) { return vzengine.New(), nil }
