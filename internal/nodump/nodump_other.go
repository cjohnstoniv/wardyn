// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package nodump

// Disable does nothing off Linux: the images that hold credentials are Linux
// only, and this build exists so the binaries still compile elsewhere.
func Disable() error { return nil }
