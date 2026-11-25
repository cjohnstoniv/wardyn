// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import "errors"

// errContainUnsupported: wardyn-runnerd ships no serve on this system, so there is nothing to contain through.
var errContainUnsupported = errors.New("wardyn-runnerd contain runs on Linux and macOS only")

func checkStateOwner(string) error { return errContainUnsupported }

func probeServe(string) (bool, error) { return false, errContainUnsupported }
