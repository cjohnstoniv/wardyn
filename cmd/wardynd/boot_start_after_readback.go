// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
)

// exitOnBadStartAfterReadback refuses to boot on a WARDYN_START_AFTER_READBACK other than on or off: a
// typo must not leave an operator believing agents wait for the policy readback when they do not.
func exitOnBadStartAfterReadback(f *bootFlags) {
	if v := *f.startAfterReadback; v != "on" && v != "off" {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_START_AFTER_READBACK=%q: want on or off\n", v)
		os.Exit(2)
	}
}
