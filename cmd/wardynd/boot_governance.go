// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
)

// exitOnGovernanceFlags refuses to boot on a governance four-eyes setting this build cannot honour,
// exit 2 like every other malformed setting, in every auth mode.
//
// WARDYN_GOVERNANCE_CHANGE_TTL must be positive: a change that expires the moment it is held (or
// before) could never be approved. A value that is not a duration was already refused by the flag.
func exitOnGovernanceFlags(f *bootFlags) {
	if *f.governanceChangeTTL <= 0 {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_GOVERNANCE_CHANGE_TTL=%q: want a positive duration such as 72h\n",
			f.governanceChangeTTL.String())
		os.Exit(2)
	}
}
