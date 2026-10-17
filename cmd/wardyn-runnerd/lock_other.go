// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

// lockSingleInstance is a no-op where flock does not exist; the runner supports Linux and macOS.
func lockSingleInstance(string) (func(), error) { return func() {}, nil }
