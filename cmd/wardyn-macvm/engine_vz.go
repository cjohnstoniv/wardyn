// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package main

import (
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/vzengine"
)

func platformEngine() (macvm.Engine, error) { return vzengine.New(), nil }
