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
//
// WARDYN_GOVERNANCE_MIN_APPROVALS is 0 (unset) or 1 to 9: more than nine people is more than the approval
// rows and the pool read are built for, and a negative count means nothing.
func exitOnGovernanceFlags(f *bootFlags) {
	if n := *f.governanceMinApprovals; n < 0 || n > 9 {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_GOVERNANCE_MIN_APPROVALS=%d: want 0 (unset) or a whole number from 1 to 9\n", n)
		os.Exit(2)
	}
	if *f.governanceChangeTTL <= 0 {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_GOVERNANCE_CHANGE_TTL=%q: want a positive duration such as 72h\n",
			f.governanceChangeTTL.String())
		os.Exit(2)
	}
}
