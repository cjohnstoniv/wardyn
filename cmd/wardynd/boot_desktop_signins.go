// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
)

// validDesktopSignins: WARDYN_DESKTOP_SIGNINS is on or off, nothing else.
func validDesktopSignins(v string) bool { return v == "on" || v == "off" }

// exitOnBadDesktopSignins refuses to boot on any other WARDYN_DESKTOP_SIGNINS:
// a typo must not leave an operator believing sign-ins are off when they
// meant on, or the reverse.
func exitOnBadDesktopSignins(f *bootFlags) {
	if v := *f.desktopSignins; !validDesktopSignins(v) {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_DESKTOP_SIGNINS=%q: want on or off\n", v)
		os.Exit(2)
	}
}
