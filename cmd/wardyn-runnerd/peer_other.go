// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package main

import "errors"

// fdPeerUID refuses every connection where runnerd cannot prove the peer's uid.
func fdPeerUID(int) (uint32, error) {
	return 0, errors.New("local attach: peer credentials are not supported on this OS")
}
