// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"fmt"
	"os"
)

// main is the default-build stub for wardyn-runner. The docker driver is a
// build-tagged add-on (parity rule — the control-plane/default build carries
// zero target-specific code and no docker client dependency), so the real
// runner in main.go compiles only with `-tags docker`.
func main() {
	fmt.Fprintln(os.Stderr, "wardyn-runner: docker driver not compiled in; rebuild with -tags docker")
	os.Exit(2)
}
