// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// setup refuses on Windows before it reads anything, so there is no descriptor to mark or close.
func markTokenFD(string) {}

func closeFD(int) {}
